// Package ctlparse classifies and decodes the lines of a tmux control-mode
// stream.
//
// It deals in neutral structs rather than the library's public event types.
// That keeps the whole of the wire format testable from a table of raw lines,
// and avoids the import cycle that would otherwise exist between the parser
// and the package that defines the events.
//
// Classification here is of one line in isolation, which is all this package
// sees. It is not on its own enough to dispatch on, and a reader that treats
// it as if it were has a hole in it: a command's output block is contiguous,
// so a line inside one is that command's output even when it is shaped like a
// notification, and capture-pane will happily print such a line. The block
// state that resolves it lives in the reader — see ControlClient.handleLine —
// and scripts/probe-interleave.sh is what says tmux never writes a
// notification into an open block, which is what makes that resolution
// correct rather than merely convenient.
package ctlparse

import (
	"strconv"
	"strings"
)

// Kind is what a control-mode line turned out to be.
type Kind int

const (
	// KindData is a line that is not a control line. Inside a command block
	// it is one line of that command's output; outside one it is unexpected.
	//
	// A data line may begin with '%': "list-panes -F '#{pane_id}'" prints
	// "%0". Classification is therefore by the whole first token against the
	// set of known notification names, never by the '%' alone.
	KindData Kind = iota
	// KindBegin opens a command's output block.
	KindBegin
	// KindEnd closes a command's output block successfully.
	KindEnd
	// KindError closes a command's output block with a failure; the block
	// body is the error message.
	KindError
	// KindNotification is an asynchronous notification.
	KindNotification
)

// String names the kind for diagnostics.
func (k Kind) String() string {
	switch k {
	case KindBegin:
		return "begin"
	case KindEnd:
		return "end"
	case KindError:
		return "error"
	case KindNotification:
		return "notification"
	default:
		return "data"
	}
}

// Line is one classified control-mode line.
type Line struct {
	Kind Kind
	// Raw is the line as received, without its trailing newline.
	Raw string

	// Time, Number and Flags are the three arguments of %begin, %end and
	// %error: epoch seconds, the command number, and a flags word.
	//
	// Number identifies the block: a %begin and the %end or %error that
	// closes it carry the same one. It is not a key a client can predict —
	// numbers neither start at zero nor run contiguously.
	//
	// Flags is 1 for a block that answers a command the control client sent
	// and 0 for one tmux opened by itself, which is what makes an unsolicited
	// block recognisable. In tmux's cmd-queue.c the field is
	// !!(state->flags & CMDQ_STATE_CONTROL), and control.c sets that bit only
	// for a line read from the control client's own input.
	Time   int64
	Number int
	Flags  int
	// HasFlags reports that the line carried a flags field at all. A tmux
	// that stopped writing one would otherwise be indistinguishable from one
	// writing zero, and zero is load-bearing.
	HasFlags bool

	// Name is a notification's name without its leading '%', for instance
	// "output" or "window-add".
	Name string
	// Args is everything after the notification name, with one separating
	// space removed.
	Args string

	// Malformed reports a line that looked like a control line but whose
	// arguments did not parse. The line is still returned so a caller can
	// surface it rather than silently dropping it.
	Malformed bool
}

