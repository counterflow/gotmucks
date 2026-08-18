package gotmucks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Subscription target selectors, the middle field of tmux's
// "refresh-client -B name:target:format".
const (
	// SubscribeSession expands the format once for the attached session.
	SubscribeSession = ""
	// SubscribeAllPanes expands the format once per pane.
	SubscribeAllPanes = "%*"
	// SubscribeAllWindows expands the format once per window.
	SubscribeAllWindows = "@*"
)

// SubscribePane is the subscription target for one pane.
func SubscribePane(id PaneID) string { return string(id) }

// SubscribeWindow is the subscription target for one window.
func SubscribeWindow(id WindowID) string { return string(id) }

// Subscribe asks tmux to report a format's value as it changes, delivered as
// [SubscriptionChanged] events.
//
// target selects what the format is expanded for: [SubscribeSession],
// [SubscribeAllPanes], [SubscribeAllWindows], or a single object via
// [SubscribePane] or [SubscribeWindow]. tmux expands the format once per
// matching object and reports a change at most once a second.
//
// Subscriptions are how a caller tracks tmux state without polling
// list-sessions. Re-subscribing under an existing name replaces it; use
// [ControlClient.Unsubscribe] to remove one.
func (cc *ControlClient) Subscribe(ctx context.Context, name, target, format string) error {
	if name == "" {
		return errors.New("gotmucks: empty subscription name")
	}
	if strings.ContainsAny(name, ": \t") {
		return fmt.Errorf("gotmucks: subscription name %q may not contain a colon or whitespace", name)
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-B", name+":"+target+":"+format)
	return err
}

// Unsubscribe removes a subscription. tmux reads a -B argument with no colons
// as a removal.
func (cc *ControlClient) Unsubscribe(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("gotmucks: empty subscription name")
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-B", name)
	return err
}

// SetSize sets the size of the control client, which bounds the size of the
// windows it is attached to.
//
// A control client has no terminal, so tmux would otherwise use a default;
// setting this is what makes pane geometry predictable.
func (cc *ControlClient) SetSize(ctx context.Context, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("gotmucks: invalid client size %dx%d", cols, rows)
	}
	// tmux 3.2 spells this "WxH"; older releases wanted "W,H". The package
	// floor is 3.2, so the comma form is only reachable if the version check
	// was skipped against an older binary.
	arg := fmt.Sprintf("%dx%d", cols, rows)
	if !cc.version.Unknown && cc.version.Major > 0 && !cc.version.AtLeast(Version{Major: 3, Minor: 2}) {
		arg = fmt.Sprintf("%d,%d", cols, rows)
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-C", arg)
	return err
}

// PauseAfter enables flow control: tmux stops sending a pane's output once
// the client is more than d behind, and says so with a [PanePaused] event.
// Call [ControlClient.Resume] to restart that pane.
//
// With flow control on, pane output arrives as %extended-output instead of
// %output, so [PaneOutput] values gain a meaningful
// [PaneOutput.Age] and report Extended.
//
// This is what stops a slow consumer from making tmux block a pane's process
// indefinitely: backpressure is a feature of the protocol rather than
// something the caller has to build.
//
// A duration of zero or less clears the flag and disables flow control.
// tmux's resolution here is whole seconds; a shorter non-zero duration is
// rounded up to one second.
func (cc *ControlClient) PauseAfter(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		_, err := cc.DoArgs(ctx, "refresh-client", "-f", "")
		return err
	}
	secs := int(d / time.Second)
	if d%time.Second != 0 || secs == 0 {
		secs++
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-f", fmt.Sprintf("pause-after=%d", secs))
	return err
}

// Resume restarts output for a pane that flow control paused.
func (cc *ControlClient) Resume(ctx context.Context, pane PaneID) error {
	if !pane.Valid() {
		return fmt.Errorf("gotmucks: %q is not a pane id", string(pane))
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-A", string(pane)+":continue")
	return err
}

// Pause stops output for a pane without waiting for it to fall behind.
func (cc *ControlClient) Pause(ctx context.Context, pane PaneID) error {
	if !pane.Valid() {
		return fmt.Errorf("gotmucks: %q is not a pane id", string(pane))
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-A", string(pane)+":pause")
	return err
}

// quoteArg renders a single argument for tmux's own command parser.
//
// tmux parses the command line it is given, so an argument containing a
// space, a '#' (which introduces a format) or a '$' (a variable) has to be
// protected. Single quotes are preferred because tmux performs no expansion
// inside them, which matters for format strings; an argument that itself
// contains a single quote falls back to backslash escaping.
func quoteArg(s string) string {
	if s == "" {
		return "''"
	}
	if !needsQuoting(s) {
		return s
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.Grow(len(s) * 2)
	for i := 0; i < len(s); i++ {
		if !safeByte(s[i]) {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func needsQuoting(s string) bool {
	for i := 0; i < len(s); i++ {
		if !safeByte(s[i]) {
			return true
		}
	}
	return false
}

// safeByte reports whether a byte can appear unquoted in a tmux command line.
// The set is deliberately small: everything outside it is quoted, and quoting
// something that did not need it is harmless.
func safeByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z',
		c >= 'A' && c <= 'Z',
		c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '_', '-', '.', '/', ':', '=', ',', '@', '+', '%':
		return true
	}
	return false
}
