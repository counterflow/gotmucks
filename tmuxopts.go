package gotmucks

import (
	"context"
	"errors"
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
	args := []string{"set-option", "-u"}
	args = append(args, scope.flags()...)
	args = append(args, targetArgs(t)...)
	args = append(args, "--", name)
	return c.runOK(ctx, args...)
}

// ShowOption returns the value of a single option and whether it was set.
//
// It uses show-options -v, which prints the bare value, so the result needs
// no unquoting.
func (c *Client) ShowOption(ctx context.Context, t Target, scope OptionScope, name string) (string, bool, error) {
	if name == "" {
		return "", false, errors.New("gotmucks: empty option name")
	}
	args := []string{"show-options", "-v"}
	args = append(args, scope.flags()...)
	args = append(args, targetArgs(t)...)
	args = append(args, "--", name)

	out, _, err := c.run(ctx, args...)
	if err != nil {
		// tmux exits non-zero for an option that is not set in the requested
		// table, which is an absence rather than a failure.
		if errors.Is(err, ErrNoServer) || errors.Is(err, ErrNoSession) {
			return "", false, nil
		}
		var xerr *ExitError
		if errors.As(err, &xerr) && strings.Contains(strings.ToLower(xerr.Stderr), "unknown option") {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSuffix(string(out), "\n"), true, nil
}

// ShowOptions returns every option in a scope.
//
// tmux prints one "name value" pair per line and quotes values that need it;
// quoted values are unquoted here. Values containing newlines cannot be
// represented in this output and are returned truncated at the newline, which
// is a limitation of show-options rather than of this package — use
// [Client.ShowOption] for those.
func (c *Client) ShowOptions(ctx context.Context, t Target, scope OptionScope) (map[string]string, error) {
	args := []string{"show-options"}
	args = append(args, scope.flags()...)
	args = append(args, targetArgs(t)...)

	lines, err := c.runLines(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrNoServer) || errors.Is(err, ErrNoSession) {
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

// unquoteOptionValue strips the quoting show-options applies to values that
// contain spaces or metacharacters.
func unquoteOptionValue(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		// show-options escapes embedded quotes and backslashes.
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
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
