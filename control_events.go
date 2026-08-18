package gotmucks

import (
	"fmt"
	"time"
)

// Event is an asynchronous notification from a control-mode connection.
//
// The interface is closed — implementations all live in this package — so a
// type switch over the concrete types below is exhaustive for a given release
// of this library. New tmux notifications appear as [UnknownNotification]
// rather than as new types, so a type switch does not silently start missing
// things when tmux gains a notification this library predates.
type Event interface {
	isEvent()
}

// PaneOutput carries bytes a pane produced.
//
// Data is unescaped and owned by the receiver: the reader allocates a fresh
// slice per delivery, so it is safe to retain.
//
// Every pane's output appears here. [ControlClient.Output] additionally taps
// one pane's bytes into a dedicated channel; it does not remove them from
// this stream.
type PaneOutput struct {
	Pane PaneID
	Data []byte
	// Extended reports that this arrived as %extended-output, which replaces
	// %output once flow control is enabled with
	// [ControlClient.PauseAfter].
	Extended bool
	// Age is how far behind the data is. Meaningful only when Extended is
	// set; zero otherwise.
	Age time.Duration
}

// PanePaused reports that tmux has stopped sending a pane's output because
// the consumer fell behind. Call [ControlClient.Resume] to restart it.
//
// This is tmux applying backpressure on the caller's behalf: a slow consumer
// cannot make tmux block the pane's process indefinitely.
type PanePaused struct{ Pane PaneID }

// PaneContinued reports that a paused pane's output has resumed.
type PaneContinued struct{ Pane PaneID }

// PaneModeChanged reports that a pane entered or left a mode, such as copy
// mode.
type PaneModeChanged struct{ Pane PaneID }

// SessionsChanged reports that a session was created or destroyed. It carries
// no detail; re-read the session list if the detail matters.
type SessionsChanged struct{}

// SessionChanged reports that the control client's attached session changed.
type SessionChanged struct {
	Session SessionID
	Name    string
}

// SessionRenamed reports that the attached session was renamed.
type SessionRenamed struct {
	Session SessionID
	Name    string
}

// SessionWindowChanged reports that a session's current window changed.
type SessionWindowChanged struct {
	Session SessionID
	Window  WindowID
}

// WindowAdded reports a new window.
type WindowAdded struct{ Window WindowID }

// WindowClosed reports a window that has gone.
type WindowClosed struct{ Window WindowID }

// WindowRenamed reports a window's new name.
type WindowRenamed struct {
	Window WindowID
	Name   string
}

// WindowPaneChanged reports that a window's active pane changed.
type WindowPaneChanged struct {
	Window WindowID
	Pane   PaneID
}

// LayoutChanged reports a window's new layout.
type LayoutChanged struct {
	Window WindowID
	// Layout is the layout string.
	Layout string
	// Visible is the visible layout, when tmux sends one.
	Visible string
	// Flags is the window flags field, when tmux sends one.
	Flags string
}

// SubscriptionChanged reports a new value for a format subscription created
// with [ControlClient.Subscribe].
//
// Subscriptions are how a caller tracks tmux state without polling. tmux
// expands the format once per matching object and sends this at most once a
// second per subscription.
type SubscriptionChanged struct {
	// Name is the subscription name given to Subscribe.
	Name string
	// Session, Window and Pane identify the object the value is for.
	// Whichever do not apply are empty.
	Session SessionID
	Window  WindowID
	Pane    PaneID
	// WindowIndex is the window index tmux includes for window
	// subscriptions, or -1 when absent.
	WindowIndex int
	// Value is the expanded format.
	Value string
}

// ClientSessionChanged reports that another client switched session.
type ClientSessionChanged struct {
	Client  string
	Session SessionID
	Name    string
}

// ClientDetached reports that a client detached from the server.
type ClientDetached struct{ Client string }

// Message is a message tmux would have shown in the status line.
type Message struct{ Text string }

// ConfigError reports an error tmux hit while reading its configuration.
type ConfigError struct{ Text string }

// PasteBufferChanged reports a change to a paste buffer.
type PasteBufferChanged struct{ Buffer string }

// PasteBufferDeleted reports a deleted paste buffer.
type PasteBufferDeleted struct{ Buffer string }

// Exited reports that the control connection has ended.
//
// It is terminal and is the last event on the channel before it closes. This
// library does not reconnect on its own: only the caller knows whether a
// reconnect is wanted, and re-establishing state after one is the caller's
// business.
type Exited struct {
	// Reason is the text tmux gave with %exit, if any.
	Reason string
	// Err is why the connection ended when tmux did not say. It is nil for a
	// clean %exit.
	Err error
}

// UnknownNotification is a notification this library does not recognise,
// reported rather than discarded so that a newer tmux is visible instead of
// silent.
type UnknownNotification struct {
	// Name is the notification name without its leading '%'.
	Name string
	// Args is everything after the name.
	Args string
}

// EventsDropped reports that the event channel was full and events were
// discarded.
//
// The reader never blocks on a slow consumer — a stalled reader would stall
// the whole connection, including command replies and every other pane — so
// it drops instead, and says so here. Count is the number discarded since the
// last EventsDropped.
type EventsDropped struct{ Count uint64 }

// OutputDropped reports that a per-pane channel from [ControlClient.Output]
// was full and pane bytes were discarded. The same reasoning as
// [EventsDropped] applies.
type OutputDropped struct {
	Pane  PaneID
	Count uint64
}

func (PaneOutput) isEvent()           {}
func (PanePaused) isEvent()           {}
func (PaneContinued) isEvent()        {}
func (PaneModeChanged) isEvent()      {}
func (SessionsChanged) isEvent()      {}
func (SessionChanged) isEvent()       {}
func (SessionRenamed) isEvent()       {}
func (SessionWindowChanged) isEvent() {}
func (WindowAdded) isEvent()          {}
func (WindowClosed) isEvent()         {}
func (WindowRenamed) isEvent()        {}
func (WindowPaneChanged) isEvent()    {}
func (LayoutChanged) isEvent()        {}
func (SubscriptionChanged) isEvent()  {}
func (ClientSessionChanged) isEvent() {}
func (ClientDetached) isEvent()       {}
func (Message) isEvent()              {}
func (ConfigError) isEvent()          {}
func (PasteBufferChanged) isEvent()   {}
func (PasteBufferDeleted) isEvent()   {}
func (Exited) isEvent()               {}
func (UnknownNotification) isEvent()  {}
func (EventsDropped) isEvent()        {}
func (OutputDropped) isEvent()        {}

// String renders the event for logs and test failures.
func (e PaneOutput) String() string {
	if e.Extended {
		return fmt.Sprintf("PaneOutput{%s, %d bytes, age %s}", e.Pane, len(e.Data), e.Age)
	}
	return fmt.Sprintf("PaneOutput{%s, %d bytes}", e.Pane, len(e.Data))
}

// String renders the event for logs and test failures.
func (e Exited) String() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("Exited{%v}", e.Err)
	case e.Reason != "":
		return fmt.Sprintf("Exited{%s}", e.Reason)
	default:
		return "Exited{}"
	}
}
