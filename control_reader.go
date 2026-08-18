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
// only goroutine that touches stdout, which is what makes dispatch by line
// prefix sufficient: there is no interleaving to resolve beyond the
// protocol's own.
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
func (cc *ControlClient) handleLine(line string) bool {
	cl := ctlparse.Classify(line)

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

// beginBlock binds the next written-but-unanswered command to the command
// number tmux has just assigned it.
//
// Binding is by queue order rather than by predicting the number: tmux
// processes a connection's commands in the order it receives them, and writes
// are serialised, so the front of the queue is always the command this block
// belongs to. That holds whatever base tmux numbers from.
func (cc *ControlClient) beginBlock(cl ctlparse.Line) {
	var stray Event

	cc.mu.Lock()
	if cc.current != nil {
		stray = &ProtocolError{Line: cl.Raw, Reason: "%begin while a block was still open"}
		cc.current = nil
	}
	if len(cc.queue) == 0 {
		// A block for a command this client did not send. Absorb its body so
		// the lines are not reported as stray output.
		cc.current = &pending{number: cl.Number, orphan: true, done: make(chan struct{})}
	} else {
		p := cc.queue[0]
		cc.queue = cc.queue[1:]
		p.number = cl.Number
		cc.current = p
	}
	cc.mu.Unlock()

	if stray != nil {
		cc.emit(stray)
	}
}

// endBlock closes the open block and wakes the command waiting on it.
func (cc *ControlClient) endBlock(cl ctlparse.Line, failed bool) {
	var (
		p     *pending
		stray Event
	)

	cc.mu.Lock()
	p, cc.current = cc.current, nil
	switch {
	case p == nil:
		stray = &ProtocolError{Line: cl.Raw, Reason: "block terminator with no open block"}
	case p.number != cl.Number:
		stray = &ProtocolError{
			Line:   cl.Raw,
			Reason: fmt.Sprintf("terminator for command %d closed block %d", cl.Number, p.number),
		}
	}
	if p != nil {
		p.failed = failed
		p.answered = true
		p.flags = cl.Flags
		p.replyTime = time.Unix(cl.Time, 0)
	}
	cc.mu.Unlock()

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
		cc.emit(PanePaused{Pane: PaneID(strings.TrimSpace(cl.Args))})

	case "continue":
		cc.emit(PaneContinued{Pane: PaneID(strings.TrimSpace(cl.Args))})

	case "pane-mode-changed":
		cc.emit(PaneModeChanged{Pane: PaneID(strings.TrimSpace(cl.Args))})

	case "sessions-changed":
		cc.emit(SessionsChanged{})

	case "session-changed":
		id, name, ok := ctlparse.ParseIDAndName(cl.Args)
		if !ok {
			cc.malformed(cl)
			break
		}
		cc.mu.Lock()
		cc.attached = SessionID(id)
		cc.mu.Unlock()
		cc.emit(SessionChanged{Session: SessionID(id), Name: name})

	case "session-renamed":
		// tmux sends only the new name for the attached session in some
		// releases, and "$id name" in others. Both are accepted.
		id, name, ok := ctlparse.ParseIDAndName(cl.Args)
		if !ok || !SessionID(id).Valid() {
			cc.emit(SessionRenamed{Session: cc.AttachedSession(), Name: strings.TrimSpace(cl.Args)})
			break
		}
		cc.emit(SessionRenamed{Session: SessionID(id), Name: name})

	case "session-window-changed":
		a, b, ok := ctlparse.ParseTwoIDs(cl.Args)
		if !ok {
			cc.malformed(cl)
			break
		}
		cc.emit(SessionWindowChanged{Session: SessionID(a), Window: WindowID(b)})

	case "window-add", "linked-window-add", "unlinked-window-add":
		cc.emit(WindowAdded{Window: WindowID(strings.TrimSpace(cl.Args))})

	case "window-close", "linked-window-close", "unlinked-window-close":
		cc.emit(WindowClosed{Window: WindowID(strings.TrimSpace(cl.Args))})

	case "window-renamed", "unlinked-window-rename":
		id, name, ok := ctlparse.ParseIDAndName(cl.Args)
		if !ok {
			cc.malformed(cl)
			break
		}
		cc.emit(WindowRenamed{Window: WindowID(id), Name: name})

	case "window-pane-changed":
		a, b, ok := ctlparse.ParseTwoIDs(cl.Args)
		if !ok {
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
		fields := strings.SplitN(cl.Args, " ", 3)
		ev := ClientSessionChanged{}
		if len(fields) > 0 {
			ev.Client = fields[0]
		}
		if len(fields) > 1 {
			ev.Session = SessionID(fields[1])
		}
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

// deliverOutput publishes pane bytes to the event stream and, if one is
// registered, to that pane's tap.
//
// Each destination gets its own copy. Sharing one slice between the firehose
// and a tap would hand two consumers the same mutable buffer, which is a
// worse trade than an allocation.
func (cc *ControlClient) deliverOutput(pane PaneID, data []byte, extended bool, age time.Duration) {
	cc.emit(PaneOutput{Pane: pane, Data: data, Extended: extended, Age: age})

	cc.mu.Lock()
	t := cc.taps[pane]
	cc.mu.Unlock()
	if t == nil {
		return
	}

	cp := make([]byte, len(data))
	copy(cp, data)

	select {
	case t.ch <- cp:
		if n := t.drops.Swap(0); n > 0 {
			cc.emit(OutputDropped{Pane: pane, Count: n})
		}
	default:
		t.drops.Add(1)
		t.total.Add(1)
	}
}

// emit publishes an event without ever blocking the reader.
//
// A reader that blocked on a slow consumer would stall command replies and
// every other pane's output as well, so a full channel drops the event
// instead. The loss is then reported as an [EventsDropped] event.
//
// The channel is allocated one slot larger than the buffer the caller asked
// for, and ordinary events are only allowed to fill it to that requested
// size. The spare slot is reserved for loss reports and for the terminal
// event. Without the reservation, a burst that filled the channel could only
// report itself once some later event happened to be emitted — so a caller
// that fell behind and then went quiet would never be told, which is the one
// case where being told matters most.
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

// emitReserved publishes an event that may use the reserved slot. It is for
// the terminal event, which is worth more than one more notification.
func (cc *ControlClient) emitReserved(ev Event) {
	select {
	case cc.events <- ev:
	default:
		cc.totalDrops.Add(1)
	}
}

// flushDrops reports accumulated losses if there is room, and puts the count
// back if there is not.
func (cc *ControlClient) flushDrops() {
	n := cc.pendingDrops.Swap(0)
	if n == 0 {
		return
	}
	select {
	case cc.events <- EventsDropped{Count: n}:
	default:
		cc.pendingDrops.Add(n)
	}
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

	taps := make([]*tap, 0, len(cc.taps))
	for _, t := range cc.taps {
		if !t.closed {
			t.closed = true
			taps = append(taps, t)
		}
	}

	exitMsg, exitErr := cc.exitMsg, cc.exitErr
	cc.mu.Unlock()

	for _, p := range waiting {
		if !p.orphan {
			p.finish()
		}
	}

	cc.emitReserved(Exited{Reason: exitMsg, Err: exitErr})

	for _, t := range taps {
		close(t.ch)
	}
	close(cc.events)
	cc.doneOnce.Do(func() { close(cc.done) })
}
