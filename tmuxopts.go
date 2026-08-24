package gotmucks

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// OptionScope selects which of tmux's option tables a command addresses.
//
// It selects less than the name suggests, and the difference is the reason
// [Client.SetOption] and [Client.ShowOptions] are not inverses. tmux does keep
// separate tables for server, session, window and pane options — but for a
// name it knows it picks between them by the *name*, not by the flag, exactly
// as it does for a hook name. Measured on 3.2a:
//
//	set-option    -t <session> -- remain-on-exit on   // a window option
//	show-options  -t <session> -- remain-on-exit      // remain-on-exit on
//	show-options  -t <session>                        // does not list it
//	show-options  -w -t <session>                     // remain-on-exit on
//
// Swept rather than sampled: over all eighty-seven names in this binary's two
// global tables, "show-options -g -- name" and "show-options -g -w -- name"
// answer identically, and over all seventeen server names so do
// "show-options -s -- name" and "show-options -- name". The flag is ignored.
//
// Three things do obey it, and they are why this type exists:
//
//   - A user option, the ones beginning with '@'. It has no entry in tmux's
//     table, so there is no name to follow and the flag is all there is:
//     "set-option -w -t <session> @a v" is invisible to
//     "show-options -t <session> -- @a".
//   - The listing form with no name, which is what [Client.ShowOptions] uses.
//     It reads the one table the flag names, whatever is in the others.
//   - '-p', for the few names tmux files under window *and* pane, of which
//     remain-on-exit is one — see [Client.SetRemainOnExit]. That is the single
//     case where the flag alters a known name's table rather than being
//     ignored by it.
//
// '-g' is a fourth thing and a different kind, because it does not choose a
// table so much as choose the global counterpart of whichever table the name
// already chose: measured on 3.2a, "set-option -g -- remain-on-exit on" is
// listed by "show-options -g -w" and not by "show-options -g".
//
// So a wrong scope on a built-in name does not, as this comment used to claim,
// make a set-option succeed and change nothing. It sets the option, in the
// table tmux chose; what it changes is which listing can find it afterwards.
type OptionScope int

