package gotmucks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ControlClient is a persistent connection to a tmux server in control mode.
//
// Control mode is a two-way protocol on one pair of pipes: commands go down,
// and command replies, live pane output and asynchronous notifications come
// back interleaved. A single goroutine owns the read side and dispatches by
// line prefix; nothing else touches the pipe.
//
// The connection uses tmux's -C. The -CC form documented for applications
// additionally puts the terminal out of canonical mode, which makes tmux call
// tcgetattr on its standard input; against a pipe that fails and tmux exits at
// once. See [WithDoubleControlMode].
//
// A ControlClient is safe for concurrent use. [ControlClient.Do] may be
// called from many goroutines at once: more than one command may be
// outstanding, and each reply is bound to the command that earned it by queue
// order rather than by tmux's command number, which cannot be predicted. See
// beginBlock.
type ControlClient struct {
	cfg     config
	version Version

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *syncBuffer

	// writeMu serialises writes to tmux and, with them, the enqueueing of
	// pending commands. Holding one lock for both is what makes the pending
	// queue's order the same as the order tmux receives commands in, which is
	// what lets %begin be bound to a command without guessing tmux's
	// numbering.
	writeMu sync.Mutex

	mu        sync.Mutex
	queue     []*pending // written, awaiting %begin
	current   *pending   // the open block, if any
	taps      map[PaneID]*tap
	closed    bool
	userClose bool // Close was called, so an EOF is expected
	sawExit   bool // tmux sent %exit
	attached  SessionID
	exitErr   error
	exitMsg   string

	events       chan Event
	pendingDrops atomic.Uint64
	totalDrops   atomic.Uint64

	// startup is closed once commands may be written. tmux opens a block for
	// the command that started the connection before it reads any of ours,
	// and a command written before the reader has absorbed that block would
	// be bound to it. See awaitStartup.
	startup     chan struct{}
	startupOnce sync.Once

	// sawFirstLine is owned by the reader goroutine and needs no lock. It
	// distinguishes the block tmux opens for itself, which arrives first,
	// from every later one.
	sawFirstLine bool

	// killed records that Close ran out of patience and killed tmux, so that
	// the resulting signalled exit is not mistaken for a crash.
	killed atomic.Bool

	done      chan struct{}
	closeOnce sync.Once
	doneOnce  sync.Once
	stdinOnce sync.Once

	// reaped is closed once the tmux process has been waited for. It is what
	// makes the last step of the teardown observable, since the reader does
	// it after closing done so that nothing waiting on the connection's end
	// is held up by a process that is slow to die.
	reaped   chan struct{}
	waitOnce sync.Once
	waitErr  error
}

// pending is a command written to tmux and awaiting its reply block.
type pending struct {
	cmd       string
	number    int
	lines     []string
	flags     int
	replyTime time.Time
	failed    bool
	// answered distinguishes "tmux replied" from "the connection ended
	// first". A reply may legitimately be command number 0 with no output, so
	// the fields alone cannot say.
	answered bool
	// orphan marks a block tmux opened for a command this client did not
	// send. Its body is absorbed and nobody is waiting on it.
	orphan bool
	// err is set when the reader could not deliver a trustworthy reply: the
	// block was abandoned, or closed by a terminator belonging to another
	// one. The command fails with it rather than being handed a body that may
	// not be its own.
	err error

	done chan struct{}
	once sync.Once
}

func (p *pending) finish() { p.once.Do(func() { close(p.done) }) }

// tap is a per-pane output channel registered by [ControlClient.Output].
type tap struct {
	ch     chan []byte
	drops  atomic.Uint64
	total  atomic.Uint64
	closed bool
}

// Reply is the result of a control-mode command.
type Reply struct {
	// Number is the command number tmux assigned. It identifies the block on
	// the wire; it is not what the reply was matched by, since the numbers
	// are neither predictable nor contiguous.
	Number int
	// Time is the timestamp tmux reported on the closing %end or %error.
	Time time.Time
	// Flags is the flags word from the block terminator.
	Flags int
	// Output is the block body, one entry per line, with no trailing blank.
	Output []string
}

