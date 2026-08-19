package gotmucks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// The control-mode tests drive the client over in-memory pipes rather than a
// subprocess. That makes the wire format the subject: a test writes exactly
// the bytes tmux would write, including orderings a real server produces only
// occasionally, and asserts what the client makes of them. End-to-end
// behaviour against real tmux is covered by the integration tests.

const testTimeout = 5 * time.Second

// ctl is a control connection wired to a scripted server.
type ctl struct {
	t    *testing.T
	cc   *ControlClient
	out  *io.PipeWriter // server -> client
	sent *cmdSink       // client -> server

	closeOnce sync.Once
}

func newCtl(t *testing.T, opts ...Option) *ctl {
	t.Helper()

	c := newBareCtl(t, opts...)

	// Real tmux opens a block for the command that started the connection
	// before it reads anything the client sends, and the client holds its
	// first write until that block has been absorbed. A harness that skipped
	// it would be testing an opening no tmux produces. Flags 0 is what 3.2a
	// writes on that block; every reply carries 1.
	c.startup(100)
	return c
}

// newBareCtl is [newCtl] without tmux's opening block, for the few tests that
// care about the very first bytes on the connection.
func newBareCtl(t *testing.T, opts ...Option) *ctl {
	t.Helper()

	cfg := newConfig(append([]Option{WithCloseTimeout(2 * time.Second)}, opts...))
	cc := newControlClient(cfg)

	pr, pw := io.Pipe()
	sink := newCmdSink()

	c := &ctl{t: t, cc: cc, out: pw, sent: sink}
	cc.start(sink, pr)

	t.Cleanup(c.stop)
	return c
}

// startup writes the unsolicited block tmux opens for its own start command.
func (c *ctl) startup(number int) {
	c.t.Helper()
	c.send(
		fmt.Sprintf("%%begin 1700000000 %d 0", number),
		fmt.Sprintf("%%end 1700000000 %d 0", number),
	)
}

// stop tears the connection down the way a real server would: the client's
// empty line detaches it, tmux exits and its stdout closes.
func (c *ctl) stop() {
	c.closeOnce.Do(func() {
		_ = c.out.Close()
		_ = c.cc.Close()
	})
}

// send writes lines to the client as tmux would.
func (c *ctl) send(lines ...string) {
	c.t.Helper()
	for _, l := range lines {
		if _, err := io.WriteString(c.out, l+"\n"); err != nil {
			c.t.Fatalf("writing %q to the client: %v", l, err)
		}
	}
}

// sendRaw writes bytes with no newline handling, for prelude and terminator
// tests.
func (c *ctl) sendRaw(s string) {
	c.t.Helper()
	if _, err := io.WriteString(c.out, s); err != nil {
		c.t.Fatalf("writing %q to the client: %v", s, err)
	}
}

// nextCommand returns the next command line the client wrote.
func (c *ctl) nextCommand() string {
	c.t.Helper()
	select {
	case line := <-c.sent.lines:
		return line
	case <-time.After(testTimeout):
		c.t.Fatal("timed out waiting for the client to send a command")
		return ""
	}
}

// reply answers the next command with a successful block.
func (c *ctl) reply(number int, body ...string) {
	c.t.Helper()
	c.send(fmt.Sprintf("%%begin 1700000000 %d 1", number))
	c.send(body...)
	c.send(fmt.Sprintf("%%end 1700000000 %d 1", number))
}

// serveOne waits for a command and answers it.
func (c *ctl) serveOne(number int, body ...string) string {
	c.t.Helper()
	cmd := c.nextCommand()
	c.reply(number, body...)
	return cmd
}

// nextEvent returns the next event, failing the test if none arrives.
func (c *ctl) nextEvent() Event {
	c.t.Helper()
	select {
	case ev, ok := <-c.cc.Events():
		if !ok {
			c.t.Fatal("event channel closed while waiting for an event")
		}
		return ev
	case <-time.After(testTimeout):
		c.t.Fatal("timed out waiting for an event")
		return nil
	}
}

// nextEventOfType skips events until one of type T arrives.
func nextEventOfType[T Event](c *ctl) T {
	c.t.Helper()
	deadline := time.After(testTimeout)
	for {
		select {
		case ev, ok := <-c.cc.Events():
			if !ok {
				var zero T
				c.t.Fatalf("event channel closed while waiting for %T", zero)
			}
			if typed, match := ev.(T); match {
				return typed
			}
		case <-deadline:
			var zero T
			c.t.Fatalf("timed out waiting for %T", zero)
		}
	}
}

// cmdSink stands in for tmux's stdin, recording whole lines.
type cmdSink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	closed  bool
	blocked chan struct{}
	lines   chan string
}

func newCmdSink() *cmdSink { return &cmdSink{lines: make(chan string, 64)} }

// block makes every later write hang, which is what a tmux that has stopped
// draining its input does to the writer.
func (s *cmdSink) block() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked = make(chan struct{})
}

