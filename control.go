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
// called from many goroutines at once: commands are correlated with their
// replies by tmux's command number, so more than one may be outstanding.
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

	done      chan struct{}
	closeOnce sync.Once
	doneOnce  sync.Once
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
	// Number is the tmux command number the reply was correlated by.
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
		// One slot beyond the caller's buffer is reserved for loss reports
		// and the terminal event, so that a burst which fills the channel can
		// still say that it did. See emit.
		events: make(chan Event, cfg.eventBuffer+1),
		done:   make(chan struct{}),
		stderr: &syncBuffer{},
	}
}

// start attaches the client to a pair of pipes and launches the reader.
func (cc *ControlClient) start(stdin io.WriteCloser, stdout io.ReadCloser) {
	cc.stdin, cc.stdout = stdin, stdout
	go cc.readLoop()
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

// Do sends a command and waits for its reply.
//
// cmd is a tmux command line, parsed by tmux itself; use [ControlClient.DoArgs]
// to have arguments quoted for you. It must be a single line: a newline would
// be a second command, and an empty line detaches the control client.
//
// It is safe to call Do concurrently. Replies are matched to commands by
// tmux's command number, so several may be outstanding at once.
//
// A command tmux answers with %error yields a [*ControlError]; the reply is
// still returned with the error body in its output.
func (cc *ControlClient) Do(ctx context.Context, cmd string) (Reply, error) {
	if strings.TrimSpace(cmd) == "" {
		return Reply{}, errors.New("gotmucks: empty control command (an empty line detaches the client)")
	}
	if strings.ContainsAny(cmd, "\n\r") {
		return Reply{}, errors.New("gotmucks: control command contains a newline")
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
// was called — and an error wrapping [ErrServerExited] otherwise. Unlike the
// [Exited] event, which is dropped if the event channel is full, this is
// always available.
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
// tmux to exit, killing it if it outstays [WithCloseTimeout]. Close is
// idempotent and safe to call concurrently with anything else.
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

		// An empty line is how a control client detaches.
		cc.writeMu.Lock()
		_, _ = io.WriteString(cc.stdin, "\n")
		cc.writeMu.Unlock()
		_ = cc.stdin.Close()

		select {
		case <-cc.done:
		case <-time.After(cc.cfg.closeTimeout):
			if cc.cmd != nil && cc.cmd.Process != nil {
				_ = cc.cmd.Process.Kill()
			}
			<-cc.done
		}
		if cc.cmd == nil {
			return // in-memory connection; there is no process to reap
		}
		err = cc.cmd.Wait()
		if err != nil && cc.isCleanExit(err) {
			err = nil
		}
	})
	return err
}

// isCleanExit reports whether a process error is the expected consequence of
// detaching or killing the client rather than a real failure.
func (cc *ControlClient) isCleanExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	// tmux exits 0 on a clean detach. A signalled exit is our own Kill.
	return exitErr.ExitCode() <= 0
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes os/exec makes
// from its own goroutine while the owner reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
