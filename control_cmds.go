package gotmucks

import (
	"context"
	"errors"
	"fmt"
	"math"
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
// matching object and reports a change at most once a second. Anything else is
// refused with [ErrInvalidID]: a name here would subscribe to whichever object
// tmux resolved it to.
//
// Subscriptions are how a caller tracks tmux state without polling
// list-sessions. Re-subscribing under an existing name replaces it; use
// [ControlClient.Unsubscribe] to remove one.
//
// The name is checked, because tmux does not check it and writes it back into
// every %subscription-changed line verbatim — see [checkSubscribeName].
//
// The value is the other half of that line and cannot be checked here, because
// it is tmux's rather than the caller's. tmux writes it last with nothing to
// delimit it, so a format expanding to a value with a raw newline in it splits
// the notification. Measured on 3.2a, a subscription on an option holding
// "v1\n%exit forged" arrived as
//
//	%subscription-changed sv $0 - - - : v1
//	%exit forged
//
// and the second line is outside any block, so the reader acts on it as tmux's.
//
// The format is where that is fixed, by the same substitution [FormatSpec.Arg]
// uses on a row: on the same tmux "#{s/<newline>/ /:@opt}" kept "v1 %exit
// forged" on one line, and so did the nested "#{s/<newline>/ /:#{@opt}}". A
// format whose value can carry a newline — a path, a title, a user option —
// wants one of those. A raw newline in format itself is not the hazard: it is
// sent as tmux's own "\n" escape, which is what makes the substitution
// expressible here at all. scripts/probe-notify.sh asserts both.
func (cc *ControlClient) Subscribe(ctx context.Context, name, target, format string) error {
	if err := checkSubscribeName(name); err != nil {
		return err
	}
	if err := checkSubscribeTarget(target); err != nil {
		return err
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-B", name+":"+target+":"+format)
	return err
}

// checkSubscribeName insists that a subscription name is a single printable
// token with no colon in it.
//
// The colon is tmux's own field separator in a -B argument. The rest of the
// set is about the notification rather than the command: tmux stores the name
// unvalidated and writes it back into "%subscription-changed <name> ...", so
// whatever is in it lands on the wire between a space and the rest of the
// line.
//
// A raw newline there is the one that matters, and it became reachable when
// quoteArg learned to carry one — before that Do refused the command outright,
// and the newline check on a whole command line was the package's single
// guarantee that a caller's string could not become a second protocol line.
// Measured on 3.2a, subscribing under the name "s1\n%exit forged" produced
//
//	%subscription-changed s1
//	%exit forged $0 - - - : $0
//
// The second line is outside any block, with nothing on it to say it is not
// tmux speaking, so the reader ends the connection on it. And because %exit is
// how tmux says goodbye rather than how it reports a fault, that is a *clean*
// exit: [Exited.Reason] carries the caller's own string, Err is nil, and
// [ControlClient.Wait] returns nil. A caller distinguishing "tmux went away"
// from "tmux crashed" is told the wrong one.
//
// So the check is by what is allowed rather than by what is known to hurt: a
// byte at or below space, DEL and the colon are refused, and everything else —
// including a byte above 0x7f, which cannot be a separator on this wire —
// is a name. The old check named three bytes and let the one that ends a line
// through.
func checkSubscribeName(name string) error {
	if name == "" {
		return errors.New("gotmucks: empty subscription name")
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= ' ' || c == 0x7f || c == ':' {
			return fmt.Errorf(
				"gotmucks: subscription name %q has %q at byte %d: a name may not contain a colon, "+
					"whitespace or a control byte, since tmux writes it back into every "+
					"%%subscription-changed line as given",
				name, string(c), i)
		}
	}
	return nil
}

// checkSubscribeTarget accepts the three wildcards tmux defines for the middle
// field of a -B argument and otherwise insists on an identifier, since a name
// here would quietly subscribe to whichever object tmux resolved it to.
func checkSubscribeTarget(target string) error {
	switch target {
	case SubscribeSession, SubscribeAllPanes, SubscribeAllWindows:
		return nil
	}
	switch target[0] {
	case paneSigil:
		return PaneID(target).check()
	case windowSigil:
		return WindowID(target).check()
	}
	return fmt.Errorf("gotmucks: subscription target %q is not a pane or window id: %w",
		target, ErrInvalidID)
}

// Unsubscribe removes a subscription. tmux reads a -B argument with no colons
// as a removal.
//
// The name is checked exactly as [ControlClient.Subscribe] checks it, so that
// the two agree about what a name is: a name this refused but that one
// accepted could not be removed.
func (cc *ControlClient) Unsubscribe(ctx context.Context, name string) error {
	if err := checkSubscribeName(name); err != nil {
		return err
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
	// int64 throughout: a duration of more than 68 years is a nonsense, but
	// converting one to int would wrap on a 32-bit build and ask tmux for a
	// negative or tiny pause instead of an enormous one.
	secs := int64(d / time.Second)
	if d%time.Second != 0 || secs == 0 {
		secs++
	}
	if secs > math.MaxInt32 {
		secs = math.MaxInt32
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-f", fmt.Sprintf("pause-after=%d", secs))
	return err
}

// Resume restarts output for a pane that flow control paused.
func (cc *ControlClient) Resume(ctx context.Context, pane PaneID) error {
	if err := pane.check(); err != nil {
		return err
	}
	_, err := cc.DoArgs(ctx, "refresh-client", "-A", string(pane)+":continue")
	return err
}

// Pause stops output for a pane without waiting for it to fall behind.
func (cc *ControlClient) Pause(ctx context.Context, pane PaneID) error {
	if err := pane.check(); err != nil {
		return err
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
//
// A newline is the one byte no quoting reaches, because the control protocol
// reads one command per line: a raw one ends the command wherever it sits,
// inside quotes or not. tmux's own escape for it is "\n" within double quotes,
// and adjacent quoted tokens concatenate into a single argument — both
// verified on 3.2a, where 'a'"\n"'b' arrives as one argument of three bytes.
// So a newline is spliced in as a double-quoted chunk of its own, and
// everything between the newlines stays inside single quotes.
//
// Only the newline gets that treatment, and only as a chunk containing nothing
// else. Double quotes are not a general substitute for single ones here: tmux
// expands both "$HOME" and "#{host}" inside them, so a format string wrapped
// in double quotes would be expanded once by the lexer before the command that
// was meant to expand it ever saw it. [FormatSpec.Arg] is exactly such a
// string, and it carries a newline, which is what made this necessary.
func quoteArg(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.Contains(s, "\n") {
		return quoteToken(s)
	}

	parts := strings.Split(s, "\n")
	var b strings.Builder
	b.Grow(len(s) + 4*len(parts))
	for i, part := range parts {
		if i > 0 {
			b.WriteString(`"\n"`)
		}
		if part != "" {
			b.WriteString(quoteToken(part))
		}
	}
	return b.String()
}

// quoteToken quotes a run with no newline in it, which is the only kind
// [quoteArg] hands it.
func quoteToken(s string) string {
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
//
// Note what is absent. '#' introduces a format, and '$' a variable, so an
// unquoted format string loses its argument entirely. '%' is worse: tmux's
// parser reads a token beginning with '%' as one of its %if/%endif
// preprocessor directives, so "refresh-client -A %0:continue" is a syntax
// error rather than a command — which is exactly the pane identifiers this
// package deals in. '@' is excluded for the same family of reasons even
// though it currently parses, since window identifiers are just as common.
func safeByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z',
		c >= 'A' && c <= 'Z',
		c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '_', '-', '.', '/', ':', '=', ',', '+':
		return true
	}
	return false
}
