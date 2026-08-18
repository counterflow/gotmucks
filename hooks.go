package gotmucks

import (
	"context"
	"errors"
	"strings"
)

// Hooks let tmux run a command when something happens on the server. They are
// the push counterpart to polling with list-sessions, and the one-shot
// counterpart to control-mode notifications: a hook can be set from a program
// that is not currently connected.

// SetHook installs a hook on a target.
//
// name is a tmux hook name such as "session-created", "pane-exited" or
// "alert-bell". command is a tmux command line, run by tmux when the hook
// fires — it is tmux syntax, not a shell command, though it may invoke one
// with run-shell.
func (c *Client) SetHook(ctx context.Context, t Target, name, command string) error {
	if name == "" {
		return errors.New("gotmucks: empty hook name")
	}
	args := []string{"set-hook"}
	args = append(args, targetArgs(t)...)
	args = append(args, "--", name, command)
	return c.runOK(ctx, args...)
}

// SetGlobalHook installs a hook on the server rather than on one object,
// tmux's set-hook -g.
func (c *Client) SetGlobalHook(ctx context.Context, name, command string) error {
	if name == "" {
		return errors.New("gotmucks: empty hook name")
	}
	return c.runOK(ctx, "set-hook", "-g", "--", name, command)
}

// UnsetHook removes a hook from a target, tmux's set-hook -u.
func (c *Client) UnsetHook(ctx context.Context, t Target, name string) error {
	if name == "" {
		return errors.New("gotmucks: empty hook name")
	}
	args := []string{"set-hook", "-u"}
	args = append(args, targetArgs(t)...)
	args = append(args, "--", name)
	return c.runOK(ctx, args...)
}

// UnsetGlobalHook removes a global hook.
func (c *Client) UnsetGlobalHook(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("gotmucks: empty hook name")
	}
	return c.runOK(ctx, "set-hook", "-g", "-u", "--", name)
}

// ShowHooks returns the hooks set on a target, keyed by hook name.
//
// tmux prints one "name command" pair per line. A hook set on an array index
// keeps its bracketed name, for instance "session-created[1]".
//
// Introspection of per-target hooks is unreliable on older tmux: on 3.2a,
// "show-hooks -t" reports nothing even for a hook that was set successfully
// and that demonstrably fires. Treat an empty result as "tmux would not say"
// rather than as "no hooks are set", and use [Client.ShowGlobalHooks], which
// does report reliably, where that will do.
func (c *Client) ShowHooks(ctx context.Context, t Target) (map[string]string, error) {
	args := []string{"show-hooks"}
	args = append(args, targetArgs(t)...)
	return c.hooks(ctx, args)
}

// ShowGlobalHooks returns the hooks set on the server, tmux's show-hooks -g.
func (c *Client) ShowGlobalHooks(ctx context.Context) (map[string]string, error) {
	return c.hooks(ctx, []string{"show-hooks", "-g"})
}

func (c *Client) hooks(ctx context.Context, args []string) (map[string]string, error) {
	lines, err := c.runLines(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrNoServer) || errors.Is(err, ErrNoSession) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	hooks := make(map[string]string, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		name, command, ok := strings.Cut(line, " ")
		if !ok {
			hooks[line] = ""
			continue
		}
		hooks[name] = unquoteOptionValue(command)
	}
	return hooks, nil
}
