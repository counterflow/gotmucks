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
	if err := checkTarget(t); err != nil {
		return err
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
	if err := checkTarget(t); err != nil {
		return err
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
// tmux prints one "name command" pair per line and puts an array index on
// every name, not only on a hook set at an index: on 3.2a a plain
// "set-hook alert-bell ..." prints back as "alert-bell[0]". The index is taken
// off here, so the name a hook was set under is the name it is found under —
// which is the whole use of the call, and did not work before.
//
// A hook with more than one command keeps its bracketed names, since a map
// cannot hold two values under one key: "alert-bell[0]" and "alert-bell[1]"
// both appear, and "alert-bell" does not. Only tmux's set-hook -a and an
// explicit index produce that; this package's [Client.SetHook] always writes
// element zero.
//
// The command is the tmux command line as tmux printed it, which is what
// [Client.SetHook] takes: tmux re-serialises the parsed command list, quoting
// what needs it, so what comes back can be handed straight back. It is not an
// option value and is deliberately not decoded as one — on 3.2a a hook whose
// argument contains a tab prints as "display-message a\tb", and turning that
// back into a raw tab would split the argument in two the next time it was set.
//
// What comes back is what would fire for that target, not what was set on it.
// Hooks live in the session's option table, so a window or a pane reports its
// session's hooks as its own — verified on 3.2a, where a hook set with
// "set-hook -t $0" is reported for that session's windows and panes too.
// Global hooks are not included; [Client.ShowGlobalHooks] reports those.
func (c *Client) ShowHooks(ctx context.Context, t Target) (map[string]string, error) {
	if err := checkTarget(t); err != nil {
		return nil, err
	}
	args := []string{"show-hooks"}
	args = append(args, targetArgs(t)...)
	return c.hooks(ctx, args)
}

// ShowGlobalHooks returns the hooks set on the server, tmux's show-hooks -g.
// The map is keyed as [Client.ShowHooks] describes.
func (c *Client) ShowGlobalHooks(ctx context.Context) (map[string]string, error) {
	return c.hooks(ctx, []string{"show-hooks", "-g"})
}

func (c *Client) hooks(ctx context.Context, args []string) (map[string]string, error) {
	lines, err := c.runLines(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrNoServer) || isMissingTarget(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}

	// Two passes, because whether a name may lose its index depends on how
	// many elements that name turns out to have, and the second element
	// arrives after the first.
	type hook struct{ printed, base, command string }
	parsed := make([]hook, 0, len(lines))
	elements := make(map[string]int, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		printed, command, _ := strings.Cut(line, " ")
		base, _ := splitArrayElement(printed)
		parsed = append(parsed, hook{printed, base, command})
		elements[base]++
	}

	hooks := make(map[string]string, len(parsed))
	for _, h := range parsed {
		if elements[h.base] > 1 {
			hooks[h.printed] = h.command
			continue
		}
		hooks[h.base] = h.command
	}
	return hooks, nil
}