const (
	// ScopeSession passes no scope flag, which is tmux's default for
	// set-option and show-options and reaches session options.
	ScopeSession OptionScope = iota
	// ScopeServer is tmux's -s, which reaches server options.
	ScopeServer
	// ScopeGlobal is tmux's -g, the global table for the option's own type.
	ScopeGlobal
	// ScopeWindow is tmux's -w, which reaches window options.
	ScopeWindow
	// ScopePane is tmux's -p, which reaches pane options.
	ScopePane
	// ScopeGlobalWindow is tmux's -g and -w together, the global window
	// option table.
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

// checkOptionName refuses a name that show-options could not print back
// unambiguously.
//
// show-options prints "name value" and both readers here split at the first
// space, because a space is the only thing between the two fields. tmux
// escapes the value — which is the whole reason [unquoteOptionValue],
// [unprefixOptionValue] and [visDecode] exist — and prints the name exactly as
// it was stored, raw. A user option's name is whatever the caller passed:
// tmux validates only the leading '@', so a space or a newline in one reaches
// the wire and comes back as a field boundary. Measured on 3.2a, with both
// options set through this package:
//
//	SetOption(t, "@a", "first")     // tmux prints: @a first
//	SetOption(t, "@a b", "second")  // tmux prints: @a b second
//
//	ShowOption("@a")   = ("first", true, nil)      // right by luck: @a is printed first
//	ShowOption("@a b") = ("b second", true, nil)   // the tail of the name, glued to the value
//	ShowOptions()      = {"@a": "b second"}        // @a's own value is gone
//
// The write half is correct throughout — the name travels in an argv element,
// so set-option and set-option -u reach exactly the option asked for — which
// is what makes the reader the place to stop it.
//
// A newline is the same fault with a second half, because the name ends the
// line: "@a\nb" prints as two lines, so [Client.ShowOptions] reports a flag
// option "@a" that is set to nothing and an option "b" that does not exist,
// both keyed by whatever the caller chose. tmux prints user options before the
// rest, so an invented line loses to the real one for any option that is set
// in the same table and wins for every option that is not — which at session
// scope is most of them, since most are inherited and never printed.
//
// So the check is on both halves rather than only on the writers. Refusing in
// [Client.ShowOption] is not symmetry for its own sake: it cannot answer for
// such a name and today it answers wrongly instead. The one thing it cannot
// reach is a name another program set — "@a b V" is genuinely ambiguous on the
// wire, and [Client.ShowOptions] says so rather than guessing.
//
// The set is every byte at or below space: the space that separates the two
// fields and the newline that ends the line, plus the rest of the control
// bytes, which tmux prints raw as well. A tab survives today, because the
// split is on a space alone — but that is tmux's choice about how it prints a
// line rather than a promise about what a name may hold, and it is the same
// choice this whole check exists because of. Refusing the run is the cheaper
// thing to be right about.
//
// A byte above space is safe, including the one that looks least so: '@a[1]'
// is refused by tmux itself ("not an array"), so [splitArrayElement] cannot be
// tricked from here. Measured across all ninety-five printable bytes in four
// positions — assertion A9 in scripts/probe-roundtrip.sh, and
// TestIntegrationOptionNameRoundTrip — where the space is the only one that
// fails.
func checkOptionName(name string) error {
	if name == "" {
		return errors.New("gotmucks: empty option name")
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= ' ' {
			return fmt.Errorf(
				"gotmucks: option name %q has %q at byte %d: show-options prints "+
					"\"name value\" and escapes only the value, so a name containing a "+
					"space or a control byte cannot be read back — the space would be "+
					"taken for the separator and a newline would end the line",
				name, string(c), i)
		}
	}
	return nil
}

// SetOption sets an option on a target, addressed at tmux's session scope.
//
// The scope is not what decides where the option goes, so this is not limited
// to session options: tmux files a name it knows in that name's own table
// whatever flag it is given, and measured on 3.2a this sets remain-on-exit (a
// window option) and escape-time (a server option) just as successfully as
// status. See [OptionScope].
//
// What the scope decides is a user option, which has no table of its own, and
// which *listing* the option turns up in afterwards. An option set here that
// tmux does not file under session is found by [Client.ShowOption] and is not
// in [Client.ShowOptions] at [ScopeSession]. Use [Client.SetOptionScoped] with
// the option's own table when the two have to agree.
func (c *Client) SetOption(ctx context.Context, t Target, name, value string) error {
	return c.SetOptionScoped(ctx, t, ScopeSession, name, value)
}

// SetOptionScoped sets an option in a named scope.
//
// The scope reaches the write only for a user option and for the '-p' of a
// window-or-pane name; tmux files every other known name by the name. What it
// always reaches is the read, since [Client.ShowOptions] lists one table — so
// this is the call to use when an option has to appear in a listing of the
// scope it was set through. See [OptionScope].
//
// The name is checked by [checkOptionName]: tmux would store one containing a
// space perfectly well, and neither reader here could tell it from a name and
// a value.
func (c *Client) SetOptionScoped(ctx context.Context, t Target, scope OptionScope, name, value string) error {
	if err := checkOptionName(name); err != nil {
		return err
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
//
// set-option -u follows a name to the same table set-option wrote it to, so
// this and [Client.SetOption] are inverses whatever scope either is given —
// measured on 3.2a, unsetting remain-on-exit through a session target empties
// the window table entry a session-scoped set put there. Only the listing in
// [Client.ShowOptions] is bound by the flag.
func (c *Client) UnsetOption(ctx context.Context, t Target, scope OptionScope, name string) error {
	if err := checkOptionName(name); err != nil {
		return err
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

// ShowOption returns the value of a single option and whether it is set on
// the target rather than inherited.
//
// That is what the bool means, and it is not "set in the table you asked for".
// tmux resolves a name it knows to that name's own table and reads it from
// there, so the scope is honoured only for a user option and for the '-p' of a
// window-or-pane name — see [OptionScope]. What the bool does distinguish is
// the distinction this call exists for: measured on 3.2a with status set
// globally to off and nothing set on the session, ShowOption at [ScopeSession]
// reports ("", false, nil) rather than the inherited value.
//
// A name tmux does not know, a server that is not running and a target that
// does not exist are all absences rather than failures: the value is empty,
// the bool is false, and the error is nil.
//
// It asks show-options for one name rather than using show-options -v, even
// though -v would print the value with no quoting to undo. Two reasons, and
// neither is the one this comment used to give. The named form is what lets
// this refuse an array rather than answer with its first element, and it
// shares its quoting and vis decode with [Client.ShowOptions] instead of being
// a second path to keep right. The reason it used to give — that -v cannot
// tell an unset option from one set to the empty string — is false on 3.2a,
// measured with od: unset prints no bytes at all and empty prints one newline,
// which is exactly the difference [splitLines] already preserves.
//
// An array option — status-format, command-alias — has no single value and is
// an error here rather than a quiet answer of its first element. Read one with
// [Client.ShowOptions], which reports every element under its own indexed
// name.
//
// A name containing a space or a control byte is refused rather than answered:
// tmux prints the name back unescaped and this splits at the first space, so
// such a name is indistinguishable from a shorter name and a longer value. See
// [checkOptionName].
func (c *Client) ShowOption(ctx context.Context, t Target, scope OptionScope, name string) (string, bool, error) {
	if err := checkOptionName(name); err != nil {
		return "", false, err
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
	return c.decodeStored(ctx, unquoteOptionValue(value)), true, nil
}

// isArrayElement reports whether the name tmux printed for a value is an
// element of an array option rather than the option itself.
//
// show-options prints one "name[index] value" line per element for those, so
// reading the first line as the whole answer returns element 0 and says
// nothing about the rest — verified on 3.2a, where status-format has two
// elements and command-alias has six.
func isArrayElement(printed, name string) bool {
	base, indexed := splitArrayElement(printed)
	return indexed && base == name
}

// splitArrayElement takes the "[index]" off a name tmux printed, reporting
// whether there was one. "status-format[1]" is the option status-format;
// "@weird[x]" and "@weird[]" are options in their own right, since the index
// tmux writes is always decimal and never empty.
//
// Hooks need this as much as array options do: 3.2a prints an index on every
// hook, not only on one set at an index, so the bracket is the rule there
// rather than the exception.
func splitArrayElement(printed string) (string, bool) {
	open := strings.LastIndexByte(printed, '[')
	if open <= 0 || !strings.HasSuffix(printed, "]") {
		return printed, false
	}
	index := printed[open+1 : len(printed)-1]
	if index == "" {
		return printed, false
	}
	for i := 0; i < len(index); i++ {
		if index[i] < '0' || index[i] > '9' {
			return printed, false
		}
	}
	return printed[:open], true
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
// One scope, and this is the one call in the package where the scope is the
// whole answer: tmux's listing form takes no name to follow, so it reads the
// table the flag names and nothing else. That makes this and [Client.SetOption]
// not inverses. An option appears here only if the scope given to this call is
// the table tmux filed the *name* under, which for a built-in name has nothing
// to do with the scope it was set with — measured on 3.2a,
// SetOption(t, "remain-on-exit", "on") succeeds, ShowOption finds it at
// [ScopeSession], [ScopeWindow] and [ScopeServer] alike, and ShowOptions at
// [ScopeSession] does not list it. See [OptionScope]. There is no call here
// for "everything set on this object"; ask each scope in turn.
//
// tmux prints one "name value" pair per line, quoting values that need it and
// escaping the characters that would otherwise break the line; both are undone
// here, so a value containing a tab or a newline comes back intact.
//
// An array option appears as one entry per element, keyed by the name tmux
// printed: "status-format[0]", "status-format[1]". That is what makes this the
// call for reading one — [Client.ShowOption] refuses an array rather than
// answering with its first element.
//
// The name is the one field on a line that tmux does not escape, and a space
// is all that separates it from the value, so a user option another program
// set with a space in its name is read wrong here and cannot be read any other
// way: "@a b V" is a name of "@a b" and a value of "V", or a name of "@a" and
// a value of "b V", and nothing on the wire says which. This package's own
// writers cannot create one — [checkOptionName] refuses the byte — so the
// ambiguity is reachable only from outside.
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
			opts[c.decodeStored(ctx, line)] = ""
			continue
		}
		// The name gets the same treatment as the value on 3.4, because the
		// fault is in what set-option stored rather than in what show-options
		// printed: a user option called "@a$b" is held as "@a\$b" and would
		// otherwise be keyed here under a name the caller never used.
		opts[c.decodeStored(ctx, name)] = c.decodeStored(ctx, unquoteOptionValue(value))
	}
	return opts, nil
}

// unquoteOptionValue undoes what tmux does to an option value on its way out
// of show-options.
//
// tmux quotes a value that contains a space or a metacharacter, and escapes
// the rest with vis(3) — see [visDecode]. The escaping is applied whether or
// not the value ends up quoted, so unquoting alone is not enough: verified on
// 3.2a, where a value containing a tab is printed unquoted as "has\ttab" and
// one containing a '$' is printed quoted as "a\$b".
//
// Which quote tmux reaches for depends on what is inside, so both are
// accepted: a value containing a double quote is wrapped in single quotes and
// a value containing a space in double ones.
//
// The quoting and the vis pass are not the whole of it, and the third part is
// positional — see [unprefixOptionValue].
func unquoteOptionValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return visDecode(unprefixOptionValue(v))
}

// unprefixOptionValue takes off the bare backslash tmux puts in front of a
// value that its own lexer would otherwise read as something else.
//
// This is not vis(3) and [visDecode] must not learn it. tmux's args_escape
// quotes, prefixes and *then* calls the same vis a name goes through, so the
// prefix is a property of an option value alone; widening the decoder would
// change what a name decodes to, and a name never carries one.
//
// Two shapes, measured on 3.2a — the two the byte sweep in
// scripts/probe-roundtrip.sh could not express until this round, because it
// set every byte as "a<byte>b" and tmux's answer depends on where in the value
// the byte sits:
//
//   - a value beginning with '~', whatever its length. "~/bin" prints as
//     "\~/bin", and "~ x" as "\~ x" inside the quotes it also needs. A leading
//     tilde is a home directory to tmux's lexer, so it is disarmed rather than
//     escaped, and "~/..." is what a path-valued option normally looks like.
//   - a value that is a single character needing quotes. Each byte of
//     argsEscapeQuoted prints as a backslash and the byte: "#" comes back as
//     "\#" and "{" as "\{".
//
// Neither is ambiguous, because the vis pass doubles a backslash that is
// really in the value: a stored "~x" prints as "\~x" and a stored "\~x" as
// "\\~x". [visDecode] keeps both bytes of an escape it does not know, which is
// the right conservative choice and is what made this wrong rather than lossy
// — every such value came back with a backslash on the front, and a caller
// that read one, edited it and wrote it back stored the backslash for good.
func unprefixOptionValue(v string) string {
	if len(v) < 2 || v[0] != '\\' {
		return v
	}
	if v[1] == '~' || (len(v) == 2 && strings.IndexByte(argsEscapeQuoted, v[1]) >= 0) {
		return v[1:]
	}
	return v
}

// argsEscapeQuoted is every byte that makes tmux quote an option value, and so
// every byte its single-character form puts a backslash in front of instead.
//
// A space is in tmux's own set and deliberately not in this one: the
// single-character form excludes a value that is one space, which is quoted
// like any other. A backslash is absent because it is not in that set either —
// a value of one backslash prints as "\\", which is the vis escape and
// [visDecode]'s to undo.
const argsEscapeQuoted = "#';${}%\""

// SetRemainOnExit controls whether a pane stays after its process exits.
//
// This is the option that makes a pane's final output readable instead of
// vanishing, so it gets a named helper. The scope follows the target: pane
// options for a [PaneID], window options otherwise, which is where tmux keeps
// remain-on-exit for each.
//
// It is also the one place in the package where the scope is doing real work
// on a built-in name. tmux files remain-on-exit under window *and* pane, and
// '-p' is the flag that picks between them — measured on 3.2a, "set-option -p"
// puts it in the pane table and "set-option" with no flag in the window table,
// while every other built-in name ignores the flag entirely. See
// [OptionScope].
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
