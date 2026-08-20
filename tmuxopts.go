package gotmucks

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// OptionScope selects which of tmux's option tables a command addresses.
//
// tmux keeps separate tables for server, session, window and pane options and
// picks between them with a flag. Getting this wrong is the usual reason a
// set-option appears to succeed but changes nothing.
type OptionScope int

const (
	// ScopeSession addresses session options, tmux's default for
	// set-option and show-options.
	ScopeSession OptionScope = iota
	// ScopeServer addresses server options, tmux's -s.
	ScopeServer
	// ScopeGlobal addresses the global table for the option's own type,
	// tmux's -g.
	ScopeGlobal
	// ScopeWindow addresses window options, tmux's -w.
	ScopeWindow
	// ScopePane addresses pane options, tmux's -p.
	ScopePane
	// ScopeGlobalWindow addresses the global window option table, tmux's
	// -g -w together.
	ScopeGlobalWindow
)

func (s OptionScope) flags() []string {
	switch s {
	case ScopeServer:
		return []string{"-s"}
	case ScopeGlobal:
		return []string{"-g"}
	case ScopeWindow:
		return []string{"-w"}
	case ScopePane:
		return []string{"-p"}
	case ScopeGlobalWindow:
		return []string{"-g", "-w"}
	default:
		return nil
	}
}

// String names the scope for diagnostics.
func (s OptionScope) String() string {
	switch s {
	case ScopeServer:
		return "server"
	case ScopeGlobal:
		return "global"
	case ScopeWindow:
		return "window"
	case ScopePane:
		return "pane"
	case ScopeGlobalWindow:
		return "global-window"
	default:
		return "session"
	}
}

// targetArgs renders a -t flag for a target, or nothing for [Global].
func targetArgs(t Target) []string {
	if t == nil {
		return nil
	}
	arg := t.TargetArg()
	if arg == "" {
		return nil
	}
	return []string{"-t", arg}
}

// SetOption sets a session option on a target.
//
// For options that live in another table — a window option such as
// remain-on-exit, or a server option — use [Client.SetOptionScoped].
func (c *Client) SetOption(ctx context.Context, t Target, name, value string) error {
	return c.SetOptionScoped(ctx, t, ScopeSession, name, value)
}

// SetOptionScoped sets an option in a named scope.
func (c *Client) SetOptionScoped(ctx context.Context, t Target, scope OptionScope, name, value string) error {
	if name == "" {
		return errors.New("gotmucks: empty option name")
	}
	if err := checkTarget(t); err != nil {
		return err
	}
	args := []string{"set-option"}
	args = append(args, scope.flags()...)
	args = append(args, targetArgs(t)...)
	args = append(args, "--", name, value)
	return c.runOK(ctx, args...)
}

// UnsetOption removes an option, restoring the inherited value. This is
// tmux's set-option -u.
func (c *Client) UnsetOption(ctx context.Context, t Target, scope OptionScope, name string) error {
	if name == "" {
		return errors.New("gotmucks: empty option name")
	}
	if err := checkTarget(t); err != nil {
		return err
	}
	args := []string{"set-option", "-u"}
	args = append(args, scope.flags()...)
	args = append(args, targetArgs(t)...)
	args = append(args, "--", name)
	return c.runOK(ctx, args...)
}

// ShowOption returns the value of a single option and whether it is set in
// the requested table.
//
// A name tmux does not know, a server that is not running and a target that
// does not exist are all absences rather than failures: the value is empty,
// the bool is false, and the error is nil.
//
// It asks show-options for one name rather than using show-options -v, even
// though -v would print the value with no quoting to undo. -v cannot answer
// the question this function exists to answer: an option that is not set in
// the table produces exit 0 and an empty line, exactly like one that is set
// to the empty string — verified on 3.2a. The named form prints nothing at
// all when the option is not set there, which is the distinction the bool
// reports.
//
// An array option — status-format, command-alias — has no single value and is
// an error here rather than a quiet answer of its first element. Read one with
// [Client.ShowOptions], which reports every element under its own indexed
// name.
func (c *Client) ShowOption(ctx context.Context, t Target, scope OptionScope, name string) (string, bool, error) {
	if name == "" {
		return "", false, errors.New("gotmucks: empty option name")
	}
	if err := checkTarget(t); err != nil {
		return "", false, err
	}
	args := []string{"show-options"}
	args = append(args, scope.flags()...)
	args = append(args, targetArgs(t)...)
	args = append(args, "--", name)

	lines, err := c.runLines(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrNoServer) || isMissingTarget(err) {
			return "", false, nil
		}
		var xerr *ExitError
		if errors.As(err, &xerr) && isUnknownOptionStderr(xerr.Stderr) {
			return "", false, nil
		}
		return "", false, err
	}
	if len(lines) == 0 || lines[0] == "" {
		return "", false, nil
	}
	// "name value", or the name alone for a flag option set with no value.
	printed, value, ok := strings.Cut(lines[0], " ")
	if isArrayElement(printed, name) {
		return "", false, fmt.Errorf(
			"gotmucks: option %s is an array of %d elements; read it with ShowOptions",
			name, len(lines))
	}
	if !ok {
		return "", true, nil
	}
	return unquoteOptionValue(value), true, nil
}

