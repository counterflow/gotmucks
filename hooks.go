package gotmucks

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Hooks let tmux run a command when something happens on the server. They are
// the push counterpart to polling with list-sessions, and the one-shot
// counterpart to control-mode notifications: a hook can be set from a program
// that is not currently connected.

// hookScopes are the option tables a hook can land in, as show-hooks spells
// them: the session table, the window table and the pane table.
//
// tmux picks between them by the hook's own name, not by the target it is
// given, and show-hooks reads one table per invocation. Measured on 3.2a:
// alert-*, client-*, session-* and after-* are session hooks; pane-* and
// window-* are window hooks; and no name at all is a pane hook, though
// "show-hooks -p" is accepted and answers empty, which is why the pane table
// is asked anyway rather than assumed to stay empty for ever.
//
// The order is the order the tables are merged in, and it does not matter: a
// hook name belongs to exactly one of them, so nothing collides.
var hookScopes = [][]string{nil, {"-w"}, {"-p"}}

// SetHook installs a hook on a target.
//
// name is a tmux hook name such as "session-created", "pane-exited" or
// "alert-bell". command is a tmux command line, run by tmux when the hook
// fires — it is tmux syntax, not a shell command, though it may invoke one
// with run-shell.
//
// Being a command line rather than a value is what makes two of its bytes
// behave unlike the same bytes in an option value, both measured on 3.2a.
//
// A trailing ';' does not survive. The package's own escape puts the byte in
// front of set-hook intact — it is [escapeTrailingSemicolon]'s whole job, and
// it is what makes an option value ending in ';' work — but set-hook then
// parses what it was handed a second time, and there the ';' is the command
// separator it always is: "display-message hi;" is stored as
// "display-message hi". So the guarantee that an argv element is a boundary
// stops at an argument tmux parses again, and this is the argument it parses
// again.
//
// And the command line is lexed when the hook is *set*, not when it fires, so
// anything tmux's lexer expands is expanded at that moment: a '~' inside
// double quotes is the one that catches a path, with
// `display-message "~/bin"` stored as "display-message /home/you/bin". Single
// quotes stop it, as they stop "$HOME". [Client.ShowHooks] shows what was
// stored, so it is visible there.
//
// Which option table the hook lands in is tmux's choice and follows the name
// rather than the target: on 3.2a "pane-exited" is a window hook whether it is
// set on a session, a window or a pane, and it is set on the window the target
// resolves to. [Client.ShowHooks] reads all three tables for that reason.
//
// A hook name tmux does not know is an error ("invalid option: nosuchhook"),
// with one exception: a name beginning with '@' is a user option, which tmux
// stores and never fires. Nothing reads it back as a hook.
//
// An empty command is refused. tmux accepts one and then prints the hook
// exactly as it prints a hook that is not set at all — the bare name, with no
// index and no command — so a hook set to nothing cannot be told from an
// absent one and [Client.ShowHooks] does not report it. Use
// [Client.UnsetHook] to remove a hook.
func (c *Client) SetHook(ctx context.Context, t Target, name, command string) error {
	if err := checkHookName(name); err != nil {
		return err
	}
	if err := checkHookCommand(command); err != nil {
		return err
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
// tmux's set-hook -g. The name and the command are checked as
// [Client.SetHook] checks them.
func (c *Client) SetGlobalHook(ctx context.Context, name, command string) error {
	if err := checkHookName(name); err != nil {
		return err
	}
	if err := checkHookCommand(command); err != nil {
		return err
	}
	return c.runOK(ctx, "set-hook", "-g", "--", name, command)
}

// UnsetHook removes a hook from a target, tmux's set-hook -u.
func (c *Client) UnsetHook(ctx context.Context, t Target, name string) error {
	if err := checkHookName(name); err != nil {
		return err
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
	if err := checkHookName(name); err != nil {
		return err
	}
	return c.runOK(ctx, "set-hook", "-g", "-u", "--", name)
}

// checkHookName refuses a name show-hooks could not print back
// unambiguously, for the reason [checkOptionName] gives: a hook lives in an
// option table, show-hooks prints "name command", and only a space separates
// the two.
//
// tmux refuses these itself in every case measured — "set-hook --
// 'alert-bell x'" is "invalid option: alert-bell x", since the name has to be
// one it knows. The check is here anyway so that this package's two halves say
// the same thing about what a name is, and so that the refusal does not depend
// on a table of tmux's that could grow a name with a space in it.
//
// It does not close the other gap, which no byte check could: a name beginning
// with '@' is a user option, and tmux stores it in the session table where it
// is neither fired nor reported. [Client.SetHook] documents that.
func checkHookName(name string) error {
	if name == "" {
		return errors.New("gotmucks: empty hook name")
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= ' ' {
			return fmt.Errorf(
				"gotmucks: hook name %q has %q at byte %d: show-hooks prints "+
					"\"name command\" separated by a space, so such a name could not "+
					"be read back",
				name, string(c), i)
		}
	}
	return nil
}

// checkHookCommand refuses the one command tmux stores and then cannot
// distinguish from an absent hook.
//
// Measured on 3.2a: set-hook with an empty command argument exits 0, and
// show-hooks then prints "alert-bell" — the bare name, with neither the "[0]"
// index every set hook carries nor a command. That is byte for byte how
// show-hooks -g prints a hook that was never set, so nothing downstream can
// tell the two apart. Refusing it at the door is what lets [Client.hooks] read
// a bare name as "not set" and keeps set and show inverses.
func checkHookCommand(command string) error {
	if command == "" {
		return errors.New(
			"gotmucks: empty hook command: tmux stores it but prints the hook exactly " +
				"as it prints one that is not set, so it could not be read back; use " +
				"UnsetHook to remove a hook")
	}
	return nil
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
// Which is also what the index costs when a name has only one element: it is
// taken off without being recorded, so a hook set elsewhere as
// "alert-bell[3]" is reported under "alert-bell" and cannot be told from one
// at element zero. Handing that entry back to [Client.SetHook] relocates it
// there. Nothing is lost or duplicated — tmux clears the array on a set
// without -a, so the hook still fires and the map still holds one entry,
// verified on 3.2a — but the hook has moved. Only set-hook -a and an explicit
// index can reach this.
//
// The command is the tmux command line as tmux printed it, which is what
// [Client.SetHook] takes: tmux re-serialises the parsed command list, quoting
// what needs it, so what comes back can be handed straight back. It is not an
// option value and is deliberately not decoded as one — on 3.2a a hook whose
// argument contains a tab prints as "display-message a\tb", and turning that
// back into a raw tab would split the argument in two the next time it was set.
//
// What comes back is what would fire for that target, and a hook's table
// decides what "for that target" means. A session hook is read off the session
// the target belongs to, so a window and a pane of that session report it as
// their own. A window hook is read off the window the target resolves to,
// which for a session target is its *active* window — so two windows of one
// session report different hooks, and which ones a session reports moves when
// the active window changes. Verified on 3.2a.
//
// All three tables are asked, because [Client.SetHook] cannot choose between
// them: tmux picks by the hook's name. Reading only the session table — which
// is what plain "show-hooks -t" does — reported nothing at all for
// "pane-exited", "window-renamed" and every other window hook, however
// successfully it had been set. Global hooks are not included;
// [Client.ShowGlobalHooks] reports those.
func (c *Client) ShowHooks(ctx context.Context, t Target) (map[string]string, error) {
	if err := checkTarget(t); err != nil {
		return nil, err
	}
	target := targetArgs(t)
	cmds := make([][]string, 0, len(hookScopes))
	for _, scope := range hookScopes {
		args := make([]string, 0, 1+len(scope)+len(target))
		args = append(args, "show-hooks")
		args = append(args, scope...)
		args = append(args, target...)
		cmds = append(cmds, args)
	}
	return c.hooks(ctx, cmds)
}

// ShowGlobalHooks returns the hooks set on the server, tmux's show-hooks -g.
// The map is keyed as [Client.ShowHooks] describes, and the same three option
// tables are read: "set-hook -g -- pane-exited" goes to the global window
// table, where "show-hooks -g" alone does not look.
//
// The global tables are the ones tmux prints in full — every hook name it
// knows appears, with no command against the ones that are not set. Those are
// dropped, so the map holds the hooks that are set and nothing else; without
// that it held sixty-odd names of which one was real. See [Client.hooks].
func (c *Client) ShowGlobalHooks(ctx context.Context) (map[string]string, error) {
	cmds := make([][]string, 0, len(hookScopes))
	for _, scope := range hookScopes {
		args := make([]string, 0, 2+len(scope))
		args = append(args, "show-hooks", "-g")
		args = append(args, scope...)
		cmds = append(cmds, args)
	}
	return c.hooks(ctx, cmds)
}

// hooks runs one show-hooks per option table and merges what they print.
//
// A hook name belongs to exactly one table, so the merge cannot collide and
// the element counting below is over the whole set rather than per table.
func (c *Client) hooks(ctx context.Context, cmds [][]string) (map[string]string, error) {
	var lines []string
	for _, args := range cmds {
		got, err := c.runLines(ctx, args...)
		if err != nil {
			if errors.Is(err, ErrNoServer) || isMissingTarget(err) {
				return map[string]string{}, nil
			}
			return nil, err
		}
		lines = append(lines, got...)
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
		printed, command, ok := strings.Cut(line, " ")
		if !ok {
			// A name with no command. At global scope that is a hook tmux
			// knows about and nobody has set — the whole table is printed
			// there, unlike a target's, which lists only what is set on it.
			// A hook really set to an empty command prints identically, which
			// is why [checkHookCommand] refuses one: this line can then only
			// mean "not set".
			continue
		}
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