// notifications is the set of notification names tmux emits. Membership in
// this set is what distinguishes "%output ..." from a data line that happens
// to start with '%', such as a pane id.
//
// Unknown names beginning with '%' followed by a non-digit are also treated
// as notifications, so that a newer tmux is reported rather than mistaken for
// command output; see [Classify].
//
// The names come from tmux, not from this package's reading of the manual,
// and scripts/probe-notify.sh is what says so: it lists the "%name" format
// strings a tmux binary contains, lists the ones a live server emits with a
// control client attached to one session while a second client mutates
// another, and diffs both against this map. A name spelled by hand instead
// passes every unit test — those feed the reader lines the tests themselves
// spell — and then matches nothing on the wire. Three did: the rename of an
// unlinked window was spelled without its trailing 'd', and there were entries
// for "linked-window-add" and "linked-window-close", which were invented
// whole. tmux pairs window-add with unlinked-window-add and window-close with
// unlinked-window-close, and has no "linked-" spelling of anything.
//
// Only one direction of that diff is a defect. A name tmux sends and this map
// does not have loses the typed event for it; a name this map has and tmux
// never sends costs nothing, so the table may run ahead of the floor and
// does. 3.2a's binary contains none of config-error, message,
// paste-buffer-changed or paste-buffer-deleted; the first is written by
// cfg.c and the last two by control-notify.c in later tmux, and message is
// carried from the protocol documentation and has not been seen on a wire
// here — it is the one entry in this map still resting on reading.
//
// begin, end and error are block framing rather than notifications and are
// here so Classify can switch on them. They are also the three names the
// probe cannot find in a binary, because tmux writes all three through one
// format with the word passed as an argument: xasprintf(&line,
// "%%%s %ld %u %d", guard, ...) in control_write_guard.
var notifications = map[string]bool{
	"begin":                   true,
	"end":                     true,
	"error":                   true,
	"output":                  true,
	"extended-output":         true,
	"pause":                   true,
	"continue":                true,
	"exit":                    true,
	"sessions-changed":        true,
	"session-changed":         true,
	"session-renamed":         true,
	"session-window-changed":  true,
	"window-add":              true,
	"window-close":            true,
	"window-renamed":          true,
	"window-pane-changed":     true,
	"unlinked-window-add":     true,
	"unlinked-window-close":   true,
	"unlinked-window-renamed": true,
	"pane-mode-changed":       true,
	"layout-change":           true,
	"client-session-changed":  true,
	"client-detached":         true,
	"subscription-changed":    true,
	"message":                 true,
	"config-error":            true,
	"paste-buffer-changed":    true,
	"paste-buffer-deleted":    true,
}

// IsKnownNotification reports whether name, given without its '%', is a
// notification this package recognises.
func IsKnownNotification(name string) bool { return notifications[name] }

// Classify decides what a single control-mode line is.
//
// The line must already have had its trailing newline removed.
func Classify(line string) Line {
	l := Line{Raw: line}
	if !strings.HasPrefix(line, "%") {
		return l // KindData
	}

	name, args, _ := strings.Cut(line[1:], " ")
	if name == "" {
		return l
	}

	// A '%' followed by a digit is a pane id in command output, not a
	// notification. tmux has no notification whose name starts with a digit.
	if name[0] >= '0' && name[0] <= '9' {
		return l
	}
	if !notifications[name] && !looksLikeNotificationName(name) {
		return l
	}

	switch name {
	case "begin", "end", "error":
		l.Kind = KindBegin
		if name == "end" {
			l.Kind = KindEnd
		} else if name == "error" {
			l.Kind = KindError
		}
		if !parseBlockArgs(&l, args) {
			l.Malformed = true
		}
		return l
	default:
		l.Kind = KindNotification
		l.Name = name
		l.Args = args
		return l
	}
}

// looksLikeNotificationName admits names this package does not know about, so
// that a newer tmux's notifications are surfaced as unknown rather than
// misread as command output. The shape required is conservative: lowercase
// letters and hyphens only.
func looksLikeNotificationName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			continue
		}
		if c == '-' && i > 0 {
			continue
		}
		return false
	}
	return len(name) > 0
}

// parseBlockArgs reads the "<timestamp> <number> <flags>" tail of %begin,
// %end and %error.
func parseBlockArgs(l *Line, args string) bool {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return false
	}
	t, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return false
	}
	l.Time, l.Number = t, n
	if len(fields) >= 3 {
		if f, err := strconv.Atoi(fields[2]); err == nil {
			l.Flags, l.HasFlags = f, true
		}
	}
	return true
}

// Output is a decoded %output notification. Data is still escaped; the caller
// unescapes it, so that this package stays free of policy about allocation.
type Output struct {
	Pane string
	Data string
	// Extended reports that this came from %extended-output, which replaces
	// %output when flow control is enabled.
	Extended bool
	// AgeMS is how many milliseconds behind the data is. It is meaningful
	// only when Extended is set.
	AgeMS int64
}

// ParseOutput decodes the arguments of %output: a pane id and the escaped
// data, separated by one space.
func ParseOutput(args string) (Output, bool) {
	// Cut leaves data empty when there is no space, which is the right answer
	// for a notification carrying a pane id and nothing else.
	pane, data, _ := strings.Cut(args, " ")
	if !isPaneID(pane) {
		return Output{}, false
	}
	return Output{Pane: pane, Data: data}, true
}

