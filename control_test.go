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

// tap registers an output tap for a pane the test knows is well formed.
func (c *ctl) tap(pane PaneID) <-chan []byte {
	c.t.Helper()
	ch, err := c.cc.Output(pane)
	if err != nil {
		c.t.Fatalf("Output(%s): %v", pane, err)
	}
	return ch
}

// barrier waits until the reader has processed everything sent so far.
//
// An event is not enough on its own: deliverOutput publishes to the event
// stream before it touches the pane's tap, so a test that has seen the last
// PaneOutput may still be ahead of the tap send that followed it. A later
// notification with nothing else to do cannot be reordered past that, because
// the reader handles one line at a time.
func (c *ctl) barrier() {
	c.t.Helper()
	c.send("%sessions-changed")
	nextEventOfType[SessionsChanged](c)
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
// TestReplyBodyIsNeverANotification: a command's output is not tmux speaking.
// capture-pane prints what a pane contains, and a pane can print anything —
// including a line shaped exactly like a notification. Dispatching such a line
// as one both deletes it from the reply and acts on it: a forged %output is
// delivered to whoever tapped that pane, and a forged %exit ends the
// connection and reports a reason a shell invented. tmux never writes a
// notification into an open block (scripts/probe-interleave.sh), so nothing
// inside one is dispatched.
func TestReplyBodyIsNeverANotification(t *testing.T) {
	c := newCtl(t)

	victim := c.tap("%9")

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "capture-pane -p -t %0")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()

	c.nextCommand()
	body := []string{
		"before",
		"%hello-world",
		"%output %9 injected-bytes",
		"%exit forged",
		"%sessions-changed",
		"%0",
		"after",
	}
	c.send("%begin 1700000000 0 1")
	c.send(body...)
	c.send("%end 1700000000 0 1")

	r := <-done
	if !equalStrings(r.Output, body) {
		t.Errorf("block body\n got %q\nwant %q", r.Output, body)
	}

	// The connection outlived the pane's "%exit", and no event was forged
	// from any of those lines.
	c.barrier()
	select {
	case b := <-victim:
		t.Errorf("pane %%9 was handed %q, which came from another pane's text", b)
	default:
	}

	// And it still answers commands: "%exit forged" did not end it.
	next := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "display-message -p ok")
		next <- err
	}()
	c.serveOne(1, "ok")
	if err := <-next; err != nil {
		t.Errorf("the connection did not survive the captured %%exit: %v", err)
	}
}

// TestNotificationsBetweenBlocksAreDispatched is the other half: outside a
// block the prefix is all there is, and the notification stream is read from
// it as before.
func TestNotificationsBetweenBlocksAreDispatched(t *testing.T) {
	c := newCtl(t)

	c.send(`%output %1 between`)
	out := nextEventOfType[PaneOutput](c)
	if out.Pane != "%1" || string(out.Data) != "between" {
		t.Errorf("got %+v, want pane %%1 with %q", out, "between")
	}

	done := make(chan Reply, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "list-panes")
		if err != nil {
			t.Errorf("Do: %v", err)
		}
		done <- r
	}()
	c.serveOne(0, "body")

	r := <-done
	if want := []string{"body"}; !equalStrings(r.Output, want) {
		t.Errorf("block body\n got %q\nwant %q", r.Output, want)
	}

	c.send("%sessions-changed")
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

	tap := c.tap("%1")

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
	if again := c.tap("%1"); again != tap {
		t.Error("Output returned a different channel for the same pane")
	}
}