func (s *cmdSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.blocked != nil {
		wedged := s.blocked
		s.mu.Unlock()
		<-wedged
		return 0, io.ErrClosedPipe
	}
	if s.closed {
		s.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	s.buf.Write(p)

	var complete []string
	for {
		data := s.buf.Bytes()
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		complete = append(complete, string(data[:i]))
		s.buf.Next(i + 1)
	}
	s.mu.Unlock()

	for _, line := range complete {
		select {
		case s.lines <- line:
		default: // a test that does not read its commands should not deadlock
		}
	}
	return len(p), nil
}

func (s *cmdSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.blocked != nil {
		close(s.blocked)
		s.blocked = nil
	}
	return nil
}

func TestDoReturnsBlockBody(t *testing.T) {
	c := newCtl(t)

	var (
		reply Reply
		err   error
		done  = make(chan struct{})
	)
	go func() {
		defer close(done)
		reply, err = c.cc.Do(context.Background(), "list-sessions")
	}()

	if cmd := c.serveOne(0, "$0: 1 windows", "$1: 2 windows"); cmd != "list-sessions" {
		t.Errorf("client sent %q, want %q", cmd, "list-sessions")
	}
	<-done

	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got, want := reply.Output, []string{"$0: 1 windows", "$1: 2 windows"}; !equalStrings(got, want) {
		t.Errorf("Output = %q, want %q", got, want)
	}
	if reply.Number != 0 {
		t.Errorf("Number = %d, want 0", reply.Number)
	}
	if reply.Time.Unix() != 1700000000 {
		t.Errorf("Time = %v, want 1700000000", reply.Time.Unix())
	}
}

func TestDoEmptyBlock(t *testing.T) {
	c := newCtl(t)

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "kill-session -t $9")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()

	c.serveOne(3)
	r := <-done
	if len(r.Output) != 0 {
		t.Errorf("Output = %q, want empty", r.Output)
	}
	if r.Number != 3 {
		t.Errorf("Number = %d, want 3", r.Number)
	}
}

func TestDoError(t *testing.T) {
	c := newCtl(t)

	type result struct {
		reply Reply
		err   error
	}
	done := make(chan result, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "kill-session -t $99")
		done <- result{r, err}
	}()

	c.nextCommand()
	c.send("%begin 1700000000 1 1")
	c.send("can't find session: $99")
	c.send("%error 1700000000 1 1")

	got := <-done
	var cerr *ControlError
	if !errors.As(got.err, &cerr) {
		t.Fatalf("got %v (%T), want *ControlError", got.err, got.err)
	}
	if cerr.Message != "can't find session: $99" {
		t.Errorf("Message = %q", cerr.Message)
	}
	if cerr.Number != 1 {
		t.Errorf("Number = %d, want 1", cerr.Number)
	}
	// The body is still returned: an error message is output worth having.
	if len(got.reply.Output) != 1 {
		t.Errorf("Output = %q, want the error body", got.reply.Output)
	}
}

// TestNotificationsInsideBlock is the ordering the protocol explicitly
// permits and that a naive reader gets wrong: a notification arriving between
// %begin and %end must be dispatched as a notification, not folded into the
// command's output.
func TestNotificationsInsideBlock(t *testing.T) {
	c := newCtl(t)

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-panes")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()

	c.nextCommand()
	c.send("%begin 1700000000 0 1")
	c.send("first line")
	c.send(`%output %1 interleaved`)
	c.send("second line")
	c.send("%sessions-changed")
	c.send("third line")
	c.send("%end 1700000000 0 1")

	r := <-done
	want := []string{"first line", "second line", "third line"}
	if !equalStrings(r.Output, want) {
		t.Errorf("block body\n got %q\nwant %q", r.Output, want)
	}

	out := nextEventOfType[PaneOutput](c)
	if out.Pane != "%1" || string(out.Data) != "interleaved" {
		t.Errorf("got %+v, want pane %%1 with %q", out, "interleaved")
	}
	nextEventOfType[SessionsChanged](c)
}

