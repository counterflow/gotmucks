package gotmucks

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/counterflow/gotmucks/internal/ctlparse"
	"github.com/counterflow/gotmucks/internal/escape"
)

// dcsEnter is what tmux -CC writes on entry so a terminal can detect control
// mode. It arrives before anything else and is not part of the protocol.
const dcsEnter = "\x1bP1000p"

// dcsExit is the string terminator tmux appends after %exit under -CC.
const dcsExit = "\x1b\\"

// readLoop owns the read side of the connection for its whole life. It is the
// only goroutine that touches stdout, so the only interleaving to resolve is
// the protocol's own.
func (cc *ControlClient) readLoop() {
	r := bufio.NewReaderSize(cc.stdout, 64<<10)

	var readErr error
	first := true

	for {
		// ReadString rather than a Scanner: pane output lines have no useful
		// upper bound and a Scanner would fail on a long one.
		raw, err := r.ReadString('\n')

		if raw != "" {
			line := strings.TrimSuffix(raw, "\n")
			line = strings.TrimSuffix(line, "\r")
			if first {
				line = strings.TrimPrefix(line, dcsEnter)
				first = false
			}
			if !cc.handleLine(line) {
				cc.finishReader(nil)
				return
			}
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	cc.finishReader(readErr)
}

// handleLine dispatches one line. It reports whether the reader should carry
// on; false means tmux said %exit.
//
// Dispatch is by line prefix only while no command block is open. Inside one
// every line is that command's output, and a command's output is not tmux
// speaking: capture-pane prints what a pane contains, show-buffer prints what
// was pasted into a buffer, display-message -p '#{pane_title}' prints a title
// the pane's own program chose. Any of them may produce a line shaped exactly
// like a notification, and taking one as a notification both removes it from
// the reply and acts on it — "%output %9 x" becomes fabricated bytes delivered
// to whoever tapped %9, and "%exit forged" ends the connection and reports a
// reason a shell invented.
//
// So while a block is open the only line that is not body is the terminator
// carrying that block's number. That discriminator exists because tmux does
// not interleave, which is a fact about tmux and therefore asked rather than
// assumed — scripts/probe-interleave.sh, on 3.2a: 17,812 notifications across
// five blocks, including a capture-pane of 2,000 lines and a run-shell that
// held a block open for a full second while a pane produced output without
// pause, and not one landed inside a block. Notifications a command causes
// itself arrive after its own %end, and so does %exit, for kill-server,
// detach-client and kill-session alike.
func (cc *ControlClient) handleLine(line string) bool {
	cl := ctlparse.Classify(line)

	// The first line settles whether there is a startup block to absorb: tmux
	// opens one for the command that started the connection, before it reads
	// anything from standard input. If the connection opens with anything
	// else, there is none, and commands may be written at once.
	firstLine := !cc.sawFirstLine
	if firstLine {
		cc.sawFirstLine = true
		if cl.Kind != ctlparse.KindBegin || cl.Malformed {
			cc.releaseStartup()
		}
	}

	if number, open := cc.openBlock(); open {
		if closesBlock(cl, number) {
			cc.endBlock(cl, cl.Kind == ctlparse.KindError)
		} else {
			cc.dataLine(line)
		}
		return true
	}

	// Malformed is only ever set for a block header, and a header whose
	// arguments did not parse has no command number to bind by, so no block
	// is opened for it: doing that on a guessed number would attach a command
	// to a body that is not its own.
	//
	// Reporting the line is not on its own enough. A %begin that opens no
	// block leaves the command it would have answered at the front of the
	// queue, so the next %begin binds that command to the next command's
	// block, and from there every reply is delivered to the caller before the
	// one it belongs to — with a nil error, since each block still ends in a
	// perfectly good %end. That is the failure awaitStartup exists to remove,
	// arriving by a different door, and it outlives the line that caused it.
	// So the command at the head of the queue is failed along with the
	// report, which confines the damage to the one command.
	//
	// Only a header does that, and only one that was not the connection's
	// first line. The first line is tmux's own opening block, which answers
	// nothing this client sent; failing a command for it would be failing one
	// that has only just been let past the startup barrier this line lifted.
	// A terminator arriving with no block open binds nothing and consumes no
	// command either, so it is reported and nothing else.
	if cl.Malformed {
		pe := &ProtocolError{
			Line:   cl.Raw,
			Reason: "block " + cl.Kind.String() + " with arguments that do not parse",
		}
		cc.emit(pe)
		if cl.Kind == ctlparse.KindBegin && !firstLine {
			cc.failHead(pe)
		}
		return true
	}

	switch cl.Kind {
	case ctlparse.KindBegin:
		cc.beginBlock(cl)
	case ctlparse.KindEnd:
		cc.endBlock(cl, false)
	case ctlparse.KindError:
		cc.endBlock(cl, true)
	case ctlparse.KindNotification:
		return cc.handleNotification(cl)
	default:
		cc.dataLine(line)
	}
	return true
}

// closesBlock reports whether cl terminates the open block numbered number.
//
// A terminator whose arguments did not parse carries no number, so it closes
// nothing. Neither does one carrying a different number: inside an open block
// that is not a stray terminator to complain about but a line of output that
// looks like one, and nothing on the wire tells the two apart.
func closesBlock(cl ctlparse.Line, number int) bool {
	if cl.Malformed || cl.Number != number {
		return false
	}
	return cl.Kind == ctlparse.KindEnd || cl.Kind == ctlparse.KindError
}

// openBlock reports the number of the block being read, if there is one.
//
// cc.current is written only by beginBlock, endBlock and the teardown, all of
// which run on the reader goroutine, so the answer cannot go stale between
// here and the call it decides.
func (cc *ControlClient) openBlock() (int, bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.current == nil {
		return 0, false
	}
	return cc.current.number, true
}

// beginBlock binds the next written-but-unanswered command to the command
// number tmux has just assigned it.
//
// Binding is by queue order rather than by predicting the number: tmux
// processes a connection's commands in the order it receives them, and writes
// are serialised, so the front of the queue is the command this block belongs
// to. That holds whatever base tmux numbers from.
//
// The rule needs a companion for blocks that did not come from the queue at
// all, since tmux opens one for the command that started the connection and
// for anything else it runs on this client's behalf. Two things identify
// those, and both are needed:
//
//   - Nothing has been written yet. Commands are held until the reader has
//     absorbed tmux's opening block (see awaitStartup), so while the barrier
//     stands the queue is empty by construction rather than by luck.
//   - The flags word is zero. tmux sets it from CMDQ_STATE_CONTROL, which is
//     set only for a command line read from the control client's own input,
//     so zero means the block answers something this client did not send.
//     Verified on 3.2a: the opening block carries 0 and every reply carries 1.
//     A tmux that stopped writing the field at all is not second-guessed.
//
// It is reached only with no block open, since a %begin arriving inside one is
// body — see handleLine. A block that never gets its terminator therefore
// stays open until the connection ends, and the teardown fails the command
// waiting on it.
func (cc *ControlClient) beginBlock(cl ctlparse.Line) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	switch {
	case !cc.started(), unsolicitedBlock(cl), len(cc.queue) == 0:
		// A block for a command this client did not send. Absorb its body so
		// the lines are not reported as stray output.
		cc.current = &pending{number: cl.Number, orphan: true, done: make(chan struct{})}
	default:
		p := cc.queue[0]
		cc.queue = cc.queue[1:]
		p.number = cl.Number
		cc.current = p
	}
}

// unsolicitedBlock reports a block tmux opened for a command this client did
// not send, by the flags word of its header. See beginBlock.
func unsolicitedBlock(cl ctlparse.Line) bool { return cl.HasFlags && cl.Flags == 0 }

// failHead fails the command at the front of the queue, for a line that
// destroyed the binding between commands and blocks rather than one that
// answered anything.
//
// It is a guess, and deliberately the loud one. The queue is bound by order,
// so the front is the command the ruined block header would have answered —
// unless tmux had opened that block for itself, which its flags word would
// have said had the header parsed. Failing the wrong command reports an error
// to a caller whose command tmux may yet answer; not failing any hands the
// next caller another command's output with no error at all, and the one
// after that the same, for the life of the connection. The first is a bounded
// lie about one command and the second is an unbounded one about all of them.
//
// The startup block is the one unsolicited block that certainly arrives, and
// handleLine keeps it out of here by hand: a ruined header on the connection's
// first line lifts the startup barrier, so a command could be queued a moment
// later and would be failed for a block that was never going to answer it.
func (cc *ControlClient) failHead(err error) {
	cc.mu.Lock()
	if len(cc.queue) == 0 {
		cc.mu.Unlock()
		return
	}
	p := cc.queue[0]
	cc.queue = cc.queue[1:]
	p.err = fmt.Errorf("gotmucks: control command %q cannot be answered: %w", p.cmd, err)
	cc.mu.Unlock()

	p.finish()
}

// endBlock closes the open block and wakes the command waiting on it.
//
// handleLine has already matched the terminator to the block by number, so the
// only case left to report here is one arriving when no block is open at all.
func (cc *ControlClient) endBlock(cl ctlparse.Line, failed bool) {
	var (
		p     *pending
		stray Event
	)

	cc.mu.Lock()
	p, cc.current = cc.current, nil
	if p == nil {
		stray = &ProtocolError{Line: cl.Raw, Reason: "block terminator with no open block"}
	} else {
		p.failed = failed
		p.answered = true
		p.flags = cl.Flags
		p.replyTime = time.Unix(cl.Time, 0)
	}
	cc.mu.Unlock()

	// A block has closed, so whatever tmux was going to say for itself has
	// been said and commands may be written.
	cc.releaseStartup()

	if stray != nil {
		cc.emit(stray)
	}
	if p != nil && !p.orphan {
		p.finish()
	}
}

// dataLine adds a line to the open block, or reports it if there is none.
func (cc *ControlClient) dataLine(line string) {
	cc.mu.Lock()
	if cc.current != nil {
		cc.current.lines = append(cc.current.lines, line)
		cc.mu.Unlock()
		return
	}
	cc.mu.Unlock()

	if line == "" {
		return
	}
	cc.emit(&ProtocolError{Line: line, Reason: "output outside a command block"})
}

// handleNotification turns a notification line into an event. It reports
// whether the reader should carry on.
//
// Every identifier is checked before it is put into an ID type, and a
// notification whose identifier is not one is reported as a protocol error
// instead of being delivered. The ID types are defined over string, so this is
// the only thing standing between a field that arrived on the wire and a value
// a caller will hand to a call that builds a -t: an event carrying a name in a
// WindowID would be the one place in this package where an identifier is not
// an identifier. No tmux in the matrix sends anything else; a release that did
// would say so here rather than quietly through every later call.
func (cc *ControlClient) handleNotification(cl ctlparse.Line) bool {
	switch cl.Name {
	case "output":
		if o, ok := ctlparse.ParseOutput(cl.Args); ok {
			cc.deliverOutput(PaneID(o.Pane), escape.UnescapeString(o.Data), false, 0)
		} else {
			cc.malformed(cl)
		}

	case "extended-output":
		if o, ok := ctlparse.ParseExtendedOutput(cl.Args); ok {
			cc.deliverOutput(PaneID(o.Pane), escape.UnescapeString(o.Data), true,
				time.Duration(o.AgeMS)*time.Millisecond)
		} else {
			cc.malformed(cl)
		}

	case "pause":
		if p, ok := paneArg(cl.Args); ok {
			cc.emit(PanePaused{Pane: p})
		} else {
			cc.malformed(cl)
		}

	case "continue":
		if p, ok := paneArg(cl.Args); ok {
			cc.emit(PaneContinued{Pane: p})
		} else {
			cc.malformed(cl)
		}

	case "pane-mode-changed":
		if p, ok := paneArg(cl.Args); ok {
			cc.emit(PaneModeChanged{Pane: p})
		} else {
			cc.malformed(cl)
		}

	case "sessions-changed":
		cc.emit(SessionsChanged{})

	case "session-changed":
		// This is also the one payload that becomes state a caller can read
		// back later, through [ControlClient.AttachedSession], so the check
		// keeps a name out of that as well as out of the event.
		// ParseIDAndName is satisfied by any non-empty first field and says
		// nothing about sigils.
		id, name, ok := ctlparse.ParseIDAndName(cl.Args)
		if !ok || !SessionID(id).Valid() {
			cc.malformed(cl)
			break
		}
		cc.mu.Lock()
		cc.attached = SessionID(id)
		cc.mu.Unlock()
		cc.emit(SessionChanged{Session: SessionID(id), Name: name})

	case "session-renamed":
		// The exception to the rule above, and why it is one: tmux sends only
		// the new name for the attached session in some releases and
		// "$id name" in others, so a first field that is not an identifier is
		// the older form rather than a fault. The event then names the
		// session this client is attached to, which was itself checked when
		// it arrived.
		id, name, ok := ctlparse.ParseIDAndName(cl.Args)
		if !ok || !SessionID(id).Valid() {
			cc.emit(SessionRenamed{Session: cc.AttachedSession(), Name: strings.TrimSpace(cl.Args)})
			break
		}
		cc.emit(SessionRenamed{Session: SessionID(id), Name: name})

	case "session-window-changed":
		a, b, ok := ctlparse.ParseTwoIDs(cl.Args)
		if !ok || !SessionID(a).Valid() || !WindowID(b).Valid() {
			cc.malformed(cl)
			break
		}
		cc.emit(SessionWindowChanged{Session: SessionID(a), Window: WindowID(b)})

	// Each of the three window notifications has two spellings, and which one
	// arrives says nothing about the window — only whether this client's own
	// session has a link to it. A caller watching for windows wants both, so
	// both produce the same event. The names are tmux's own, taken from
	// scripts/probe-notify.sh rather than from the manual; there is no
	// "linked-" spelling of any of them.
	case "window-add", "unlinked-window-add":
		if w, ok := windowArg(cl.Args); ok {
			cc.emit(WindowAdded{Window: w})
		} else {
			cc.malformed(cl)
		}

	case "window-close", "unlinked-window-close":
		if w, ok := windowArg(cl.Args); ok {
			cc.emit(WindowClosed{Window: w})
		} else {
			cc.malformed(cl)
		}

	case "window-renamed", "unlinked-window-renamed":
		id, name, ok := ctlparse.ParseIDAndName(cl.Args)
		if !ok || !WindowID(id).Valid() {
			cc.malformed(cl)
			break
		}
		cc.emit(WindowRenamed{Window: WindowID(id), Name: name})

	case "window-pane-changed":
		a, b, ok := ctlparse.ParseTwoIDs(cl.Args)
		if !ok || !WindowID(a).Valid() || !PaneID(b).Valid() {
			cc.malformed(cl)
			break
		}
		cc.emit(WindowPaneChanged{Window: WindowID(a), Pane: PaneID(b)})

	case "layout-change":
		l, ok := ctlparse.ParseLayoutChange(cl.Args)
		if !ok {
			cc.malformed(cl)
			break
		}
		cc.emit(LayoutChanged{
			Window:  WindowID(l.Window),
			Layout:  l.Layout,
			Visible: l.Visible,
			Flags:   l.Flags,
		})

	case "subscription-changed":
		s, ok := ctlparse.ParseSubscriptionChanged(cl.Args)
		if !ok {
			cc.malformed(cl)
			break
		}
		cc.emit(SubscriptionChanged{
			Name:        s.Name,
			Session:     SessionID(s.Session),
			Window:      WindowID(s.Window),
			Pane:        PaneID(s.Pane),
			WindowIndex: s.WindowIndex,
			Value:       s.Value,
		})

	case "client-session-changed":
		// "<client> <session-id> <session-name>". The client is a name — a
		// tty path — so only the middle field is an identifier.
		fields := strings.SplitN(cl.Args, " ", 3)
		if len(fields) < 2 || fields[0] == "" || !SessionID(fields[1]).Valid() {
			cc.malformed(cl)
			break
		}
		ev := ClientSessionChanged{Client: fields[0], Session: SessionID(fields[1])}
		if len(fields) > 2 {
			ev.Name = fields[2]
		}
		cc.emit(ev)

	case "client-detached":
		cc.emit(ClientDetached{Client: strings.TrimSpace(cl.Args)})

	case "message":
		cc.emit(Message{Text: cl.Args})

	case "config-error":
		cc.emit(ConfigError{Text: cl.Args})

	case "paste-buffer-changed":
		cc.emit(PasteBufferChanged{Buffer: strings.TrimSpace(cl.Args)})

	case "paste-buffer-deleted":
		cc.emit(PasteBufferDeleted{Buffer: strings.TrimSpace(cl.Args)})

	case "exit":
		reason := strings.TrimSuffix(strings.TrimSpace(cl.Args), dcsExit)
		cc.mu.Lock()
		cc.sawExit = true
		cc.exitMsg = strings.TrimSpace(reason)
		cc.mu.Unlock()
		return false

	default:
		cc.emit(UnknownNotification{Name: cl.Name, Args: cl.Args})
	}
	return true
}

func (cc *ControlClient) malformed(cl ctlparse.Line) {
	cc.emit(&ProtocolError{Line: cl.Raw, Reason: "could not parse %" + cl.Name + " arguments"})
}

// paneArg reads a notification whose whole argument is a pane identifier.
func paneArg(args string) (PaneID, bool) {
	p := PaneID(strings.TrimSpace(args))
	return p, p.Valid()
}

// windowArg reads a notification whose whole argument is a window identifier.
func windowArg(args string) (WindowID, bool) {
	w := WindowID(strings.TrimSpace(args))
	return w, w.Valid()
}

// deliverOutput publishes pane bytes to the event stream and, if one is
// registered, to that pane's tap.
//
// Each destination gets its own copy. Sharing one slice between the firehose
// and a tap would hand two consumers the same mutable buffer, which is a
// worse trade than an allocation.
//
// The send to the tap happens under the lock. It cannot block — the send is
// non-blocking and the reader never waits on a consumer — and holding the
// lock across the lookup and the send is what makes [ControlClient.Untap]
// safe, since a tap closed between the two would otherwise be sent on after
// it was closed.
func (cc *ControlClient) deliverOutput(pane PaneID, data []byte, extended bool, age time.Duration) {
	cc.emit(PaneOutput{Pane: pane, Data: data, Extended: extended, Age: age})

	cc.mu.Lock()
	t := cc.taps[pane]
	if t == nil || t.closed {
		cc.mu.Unlock()
		return
	}

	cp := make([]byte, len(data))
	copy(cp, data)

	var recovered uint64
	select {
	case t.ch <- cp:
		recovered = t.drops.Swap(0)
	default:
		t.drops.Add(1)
		t.total.Add(1)
	}
	cc.mu.Unlock()

	if recovered > 0 {
		cc.emit(OutputDropped{Pane: pane, Count: recovered})
	}
}

// emit publishes an event without ever blocking the reader.
//
// A reader that blocked on a slow consumer would stall command replies and
// every other pane's output as well, so a full channel drops the event
// instead. The loss is then reported as an [EventsDropped] event.
//
// The channel is allocated two slots larger than the buffer the caller asked
// for, and ordinary events are only allowed to fill it to that requested
// size. The first spare slot is for loss reports: without it, a burst that
// filled the channel could only report itself once some later event happened
// to be emitted, so a caller that fell behind and then went quiet would never
// be told, which is the one case where being told matters most. The second is
// for the terminal event alone, so that a loss report cannot take the slot
// [Exited] needs — a consumer ranging over the channel and waiting for it
// would otherwise just see the channel close.
func (cc *ControlClient) emit(ev Event) {
	cc.flushDrops()

	if len(cc.events) < cc.cfg.eventBuffer {
		select {
		case cc.events <- ev:
			return
		default:
		}
	}

	cc.totalDrops.Add(1)
	cc.pendingDrops.Add(1)
	cc.flushDrops()
}

// emitReserved publishes an event into the slot kept for the terminal event.
// Nothing else may use that slot, so this never drops in practice; the
// default arm is there because a send on a full channel would otherwise block
// the teardown.
func (cc *ControlClient) emitReserved(ev Event) {
	select {
	case cc.events <- ev:
	default:
		cc.totalDrops.Add(1)
	}
}

// flushDrops reports accumulated losses if there is room, and puts the count
// back if there is not. It may use the first reserved slot but not the second,
// which belongs to the terminal event.
func (cc *ControlClient) flushDrops() {
	n := cc.pendingDrops.Swap(0)
	if n == 0 {
		return
	}
	if len(cc.events) <= cc.cfg.eventBuffer {
		select {
		case cc.events <- EventsDropped{Count: n}:
			return
		default:
		}
	}
	cc.pendingDrops.Add(n)
}

// finishReader tears the connection down once the read side has ended. It is
// the only place that closes the event and tap channels, which is why it must
// be the last thing the reader goroutine does.
func (cc *ControlClient) finishReader(readErr error) {
	cc.mu.Lock()
	cc.closed = true

	switch {
	case cc.sawExit || cc.userClose:
		// Expected end: tmux said so, or we asked for it.
	case readErr != nil:
		cc.exitErr = fmt.Errorf("gotmucks: reading control stream: %v: %w", readErr, ErrServerExited)
	default:
		if msg := strings.TrimSpace(cc.stderr.String()); msg != "" {
			cc.exitErr = fmt.Errorf("gotmucks: control connection closed: %s: %w", msg, ErrServerExited)
		} else {
			cc.exitErr = fmt.Errorf("gotmucks: control connection closed unexpectedly: %w", ErrServerExited)
		}
	}

	// Wake every command that will now never be answered.
	waiting := cc.queue
	cc.queue = nil
	if cc.current != nil {
		waiting = append(waiting, cc.current)
		cc.current = nil
	}

	// The pane is carried alongside the tap because the drops still owed to
	// it are reported below, and an [OutputDropped] says which pane lost.
	type closing struct {
		pane PaneID
		t    *tap
	}
	taps := make([]closing, 0, len(cc.taps))
	for pane, t := range cc.taps {
		if !t.closed {
			t.closed = true
			taps = append(taps, closing{pane, t})
		}
	}

	exitMsg, exitErr := cc.exitMsg, cc.exitErr
	cc.mu.Unlock()

	for _, p := range waiting {
		if !p.orphan {
			p.finish()
		}
	}

	// Say what each tap lost and never got the chance to report. A drop is
	// otherwise only reported when delivery to that pane next succeeds, so a
	// pane that overflowed in the burst that ended the connection would have
	// gone silently short. This is the tap's half of what emit's flushDrops
	// does for the event stream, and for the same reason: being told matters
	// most when nothing further is coming.
	for _, c := range taps {
		if n := c.t.drops.Swap(0); n > 0 {
			cc.emit(OutputDropped{Pane: c.pane, Count: n})
		}
	}

	cc.emitReserved(Exited{Reason: exitMsg, Err: exitErr})

	for _, c := range taps {
		close(c.t.ch)
	}
	close(cc.events)

	// A command written now would never be answered, so nothing should be
	// waiting on the barrier for the two seconds the backstop would take.
	cc.releaseStartup()
	cc.closeStdin()
	cc.doneOnce.Do(func() { close(cc.done) })

	// Reap last. Close waits on done and then reaps itself, so a caller that
	// never calls Close still does not leave a zombie and its pipes behind,
	// and one that does is not made to wait for the process here.
	_ = cc.reap()
}
