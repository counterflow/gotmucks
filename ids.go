package gotmucks

import (
	"fmt"
	"strconv"
	"strings"
)

// tmux gives every session, window and pane a stable identifier that survives
// renaming and renumbering: sessions are "$0", windows "@1", panes "%2". Names
// and indexes are neither stable nor unique, so this package addresses objects
// by ID everywhere and exposes the three kinds as distinct types — a PaneID
// will not compile where a WindowID is wanted.

// SessionID is a tmux session identifier, of the form "$0".
type SessionID string

// WindowID is a tmux window identifier, of the form "@0".
type WindowID string

// PaneID is a tmux pane identifier, of the form "%0".
type PaneID string

// Sigils introducing each kind of identifier.
const (
	sessionSigil = '$'
	windowSigil  = '@'
	paneSigil    = '%'
)

// Target is anything that can be named in a tmux -t argument. The three ID
// types implement it, as does [GlobalTarget] for server- and global-scoped
// commands.
//
// The interface is closed: it has an unexported method so that only this
// package can add targets, which keeps -t construction total.
type Target interface {
	// TargetArg is the literal value passed to tmux's -t flag.
	TargetArg() string
	isTarget()
}

// TargetArg implements [Target].
func (id SessionID) TargetArg() string { return string(id) }

// TargetArg implements [Target].
func (id WindowID) TargetArg() string { return string(id) }

// TargetArg implements [Target].
func (id PaneID) TargetArg() string { return string(id) }

func (SessionID) isTarget() {}
func (WindowID) isTarget()  {}
func (PaneID) isTarget()    {}

// GlobalTarget addresses no particular object. Commands that accept it omit
// -t entirely, which tmux reads as "the server" or "the global option set"
// depending on the command.
type GlobalTarget struct{}

// Global is the zero-object target. Passing it to a command omits -t.
var Global = GlobalTarget{}

// TargetArg implements [Target]. It is always empty; callers building argv
// must check for the empty string and omit -t rather than passing it blank.
func (GlobalTarget) TargetArg() string { return "" }

func (GlobalTarget) isTarget() {}

// String returns the identifier as tmux writes it.
func (id SessionID) String() string { return string(id) }

// String returns the identifier as tmux writes it.
func (id WindowID) String() string { return string(id) }

// String returns the identifier as tmux writes it.
func (id PaneID) String() string { return string(id) }

// Valid reports whether the identifier is well formed: a '$' followed by at
// least one decimal digit.
func (id SessionID) Valid() bool { return validID(string(id), sessionSigil) }

// Valid reports whether the identifier is well formed: an '@' followed by at
// least one decimal digit.
func (id WindowID) Valid() bool { return validID(string(id), windowSigil) }

// Valid reports whether the identifier is well formed: a '%' followed by at
// least one decimal digit.
func (id PaneID) Valid() bool { return validID(string(id), paneSigil) }

// Ordinal returns the numeric part of the identifier. It reports an error if
// the identifier is malformed.
func (id SessionID) Ordinal() (int, error) { return idOrdinal(string(id), sessionSigil, "session") }

// Ordinal returns the numeric part of the identifier.
func (id WindowID) Ordinal() (int, error) { return idOrdinal(string(id), windowSigil, "window") }

// Ordinal returns the numeric part of the identifier.
func (id PaneID) Ordinal() (int, error) { return idOrdinal(string(id), paneSigil, "pane") }

// ParseSessionID validates s and returns it as a [SessionID].
func ParseSessionID(s string) (SessionID, error) {
	if !validID(s, sessionSigil) {
		return "", badID(s, sessionSigil, "session")
	}
	return SessionID(s), nil
}

// ParseWindowID validates s and returns it as a [WindowID].
func ParseWindowID(s string) (WindowID, error) {
	if !validID(s, windowSigil) {
		return "", badID(s, windowSigil, "window")
	}
	return WindowID(s), nil
}

// ParsePaneID validates s and returns it as a [PaneID].
func ParsePaneID(s string) (PaneID, error) {
	if !validID(s, paneSigil) {
		return "", badID(s, paneSigil, "pane")
	}
	return PaneID(s), nil
}

func validID(s string, sigil byte) bool {
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

func idOrdinal(s string, sigil byte, kind string) (int, error) {
	if !validID(s, sigil) {
		return 0, badID(s, sigil, kind)
	}
	n, err := strconv.Atoi(s[1:])
	if err != nil {
		// Unreachable while validID only admits decimal digits, but a huge
		// run of digits can still overflow.
		return 0, fmt.Errorf("gotmucks: %s id %q: %w", kind, s, err)
	}
	return n, nil
}

func badID(s string, sigil byte, kind string) error {
	return fmt.Errorf("gotmucks: %q is not a %s id (want %c followed by digits)", s, kind, sigil)
}

// trimID strips a trailing newline and surrounding space from raw tmux output
// before it is treated as an identifier.
func trimID(b []byte) string { return strings.TrimSpace(string(b)) }