// String joins the reply's output lines with newlines.
func (r Reply) String() string { return strings.Join(r.Output, "\n") }

// Rows parses the reply's output against a format spec, so that a
// control-mode query is typed the same way a one-shot [Client.Query] is.
func (r Reply) Rows(spec FormatSpec) ([]Row, error) { return ParseRows(spec, r.Output) }

// Connect opens a control-mode connection.
//
// ctx bounds the connection setup only. The connection itself lives until
// [ControlClient.Close], tmux exits, or the process dies — cancelling ctx
// afterwards does not close it, because a connection whose lifetime was tied
// to a setup context would be surprising to hand to another goroutine.
//
// By default the connection creates a new session. Use [WithAttach] to attach
// to an existing one, or [WithControlArgs] for anything else.
func Connect(ctx context.Context, opts ...Option) (*ControlClient, error) {
	cfg := newConfig(opts)
	cc := newControlClient(cfg)

	if !cfg.skipVersionCk {
		v, err := New(opts...).Version(ctx)
		if err != nil {
			return nil, fmt.Errorf("gotmucks: reading tmux version: %w", err)
		}
		if !v.AtLeast(MinimumVersion) {
			return nil, fmt.Errorf("gotmucks: tmux %s is older than the required %s: %w",
				v, MinimumVersion, ErrUnsupportedVersion)
		}
		cc.version = v
	}

	tmuxArgs := append([]string{cfg.controlFlag()}, controlCommand(cfg)...)
	args, _ := cfg.argv(tmuxArgs)

	// Deliberately exec.Command and not exec.CommandContext: ctx bounds
	// setup, not the connection's life.
	cmd := exec.Command(cfg.binary, args...)
	cmd.Env = cfg.environ()
	cmd.Stderr = cc.stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("gotmucks: control stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("gotmucks: control stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("gotmucks: starting %s %s: %w", cfg.binary, strings.Join(args, " "), err)
	}

	cc.cmd = cmd
	cc.start(stdin, stdout)

	// Prove the connection works before handing it back. If tmux could not
	// start or the requested session does not exist, it exits immediately and
	// this turns that into a Connect error rather than a surprise later.
	if _, err := cc.Do(ctx, "list-sessions -F '#{session_id}'"); err != nil {
		_ = cc.Close()
		if msg := strings.TrimSpace(cc.stderr.String()); msg != "" {
			return nil, fmt.Errorf("gotmucks: control connection failed: %s: %w", msg, err)
		}
		return nil, fmt.Errorf("gotmucks: control connection failed: %w", err)
	}
	return cc, nil
}

// newControlClient builds an unstarted client. [Connect] then attaches it to
// a tmux process; the tests attach it to in-memory pipes, which is how the
// reader and its dispatch are covered without a subprocess.
func newControlClient(cfg config) *ControlClient {
	return &ControlClient{
		cfg:  cfg,
		taps: make(map[PaneID]*tap),
		// Two slots beyond the caller's buffer are reserved: one for a loss
		// report, so that a burst which fills the channel can still say that
		// it did, and one for the terminal event, so that a loss report
		// cannot take the slot the terminal event needs. See emit.
		events:  make(chan Event, cfg.eventBuffer+2),
		done:    make(chan struct{}),
		startup: make(chan struct{}),
		reaped:  make(chan struct{}),
		stderr:  &syncBuffer{},
	}
}

// start attaches the client to a pair of pipes and launches the reader.
func (cc *ControlClient) start(stdin io.WriteCloser, stdout io.ReadCloser) {
	cc.stdin, cc.stdout = stdin, stdout
	go cc.readLoop()

	// The reader lifts the startup barrier as soon as it knows whether tmux
	// opened a block of its own. This is the backstop for one that says
	// nothing at all until it is given a command: without it such a tmux
	// would never be sent one, which is a worse failure than the race the
	// barrier exists to remove.
	time.AfterFunc(startupGrace, cc.releaseStartup)
}