// isArrayElement reports whether the name tmux printed for a value is an
// element of an array option rather than the option itself.
//
// show-options prints one "name[index] value" line per element for those, so
// reading the first line as the whole answer returns element 0 and says
// nothing about the rest — verified on 3.2a, where status-format has two
// elements and command-alias has six.
func isArrayElement(printed, name string) bool {
	if !strings.HasPrefix(printed, name+"[") || !strings.HasSuffix(printed, "]") {
		return false
	}
	index := printed[len(name)+1 : len(printed)-1]
	if index == "" {
		return false
	}
	for i := 0; i < len(index); i++ {
		if index[i] < '0' || index[i] > '9' {
			return false
		}
	}
	return true
}

// unknownOptionPatterns are how tmux says it has never heard of an option
// name. 3.2a says "invalid option: <name>"; the other spelling is kept
// because the wording has moved before and matching both costs nothing.
var unknownOptionPatterns = []string{"invalid option", "unknown option"}

func isUnknownOptionStderr(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, p := range unknownOptionPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// ShowOptions returns every option in a scope.
//
// tmux prints one "name value" pair per line, quoting values that need it and
// escaping the characters that would otherwise break the line; both are undone
// here, so a value containing a tab or a newline comes back intact.
//
// An array option appears as one entry per element, keyed by the name tmux
// printed: "status-format[0]", "status-format[1]". That is what makes this the
// call for reading one — [Client.ShowOption] refuses an array rather than
// answering with its first element.
func (c *Client) ShowOptions(ctx context.Context, t Target, scope OptionScope) (map[string]string, error) {
	if err := checkTarget(t); err != nil {
		return nil, err
	}
	args := []string{"show-options"}
	args = append(args, scope.flags()...)
	args = append(args, targetArgs(t)...)

	lines, err := c.runLines(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrNoServer) || isMissingTarget(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}

	opts := make(map[string]string, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			// A flag option printed alone means it is set with no value.
			opts[line] = ""
			continue
		}
		opts[name] = unquoteOptionValue(value)
	}
	return opts, nil
}

// unquoteOptionValue undoes what tmux does to an option value on its way out
// of show-options.
//
// tmux quotes a value that contains a space or a metacharacter, and escapes
// the rest with vis(3) in its C style: a backslash becomes "\\", a tab "\t", a
// newline "\n", and anything else unprintable three octal digits. The
// escaping is applied whether or not the value ends up quoted, so unquoting
// alone is not enough — verified on 3.2a, where a value containing a tab is
// printed unquoted as "has\ttab".
//
// This is one left-to-right pass rather than a sequence of replacements. Two
// passes over the whole string — undoing "\\" and then "\t" or the other way
// round — is the shape that turns a literal backslash followed by a t into a
// tab, and it happens to be safe here only because of the order the escapes
// are written in.
func unquoteOptionValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	if !strings.Contains(v, `\`) {
		return v
	}

	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 >= len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch c := v[i]; c {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case 's':
			b.WriteByte(' ')
		case '\\', '"', '\'':
			b.WriteByte(c)
		default:
			if o, ok := octalByte(v, i); ok {
				b.WriteByte(o)
				i += 2
				continue
			}
			// Not an escape this package knows. Keep both bytes: the value is
			// worth more than the objection.
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String()
}

// octalByte decodes exactly three octal digits starting at i.
func octalByte(s string, i int) (byte, bool) {
	if i+3 > len(s) {
		return 0, false
	}
	n := 0
	for j := i; j < i+3; j++ {
		if s[j] < '0' || s[j] > '7' {
			return 0, false
		}
		n = n*8 + int(s[j]-'0')
	}
	if n > 0xFF {
		return 0, false
	}
	return byte(n), true
}

// SetRemainOnExit controls whether a pane stays after its process exits.
//
// This is the option that makes a pane's final output readable instead of
// vanishing, so it gets a named helper. The scope follows the target: pane
// options for a [PaneID], window options otherwise, which is where tmux keeps
// remain-on-exit for each.
func (c *Client) SetRemainOnExit(ctx context.Context, t Target, on bool) error {
	scope := ScopeWindow
	if _, isPane := t.(PaneID); isPane {
		scope = ScopePane
	}
	return c.SetOptionScoped(ctx, t, scope, "remain-on-exit", onOff(on))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