// ParseExtendedOutput decodes the arguments of %extended-output.
//
// The documented shape is "%pane age ... : data": a pane id, a millisecond
// age, zero or more further fields reserved by tmux, then a lone colon and
// the escaped data. Parsing keys off the " : " separator rather than a fixed
// field count so that fields added later do not break it.
func ParseExtendedOutput(args string) (Output, bool) {
	pane, rest, ok := strings.Cut(args, " ")
	if !ok || !isPaneID(pane) {
		return Output{}, false
	}

	out := Output{Pane: pane, Extended: true}

	meta, data, ok := strings.Cut(rest, " : ")
	if !ok {
		// Tolerate a form without the colon: "<pane> <age> <data>".
		age, d, ok2 := strings.Cut(rest, " ")
		if !ok2 {
			return Output{}, false
		}
		n, err := strconv.ParseInt(age, 10, 64)
		if err != nil {
			return Output{}, false
		}
		out.AgeMS, out.Data = n, d
		return out, true
	}

	fields := strings.Fields(meta)
	if len(fields) == 0 {
		return Output{}, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return Output{}, false
	}
	out.AgeMS, out.Data = n, data
	return out, true
}

// Subscription is a decoded %subscription-changed notification.
type Subscription struct {
	Name string
	// Session, Window and Pane are whichever identifiers tmux included; each
	// is empty when absent.
	Session, Window, Pane string
	// WindowIndex is the bare integer field tmux includes for window
	// subscriptions, or -1 when absent.
	WindowIndex int
	// Value is the expanded format.
	Value string
}

// ParseSubscriptionChanged decodes %subscription-changed.
//
// The shape is "<name> <ids...> : <value>". The identifier fields vary with
// the subscription's target, so they are matched by sigil rather than by
// position, and anything unrecognised is ignored rather than fatal.
func ParseSubscriptionChanged(args string) (Subscription, bool) {
	name, rest, ok := strings.Cut(args, " ")
	if !ok || name == "" {
		if name == "" {
			return Subscription{}, false
		}
		// A subscription with no value yet.
		return Subscription{Name: name, WindowIndex: -1}, true
	}

	sub := Subscription{Name: name, WindowIndex: -1}

	meta, value, ok := strings.Cut(rest, " : ")
	if !ok {
		// No separator: treat the remainder as the value.
		sub.Value = rest
		return sub, true
	}
	sub.Value = value

	for _, f := range strings.Fields(meta) {
		switch {
		case isSessionID(f):
			sub.Session = f
		case isWindowID(f):
			sub.Window = f
		case isPaneID(f):
			sub.Pane = f
		default:
			if n, err := strconv.Atoi(f); err == nil && sub.WindowIndex < 0 {
				sub.WindowIndex = n
			}
		}
	}
	return sub, true
}

// Layout is a decoded %layout-change notification.
type Layout struct {
	Window string
	// Layout is the window's layout string.
	Layout string
	// Visible is the visible layout, present since tmux 2.x. Empty if absent.
	Visible string
	// Flags is the window flags field. Empty if absent.
	Flags string
}

// ParseLayoutChange decodes "%layout-change @window layout [visible] [flags]".
func ParseLayoutChange(args string) (Layout, bool) {
	fields := strings.Fields(args)
	if len(fields) < 2 || !isWindowID(fields[0]) {
		return Layout{}, false
	}
	l := Layout{Window: fields[0], Layout: fields[1]}
	if len(fields) > 2 {
		l.Visible = fields[2]
	}
	if len(fields) > 3 {
		l.Flags = fields[3]
	}
	return l, true
}

// ParseIDAndName decodes notifications shaped "<id> <name...>", such as
// %window-renamed and %session-changed. The name is everything after the
// first space and may itself contain spaces.
func ParseIDAndName(args string) (id, name string, ok bool) {
	id, name, ok = strings.Cut(args, " ")
	if id == "" {
		return "", "", false
	}
	return id, name, true
}

// ParseTwoIDs decodes notifications shaped "<id> <id>", such as
// %window-pane-changed.
func ParseTwoIDs(args string) (first, second string, ok bool) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

func isSessionID(s string) bool { return hasSigilAndDigits(s, '$') }
func isWindowID(s string) bool  { return hasSigilAndDigits(s, '@') }
func isPaneID(s string) bool    { return hasSigilAndDigits(s, '%') }

func hasSigilAndDigits(s string, sigil byte) bool {
	if len(s) < 2 || s[0] != sigil {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