func TestTapDoesNotAliasFirehoseBuffer(t *testing.T) {
	c := newCtl(t)
	tap := c.tap("%0")

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

// TestTapDropsAreReportedOnNextDelivery covers the reporting path that always
// worked: a tap that overflowed and then received again is told what it lost,
// and the lifetime count answers the same question without an event.
func TestTapDropsAreReportedOnNextDelivery(t *testing.T) {
	c := newCtl(t, WithOutputBuffer(1))
	tap := c.tap("%0")

	// The first fills the buffer; the other three have nowhere to go.
	for _, s := range []string{"a", "b", "c", "d"} {
		c.send("%output %0 " + s)
	}
	c.barrier()

	if got := c.cc.DroppedOutput("%0"); got != 3 {
		t.Fatalf("DroppedOutput = %d, want 3", got)
	}
	// A pane with no tap, and something that is not a pane id, are zero
	// rather than a question the caller has to handle.
	if got := c.cc.DroppedOutput("%9"); got != 0 {
		t.Errorf("DroppedOutput for an untapped pane = %d, want 0", got)
	}
	if got := c.cc.DroppedOutput("bash"); got != 0 {
		t.Errorf("DroppedOutput for a name = %d, want 0", got)
	}

	// Make room, and the report comes with the next delivery.
	<-tap
	c.send("%output %0 e")

	ev := nextEventOfType[OutputDropped](c)
	if ev.Pane != "%0" || ev.Count != 3 {
		t.Errorf("OutputDropped = %+v, want pane %%0 and count 3", ev)
	}

	// Reporting is not what the lifetime count measures, and the event
	// stream's own losses are a different number again.
	if got := c.cc.DroppedOutput("%0"); got != 3 {
		t.Errorf("DroppedOutput after the report = %d, want 3", got)
	}
	if got := c.cc.Dropped(); got != 0 {
		t.Errorf("Dropped = %d, want 0: the event channel itself lost nothing", got)
	}
}

// TestTapDropsAreReportedAtTeardown is F2. A pane that overflowed and then
// fell quiet said nothing at all: the report was waiting on a delivery that
// never came, and the connection ending is when the waiting has to stop.
func TestTapDropsAreReportedAtTeardown(t *testing.T) {
	c := newCtl(t, WithOutputBuffer(1))
	c.tap("%0")

	for _, s := range []string{"a", "b", "c", "d"} {
		c.send("%output %0 " + s)
	}
	c.barrier()

	c.send("%exit")

	// Looking for the drop report first also asserts it arrives before the
	// terminal event: the other order would consume Exited here and leave a
	// closed channel, which nextEventOfType fails on.
	ev := nextEventOfType[OutputDropped](c)
	if ev.Pane != "%0" || ev.Count != 3 {
		t.Errorf("OutputDropped = %+v, want pane %%0 and count 3", ev)
	}
	nextEventOfType[Exited](c)

	// The count outlives the connection, which is what makes it answerable at
	// all for output lost in the burst that ended it.
	if got := c.cc.DroppedOutput("%0"); got != 3 {
		t.Errorf("DroppedOutput after the connection ended = %d, want 3", got)
	}
}

// TestSessionChangedRefusesAName is F4. %session-changed is the one
// notification whose payload becomes state a caller reads back, so a first
// field that is not an identifier is refused where it arrives rather than
// stored for AttachedSession to hand out and every -t built from it to reject.
func TestSessionChangedRefusesAName(t *testing.T) {
	c := newCtl(t)
	c.send("%session-changed $4 work")
	nextEventOfType[SessionChanged](c)

	c.send("%session-changed work other")

	ev := nextEventOfType[*ProtocolError](c)
	if !strings.Contains(ev.Line, "session-changed work other") {
		t.Errorf("ProtocolError.Line = %q, want the line that caused it", ev.Line)
	}
	if got := c.cc.AttachedSession(); got != "$4" {
		t.Errorf("AttachedSession = %q, want it left at $4", got)
	}
}

// TestNotificationsRefuseAnIdentifierThatIsNotOne holds the rest of the
// notifications to what %session-changed is held to. The ID types are defined
// over string, so a field that arrived on the wire becomes an identifier
// simply by being assigned to one; an event carrying a name in a WindowID
// would be the one place in this package where an identifier is not one, and
// the caller only finds out when a -t built from it is rejected.
func TestNotificationsRefuseAnIdentifierThatIsNotOne(t *testing.T) {
	lines := []string{
		"%pause not-a-pane",
		"%continue @3",
		"%pane-mode-changed 3",
		"%window-add name",
		"%window-close %3",
		"%unlinked-window-add ",
		"%unlinked-window-close name",
		"%window-renamed name other",
		"%unlinked-window-renamed name other",
		"%window-pane-changed @7 @9",
		"%session-window-changed work @5",
		"%client-session-changed client-1 work name",
		"%client-session-changed client-1",
	}

	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			c := newCtl(t)
			c.send(line)

			ev := nextEventOfType[*ProtocolError](c)
			if ev.Line != line {
				t.Errorf("ProtocolError.Line = %q, want %q", ev.Line, line)
			}
		})
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
		// The unlinked spellings of the same three. tmux picks between them
		// by whether this client's session has a link to the window, which is
		// a fact about the client and not about the window, so both produce
		// the same event. These lines are tmux's own spelling — see
		// scripts/probe-notify.sh, which is what caught the reader looking
		// for "unlinked-window-rename" for a notification tmux writes as
		// "unlinked-window-renamed".
		{"%unlinked-window-add @7", func(ev Event) error {
			e, ok := ev.(WindowAdded)
			if err := want(ok, "WindowAdded", ev); err != nil {
				return err
			}
			return wantEq(e.Window == "@7", ev)
		}},
		{"%unlinked-window-close @7", func(ev Event) error {
			e, ok := ev.(WindowClosed)
			if err := want(ok, "WindowClosed", ev); err != nil {
				return err
			}
			return wantEq(e.Window == "@7", ev)
		}},
		{"%unlinked-window-renamed @7 a name with spaces", func(ev Event) error {
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
		{"%client-session-changed client-1 $3 work", func(ev Event) error {
			e, ok := ev.(ClientSessionChanged)
			if err := want(ok, "ClientSessionChanged", ev); err != nil {
				return err
			}
			return wantEq(e.Client == "client-1" && e.Session == "$3" && e.Name == "work", ev)
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
	tap := c.tap("%0")

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
	// command comes next. A brace block is that same defect in tmux's other
	// quoting form — see scripts/probe-blocks.sh.
	cmds := []string{
		"", "   ", "a\nb", "a\rb", "a\x00b",
		"list-sessions ; list-sessions",
		`if-shell "true" { list-sessions }`,
		`if-shell "true" {list-sessions}`,
	}
	for _, cmd := range cmds {
		if _, err := c.cc.Do(context.Background(), cmd); err == nil {
			t.Errorf("Do(%q) was accepted", cmd)
		}
	}

	// None of them reached tmux. That is the point of refusing them: a line
	// that was written could not be unsent, and the queue would already be one
	// reply out of step.
	select {
	case line := <-c.sent.lines:
		t.Errorf("a rejected command was written anyway: %q", line)
	default:
	}
}

func TestCommandBreak(t *testing.T) {
	tests := []struct {
		cmd  string
		want byte // 0 if this is one command
	}{
		{`list-sessions`, 0},
		{`list-sessions ; list-sessions`, ';'},
		{`kill-session;`, ';'},
		// Quoting is what tmux goes by, and quoteArg quotes both ';' and '{'
		// because neither is in the safe set, so DoArgs can never trip this.
		{`list-sessions -F 'A;B'`, 0},
		{`list-sessions -F "A;B"`, 0},
		{`send-keys a\;b`, 0},
		// A backslash inside single quotes is literal, so it does not shield
		// the quote that follows it.
		{`display-message -p 'a' ; kill-server`, ';'},
		{`display-message -p "it's here"`, 0},

		// A '{' opens a token, and tmux reads that token as a command list
		// wherever the command takes one. Both readings produce the same line,
		// so both are refused.
		{`if-shell "true" { list-sessions }`, '{'},
		{`if-shell "true" {list-sessions}`, '{'},
		{`display-message -p {a}`, '{'},
		{`{list-sessions}`, '{'},
		// A brace anywhere but the start of a token is data, and the common
		// case — a format — is quoted anyway.
		{`display-message -p a{b`, 0},
		{`list-sessions -F '#{session_id}'`, 0},
		{`display-message -p "{a}"`, 0},
		{`display-message -p \{a}`, 0},
		// The ';' is found first, which is the more useful thing to say.
		{`if-shell "true" ; { list-sessions }`, ';'},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			b, i := commandBreak(tt.cmd)
			if tt.want == 0 {
				if i >= 0 {
					t.Errorf("commandBreak(%q) = (%q, %d), want no break", tt.cmd, string(b), i)
				}
				return
			}
			if b != tt.want || i < 0 {
				t.Errorf("commandBreak(%q) = (%q, %d), want %q", tt.cmd, string(b), i, string(tt.want))
			}
			if i >= 0 && tt.cmd[i] != tt.want {
				t.Errorf("commandBreak(%q) pointed at %q, not the %q it reported",
					tt.cmd, string(tt.cmd[i]), string(tt.want))
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
// TestBlockLinesInsideABlockAreBody: %begin, %end and %error are as printable
// as any other text, so inside an open block they are output too. Only the
// terminator carrying this block's number closes it — the number is the one
// thing in the protocol a pane cannot know.
func TestBlockLinesInsideABlockAreBody(t *testing.T) {
	c := newCtl(t)

	done := make(chan Reply, 1)
	errs := make(chan error, 1)
	go func() {
		r, err := c.cc.Do(context.Background(), "capture-pane -p -t %0")
		done <- r
		errs <- err
	}()
	c.nextCommand()

	body := []string{
		"before",
		"%begin 1700000000 2 1",
		"%end 1700000000 9 1",
		"%error 1700000000 9 1",
		"%end garbage",
		"after",
	}
	c.send("%begin 1700000000 5 1")
	c.send(body...)
	c.send("%end 1700000000 5 1")

	r := <-done
	if err := <-errs; err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !equalStrings(r.Output, body) {
		t.Errorf("block body\n got %q\nwant %q", r.Output, body)
	}
	if r.Number != 5 {
		t.Errorf("Number = %d, want 5", r.Number)
	}
}

// TestUnterminatedBlockFailsItsCommandAtTeardown is what the rule above costs:
// a block whose terminator never arrives stays open, so the command waiting on
// it is answered only when the connection ends. It must be answered then.
func TestUnterminatedBlockFailsItsCommandAtTeardown(t *testing.T) {
	c := newCtl(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "list-sessions")
		done <- err
	}()
	c.nextCommand()

	c.send("%begin 1700000000 5 1", "partial")
	_ = c.out.Close() // tmux died mid-block

	select {
	case err := <-done:
		if !errors.Is(err, ErrServerExited) {
			t.Fatalf("got %v, want an error wrapping ErrServerExited", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the command in the unterminated block was stranded")
	}
}

// TestMalformedBlockHeaderIsNotABlock: a header whose arguments do not parse
// has no command number, so binding on it would attach a command to a body
// that may not be its own. It used to consume a queued command and deliver
// the body as a successful reply.
//
// Refusing to open a block is only half of it. The command the ruined header
// would have answered stays at the front of the queue, so the next %begin —
// the next command's — binds to it, and from there every reply goes to the
// caller before the one that earned it, each with a nil error. So the head of
// the queue is failed along with the report, and the test that matters is the
// second command's: it must get its own reply, not the first's.
func TestMalformedBlockHeaderIsNotABlock(t *testing.T) {
	c := newCtl(t)

	do := func(cmd string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := c.cc.Do(context.Background(), cmd)
			done <- err
		}()
		c.nextCommand()
		return done
	}

	first := do("list-sessions")
	second := do("display-message -p second")

	c.send("%begin garbage", "payload", "%end garbage")

	ev := nextEventOfType[*ProtocolError](c)
	if !strings.Contains(ev.Reason, "do not parse") {
		t.Errorf("Reason = %q", ev.Reason)
	}

	// The command that block would have answered is abandoned, with the line
	// that caused it as the reason. The connection is still up, so this must
	// not be reported as the connection having ended.
	select {
	case err := <-first:
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("Do = %v, want an error wrapping *ProtocolError", err)
		}
		if !strings.Contains(pe.Line, "%begin garbage") {
			t.Errorf("ProtocolError.Line = %q, want the header that caused it", pe.Line)
		}
		if errors.Is(err, ErrServerExited) {
			t.Errorf("Do = %v, want an error that does not claim the connection ended", err)
		}
		if !strings.Contains(err.Error(), "list-sessions") {
			t.Errorf("Do = %v, want the abandoned command named", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the command the malformed header displaced was never failed")
	}

	// The next block is the second command's, and it gets it. Before, this is
	// where the queue slipped: the body below was delivered to "list-sessions"
	// with a nil error and every later reply was one command behind.
	c.reply(7, "second")
	select {
	case err := <-second:
		if err != nil {
			t.Errorf("Do: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the second command never got its reply")
	}
}

// TestMalformedBlockTerminatorFailsNothing is the other half of the rule.
// A terminator arriving with no block open binds nothing and consumes no
// command, so there is no desync to confine: it is reported and the queue is
// left alone.
func TestMalformedBlockTerminatorFailsNothing(t *testing.T) {
	c := newCtl(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "list-sessions")
		done <- err
	}()
	c.nextCommand()

	c.send("%end garbage")
	if ev := nextEventOfType[*ProtocolError](c); !strings.Contains(ev.Reason, "do not parse") {
		t.Errorf("Reason = %q", ev.Reason)
	}

	select {
	case err := <-done:
		t.Fatalf("Do returned %v; a stray terminator answers nothing", err)
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

// TestMalformedFirstLineFailsNoCommand: a ruined header on the connection's
// first line is tmux's own opening block. It answers nothing this client sent,
// so no command is failed for it — and it is what would have lifted the
// startup barrier, so the command it released is exactly the one that would be
// failed if the head of the queue were taken here.
//
// The command is started first, so it is waiting on the barrier when the line
// arrives. The barrier now lifts on the terminator rather than on the ruined
// header, because the header opens an orphan block: a command written while
// that block was open would have its own %begin absorbed into it. tmux writes
// the whole block before it reads standard input, so the terminator below is
// what a real one sends next.
func TestMalformedFirstLineFailsNoCommand(t *testing.T) {
	c := newBareCtl(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "list-sessions")
		done <- err
	}()

	c.send("%begin garbage", "opening block body", "%end 1700000000 0 0")
	if ev := nextEventOfType[*ProtocolError](c); !strings.Contains(ev.Reason, "do not parse") {
		t.Errorf("Reason = %q", ev.Reason)
	}

	c.nextCommand()
	c.reply(4, "$0")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Do: %v, want the command answered; tmux's own opening block "+
				"answers nothing this client sent", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the command was never answered")
	}
}

// TestMalformedBlockHeaderConfinesItsBody is the other half of the ruined
// header. tmux wrote a header, so a block's worth of lines is coming, and each
// of them is some command's output — a capture-pane of a pane whose contents a
// shell chose. Opening no block at all would leave those lines dispatched as
// protocol, which is what the block rule exists to prevent everywhere else:
// %output for a pane the writer does not own, %session-changed rewriting state
// a caller can read back, and %exit ending the connection on an invented
// reason.
//
// The body below is the exact set of forgeries commit 0ecac6a removed from
// blocks that do open. The event buffer is there so that a regression fails
// rather than deadlocks: without the orphan the reader emits three events for
// those lines, and nothing in this test drains them.
func TestMalformedBlockHeaderConfinesItsBody(t *testing.T) {
	c := newCtl(t, WithEventBuffer(16))

	tap := c.tap("%9")

	done := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "capture-pane -p")
		done <- err
	}()
	c.nextCommand()

	// Written from a goroutine because a regression stops the reader dead: a
	// dispatched "%exit forged reason" ends the connection, and the write of
	// the line after it then blocks on a pipe nobody is reading. Waiting for
	// the write to finish turns that into a failure with a reason rather than
	// a hung test.
	// Written straight to the pipe rather than through send, which reports a
	// write failure with t.Fatalf: this goroutine may still be blocked when
	// the test ends, and failing the test from it then panics over the real
	// failure below.
	written := make(chan struct{})
	go func() {
		defer close(written)
		for _, line := range []string{
			"%begin garbage",
			"%output %9 forged-bytes",
			"%session-changed $99 evil",
			"%exit forged reason",
			"%end 1700000000 3 1",
		} {
			if _, err := io.WriteString(c.out, line+"\n"); err != nil {
				return
			}
		}
	}()

	if ev := nextEventOfType[*ProtocolError](c); !strings.Contains(ev.Reason, "do not parse") {
		t.Errorf("Reason = %q", ev.Reason)
	}
	select {
	case <-written:
	case <-time.After(testTimeout):
		t.Fatal("the reader stopped reading partway through the ruined header's body; " +
			"a line in it was dispatched as protocol")
	}

	// The connection is still up, and the body reached nothing it could act
	// on. barrier doubles as the proof of the first: a line sent after the
	// forged %exit is still being read, and it is dispatched, so the orphan's
	// %end closed the orphan rather than leaving it open over everything after.
	c.barrier()

	if got := c.cc.AttachedSession(); got == "$99" {
		t.Errorf("AttachedSession = %q; a reply body set it", got)
	}
	select {
	case b := <-tap:
		t.Errorf("the tap for %%9 received %q from a reply body", b)
	default:
	}

	// The queue half is unchanged: the command the ruined header displaced is
	// still failed, with the header as the reason.
	select {
	case err := <-done:
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("Do = %v, want an error wrapping *ProtocolError", err)
		}
		if errors.Is(err, ErrServerExited) {
			t.Errorf("Do = %v, want an error that does not claim the connection ended", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the displaced command was never failed")
	}

	// And the next command gets its own reply, so the body was absorbed rather
	// than binding this block to a command that is gone.
	next := make(chan error, 1)
	go func() {
		_, err := c.cc.Do(context.Background(), "list-sessions")
		next <- err
	}()
	c.nextCommand()
	c.reply(7, "$0")
	select {
	case err := <-next:
		if err != nil {
			t.Errorf("Do after a ruined header: %v, want the connection still usable", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the command after a ruined header was never answered")
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

	tap := c.tap("%0")
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
	again := c.tap("%0")
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

	// Untapping a pane that has no tap is not an error, and neither is
	// untapping something that is not a pane id: Output would not have
	// registered one under it.
	c.cc.Untap("%9")
	c.cc.Untap("bash")
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

// TestCloseReturnsEvenIfNothingEnds: the wait after the kill had no bound of
// its own, so the documented one held only because killing the client makes
// the reader see end of file. This connection has no process to kill, which is
// the shape that used to wait for ever.
func TestCloseReturnsEvenIfNothingEnds(t *testing.T) {
	c := newBareCtl(t, WithCloseTimeout(100*time.Millisecond))
	c.sent.block()

	done := make(chan error, 1)
	go func() { done <- c.cc.Close() }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrServerExited) {
			t.Errorf("Close: err = %v, want one reporting that tmux never exited", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Close never returned although its timeout had expired twice over")
	}
}

// TestUseAfterCloseReportsErrClosed: ErrClosed was exported and documented as
// what this reports, and nothing ever returned it — a caller who followed the
// documentation and wrote errors.Is(err, ErrClosed) got a branch that could
// not run.
func TestUseAfterCloseReportsErrClosed(t *testing.T) {
	c := newCtl(t)

	closed := make(chan error, 1)
	go func() { closed <- c.cc.Close() }()

	// The detach is what makes real tmux go away; the scripted server has to
	// be told, so answer the empty line with end of file.
	if line := c.nextCommand(); line != "" {
		t.Fatalf("Close sent %q, want the empty line that detaches", line)
	}
	_ = c.out.Close()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Close did not return")
	}

	_, err := c.cc.Do(context.Background(), "list-sessions")
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Do after Close: err = %v, want ErrClosed", err)
	}
	// The answer a caller gets today has to keep working: ErrClosed wraps the
	// sentinel that used to be all this reported.
	if !errors.Is(err, ErrServerExited) {
		t.Errorf("Do after Close: err = %v, want it to still wrap ErrServerExited", err)
	}
	// Being asked to end is not a fault, so the two calls that report faults
	// still report none.
	if err := c.cc.Wait(context.Background()); err != nil {
		t.Errorf("Wait after Close: %v, want nil", err)
	}
	if err := c.cc.Err(); err != nil {
		t.Errorf("Err after Close: %v, want nil", err)
	}
}

// TestFailureBeatsCloseInTheReportedError: a connection that died and was then
// tidied up with Close must still report why it died. "You closed it" is only
// the answer when there is nothing better to say.
func TestFailureBeatsCloseInTheReportedError(t *testing.T) {
	c := newCtl(t)

	c.send("%exit server exited unexpectedly")
	nextEventOfType[Exited](c)
	_ = c.cc.Close()

	_, err := c.cc.Do(context.Background(), "list-sessions")
	if errors.Is(err, ErrClosed) {
		t.Errorf("Do reported ErrClosed for a connection tmux ended: %v", err)
	}
	if !strings.Contains(err.Error(), "server exited unexpectedly") {
		t.Errorf("Do lost tmux's own reason: %v", err)
	}
}

// TestControlStartupCommand pins which option builds the command that opens a
// connection. WithAttach documents itself as equivalent to a WithControlArgs,
// so it loses to one; what mattered is that Connect used to check the attached
// session even then, and would refuse a connection over an identifier it was
// not going to use.
func TestControlStartupCommand(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want []string
	}{
		{
			name: "by default a connection makes its own session",
			want: []string{"new-session"},
		},
		{
			name: "attach",
			opts: []Option{WithAttach("$3")},
			want: []string{"attach-session", "-t", "$3"},
		},
		{
			name: "explicit arguments",
			opts: []Option{WithControlArgs("new-session", "-A", "-s", "work")},
			want: []string{"new-session", "-A", "-s", "work"},
		},
		{
			name: "explicit arguments beat an attach",
			opts: []Option{WithAttach("$3"), WithControlArgs("new-session", "-A")},
			want: []string{"new-session", "-A"},
		},
		{
			name: "and beat it whichever order they are given in",
			opts: []Option{WithControlArgs("new-session", "-A"), WithAttach("$3")},
			want: []string{"new-session", "-A"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controlCommand(newConfig(tt.opts)); !equalStrings(got, tt.want) {
				t.Errorf("controlCommand = %q, want %q", got, tt.want)
			}
		})
	}

	// An attach that will not be made is not an attach to check.
	cfg := newConfig([]Option{WithAttach("work"), WithControlArgs("new-session")})
	if got := cfg.attachTarget(); got != "" {
		t.Errorf("attachTarget = %q, want empty when WithControlArgs replaced the command", got)
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

// TestOutputRejectsANameForAPane: Output builds no -t, so a name here reaches
// no tmux — it registers a tap that matches no %output notification and is
// silent for the life of the connection. That is the failure shape the
// identifier rule exists to remove, so it is refused rather than registered.
func TestOutputRejectsANameForAPane(t *testing.T) {
	c := newCtl(t)

	for _, pane := range []PaneID{"bash", "", "%", "0", "@1", "%1x"} {
		ch, err := c.cc.Output(pane)
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("Output(%q) = (%v, %v), want an error wrapping ErrInvalidID", string(pane), ch, err)
		}
		if ch != nil {
			t.Errorf("Output(%q) handed back a channel anyway", string(pane))
		}
	}

	// Nothing was registered, so real output still reaches the firehose and a
	// later well-formed tap.
	tap := c.tap("%0")
	c.send(`%output %0 hello`)

	select {
	case b := <-tap:
		if string(b) != "hello" {
			t.Errorf("tap got %q, want hello", b)
		}
	case <-time.After(testTimeout):
		t.Fatal("the tap delivered nothing")
	}

	c.cc.mu.Lock()
	taps := len(c.cc.taps)
	c.cc.mu.Unlock()
	if taps != 1 {
		t.Errorf("%d taps registered, want 1", taps)
	}
}

// TestSubscribeRejectsBadTargets: the middle field of a -B argument is one of
// tmux's three wildcards or an identifier. A name there subscribes to whatever
// tmux resolved it to, which is the failure addressing by id exists to remove.
func TestSubscribeRejectsBadTargets(t *testing.T) {
	c := newCtl(t)
	for _, target := range []string{"bash", "editor", "%", "@", "%x", "@one", "$0"} {
		err := c.cc.Subscribe(context.Background(), "s", target, "#{pane_title}")
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("Subscribe(target=%q) = %v, want an error wrapping ErrInvalidID", target, err)
		}
	}
}

// TestZeroControlClientIsAnEndedConnection pins what a ControlClient nobody
// opened does. It is not a usable connection — only Connect makes one — but
// reaching a method through it used to panic in three places, and one of those
// panics was raised on a goroutine this package started, where no recover the
// caller writes can reach it: it ended the process rather than the call.
//
// So the zero value answers as a connection that has already ended, which is
// the one reading of it that is true.
func TestZeroControlClientIsAnEndedConnection(t *testing.T) {
	var cc ControlClient

	if got := cc.Stderr(); got != "" {
		t.Errorf("Stderr = %q, want empty", got)
	}
	if err := cc.Err(); !errors.Is(err, ErrServerExited) {
		t.Errorf("Err = %v, want an error wrapping ErrServerExited", err)
	}
	if id := cc.AttachedSession(); id != "" {
		t.Errorf("AttachedSession = %q, want empty", id)
	}
	if v := cc.Version(); !v.Unknown && v != (Version{}) {
		t.Errorf("Version = %v, want the zero version", v)
	}
	if n := cc.Dropped(); n != 0 {
		t.Errorf("Dropped = %d, want 0", n)
	}

	// Every channel is closed, so a caller ranging over one finishes instead
	// of waiting on a connection that will never say anything.
	tap, err := cc.Output("%0")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	select {
	case _, ok := <-tap:
		if ok {
			t.Error("the output tap delivered data")
		}
	default:
		t.Error("Output left its channel open")
	}
	select {
	case _, ok := <-cc.Events():
		if ok {
			t.Error("the event stream delivered an event")
		}
	default:
		t.Error("Events left its channel open")
	}
	select {
	case <-cc.Done():
	default:
		t.Error("Done is not closed")
	}

	cc.Untap("%0")

	if _, err := cc.Do(context.Background(), "list-sessions"); !errors.Is(err, ErrServerExited) {
		t.Errorf("Do = %v, want an error wrapping ErrServerExited", err)
	}
	if err := cc.Wait(context.Background()); !errors.Is(err, ErrServerExited) {
		t.Errorf("Wait = %v, want an error wrapping ErrServerExited", err)
	}
	// Nothing was started, so there is nothing to report on and nothing to
	// wait for. Close must say so rather than blocking on a channel no reader
	// will ever close.
	if err := cc.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := cc.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
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
		// A newline is the one byte quoting cannot reach, since the protocol
		// reads one command per line. tmux's escape for it is "\n" inside
		// double quotes and adjacent quoted tokens concatenate, so it is
		// spliced in as a chunk of its own and everything around it stays in
		// single quotes, which expand nothing.
		// A chunk that needs no quoting keeps none: tmux concatenates a bare
		// token with a quoted one just as readily.
		{"a\nb", `a"\n"b`},
		{"\n", `"\n"`},
		{"a\n", `a"\n"`},
		{"\nb", `"\n"b`},
		{"a\n\nb", `a"\n""\n"b`},
		{"a b\n#{x}", `'a b'"\n"'#{x}'`},
		{"it's\nb", `it\'s"\n"b`},
	}
	for _, tt := range tests {
		if got := quoteArg(tt.in); got != tt.want {
			t.Errorf("quoteArg(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestQuoteArgLeavesNoRawNewline is the property behind the table above, over
// the shapes a [FormatSpec] produces: whatever quoteArg returns has to be
// something [ControlClient.Do] will accept, and Do refuses a raw newline
// because one would end the command wherever it sat.
func TestQuoteArgLeavesNoRawNewline(t *testing.T) {
	specs := []FormatSpec{
		paneSpec, windowSpec, sessionSpec,
		{"session_id"},
		{"#H", "pane_id"},
	}
	for _, spec := range specs {
		arg := spec.Arg()
		if !strings.Contains(arg, "\n") {
			t.Errorf("%v renders without a newline; this test is checking nothing", []string(spec))
			continue
		}
		if quoted := quoteArg(arg); strings.ContainsAny(quoted, "\n\r\x00") {
			t.Errorf("quoteArg(%q) still contains a byte Do refuses: %q", arg, quoted)
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
	tap := c.tap("%5")
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