// startupGrace bounds the wait for tmux's own opening block. Every release in
// the support matrix emits it within milliseconds of starting — verified for
// attach-session and new-session on 3.2a, including when the first command
// was already sitting in the pipe — so this expires only against a tmux that
// behaves differently from any known one.
const startupGrace = 2 * time.Second

// awaitStartup blocks until it is safe to write a command.
//
// tmux opens a block for the command that started the connection, and does so
// before it reads anything from standard input. A command written before the
// reader has absorbed that block would be bound to it by the queue-order
// rule, and the caller would be handed the start command's output as its own
// with no error, every later reply then shifted by one. Holding the first
// write until the block has been seen removes the race rather than trying to
// detect it afterwards.
func (cc *ControlClient) awaitStartup(ctx context.Context) error {
	select {
	case <-cc.startup:
		return nil
	default:
	}
	select {
	case <-cc.startup:
		return nil
	case <-cc.done:
		return cc.terminalErr()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseStartup lifts the barrier. It is idempotent: the reader calls it on
// the first line that rules a startup block out, on every block terminator,
// and on teardown.
func (cc *ControlClient) releaseStartup() {
	cc.startupOnce.Do(func() { close(cc.startup) })
}

// started reports whether a command may already have been written. While it
// is false the queue is certainly empty, so any block that opens is certainly
// not a reply to one of ours.
func (cc *ControlClient) started() bool {
	select {
	case <-cc.startup:
		return true
	default:
		return false
	}
}

// controlCommand is the tmux command a control connection issues on startup.
func controlCommand(cfg config) []string {
	switch {
	case len(cfg.controlArgs) > 0:
		return cfg.controlArgs
	case cfg.attach != "":
		return []string{"attach-session", "-t", string(cfg.attach)}
	default:
		return []string{"new-session"}
	}
}

// Version reports the tmux version this connection checked at Connect time.
// It is the zero Version when [WithoutVersionCheck] was used.
func (cc *ControlClient) Version() Version { return cc.version }

// AttachedSession reports the session tmux said this client is attached to,
// learned from the %session-changed notification sent at startup. It is empty
// if tmux has not said.
func (cc *ControlClient) AttachedSession() SessionID {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.attached
}

// Events returns the notification stream.
//
// The channel is closed when the connection ends. It is buffered, and the
// reader never blocks on it: a consumer that stops reading loses events
// rather than stalling the connection — which would stall command replies and
// every other pane too — and is told what it lost through [EventsDropped] and
// [ControlClient.Dropped].
//
// Every notification appears here, including pane output. Calling
// [ControlClient.Output] adds a per-pane tap; it does not divert anything
// from this stream.
func (cc *ControlClient) Events() <-chan Event { return cc.events }

// Dropped reports how many events have been discarded because the event
// channel was full.
func (cc *ControlClient) Dropped() uint64 { return cc.totalDrops.Load() }

// Output returns a channel carrying one pane's unescaped output bytes.
//
// The first call for a pane registers the tap; later calls for the same pane
// return the same channel. The channel is closed when the connection ends.
// Like [ControlClient.Events] it is buffered and lossy rather than blocking;
// drops are reported as [OutputDropped] on the event stream.
//
// Bytes are delivered as tmux framed them, which is not line-oriented: one
// receive is one %output notification, not one line.
//
// A tap lasts until [ControlClient.Untap] removes it or the connection ends.
func (cc *ControlClient) Output(pane PaneID) <-chan []byte {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if t, ok := cc.taps[pane]; ok {
		return t.ch
	}
	t := &tap{ch: make(chan []byte, cc.cfg.outputBuffer)}
	if cc.closed {
		// The connection is already gone; hand back a closed channel so the
		// caller's range loop terminates immediately instead of hanging.
		close(t.ch)
		t.closed = true
	}
	cc.taps[pane] = t
	return t.ch
}

// Untap removes the tap [ControlClient.Output] registered for a pane and
// closes its channel. A later Output for the same pane registers a fresh one.
//
// Taps are otherwise permanent, so a long-lived connection to a session that
// churns through panes would accumulate a channel and a buffer for every pane
// it ever saw. Untapping a pane that has none does nothing.
func (cc *ControlClient) Untap(pane PaneID) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	t, ok := cc.taps[pane]
	if !ok {
		return
	}
	delete(cc.taps, pane)
	if !t.closed {
		t.closed = true
		close(t.ch)
	}
}

// Do sends a command and waits for its reply.
//
// cmd is one tmux command line, parsed by tmux itself; use
// [ControlClient.DoArgs] to have arguments quoted for you. It must be a single
// line: a newline would be a second command, and an empty line detaches the
// control client. It must also be a single command: tmux answers each command
// of a ";"-separated list with a block of its own, so a list would leave
// blocks over for the commands after it, and an unquoted ";" is rejected for
// that reason. Send the commands one at a time instead — Do may be called
// concurrently.
//
// The same caveat applies, undetectably, to a command that makes tmux queue
// further commands on this client. Verified on 3.2a:
// "if-shell 'true' { list-sessions ; list-sessions }" produces three blocks,
// one for the if-shell and one for each command in the braces, and all three
// are marked as this client's. Nothing distinguishes the extra ones from
// replies, so do not send a command block through a connection that other
// goroutines are using.
//
// It is safe to call Do concurrently: several commands may be outstanding at
// once, and each reply is bound to the command that earned it by the order the
// commands were written in.
//
// A command tmux answers with %error yields a [*ControlError]; the reply is
// still returned with the error body in its output. A [*ProtocolError] means
// tmux said something about this command that could not be made sense of, and
// the reply must not be trusted.
func (cc *ControlClient) Do(ctx context.Context, cmd string) (Reply, error) {
	if strings.TrimSpace(cmd) == "" {
		return Reply{}, errors.New("gotmucks: empty control command (an empty line detaches the client)")
	}
	// A newline would be a second command, and an empty line detaches the
	// client. A NUL is worse than either: tmux reads the command line as a C
	// string, so the command runs truncated at the NUL and reports success —
	// verified on 3.2a, where rename-session with a NUL in the new name
	// renamed the session to the part before it.
	if i := strings.IndexAny(cmd, "\n\r\x00"); i >= 0 {
		return Reply{}, fmt.Errorf(
			"gotmucks: control command contains %q at byte %d and cannot be sent as written",
			cmd[i:i+1], i)
	}
	if i := commandSeparator(cmd); i >= 0 {
		return Reply{}, fmt.Errorf(
			"gotmucks: control command has an unquoted %q at byte %d, which tmux reads as the "+
				"start of a second command; each command is answered with its own reply block, so "+
				"the extra one would be delivered to whichever command follows. Send them separately",
			";", i)
	}
	if err := cc.awaitStartup(ctx); err != nil {
		return Reply{}, err
	}

	p := &pending{cmd: cmd, done: make(chan struct{})}

	cc.writeMu.Lock()
	cc.mu.Lock()
	if cc.closed {
		cc.mu.Unlock()
		cc.writeMu.Unlock()
		return Reply{}, cc.terminalErr()
	}
	cc.queue = append(cc.queue, p)
	cc.mu.Unlock()

	_, werr := io.WriteString(cc.stdin, cmd+"\n")
	cc.writeMu.Unlock()

	if werr != nil {
		cc.unqueue(p)
		return Reply{}, fmt.Errorf("gotmucks: writing control command: %w", werr)
	}

	select {
	case <-p.done:
	case <-cc.done:
		// The reader failed every pending command on its way out, so p.done
		// is closed too; fall through to read whatever it recorded.
		<-p.done
	case <-ctx.Done():
		// Leave the pending entry registered. tmux will still answer, the
		// reader will still bind the reply, and the buffered done channel
		// costs nothing. Abandoning it would desynchronise the queue.
		return Reply{}, ctx.Err()
	}

	reply := Reply{Number: p.number, Output: p.lines, Flags: p.flags, Time: p.replyTime}
	if p.err != nil {
		return reply, p.err
	}
	if !p.answered {
		return reply, cc.terminalErr()
	}
	if p.failed {
		return reply, &ControlError{
			Command: cmd,
			Number:  p.number,
			Message: strings.Join(p.lines, "\n"),
		}
	}
	return reply, nil
}

// commandSeparator reports the offset of a ';' tmux would read as ending one
// command and starting another, or -1 if there is none.
//
// Quoting is what makes the difference, and tmux's rules for it are the ones
// applied here: single quotes take everything literally, which is why
// quoteArg prefers them, while a backslash escapes the next byte anywhere
// else. Verified on 3.2a, where "list-sessions -F 'A;B'" is one command
// producing one block.
func commandSeparator(cmd string) int {
	var quote byte
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case c == '\\' && quote != '\'':
			i++ // an escaped byte is data, ";" included
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ';':
			return i
		}
	}
	return -1
}

