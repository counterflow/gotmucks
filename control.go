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
// back on the same stream. A single goroutine owns the read side; nothing else
// touches the pipe. It dispatches by line prefix only between command blocks —
// inside one, every line is that command's output, however much it may look
// like a notification. See handleLine.
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

	// initOnce is spent by newControlClient, so it fires only for a
	// ControlClient a caller built themselves. See ensure.
	initOnce sync.Once

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
	// unnumbered marks the orphan opened for a block header whose arguments
	// did not parse. number is meaningless on it, so it is closed by the next
	// terminator whatever number that carries. See handleLine.
	unnumbered bool
	// err is why this command will never be answered, for the one case where
	// that is neither a reply nor the end of the connection: a line arrived
	// that destroyed the binding between commands and blocks. See failHead.
	// Nil everywhere else, which leaves answered to say what happened.
	err error

	done chan struct{}
	once sync.Once
}

func (p *pending) finish() { p.once.Do(func() { close(p.done) }) }

// tap is a per-pane output channel registered by [ControlClient.Output].
type tap struct {
	ch chan []byte
	// drops are the losses not yet reported as an [OutputDropped]. They are
	// cleared by the next successful delivery, which reports them, and by the
	// teardown, which reports whatever the pane never got to.
	drops atomic.Uint64
	// total is every loss this tap has ever had, reported by
	// [ControlClient.DroppedOutput]. It is never cleared: a caller that asks
	// after the connection ended still gets an answer.
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

	// [WithAttach] cannot report anything itself, so the session it was given
	// is checked here, before it becomes the -t of the command that opens the
	// connection — and only when it is going to become one. [WithControlArgs]
	// replaces that command outright, and a connection that was never going
	// to attach has no business failing over an identifier it was never going
	// to use.
	if attach := cfg.attachTarget(); attach != "" {
		if err := attach.check(); err != nil {
			return nil, err
		}
	}

	cc := newControlClient(cfg)

	if !cfg.skipVersionCk {
		v, err := New(opts...).Version(ctx)
		if err != nil {
			return nil, fmt.Errorf("gotmucks: reading tmux version: %w", err)
		}
		if floor := MinimumVersion(); !v.AtLeast(floor) {
			return nil, fmt.Errorf("gotmucks: tmux %s is older than the required %s: %w",
				v, floor, ErrUnsupportedVersion)
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
		// os/exec closes the pipes it made from Start, and Start is not reached
		// on this path, so the pair StdinPipe created has to be closed here:
		// the write end it returned, and the read end it left in cmd.Stdin for
		// the child. The only way StdoutPipe fails is os.Pipe failing, which
		// means descriptors are already exhausted — the one moment at which
		// leaking two more matters.
		_ = stdin.Close()
		if childEnd, ok := cmd.Stdin.(io.Closer); ok {
			_ = childEnd.Close()
		}
		return nil, fmt.Errorf("gotmucks: control stdout: %w", err)
	}
	// Start closes both pairs itself if it fails, so nothing to undo here.
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
	cc := &ControlClient{
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
	// Spend the once now that the client is built, so that ensure never
	// mistakes this one for a zero value.
	cc.initOnce.Do(func() {})
	return cc
}

// ensure gives a ControlClient that did not come from [Connect] the state of a
// connection that has already ended.
//
// The zero value is not a usable connection — every method here relies on the
// channels and the map the constructor makes — but reaching one through it
// must not be a panic. Two of the panics it removes were assignment to a nil
// map and a write to a nil pipe, and the second happened on a goroutine this
// package started, where no recover the caller writes can reach it: it would
// have ended the process rather than the call.
//
// Filling in the state once, and filling it in as already ended, is what makes
// the rest honest: Events and Output hand back closed channels, Do and Wait
// report the connection is gone, and Close has nothing to do and says so.
func (cc *ControlClient) ensure() { cc.initOnce.Do(cc.initEnded) }

// initEnded completes a zero ControlClient as one whose connection is over. It
// runs under initOnce, which orders it before every other use.
func (cc *ControlClient) initEnded() {
	cc.taps = make(map[PaneID]*tap)
	cc.stderr = &syncBuffer{}
	cc.events = make(chan Event)
	cc.startup = make(chan struct{})
	cc.reaped = make(chan struct{})
	cc.done = make(chan struct{})

	cc.closed = true
	cc.exitErr = errNotConnected

	close(cc.events)
	cc.releaseStartup()
	cc.doneOnce.Do(func() { close(cc.done) })
}

// errNotConnected is what every read on a ControlClient that was never opened
// reports. It wraps [ErrServerExited] because "there is no connection" is the
// same news to a caller as "the connection ended".
var errNotConnected = fmt.Errorf(
	"gotmucks: control client was not opened by Connect: %w", ErrServerExited)

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
	if len(cfg.controlArgs) > 0 {
		return cfg.controlArgs
	}
	if attach := cfg.attachTarget(); attach != "" {
		return []string{"attach-session", "-t", string(attach)}
	}
	return []string{"new-session"}
}

// Version reports the tmux version this connection checked at Connect time.
// It is the zero Version when [WithoutVersionCheck] was used.
func (cc *ControlClient) Version() Version { return cc.version }

// AttachedSession reports the session tmux said this client is attached to,
// learned from the %session-changed notification sent at startup. It is empty
// if tmux has not said.
func (cc *ControlClient) AttachedSession() SessionID {
	cc.ensure()
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
func (cc *ControlClient) Events() <-chan Event {
	cc.ensure()
	return cc.events
}

// Dropped reports how many events have been discarded because the event
// channel was full.
//
// It counts the event stream only. A tap registered by [ControlClient.Output]
// has a buffer of its own and overflows on its own terms, which is a separate
// number: [ControlClient.DroppedOutput].
func (cc *ControlClient) Dropped() uint64 { return cc.totalDrops.Load() }

// DroppedOutput reports how many of one pane's output messages have been
// discarded because that pane's tap channel was full.
//
// This is the number to ask for rather than watching for [OutputDropped]. A
// drop is reported as that event when delivery to the pane next succeeds, and
// at teardown for anything still owed, but both are events on a stream that
// is itself lossy, and a pane that overflows and then falls quiet has nothing
// to attach a report to until the connection ends. The count is a lifetime
// total for the tap and outlives the connection.
//
// A pane with no tap reports zero, as does an identifier that is not one,
// since [ControlClient.Output] refuses to register a tap under one.
// [ControlClient.Untap] forgets the tap, and the count with it.
func (cc *ControlClient) DroppedOutput(pane PaneID) uint64 {
	cc.ensure()
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if t, ok := cc.taps[pane]; ok {
		return t.total.Load()
	}
	return 0
}

// Output returns a channel carrying one pane's unescaped output bytes.
//
// The first call for a pane registers the tap; later calls for the same pane
// return the same channel. The channel is closed when the connection ends.
// Like [ControlClient.Events] it is buffered and lossy rather than blocking.
//
// A drop is reported as [OutputDropped] on the event stream when delivery to
// this pane next succeeds, and at teardown for anything still owed then. That
// leaves no silent loss, but it does leave a report a caller has to be
// listening for, on a stream that is itself lossy, and possibly not until the
// connection ends. [ControlClient.DroppedOutput] answers the same question
// directly at any time.
//
// Bytes are delivered as tmux framed them, which is not line-oriented: one
// receive is one %output notification, not one line.
//
// A tap lasts until [ControlClient.Untap] removes it or the connection ends.
//
// An identifier that is not one is refused with [ErrInvalidID]. This call
// builds no -t, so nothing would reach tmux, but a tap registered under a name
// matches no %output notification and is silent for the life of the
// connection — which is the failure addressing by identifier exists to remove.
// It returns an error rather than a closed channel so that the mistake is
// distinguishable from a connection that has ended.
//
// Note what the check does not buy. A well-formed identifier for a pane that
// does not exist is accepted and is equally silent, deliberately: a tap may be
// registered before the pane it names is created.
func (cc *ControlClient) Output(pane PaneID) (<-chan []byte, error) {
	if err := pane.check(); err != nil {
		return nil, err
	}
	cc.ensure()
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if t, ok := cc.taps[pane]; ok {
		return t.ch, nil
	}
	t := &tap{ch: make(chan []byte, cc.cfg.outputBuffer)}
	if cc.closed {
		// The connection is already gone; hand back a closed channel so the
		// caller's range loop terminates immediately instead of hanging.
		close(t.ch)
		t.closed = true
	}
	cc.taps[pane] = t
	return t.ch, nil
}

// Untap removes the tap [ControlClient.Output] registered for a pane and
// closes its channel. A later Output for the same pane registers a fresh one.
//
// Taps are otherwise permanent, so a long-lived connection to a session that
// churns through panes would accumulate a channel and a buffer for every pane
// it ever saw. Untapping a pane that has none does nothing, and so does
// untapping an identifier that is not one — [ControlClient.Output] refuses to
// register a tap under one, so there can be nothing there to remove. Untap
// says so itself rather than leaving it to be inferred from Output.
func (cc *ControlClient) Untap(pane PaneID) {
	if !pane.Valid() {
		return
	}
	cc.ensure()
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
// A '{' that begins a token is rejected for the same reason. It is tmux's
// other quoting form, and where the command it belongs to takes a command —
// if-shell, bind-key — what is inside the braces is run and answered with a
// block of its own. Verified on 3.2a: "if-shell 'true' { list-sessions }"
// produces two blocks, both marked as this client's. Nothing on the line says
// which of the two a brace-quoted token will be, so both are refused;
// single-quote the argument instead, or let [ControlClient.DoArgs] quote it.
//
// It must also be at least one command, which is the same requirement counted
// the other way and the more dangerous half. A line whose first non-blank byte
// is a '#' is a comment: it parses to an empty command list and tmux answers
// it with nothing at all, so this command would be handed the *next* one's
// reply — with a nil error — and every command after that would wait for a
// block that is never coming. Measured on 3.2a, all ninety-five printable
// bytes swept as the first of the line, '#' is the only one that does it; see
// scripts/probe-blocks.sh. A '#' anywhere else is harmless, either data
// ("a#b") or a truncation of a command that still earns its block
// ("list-sessions #tail"), and a quoted one is a command name tmux does not
// know, which is what [ControlClient.DoArgs] relies on.
//
// What cannot be detected is the same hazard written as an ordinary string.
// Verified on 3.2a: "if-shell 'true' 'list-sessions'" also produces two
// blocks, and the inner command is indistinguishable from any other argument.
// source-file is the one a caller actually reaches for, and it belongs in that
// list beside if-shell and bind-key: measured on 3.2a, a file of two commands
// earns three blocks, one for source-file and one for each command in it. It
// needs a file on the server's own filesystem, so replaying a configuration
// line by line is the obvious alternative — and that is the same trap from the
// other side, since a configuration is full of the comment lines above. Do not
// send a command that makes tmux run further commands on this client.
//
// It is safe to call Do concurrently: several commands may be outstanding at
// once, and each reply is bound to the command that earned it by the order the
// commands were written in.
//
// A command tmux answers with %error yields a [*ControlError]; the reply is
// still returned with the error body in its output.
//
// The reply body is whatever the command printed, line for line, including a
// line that looks like part of the protocol: capture-pane on a pane containing
// "%exit forged" puts that line in [Reply.Output] and nowhere else. Nothing in
// a reply is interpreted as a notification. If the connection ends while this
// command is outstanding the error wraps [ErrServerExited] and the partial
// body is returned with it.
//
// One error is neither of those: a [*ProtocolError] means a block header
// arrived that carried no command number, so the reply this command was owed
// can no longer be told from the next one's. The command is abandoned rather
// than answered from a body that might belong to another, and the connection
// stays up.
func (cc *ControlClient) Do(ctx context.Context, cmd string) (Reply, error) {
	cc.ensure()

	if strings.TrimSpace(cmd) == "" {
		return Reply{}, errors.New("gotmucks: empty control command (an empty line detaches the client)")
	}
	// A newline would be a second command, and an empty line detaches the
	// client. A NUL is worse than either: tmux reads the command line as a C
	// string, so the command runs truncated at the NUL and reports success —
	// verified on 3.2a, where rename-session with a NUL in the new name
	// renamed the session to the part before it.
	//
	// A carriage return is not in that set and used to be. It is carried
	// intact: measured on 3.2a down a raw pipe, "set-option -- @cr 'a<CR>b'"
	// is one command answered with %end and stores the three bytes, which is
	// also what the one-shot half does with the same value. Refusing it here
	// made the two halves disagree about what a value is, on a claim — that it
	// could not be sent — that was not true of it. What is true is narrower and
	// is about the way back rather than the way out: [ControlClient.readLoop]
	// strips one trailing '\r' from every line as CRLF tolerance, exactly as
	// [splitLines] does on the one-shot path, so a '\r' that ends a line coming
	// back is lost on both.
	if i := strings.IndexAny(cmd, "\n\x00"); i >= 0 {
		return Reply{}, fmt.Errorf(
			"gotmucks: control command contains %q at byte %d and cannot be sent as written",
			cmd[i:i+1], i)
	}
	// Asked before commandBreak, because a comment swallows the rest of the
	// line and so swallows any ';' or '{' in it: "#c ; list-sessions" earns no
	// block at all on 3.2a, and blaming the ';' would name the wrong byte.
	if i := commentStart(cmd); i >= 0 {
		return Reply{}, commentErr(i)
	}
	if b, i := commandBreak(cmd); i >= 0 {
		return Reply{}, commandBreakErr(b, i)
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
		// The reader abandoned this command over a line it could not bind a
		// block by. The connection is still up, so terminalErr would be a
		// worse answer than the reason itself.
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

// commentStart reports the offset of a '#' that tmux would read as commenting
// out this whole line, or -1 if the line makes a command at all.
//
// It is [commandBreak]'s mirror and the half nothing used to ask. Every guard
// on [ControlClient.Do] counted upwards — an extra command earns an extra
// reply block — and a line that earns *no* block is worse: the reader binds
// replies by queue order, so the next command's block is handed to this one
// with a nil error and the queue stays one place out of step for the life of
// the connection. Nothing in the package notices. [ControlClient.Err],
// [ControlClient.Done] and [ControlClient.Stderr] all report a healthy
// connection while every command on it times out.
//
// The first non-blank byte decides the whole line, which is what makes the
// check this short. Measured on 3.2a against a raw pipe, counting only the
// blocks tmux attributed to the client:
//
//	#comment                         0 blocks
//	#                                0
//	  #x                             0
//	#{session_id}                    0   (an unquoted format is a comment too)
//	#c ; list-sessions               0   (the comment eats the separator)
//	'#comment'                       1   %error unknown command: #comment
//	list-sessions #tail              1   truncated, and still answered
//	a#b                              1   %error unknown command: a#b
//	nosuchcommand                    1   %error unknown command
//	}                                1   %error syntax error
//
// Refusing the shape is exact rather than conservative: no tmux command is
// named with a leading '#', and every one of the ninety-four other printable
// bytes was swept as the first byte of a line and earned exactly one block —
// scripts/probe-blocks.sh, which asserts it. A quoted '#' still reaches tmux
// and still earns its error block, which is what lets [ControlClient.DoArgs]
// pass one through: '#' is not in [safeByte], so the first argument is quoted
// and tmux reads it as a command name.
func commentStart(cmd string) int {
	for i := 0; i < len(cmd); i++ {
		switch cmd[i] {
		case ' ', '\t':
		case '#':
			return i
		default:
			return -1
		}
	}
	return -1
}

// commentErr explains what a line that is not a command would have done. The
// advice is the case it comes from: a caller replaying a tmux configuration
// down this connection, which is what a caller does when source-file cannot
// reach the file.
func commentErr(i int) error {
	return fmt.Errorf(
		"gotmucks: control command has a '#' at byte %d with nothing but blanks before it, "+
			"where tmux reads it as opening a comment; the line then parses to no command, "+
			"tmux answers it with no reply block at all, and this command would be given the "+
			"next one's reply while every command after it waited for ever. Skip the comment "+
			"lines of a configuration rather than sending them",
		i)
}

// commandBreak reports the first byte tmux would read as ending this command
// line and starting another, with its offset, or -1 if the line is one
// command. Both forms matter to [ControlClient.Do] for the same reason: an
// extra command earns an extra reply block, and every reply after it is
// delivered to the wrong caller.
//
// A ';' separates the commands of a list. A '{' beginning a token opens
// tmux's other quoting form, which is read as a command list wherever the
// command it belongs to takes commands. Nothing on the line distinguishes the
// two readings — verified on 3.2a, where "display-message -p {a}" is one
// command printing "a" while "if-shell 'true' {list-sessions}" is two — so a
// token-initial brace is refused whichever it would have been. A brace
// elsewhere in a token is ordinary data: "display-message -p a{b" prints
// "a{b".
//
// Quoting is what tmux goes by, and its rules are the ones applied here:
// single quotes take everything literally, which is why quoteArg prefers them,
// while a backslash escapes the next byte anywhere else. Verified on 3.2a,
// where "list-sessions -F 'A;B'" is one command producing one block.
func commandBreak(cmd string) (byte, int) {
	var quote byte
	atTokenStart := true

	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case c == '\\' && quote != '\'':
			i++ // an escaped byte is data, ";" and "{" included
			atTokenStart = false
		case quote != 0:
			if c == quote {
				quote = 0
			}
			atTokenStart = false
		case c == '\'' || c == '"':
			quote = c
			atTokenStart = false
		case c == ';':
			return ';', i
		case c == '{' && atTokenStart:
			return '{', i
		case c == ' ' || c == '\t':
			atTokenStart = true
		default:
			atTokenStart = false
		}
	}
	return 0, -1
}

// commandBreakErr explains what the offending byte would have done. The two
// cases share a consequence and not much else, so they are worded apart.
func commandBreakErr(b byte, i int) error {
	if b == ';' {
		return fmt.Errorf(
			"gotmucks: control command has an unquoted %q at byte %d, which tmux reads as the "+
				"start of a second command; each command is answered with its own reply block, so "+
				"the extra one would be delivered to whichever command follows. Send them separately",
			string(b), i)
	}
	return fmt.Errorf(
		"gotmucks: control command has an unquoted %q at byte %d, where tmux reads it as opening "+
			"a brace-quoted token; if the command takes a command there, what is inside is run and "+
			"answered with a reply block of its own, which would be delivered to whichever command "+
			"follows. Quote the argument another way, and send commands separately",
		string(b), i)
}

// DoArgs sends a command built from separate arguments, each quoted for
// tmux's parser. This is the safer form when any argument is caller data.
//
// Safer is about splitting, not about addressing. Quoting stops an argument
// containing a space, a ";" or a "{" from becoming a second command whose
// reply block would be delivered to whatever this connection sends next; it
// does nothing about what the command addresses. A quoted -t is still a -t,
// so DoArgs("kill-session", "-t", "work") kills whichever session answered to
// that name — this is one of the four exceptions to the identifier rule
// described in the package documentation, not an escape from it. Build the -t
// from a [SessionID], [WindowID] or [PaneID] that came from tmux.
//
// Nor does quoting stop a command expanding its own argument. rename-window,
// rename-session, new-session's -s and -n and new-session's -c run what they
// are given through tmux's format expansion before using it, so "v#{host}"
// names an object after the host and no quoting here prevents it: the
// expansion happens after the lexer, inside the command. Double the '#' — see
// [escapeFormat], which is what [Client.RenameWindow] and the other four do.
//
// On this path that is not a cosmetic difference. A "#(...)" reaches tmux's
// job machinery, and a job belongs to the client that asked for the expansion:
// a one-shot tmux exits before its own job can run, but a control connection
// stays alive, so the job runs. Measured on 3.2a, the same rename-window line
// that left a one-shot client's window merely misnamed executed the shell
// command when sent down a control connection, twice. Through DoArgs, an
// unescaped '#' in caller data is arbitrary command execution rather than a
// wrong name.
//
// One further thing changed with the ability to send a newline. Every argument
// is spliced as tmux's own "\n" escape, so it reaches tmux intact rather than
// ending the command — which is what lets a [FormatSpec.Arg] template down
// this pipe, and what removed [ControlClient.Do]'s newline check as a
// guarantee that nothing a caller sends can become a second protocol line.
// DoArgs cannot restore that centrally: it cannot tell a newline that is a
// substitution pattern, which is safe and necessary, from one in a value tmux
// will store and write back into a notification, which is not. So the
// guarantee now belongs to whichever call hands tmux such a value, and
// [ControlClient.Subscribe] is the one this package offers — see
// [checkSubscribeName] for the shape a new one should copy.
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

// terminalErrLocked says why the connection can no longer be used.
//
// The order is by how much the answer tells the caller. A recorded failure or
// tmux's own reason for exiting is news; "you closed it" is only news when
// there is nothing else to report, which is exactly the case [ErrClosed]
// names. Close after a crash therefore still reports the crash.
func (cc *ControlClient) terminalErrLocked() error {
	switch {
	case cc.exitErr != nil:
		return cc.exitErr
	case cc.exitMsg != "":
		return fmt.Errorf("gotmucks: %s: %w", cc.exitMsg, ErrServerExited)
	case cc.userClose:
		return ErrClosed
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
	cc.ensure()
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
func (cc *ControlClient) Done() <-chan struct{} {
	cc.ensure()
	return cc.done
}

// Err reports why the connection ended, or nil while it is still open or if
// it ended cleanly.
func (cc *ControlClient) Err() error {
	cc.ensure()
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.exitErr
}

// Stderr returns whatever tmux wrote to standard error. It is normally empty
// and is worth reading when Connect or a command fails unexpectedly.
func (cc *ControlClient) Stderr() string {
	cc.ensure()
	return cc.stderr.String()
}

// Close ends the connection.
//
// It writes the empty line that detaches a control client, then waits for
// tmux to exit, killing it if it outstays [WithCloseTimeout]. Close is
// idempotent and safe to call concurrently with anything else.
//
// The bound is two of those timeouts, and it holds whatever tmux does: one for
// the detach, including the write, and one more for the kill to take effect.
// The second has never been reached — killing the client makes the reader see
// end of file at once, and with the tmux client stopped outright Close still
// returned in 1.09s under a one-second timeout — but a guarantee that rests on
// the kill working is not a guarantee.
//
// It returns an error if tmux did not end the way it was asked to — an exit
// status of its own, a signal this package did not send, or the process
// outstaying the kill.
//
// Calling it is good manners rather than a requirement for tidiness: the
// reader reaps the process whenever the connection ends, so a caller that
// watches [ControlClient.Done] instead leaks nothing. What Close adds is the
// clean detach and the report of how tmux went.
//
// Closing detaches the control client; it does not kill the session or the
// server. Use [Client.KillSession] for that.
func (cc *ControlClient) Close() error {
	cc.ensure()

	var err error
	cc.closeOnce.Do(func() {
		cc.mu.Lock()
		cc.closed = true
		cc.userClose = true
		cc.mu.Unlock()

		// A connection that is already over has nothing to detach from and
		// nothing to wait for, and asking here rather than falling into the
		// select below is what makes the answer deterministic. Both cases of
		// that select are ready at once for such a client: done is closed, and
		// cfg.closeTimeout is zero on one nobody opened — the zero value never
		// passes through a config's defaults — so time.After fires
		// immediately. select chooses uniformly among ready cases, so Close
		// reported a kill timeout for a connection that had ended cleanly
		// about half the time it was asked. It survived locally because the
		// timer has usually not fired yet at the moment select evaluates; a
		// loaded CI runner under -race is where it does.
		select {
		case <-cc.done:
			// Still close the pipe. detach does it on the way out, and this
			// path is the one that skips detach; leaving it to reap would
			// make it depend on cmd.Wait having created the pipe itself.
			cc.closeStdin()
			err = cc.reap()
			return
		default:
		}

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
			select {
			case <-cc.done:
			case <-time.After(cc.cfg.closeTimeout):
				// Waiting here with no bound of its own is what made the
				// documented one incidental: it ends because the kill makes
				// the reader see end of file, not because anything says it
				// must. Reaping is left to the reader, which calls it when it
				// does finish — cmd.Wait would block here for exactly as long
				// as this wait was going to.
				err = fmt.Errorf(
					"gotmucks: tmux did not exit within %s of being killed: %w",
					cc.cfg.closeTimeout, ErrServerExited)
				return
			}
		}
		err = cc.reap()
	})
	return err
}

// detach writes the empty line that ends a control client's connection, then
// closes the pipe so that tmux sees end of file either way.
//
// It runs on its own goroutine, so a nil pipe here would be an unrecoverable
// panic rather than a failed call. There is one when the client was never
// attached to a process; there is nothing to detach from in that case.
func (cc *ControlClient) detach() {
	cc.writeMu.Lock()
	if cc.stdin != nil {
		_, _ = io.WriteString(cc.stdin, "\n")
	}
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