// TestBlockBodyMayLookLikeANotification covers the collision that makes
// prefix-only dispatch wrong: "list-panes -F '#{pane_id}'" prints lines
// beginning with '%'.
func TestBlockBodyMayLookLikeANotification(t *testing.T) {
	c := newCtl(t)

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-panes -F '#{pane_id}'")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()

	c.nextCommand()
	c.send("%begin 1700000000 0 1")
	c.send("%0", "%1", "%12")
	c.send("%end 1700000000 0 1")

	r := <-done
	if want := []string{"%0", "%1", "%12"}; !equalStrings(r.Output, want) {
		t.Errorf("block body\n got %q\nwant %q", r.Output, want)
	}

	select {
	case ev := <-c.cc.Events():
		t.Errorf("pane ids in command output produced an event: %#v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDoConcurrent issues many commands at once and checks each caller gets
// its own reply. Correlation is the reason more than one may be outstanding.
func TestDoConcurrent(t *testing.T) {
	c := newCtl(t)

	const n = 25

	// Serve every command as it arrives, numbering blocks the way tmux does.
	go func() {
		for i := 0; i < n; i++ {
			cmd := c.nextCommand()
			// Echo the command back as its own reply body so a mismatched
			// correlation is visible rather than merely suspected.
			c.reply(i, "reply-for:"+cmd)
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := fmt.Sprintf("display-message -p cmd%d", i)
			r, err := c.cc.Do(context.Background(), cmd)
			if err != nil {
				errs <- fmt.Errorf("command %d: %w", i, err)
				return
			}
			if len(r.Output) != 1 || r.Output[0] != "reply-for:"+cmd {
				errs <- fmt.Errorf("command %d got reply %q", i, r.Output)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestOutputEscapingIsUndone(t *testing.T) {
	c := newCtl(t)

	c.send(`%output %0 $ echo hi\015\012hi\015\012`)
	got := nextEventOfType[PaneOutput](c)

	want := "$ echo hi\r\nhi\r\n"
	if string(got.Data) != want {
		t.Errorf("Data = %q, want %q", got.Data, want)
	}
	if got.Extended {
		t.Error("plain output reported as extended")
	}
}

func TestOutputTapAndFirehose(t *testing.T) {
	c := newCtl(t)

	tap := c.cc.Output("%1")

	c.send(`%output %1 hello`)
	c.send(`%output %2 other`)

	// The tap sees only its pane.
	select {
	case b := <-tap:
		if string(b) != "hello" {
			t.Errorf("tap got %q, want %q", b, "hello")
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting on the pane tap")
	}

	// The firehose still sees both, in order.
	first := nextEventOfType[PaneOutput](c)
	second := nextEventOfType[PaneOutput](c)
	if first.Pane != "%1" || second.Pane != "%2" {
		t.Errorf("firehose saw %s then %s, want %%1 then %%2", first.Pane, second.Pane)
	}

	// Nothing else should arrive on the tap.
	select {
	case b := <-tap:
		t.Errorf("tap received another pane's output: %q", b)
	case <-time.After(100 * time.Millisecond):
	}

	// Repeat calls return the same channel rather than registering a second.
	if again := c.cc.Output("%1"); again != tap {
		t.Error("Output returned a different channel for the same pane")
	}
}

func TestTapDoesNotAliasFirehoseBuffer(t *testing.T) {
	c := newCtl(t)
	tap := c.cc.Output("%0")

	c.send(`%output %0 abc`)

	var fromTap []byte
	select {
	case fromTap = <-tap:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting on the tap")
	}

	ev := nextEventOfType[PaneOutput](c)
	fromTap[0] = 'z'
	if string(ev.Data) != "abc" {
		t.Errorf("mutating the tap's slice changed the event's: %q", ev.Data)
	}
}

func TestExtendedOutputAndFlowControl(t *testing.T) {
	c := newCtl(t)

	c.send(`%extended-output %3 250 : slow\012`)
	out := nextEventOfType[PaneOutput](c)
	if !out.Extended {
		t.Error("Extended not set")
	}
	if out.Age != 250*time.Millisecond {
		t.Errorf("Age = %v, want 250ms", out.Age)
	}
	if string(out.Data) != "slow\n" {
		t.Errorf("Data = %q, want %q", out.Data, "slow\n")
	}

	c.send("%pause %3")
	if p := nextEventOfType[PanePaused](c); p.Pane != "%3" {
		t.Errorf("PanePaused pane = %s, want %%3", p.Pane)
	}

	c.send("%continue %3")
	if p := nextEventOfType[PaneContinued](c); p.Pane != "%3" {
		t.Errorf("PaneContinued pane = %s, want %%3", p.Pane)
	}
}

func TestNotificationMapping(t *testing.T) {
	c := newCtl(t)

	tests := []struct {
		line  string
		check func(Event) error
	}{
		{"%sessions-changed", func(ev Event) error {
			_, ok := ev.(SessionsChanged)
			return want(ok, "SessionsChanged", ev)
		}},
		{"%session-changed $2 work", func(ev Event) error {
			e, ok := ev.(SessionChanged)
			if err := want(ok, "SessionChanged", ev); err != nil {
				return err
			}
			return wantEq(e.Session == "$2" && e.Name == "work", ev)
		}},
		{"%session-window-changed $2 @5", func(ev Event) error {
			e, ok := ev.(SessionWindowChanged)
			if err := want(ok, "SessionWindowChanged", ev); err != nil {
				return err
			}
			return wantEq(e.Session == "$2" && e.Window == "@5", ev)
		}},
		{"%window-add @7", func(ev Event) error {
			e, ok := ev.(WindowAdded)
			if err := want(ok, "WindowAdded", ev); err != nil {
				return err
			}
			return wantEq(e.Window == "@7", ev)
		}},
		{"%window-close @7", func(ev Event) error {
			e, ok := ev.(WindowClosed)
			if err := want(ok, "WindowClosed", ev); err != nil {
				return err
			}
			return wantEq(e.Window == "@7", ev)
		}},
		{"%window-renamed @7 a name with spaces", func(ev Event) error {
			e, ok := ev.(WindowRenamed)
			if err := want(ok, "WindowRenamed", ev); err != nil {
				return err
			}
			return wantEq(e.Window == "@7" && e.Name == "a name with spaces", ev)
		}},
		{"%window-pane-changed @7 %9", func(ev Event) error {
			e, ok := ev.(WindowPaneChanged)
			if err := want(ok, "WindowPaneChanged", ev); err != nil {
				return err
			}
			return wantEq(e.Window == "@7" && e.Pane == "%9", ev)
		}},
		{"%pane-mode-changed %9", func(ev Event) error {
			e, ok := ev.(PaneModeChanged)
			if err := want(ok, "PaneModeChanged", ev); err != nil {
				return err
			}
			return wantEq(e.Pane == "%9", ev)
		}},
		{"%layout-change @0 bb62,80x23,0,0,0 bb62,80x23,0,0,0 *", func(ev Event) error {
			e, ok := ev.(LayoutChanged)
			if err := want(ok, "LayoutChanged", ev); err != nil {
				return err
			}
			return wantEq(e.Window == "@0" && e.Layout == "bb62,80x23,0,0,0" && e.Flags == "*", ev)
		}},
		{"%subscription-changed title $0 @1 1 %2 : bash", func(ev Event) error {
			e, ok := ev.(SubscriptionChanged)
			if err := want(ok, "SubscriptionChanged", ev); err != nil {
				return err
			}
			return wantEq(e.Name == "title" && e.Pane == "%2" && e.Value == "bash" && e.WindowIndex == 1, ev)
		}},
		{"%client-detached client-1", func(ev Event) error {
			e, ok := ev.(ClientDetached)
			if err := want(ok, "ClientDetached", ev); err != nil {
				return err
			}
			return wantEq(e.Client == "client-1", ev)
		}},
		{"%message something happened", func(ev Event) error {
			e, ok := ev.(Message)
			if err := want(ok, "Message", ev); err != nil {
				return err
			}
			return wantEq(e.Text == "something happened", ev)
		}},
		{"%config-error line 3: bad", func(ev Event) error {
			e, ok := ev.(ConfigError)
			if err := want(ok, "ConfigError", ev); err != nil {
				return err
			}
			return wantEq(e.Text == "line 3: bad", ev)
		}},
		// A notification this library predates must surface rather than
		// vanish.
		{"%some-future-thing a b", func(ev Event) error {
			e, ok := ev.(UnknownNotification)
			if err := want(ok, "UnknownNotification", ev); err != nil {
				return err
			}
			return wantEq(e.Name == "some-future-thing" && e.Args == "a b", ev)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			c.send(tt.line)
			if err := tt.check(c.nextEvent()); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestSessionChangedRecordsAttachedSession(t *testing.T) {
	c := newCtl(t)
	c.send("%session-changed $4 work")
	nextEventOfType[SessionChanged](c)

	if got := c.cc.AttachedSession(); got != "$4" {
		t.Errorf("AttachedSession = %q, want $4", got)
	}
}

func TestDCSPreludeIsStripped(t *testing.T) {
	c := newBareCtl(t)

	// tmux -CC announces itself with a DCS sequence before anything else.
	c.sendRaw(dcsEnter)
	c.send("%sessions-changed")

	nextEventOfType[SessionsChanged](c)
}

func TestExitIsTerminal(t *testing.T) {
	c := newCtl(t)

	c.send("%exit server exited")

	ev := nextEventOfType[Exited](c)
	if ev.Reason != "server exited" {
		t.Errorf("Reason = %q, want %q", ev.Reason, "server exited")
	}
	if ev.Err != nil {
		t.Errorf("Err = %v, want nil for a clean exit", ev.Err)
	}

	// The channel closes after the terminal event.
	select {
	case _, ok := <-c.cc.Events():
		if ok {
			t.Error("more events after Exited")
		}
	case <-time.After(testTimeout):
		t.Fatal("event channel not closed after exit")
	}

	if err := c.cc.Wait(context.Background()); err != nil {
		t.Errorf("Wait after a clean exit = %v, want nil", err)
	}

	// Commands issued afterwards fail rather than block.
	if _, err := c.cc.Do(context.Background(), "list-sessions"); !errors.Is(err, ErrServerExited) {
		t.Errorf("Do after exit = %v, want ErrServerExited", err)
	}
}

func TestExitClosesPaneTaps(t *testing.T) {
	c := newCtl(t)
	tap := c.cc.Output("%0")

	c.send("%exit")

	select {
	case _, ok := <-tap:
		if ok {
			t.Error("tap delivered data after exit")
		}
	case <-time.After(testTimeout):
		t.Fatal("pane tap not closed after exit")
	}
}

func TestUnexpectedEOFIsAnError(t *testing.T) {
	c := newCtl(t)

	// The server vanishes without saying %exit.
	_ = c.out.Close()

	ev := nextEventOfType[Exited](c)
	if ev.Err == nil {
		t.Error("Exited.Err is nil after an unannounced disconnect")
	}
	if err := c.cc.Wait(context.Background()); !errors.Is(err, ErrServerExited) {
		t.Errorf("Wait = %v, want an error wrapping ErrServerExited", err)
	}
}

func TestOutstandingCommandFailsOnDisconnect(t *testing.T) {
	c := newCtl(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "list-sessions")
		done <- err
	}()

	c.nextCommand()
	_ = c.out.Close() // die mid-command

	select {
	case err := <-done:
		if !errors.Is(err, ErrServerExited) {
			t.Errorf("got %v, want an error wrapping ErrServerExited", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Do did not return after the connection dropped")
	}
}

func TestDoRespectsContext(t *testing.T) {
	c := newCtl(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(ctx, "list-sessions")
		done <- err
	}()

	c.nextCommand() // written, but never answered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Do ignored its context")
	}
}

// TestAbandonedCommandKeepsCorrelation checks that giving up on a command
// does not desynchronise the queue: the reply still arrives and must still be
// matched to the command that was abandoned, not to the next one.
func TestAbandonedCommandKeepsCorrelation(t *testing.T) {
	c := newCtl(t)

	ctx, cancel := context.WithCancel(context.Background())
	abandoned := make(chan struct{})
	go func() {
		defer close(abandoned)
		_, _ = c.cc.Do(ctx, "first")
	}()

	if cmd := c.nextCommand(); cmd != "first" {
		t.Fatalf("first command was %q", cmd)
	}
	cancel()
	<-abandoned

	// The server answers the abandoned command late, then the next one.
	c.reply(0, "late reply to first")

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "second")
		if err != nil {
			t.Errorf("second command: %v", err)
		}
		done <- r
	}()

	if cmd := c.nextCommand(); cmd != "second" {
		t.Fatalf("second command was %q", cmd)
	}
	c.reply(1, "reply to second")

	select {
	case r := <-done:
		if len(r.Output) != 1 || r.Output[0] != "reply to second" {
			t.Errorf("second command got %q, want the reply meant for it", r.Output)
		}
	case <-time.After(testTimeout):
		t.Fatal("second command never completed")
	}
}

func TestDoRejectsUnsendableCommands(t *testing.T) {
	c := newCtl(t)

	// A NUL is the subtle one: tmux reads the command line as a C string, so
	// it does not fail, it truncates — verified against 3.2a, where
	// "rename-session -t $0 'a\x00b'" renamed the session to "a" and reported
	// success. An unquoted ';' is subtler still: tmux runs both commands and
	// answers each with its own block, so the second reply lands on whichever
	// command comes next.
	for _, cmd := range []string{"", "   ", "a\nb", "a\rb", "a\x00b", "list-sessions ; list-sessions"} {
		if _, err := c.cc.Do(context.Background(), cmd); err == nil {
			t.Errorf("Do(%q) was accepted", cmd)
		}
	}
}

func TestCommandSeparator(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool // does tmux see a second command here?
	}{
		{`list-sessions`, false},
		{`list-sessions ; list-sessions`, true},
		{`kill-session;`, true},
		// Quoting is what tmux goes by, and quoteArg quotes a ';' because it
		// is not in the safe set, so DoArgs can never trip this.
		{`list-sessions -F 'A;B'`, false},
		{`list-sessions -F "A;B"`, false},
		{`send-keys a\;b`, false},
		// A backslash inside single quotes is literal, so it does not shield
		// the quote that follows it.
		{`display-message -p 'a' ; kill-server`, true},
		{`display-message -p "it's here"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			if got := commandSeparator(tt.cmd) >= 0; got != tt.want {
				t.Errorf("commandSeparator(%q) found a separator = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// TestEventsDroppedRatherThanBlocking is the load-bearing property of the
// reader: a consumer that stops reading must not be able to stall the
// connection, because that would stall command replies and every other pane
// as well.
func TestEventsDroppedRatherThanBlocking(t *testing.T) {
	c := newCtl(t, WithEventBuffer(4))

	// Flood the event channel without reading any of it.
	for i := 0; i < 200; i++ {
		c.send("%sessions-changed")
	}

	// The connection must still work.
	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-sessions")
		if err != nil {
			t.Errorf("Do after flooding the event channel: %v", err)
		}
		done <- r
	}()

	c.serveOne(0, "$0")

	select {
	case r := <-done:
		if len(r.Output) != 1 {
			t.Errorf("reply = %q", r.Output)
		}
	case <-time.After(testTimeout):
		t.Fatal("the reader stalled on a full event channel")
	}

	if c.cc.Dropped() == 0 {
		t.Error("events were dropped but Dropped() reports none")
	}

	// And the loss is reported rather than silent.
	deadline := time.After(testTimeout)
	for {
		select {
		case ev := <-c.cc.Events():
			if d, ok := ev.(EventsDropped); ok {
				if d.Count == 0 {
					t.Error("EventsDropped reported a count of zero")
				}
				return
			}
		case <-deadline:
			t.Fatal("no EventsDropped event was ever delivered")
		}
	}
}

func TestStrayOutputIsReported(t *testing.T) {
	c := newCtl(t)

	c.send("this is not inside a block")

	ev := nextEventOfType[*ProtocolError](c)
	if !strings.Contains(ev.Reason, "outside a command block") {
		t.Errorf("Reason = %q", ev.Reason)
	}
	if ev.Line != "this is not inside a block" {
		t.Errorf("Line = %q", ev.Line)
	}
}

func TestUnsolicitedBlockIsAbsorbed(t *testing.T) {
	c := newCtl(t)

	// A block for a command this client never sent. Its body must not be
	// reported as stray output, and it must not consume a later command's
	// reply slot.
	c.send("%begin 1700000000 0 1")
	c.send("orphan output")
	c.send("%end 1700000000 0 1")

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-sessions")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()

	c.serveOne(1, "$0")

	select {
	case r := <-done:
		if len(r.Output) != 1 || r.Output[0] != "$0" {
			t.Errorf("reply = %q, want the command's own output", r.Output)
		}
	case <-time.After(testTimeout):
		t.Fatal("an unsolicited block broke correlation")
	}
}

// TestStartupBlockIsNotBoundToTheFirstCommand is the regression test for the
// failure this whole barrier exists to prevent. tmux opens a block for its own
// start command before it reads any of ours; a client that wrote first and
// bound by queue order alone would hand that block's body to its first command
// and shift every reply afterwards by one.
func TestStartupBlockIsNotBoundToTheFirstCommand(t *testing.T) {
	c := newBareCtl(t)

	// The command is issued before tmux has said anything at all, which is
	// the race: the queue is not empty by the time the opening block arrives.
	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-sessions")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()

	// Nothing may be written until the opening block has been absorbed.
	select {
	case line := <-c.sent.lines:
		t.Fatalf("wrote %q before tmux opened its first block", line)
	case <-time.After(100 * time.Millisecond):
	}

	c.send("%begin 1700000000 261 0", "startup output", "%end 1700000000 261 0")

	if cmd := c.nextCommand(); cmd != "list-sessions" {
		t.Fatalf("command was %q", cmd)
	}
	c.reply(265, "$0")

	select {
	case r := <-done:
		if len(r.Output) != 1 || r.Output[0] != "$0" {
			t.Errorf("reply = %q, want the command's own output", r.Output)
		}
	case <-time.After(testTimeout):
		t.Fatal("the command never completed")
	}
}

// TestBlockWithZeroFlagsIsUnsolicited covers the blocks the barrier cannot:
// tmux runs commands on a client's behalf at any time — from a hook, say —
// and guards those too. The flags word is how they are told apart, and it is
// not a guess: tmux sets it from CMDQ_STATE_CONTROL, which only a command line
// read from this client's input carries.
func TestBlockWithZeroFlagsIsUnsolicited(t *testing.T) {
	c := newCtl(t)

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-sessions")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()
	c.nextCommand()

	// A block tmux opened for itself, arriving while our command is in flight.
	c.send("%begin 1700000000 300 0", "not ours", "%end 1700000000 300 0")
	c.reply(301, "$0")

	select {
	case r := <-done:
		if len(r.Output) != 1 || r.Output[0] != "$0" {
			t.Errorf("reply = %q, want the command's own output", r.Output)
		}
	case <-time.After(testTimeout):
		t.Fatal("an unsolicited block stole the command's reply")
	}
}

// TestBlockWithNoFlagsFieldStillBinds keeps the flags rule from turning into a
// hang against a tmux that stops writing the field: an absent flags word is
// not the same statement as a zero one.
func TestBlockWithNoFlagsFieldStillBinds(t *testing.T) {
	c := newCtl(t)

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-sessions")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()
	c.nextCommand()

	c.send("%begin 1700000000 7", "$0", "%end 1700000000 7")

	select {
	case r := <-done:
		if len(r.Output) != 1 || r.Output[0] != "$0" {
			t.Errorf("reply = %q", r.Output)
		}
	case <-time.After(testTimeout):
		t.Fatal("a block with no flags field was treated as unsolicited")
	}
}

// TestAbandonedBlockFailsItsCommand: a second %begin while a block is open
// leaves the pending in neither the queue nor cc.current, so nothing else can
// ever wake it. It has to be failed here or the caller waits for ever.
func TestAbandonedBlockFailsItsCommand(t *testing.T) {
	c := newCtl(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "first")
		done <- err
	}()
	c.nextCommand()

	c.send("%begin 1700000000 1 1", "partial")
	c.send("%begin 1700000000 2 1") // the first block never closed

	select {
	case err := <-done:
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("got %v (%T), want a *ProtocolError", err, err)
		}
		if !strings.Contains(pe.Reason, "still open") {
			t.Errorf("Reason = %q", pe.Reason)
		}
	case <-time.After(testTimeout):
		t.Fatal("the abandoned command was stranded")
	}
}

// TestMismatchedTerminatorFailsItsCommand: the body under a terminator for
// some other block cannot be said to be this command's, and reporting it as a
// successful reply would hand the caller another command's output.
func TestMismatchedTerminatorFailsItsCommand(t *testing.T) {
	c := newCtl(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "list-sessions")
		done <- err
	}()
	c.nextCommand()

	c.send("%begin 1700000000 5 1", "body", "%end 1700000000 9 1")

	select {
	case err := <-done:
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("got %v (%T), want a *ProtocolError", err, err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Do never returned")
	}
}

// TestMalformedBlockHeaderIsNotABlock: a header whose arguments do not parse
// has no command number, so binding on it would attach a command to a body
// that may not be its own. It used to consume a queued command and deliver
// the body as a successful reply.
func TestMalformedBlockHeaderIsNotABlock(t *testing.T) {
	c := newCtl(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "list-sessions")
		done <- err
	}()
	c.nextCommand()

	c.send("%begin garbage", "payload", "%end garbage")

	ev := nextEventOfType[*ProtocolError](c)
	if !strings.Contains(ev.Reason, "do not parse") {
		t.Errorf("Reason = %q", ev.Reason)
	}

	// The command is still outstanding: nothing bound to it, so a real reply
	// still can.
	select {
	case err := <-done:
		t.Fatalf("Do returned %v; the malformed block should not have answered it", err)
	case <-time.After(100 * time.Millisecond):
	}

	c.reply(0, "$0")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Do: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the real reply never arrived")
	}
}

// TestExitedSurvivesAFloodedChannel: the terminal event has a slot of its own,
// because a consumer ranging over the channel and waiting for Exited would
// otherwise just see the channel close.
func TestExitedSurvivesAFloodedChannel(t *testing.T) {
	c := newCtl(t, WithEventBuffer(2))

	for i := 0; i < 50; i++ {
		c.send("%sessions-changed")
	}
	c.send("%exit flooded")

	deadline := time.After(testTimeout)
	for {
		select {
		case ev, ok := <-c.cc.Events():
			if !ok {
				t.Fatal("the channel closed without ever delivering Exited")
			}
			if ex, isExit := ev.(Exited); isExit {
				if ex.Reason != "flooded" {
					t.Errorf("Reason = %q", ex.Reason)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for Exited")
		}
	}
}

func TestUntapClosesTheChannel(t *testing.T) {
	c := newCtl(t)

	tap := c.cc.Output("%0")
	c.cc.Untap("%0")

	select {
	case _, ok := <-tap:
		if ok {
			t.Error("an untapped channel delivered data")
		}
	case <-time.After(testTimeout):
		t.Fatal("Untap did not close the channel")
	}

	// Output after Untap registers a fresh tap rather than handing back the
	// closed one.
	again := c.cc.Output("%0")
	c.send(`%output %0 world`)

	select {
	case data, ok := <-again:
		if !ok {
			t.Fatal("the replacement tap was closed")
		}
		if string(data) != "world" {
			t.Errorf("got %q, want world", data)
		}
	case <-time.After(testTimeout):
		t.Fatal("the replacement tap delivered nothing")
	}

	// Untapping a pane that has no tap is not an error.
	c.cc.Untap("%9")
}

// TestFailedWriteLeavesNoPendingCommand: a command whose write failed was
// never sent, so tmux will never open a block for it. Leaving it queued would
// bind the next reply that does arrive to a command nobody is waiting on, and
// every reply after that to the wrong one.
func TestFailedWriteLeavesNoPendingCommand(t *testing.T) {
	c := newCtl(t)

	_ = c.sent.Close() // the pipe to tmux breaks

	if _, err := c.cc.Do(context.Background(), "list-sessions"); err == nil {
		t.Fatal("Do reported success although the command was never written")
	}

	c.cc.mu.Lock()
	queued := len(c.cc.queue)
	c.cc.mu.Unlock()
	if queued != 0 {
		t.Errorf("%d commands still queued after a failed write", queued)
	}
}

// TestCloseIsBoundedByItsTimeout: the detach write is unbounded, so a tmux
// that has stopped reading its input used to hold Close open indefinitely and
// WithCloseTimeout never got a chance to apply.
func TestCloseIsBoundedByItsTimeout(t *testing.T) {
	c := newBareCtl(t, WithCloseTimeout(200*time.Millisecond))
	c.sent.block()

	// There is no process to kill here, so the reader has to be the thing
	// that ends: closing the server side is what a killed tmux would look
	// like. Close must not still be inside its write when that happens.
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = c.out.Close()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.cc.Close()
	}()

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Close blocked on a write that tmux was not reading")
	}
}

func TestControlCommandArgv(t *testing.T) {
	tests := []struct {
		name string
		run  func(*ControlClient) error
		want string
	}{
		{
			name: "subscribe to all panes",
			run: func(cc *ControlClient) error {
				return cc.Subscribe(context.Background(), "title", SubscribeAllPanes, "#{pane_title}")
			},
			want: `refresh-client -B 'title:%*:#{pane_title}'`,
		},
		{
			name: "subscribe to the session",
			run: func(cc *ControlClient) error {
				return cc.Subscribe(context.Background(), "name", SubscribeSession, "#{session_name}")
			},
			want: `refresh-client -B 'name::#{session_name}'`,
		},
		{
			name: "subscribe to one pane",
			run: func(cc *ControlClient) error {
				return cc.Subscribe(context.Background(), "cmd", SubscribePane("%3"), "#{pane_current_command}")
			},
			want: `refresh-client -B 'cmd:%3:#{pane_current_command}'`,
		},
		{
			name: "unsubscribe",
			run:  func(cc *ControlClient) error { return cc.Unsubscribe(context.Background(), "title") },
			want: "refresh-client -B title",
		},
		{
			name: "set size",
			run:  func(cc *ControlClient) error { return cc.SetSize(context.Background(), 132, 43) },
			want: "refresh-client -C 132x43",
		},
		{
			name: "pause after",
			run:  func(cc *ControlClient) error { return cc.PauseAfter(context.Background(), 3*time.Second) },
			want: "refresh-client -f pause-after=3",
		},
		{
			// tmux's resolution here is whole seconds, so anything shorter
			// must round up rather than to zero, which would disable it.
			name: "sub-second pause rounds up",
			run:  func(cc *ControlClient) error { return cc.PauseAfter(context.Background(), 100*time.Millisecond) },
			want: "refresh-client -f pause-after=1",
		},
		{
			name: "pause after zero disables",
			run:  func(cc *ControlClient) error { return cc.PauseAfter(context.Background(), 0) },
			want: "refresh-client -f ''",
		},
		{
			// The pane id must be quoted: unquoted, tmux reads the leading
			// '%' as a preprocessor directive and rejects the whole line.
			name: "resume",
			run:  func(cc *ControlClient) error { return cc.Resume(context.Background(), "%4") },
			want: `refresh-client -A '%4:continue'`,
		},
		{
			name: "pause",
			run:  func(cc *ControlClient) error { return cc.Pause(context.Background(), "%4") },
			want: `refresh-client -A '%4:pause'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCtl(t)

			done := make(chan error, 1)
			go func() { done <- tt.run(c.cc) }()

			got := c.nextCommand()
			c.reply(0)

			if err := <-done; err != nil {
				t.Fatalf("call failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("sent %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubscribeRejectsBadNames(t *testing.T) {
	c := newCtl(t)
	for _, name := range []string{"", "has:colon", "has space"} {
		if err := c.cc.Subscribe(context.Background(), name, SubscribeSession, "#{x}"); err == nil {
			t.Errorf("Subscribe accepted the name %q", name)
		}
	}
}

func TestQuoteArg(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"list-sessions", "list-sessions"},
		{"$0", `'$0'`},
		{"", "''"},
		{"#{pane_id}", `'#{pane_id}'`},
		{"has space", `'has space'`},
		// tmux reads a token beginning with '%' as an %if-style preprocessor
		// directive, so pane identifiers must be quoted.
		{"%3:continue", `'%3:continue'`},
		{"@1", `'@1'`},
		{"/tmp/path.sock", "/tmp/path.sock"},
		{"a=b,c", "a=b,c"},
		{`it's`, `it\'s`},
		{`a b 'c'`, `a\ b\ \'c\'`},
	}
	for _, tt := range tests {
		if got := quoteArg(tt.in); got != tt.want {
			t.Errorf("quoteArg(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReplyRows(t *testing.T) {
	r := Reply{Output: []string{"$0\tmain", "$1\tbuild"}}
	rows, err := r.Rows(FormatSpec{"session_id", "session_name"})
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if got := rows[1].Get("session_name"); got != "build" {
		t.Errorf("session_name = %q, want build", got)
	}
}

func TestOutputAfterCloseIsAClosedChannel(t *testing.T) {
	c := newCtl(t)
	c.send("%exit")
	nextEventOfType[Exited](c)

	// Registering a tap on a dead connection must not hang the caller.
	tap := c.cc.Output("%5")
	select {
	case _, ok := <-tap:
		if ok {
			t.Error("a tap on a closed connection delivered data")
		}
	case <-time.After(testTimeout):
		t.Fatal("a tap registered after close never closed")
	}
}

func want(ok bool, kind string, ev Event) error {
	if !ok {
		return fmt.Errorf("got %T (%#v), want %s", ev, ev, kind)
	}
	return nil
}

func wantEq(ok bool, ev Event) error {
	if !ok {
		return fmt.Errorf("unexpected field values: %#v", ev)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