// DoArgs sends a command built from separate arguments, each quoted for
// tmux's parser. This is the safer form when any argument is caller data.
func (cc *ControlClient) DoArgs(ctx context.Context, args ...string) (Reply, error) {
	if len(args) == 0 {
		return Reply{}, errors.New("gotmucks: no command given")
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = quoteArg(a)
	}
	return cc.Do(ctx, strings.Join(parts, " "))
}

func (cc *ControlClient) terminalErr() error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.terminalErrLocked()
}

func (cc *ControlClient) terminalErrLocked() error {
	switch {
	case cc.exitErr != nil:
		return cc.exitErr
	case cc.exitMsg != "":
		return fmt.Errorf("gotmucks: %s: %w", cc.exitMsg, ErrServerExited)
	default:
		return ErrServerExited
	}
}

// unqueue removes a pending command that was never written.
func (cc *ControlClient) unqueue(p *pending) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for i, q := range cc.queue {
		if q == p {
			cc.queue = append(cc.queue[:i], cc.queue[i+1:]...)
			break
		}
	}
}

// Wait blocks until the connection ends and reports why.
//
// It returns nil for a clean exit — tmux sent %exit, or [ControlClient.Close]
// was called — and an error wrapping [ErrServerExited] otherwise. It reports
// the same thing as the [Exited] event, for a caller that is not reading the
// event stream.
func (cc *ControlClient) Wait(ctx context.Context) error {
	select {
	case <-cc.done:
		cc.mu.Lock()
		defer cc.mu.Unlock()
		return cc.exitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done returns a channel closed when the connection ends.
func (cc *ControlClient) Done() <-chan struct{} { return cc.done }

// Err reports why the connection ended, or nil while it is still open or if
// it ended cleanly.
func (cc *ControlClient) Err() error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.exitErr
}

// Stderr returns whatever tmux wrote to standard error. It is normally empty
// and is worth reading when Connect or a command fails unexpectedly.
func (cc *ControlClient) Stderr() string { return cc.stderr.String() }

// Close ends the connection.
//
// It writes the empty line that detaches a control client, then waits for
// tmux to exit, killing it if it outstays [WithCloseTimeout]. That timeout
// bounds the whole of Close, the detach write included. Close is idempotent
// and safe to call concurrently with anything else.
//
// It returns an error if tmux did not end the way it was asked to — an exit
// status of its own, or a signal this package did not send.
//
// Calling it is good manners rather than a requirement for tidiness: the
// reader reaps the process whenever the connection ends, so a caller that
// watches [ControlClient.Done] instead leaks nothing. What Close adds is the
// clean detach and the report of how tmux went.
//
// Closing detaches the control client; it does not kill the session or the
// server. Use [Client.KillSession] for that.
func (cc *ControlClient) Close() error {
	var err error
	cc.closeOnce.Do(func() {
		cc.mu.Lock()
		cc.closed = true
		cc.userClose = true
		cc.mu.Unlock()

		// The detach write is unbounded: a tmux that has stopped draining its
		// input blocks it for as long as it likes, and it would then consume
		// the close timeout rather than being covered by it. Running it on its
		// own goroutine is what makes WithCloseTimeout bound the whole of
		// Close rather than only the part after the write returned.
		go cc.detach()

		select {
		case <-cc.done:
		case <-time.After(cc.cfg.closeTimeout):
			// Closing the pipe releases a detach write that is still stuck.
			cc.closeStdin()
			cc.killed.Store(true)
			if cc.cmd != nil && cc.cmd.Process != nil {
				_ = cc.cmd.Process.Kill()
			}
			<-cc.done
		}
		err = cc.reap()
	})
	return err
}

// detach writes the empty line that ends a control client's connection, then
// closes the pipe so that tmux sees end of file either way.
func (cc *ControlClient) detach() {
	cc.writeMu.Lock()
	_, _ = io.WriteString(cc.stdin, "\n")
	cc.writeMu.Unlock()
	cc.closeStdin()
}

// closeStdin closes the command pipe once, whoever gets there first.
func (cc *ControlClient) closeStdin() {
	cc.stdinOnce.Do(func() {
		if cc.stdin != nil {
			_ = cc.stdin.Close()
		}
	})
}

// reap waits for the tmux process and reports whether it ended cleanly.
//
// It is idempotent, and both the reader and Close call it: the reader so that
// a caller who watches [ControlClient.Done] and never calls Close still does
// not leak a zombie and the pipes that came with it, and Close because it is
// the one that reports the result.
func (cc *ControlClient) reap() error {
	cc.waitOnce.Do(func() {
		if cc.cmd != nil { // an in-memory connection has no process
			err := cc.cmd.Wait()
			if err != nil && cc.isCleanExit(err) {
				err = nil
			}
			cc.waitErr = err
		}
		close(cc.reaped)
	})
	return cc.waitErr
}

// isCleanExit reports whether a process error is the expected consequence of
// detaching or killing the client rather than a real failure.
func (cc *ControlClient) isCleanExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return cleanExitCode(exitErr.ExitCode(), cc.killed.Load())
}

