package gotmucks

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoServer reports that no tmux server is listening on the configured
// socket.
//
// tmux exits 1 for this, the same as for a genuine failure, so this package
// classifies it from stderr and treats it as a fact rather than a fault:
// read paths ([Client.ListSessions], [Client.HasSession], [Client.ServerRunning]
// and friends) report emptiness instead of failing. Write paths that cannot
// start a server return an error wrapping ErrNoServer.
var ErrNoServer = errors.New("gotmucks: no tmux server running")

// ErrNoSession reports that the named session does not exist.
var ErrNoSession = errors.New("gotmucks: session not found")

// ErrClosed reports use of a [ControlClient] after [ControlClient.Close].
var ErrClosed = errors.New("gotmucks: control client closed")

// ErrServerExited reports that the control-mode connection ended, either
// because tmux sent %exit or because the process died. It is terminal: the
// caller reconnects if it wants to, because only the caller knows whether
// that is wanted.
var ErrServerExited = errors.New("gotmucks: control connection exited")

// ErrUnsupportedVersion reports that the tmux binary predates the minimum
// this package supports.
var ErrUnsupportedVersion = errors.New("gotmucks: tmux version too old")

// ExitError is a tmux invocation that exited non-zero.
type ExitError struct {
	// Args is the argument vector as executed, excluding the binary itself.
	Args []string
	// Code is the process exit status.
	Code int
	// Stderr is the trimmed standard error of the process.
	Stderr string
	// Err is the underlying error from os/exec, if any.
	Err error
}

func (e *ExitError) Error() string {
	var b strings.Builder
	b.WriteString("gotmucks: tmux ")
	b.WriteString(strings.Join(e.Args, " "))
	fmt.Fprintf(&b, ": exit status %d", e.Code)
	if e.Stderr != "" {
		b.WriteString(": ")
		b.WriteString(e.Stderr)
	}
	return b.String()
}

// Unwrap exposes the os/exec error, if the failure came from process
// management rather than from tmux itself.
func (e *ExitError) Unwrap() error { return e.Err }

// ControlError is a control-mode command that tmux answered with %error
// instead of %end.
type ControlError struct {
	// Command is the command line as sent.
	Command string
	// Number is the tmux command number the error was reported against.
	Number int
	// Message is the error body, with block lines joined by newlines.
	Message string
}

func (e *ControlError) Error() string {
	return fmt.Sprintf("gotmucks: control command %q (#%d): %s", e.Command, e.Number, e.Message)
}

// ProtocolError is a control-mode line the parser could not make sense of.
// It is reported as an event rather than tearing the connection down, because
// an unrecognised line is far more likely to be a newer tmux than a broken
// stream.
type ProtocolError struct {
	// Line is the offending input, verbatim.
	Line string
	// Reason describes what was wrong with it.
	Reason string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("gotmucks: malformed control line %q: %s", e.Line, e.Reason)
}

func (*ProtocolError) isEvent() {}

// noServerPatterns are the stderr shapes tmux uses to say the server is not
// there. They have drifted across releases, so all the known forms are
// matched rather than just the current one.
var noServerPatterns = []string{
	"no server running on",
	"error connecting to",
	"no such file or directory",
	"connect failed: no such file or directory",
	"server not found",
}

// isNoServerStderr reports whether stderr is tmux's way of saying the socket
// has no server behind it.
func isNoServerStderr(stderr string) bool {
	s := strings.ToLower(strings.TrimSpace(stderr))
	if s == "" {
		return false
	}
	for _, p := range noServerPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// noSessionPatterns are the stderr shapes tmux uses for a target that does
// not resolve.
var noSessionPatterns = []string{
	"can't find session",
	"can't find window",
	"can't find pane",
	"session not found",
	"no current session",
}

func isNoSessionStderr(stderr string) bool {
	s := strings.ToLower(strings.TrimSpace(stderr))
	if s == "" {
		return false
	}
	for _, p := range noSessionPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