// cleanExitCode decides whether an exit status is one this package caused.
//
// tmux exits 0 on a clean detach. ExitCode reports -1 for an exit by any
// signal, which covers this package's own Kill and equally a tmux that died on
// SIGSEGV, so only the former is forgiven: a server that crashed without the
// caller being told is exactly what Close's error is for.
func cleanExitCode(code int, killed bool) bool {
	if code < 0 {
		return killed
	}
	return code == 0
}

// maxStderr caps how much of tmux's standard error is kept.
//
// A control connection lives as long as the caller wants it to, and a tmux
// writing a warning a minute would otherwise grow this without bound. The
// earliest bytes are the ones worth keeping: what appears here is
// configuration errors and startup complaints, and a later flood is far more
// likely to be repetition than news.
const maxStderr = 64 << 10

// syncBuffer is a capped buffer safe for the concurrent writes os/exec makes
// from its own goroutine while the owner reads.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	dropped int
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	room := maxStderr - b.buf.Len()
	switch {
	case room <= 0:
		b.dropped += len(p)
	case len(p) > room:
		b.buf.Write(p[:room])
		b.dropped += len(p) - room
	default:
		return b.buf.Write(p)
	}
	// The whole write is reported as accepted: os/exec's copier abandons the
	// stream on a short write, and there is nothing to be gained by telling
	// it that a diagnostic buffer is full.
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dropped == 0 {
		return b.buf.String()
	}
	return fmt.Sprintf("%s\n[%d further bytes of tmux stderr discarded]", b.buf.String(), b.dropped)
}
