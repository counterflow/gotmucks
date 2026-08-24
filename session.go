package gotmucks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Session is a tmux session.
type Session struct {
	// ID is the stable identifier, "$0". Address sessions by this, never by
	// Name: names are neither unique nor stable.
	ID SessionID
	// Name is the current session name, as it was set. tmux stores it escaped
	// with vis(3) and "#{session_name}" expands to the escaped form, so it is
	// decoded here for the reason [Window.Name] gives; unlike a window name
	// there is no path that skips the escaping, so this one is always exact.
	//
	// It can still differ from a name this package did not set, because tmux
	// rewrites a ':' or a '.' in a session name to '_' before storing it and
	// no decoding undoes that. The calls here refuse those two bytes rather
	// than hand back a name nobody asked for — see [checkSessionName] — but a
	// session another program named is reported as tmux holds it.
	Name string
	// Windows is the number of windows in the session.
	Windows int
	// Created is when the session was created.
	Created time.Time
	// Activity is the time of the last activity in the session.
	Activity time.Time
	// Attached is the number of clients attached to the session.
	Attached int
}

// A session has no size field here on purpose. tmux's session_width and
// session_height formats are deprecated and expand to nothing from tmux 3.1
// onwards — verified empty on 3.2a — so a Session.Width would be zero on
// every supported version. Window carries the real dimensions.

// IsAttached reports whether any client is attached to the session.
func (s Session) IsAttached() bool { return s.Attached > 0 }

// sessionSpec is the format used to read sessions.
var sessionSpec = FormatSpec{
	"session_id",
	"session_name",
	"session_windows",
	"session_created",
	"session_activity",
	"session_attached",
}

func sessionFromRow(r Row) (Session, error) {
	var s Session
	var err error
	if s.ID, err = r.SessionID("session_id"); err != nil {
		return Session{}, err
	}
	s.Name = visDecode(r.Get("session_name"))
	if s.Windows, err = r.Int("session_windows"); err != nil {
		return Session{}, err
	}
	if s.Created, err = r.Time("session_created"); err != nil {
		return Session{}, err
	}
	if s.Activity, err = r.Time("session_activity"); err != nil {
		return Session{}, err
	}
	if s.Attached, err = r.Int("session_attached"); err != nil {
		return Session{}, err
	}
	return s, nil
}

// NewSessionOptions configures [Client.NewSession].
//
// Sessions are always created detached. Attaching a session requires a
// terminal, which a library driving tmux programmatically does not have, and
// is out of this package's scope.
type NewSessionOptions struct {
	// Name is the session name. Empty lets tmux pick the next number.
	//
	// A name is a convenience for humans reading tmux output; it is not how
	// this package addresses the session afterwards.
	//
	// tmux expands it as a format before storing it, so a '#' in it is
	// doubled to keep it a '#' — see [escapeFormat]. [Session.Name] reads
	// back what was given here.
	//
	// A ':' or a '.' is refused: tmux rewrites either to '_' in a session
	// name and reports nothing, so there is no name to read back. See
	// [checkSessionName]. [NewSessionOptions.WindowName] takes both, since
	// tmux only does this to sessions.
	Name string

	// StartDir is the working directory for the session's first window.
	//
	// tmux expands it as a format too — the fifth argument that does, beside
	// the four names — so a '#' in it is doubled here as well, see
	// [escapeFormat]. Without that a path containing "#H" silently became a
	// different path: verified on 3.2a, where tmux expands it, finds no such
	// directory, falls back to the home directory, exits 0 and says nothing on
	// stderr. The session is created and the pane is somewhere else.
	//
	// A working directory is the kind of value a program takes from a config
	// file, a checkout path or a request, which is what makes this reachable
	// by data rather than only by a caller writing a format on purpose.
	StartDir string

	// Env is set in the session environment, tmux's new-session -e. It
	// requires tmux 3.2 or newer, which is this package's floor.
	Env map[string]string

	// Command is the program to run in the first window, as an argument
	// vector. It is passed to tmux after a literal "--" so the elements
	// arrive as separate arguments.
	//
	// A vector of two or more elements is executed directly, so shell
	// metacharacters anywhere in it are inert. A vector of exactly one
	// element is not: tmux hands a lone argument to the shell, so
	// {"rm -rf /tmp/x; reboot"} would run both commands.
	//
	// Rather than let that promise quietly fail, a single-element vector is
	// required to be a bare command word — no whitespace and no shell
	// metacharacters. To run a shell fragment, say so:
	//
	//	Command: []string{"sh", "-c", "make && ./run"}
	Command []string

	// WindowName names the session's first window.
	//
	// It is escaped twice on the way out, where [NewSessionOptions.Name] is
	// escaped once. tmux expands "-n" as a format like the others, but it is
	// the one name argument it then stores without applying vis(3) — verified
	// on 3.2a — so the escaping tmux would have done is done here instead.
	// That is what makes [Window.Name] read back what was given, whichever
	// call named the window.
	WindowName string

	// Width and Height set the size of the detached session. tmux defaults to
	// 80x24 for a session with no attached client.
	Width, Height int
}

func (o NewSessionOptions) args() []string {
	// -d: detached. -P -F: print the new session's id so the caller gets a
	// stable handle back without a follow-up list-sessions.
	args := []string{"new-session", "-d", "-P", "-F", "#{session_id}"}

	if o.Name != "" {
		args = append(args, "-s", escapeFormat(o.Name))
	}
	if o.WindowName != "" {
		args = append(args, "-n", escapeFormat(visEncode(o.WindowName)))
	}
	if o.StartDir != "" {
		args = append(args, "-c", escapeFormat(o.StartDir))
	}
	if o.Width > 0 {
		args = append(args, "-x", fmt.Sprint(o.Width))
	}
	if o.Height > 0 {
		args = append(args, "-y", fmt.Sprint(o.Height))
	}
	// Sorted so that the argument vector is deterministic and therefore
	// testable; tmux does not care about the order.
	for _, k := range sortedKeys(o.Env) {
		args = append(args, "-e", k+"="+o.Env[k])
	}
	if len(o.Command) > 0 {
		args = append(args, "--")
		args = append(args, o.Command...)
	}
	return args
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NewSession creates a detached session and returns it.
//
// tmux starts a server if one is not already running, so this is the one read
// path that legitimately fails when there is no server: it is a write.
func (c *Client) NewSession(ctx context.Context, opts NewSessionOptions) (*Session, error) {
	if err := checkSessionName(opts.Name); err != nil {
		return nil, err
	}
	if err := validateEnv(opts.Env); err != nil {
		return nil, err
	}
	if err := validateCommand(opts.Command); err != nil {
		return nil, err
	}

	out, _, err := c.run(ctx, opts.args()...)
	if err != nil {
		return nil, err
	}

	id, err := ParseSessionID(trimID(out))
	if err != nil {
		return nil, fmt.Errorf("gotmucks: new-session returned %q: %w", trimID(out), err)
	}

	if opts.Width > 0 || opts.Height > 0 {
		if err := c.pinWindowSize(ctx, id, opts.Width, opts.Height); err != nil {
			return nil, err
		}
	}

	s, err := c.Session(ctx, id)
	if err != nil {
		// The session was created — we have its id — but reading it back
		// failed. A short-lived Command is the usual reason: it can exit, end
		// the session, and take the server with it (a server with no sessions
		// exits at once) before this second call lands. Report what is known
		// rather than an error for a session that really was created.
		if isMissingTarget(err) || errors.Is(err, ErrNoServer) {
			return &Session{ID: id, Name: opts.Name}, nil
		}
		return nil, err
	}
	return s, nil
}

// pinWindowSize makes the size new-session was asked for the size it keeps.
//
// -x and -y set the session size, and the window is sized by the window-size
// option rather than by that. Its default is "latest" — take the size of the
// most recently attached client — so whether -x/-y is honoured depends on
// whether tmux believes it has ever seen a client, which is a property of the
// environment and not of the call. It holds on a developer's machine and does
// not on a GitHub runner, where every window came back 80x23 whatever was
// asked for: 80x24 less a status line, the size of a client that reported
// none. default-size was ignored there too, which is what rules that out as
// the explanation. Measured by scripts/probe-size.sh, which also shows the
// remedy: with window-size "manual" the runner honours the request exactly.
//
// So the option is set on the window this call created rather than globally —
// a library has no business changing how the caller's other windows resize —
// and the window is then resized, because setting the option does not itself
// move a window that has already been made at the wrong size.
func (c *Client) pinWindowSize(ctx context.Context, id SessionID, w, h int) error {
	if err := id.check(); err != nil {
		return err
	}

	// A session one command old has exactly one window, so addressing the
	// session resolves to it.
	if _, _, err := c.run(ctx,
		"set-option", "-w", "-t", string(id), "--", "window-size", "manual"); err != nil {
		return err
	}

	args := []string{"resize-window", "-t", string(id)}
	if w > 0 {
		args = append(args, "-x", fmt.Sprint(w))
	}
	if h > 0 {
		args = append(args, "-y", fmt.Sprint(h))
	}
	_, _, err := c.run(ctx, args...)
	return err
}

// checkSessionName refuses a name tmux would store as a different one.
//
// tmux's session_check_name replaces ':' and '.' with '_' before the name is
// stored, because both are its own target separators — "-t sess:win.pane". It
// exits 0 with an empty stderr, so "web.example.com" becomes
// "web_example_com" with a nil error, and unlike every other alteration in
// this file the rewrite is not an encoding: nothing read back can undo it.
// It is a property of sessions alone — a window keeps both bytes, measured on
// the same server at the same moment, which is why the window half of the
// round-trip test would pass whatever the session half did.
//
// Refusing rather than reporting the rewritten name is the shape this package
// already uses three times for a value tmux would silently mangle
// ([validateEnv], [validateCommand], [checkSubscribeName]), and two
// consequences of the rewrite are invisible from the rename itself. A rename
// that collapses onto the name already stored emits no notification at all,
// since tmux compares the two and returns early — so a caller following
// [SessionRenamed] sees one rename where it made two, and its model of the
// server is right only by accident. And a collision is reported against a name
// the caller never used: creating "dup_x" and then "dup:x" fails with
// "duplicate session: dup_x".
//
// A host name, a version, a host and port, a time — "web.example.com",
// "v1.2", "db:5432", "12:30" — are all ordinary things to name a session
// after, which is what makes this reachable by data rather than only by a
// caller writing a separator on purpose.
func checkSessionName(name string) error {
	i := strings.IndexAny(name, ":.")
	if i < 0 {
		return nil
	}
	return fmt.Errorf(
		"gotmucks: session name %q has %q at byte %d: tmux replaces a colon and a dot in a "+
			"session name with an underscore, since both are its own target separators, so "+
			"the session would be called %q; pass that if it is what you meant",
		name, string(name[i]), i, sessionNameAsStored(name))
}

// sessionNameAsStored is what tmux would have called the session, for the sake
// of an error a caller can act on.
func sessionNameAsStored(name string) string {
	return strings.Map(func(r rune) rune {
		if r == ':' || r == '.' {
			return '_'
		}
		return r
	}, name)
}

// validateEnv rejects environment keys tmux cannot express. tmux splits -e on
// the first '=', so a key containing '=' would silently become part of the
// value, and an empty key is meaningless.
//
// The set is deliberately narrower than [checkOptionName]'s, and the reason is
// which byte is the delimiter. An option is printed back as "name value", so
// the space that separates the two fields cannot appear in a name; an
// environment pair is printed as "KEY=value", so the delimiter is the '=' and
// a space in a key is unambiguous — measured on 3.2a, "-e 'A B=v'" is exit 0
// and show-environment prints "A B=v" on one line. Refusing the space here
// would refuse something that works.
//
// A newline is where the two do converge, and it is left alone for a reason
// worth stating rather than for none. It splits the show-environment listing
// exactly as it splits show-options, but the value reaches the pane's process
// intact — measured, "-e $'VAL=a\nb'" arrives in the process environment as
// the seven bytes "VAL=a\nb", which is a legitimate thing for a caller to
// want. The ambiguity is confined to a listing this package never asks for:
// there is no environment reader here, so nothing misreads it the way
// [Client.ShowOptions] would. If one is ever added, this is where the check
// belongs, and the option-name check is the shape to copy.
//
// Neither half of an -e pair is format-expanded, which bounds [escapeFormat]
// to its five arguments and is the one claim here that would be dangerous to
// get wrong: an expanded key would make an unescaped "#(...)" in one a shell
// command rather than a wrong name, and on the control path — where the client
// outlives its own job — it would run. Measured both ways down a control
// connection, neither the key nor the value runs one. Assertion A2 in
// scripts/probe-roundtrip.sh sweeps it, so a tmux that started expanding
// either fails the build rather than a caller.
func validateEnv(env map[string]string) error {
	for k := range env {
		if k == "" {
			return errors.New("gotmucks: empty environment variable name")
		}
		if strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("gotmucks: invalid environment variable name %q", k)
		}
	}
	return nil
}

// validateCommand enforces the one case where tmux will not keep this
// package's promise that a command vector is executed directly.
//
// tmux passes two or more arguments to execvp, but hands a lone argument to
// the shell. Verified against tmux 3.2a: a session started with the single
// argument "touch x; touch y" runs both commands. Since callers are told that
// metacharacters are inert, a single element that could be interpreted is
// refused rather than silently interpreted.
func validateCommand(cmd []string) error {
	if len(cmd) == 0 {
		return nil
	}
	for i, arg := range cmd {
		if arg == "" {
			return fmt.Errorf("gotmucks: command element %d is empty", i)
		}
		if strings.ContainsAny(arg, "\x00\n\r") {
			return fmt.Errorf("gotmucks: command element %d contains a newline or NUL", i)
		}
	}
	if len(cmd) > 1 {
		return nil
	}

	if i := strings.IndexAny(cmd[0], shellMetacharacters); i >= 0 {
		return fmt.Errorf(
			"gotmucks: a one-element Command is run by the shell, and %q contains %q; "+
				"pass []string{\"sh\", \"-c\", %q} to mean that, or split it into separate arguments",
			cmd[0], cmd[0][i:i+1], cmd[0])
	}
	return nil
}

// shellMetacharacters is every byte that could make a lone command argument
// mean more than one thing to a shell. Whitespace is included: a command with
// an argument in it is already two things.
const shellMetacharacters = " \t;&|<>()$`\"'\\*?[]{}~#!\n\r"

// ListSessions returns every session on the server, in no promised order.
//
// tmux prints them ordered by name, which is not the order of their
// identifiers and not the order they were created in — verified on 3.2a,
// where sessions created as zulu, mike, alpha come back as $2, $1, $0. That
// is tmux's business rather than a promise made here, so sort the result if
// an order matters. Sort on [SessionID.Ordinal] rather than on the identifier
// as a string: the number is the part tmux never reuses or renumbers, and
// "$10" sorts before "$9" as text.
//
// No server running is not an error: the result is an empty slice.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := c.Query(ctx, "list-sessions", sessionSpec)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(rows))
	for _, r := range rows {
		s, err := sessionFromRow(r)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// Session returns one session by identifier.
//
// A session that does not exist, or a server that is not running, yields an
// error wrapping [ErrNoSession] or [ErrNoServer] respectively. This differs
// from the list calls deliberately: asking for a specific object and not
// getting it is a failure, whereas listing an empty server is not.
// The filter is applied here rather than with tmux's list-sessions -f, which
// would tie this call to a flag whose availability varies across the
// supported version range. Session counts are small enough that filtering in
// process costs nothing.
func (c *Client) Session(ctx context.Context, id SessionID) (*Session, error) {
	if err := id.check(); err != nil {
		return nil, err
	}
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		if sessions[i].ID == id {
			s := sessions[i]
			return &s, nil
		}
	}
	return nil, fmt.Errorf("gotmucks: session %s: %w", id, ErrNoSession)
}

// HasSession reports whether a session exists.
//
// Neither a missing session nor a missing server is an error; both are false.
// An id that is not an identifier is: an absence is an answer, whereas asking
// the wrong question is a caller mistake. See [ErrInvalidID].
func (c *Client) HasSession(ctx context.Context, id SessionID) (bool, error) {
	if err := id.check(); err != nil {
		return false, err
	}
	_, _, err := c.run(ctx, "has-session", "-t", string(id))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNoServer), isMissingTarget(err):
		return false, nil
	default:
		return false, err
	}
}

// KillSession destroys a session.
//
// It is idempotent: a session that is already gone, or a server that is not
// running, is success. An id that is not an identifier is an error, because
// tmux would otherwise read it as a name and kill whichever session answers to
// it. See [ErrInvalidID].
func (c *Client) KillSession(ctx context.Context, id SessionID) error {
	if err := id.check(); err != nil {
		return err
	}
	err := c.runOK(ctx, "kill-session", "-t", string(id))
	if err != nil && (errors.Is(err, ErrNoServer) || isMissingTarget(err)) {
		return nil
	}
	return err
}

// RenameSession gives a session a new name. The session's identifier is
// unchanged, which is why identifiers rather than names are the addressing
// scheme here.
//
// The name goes after "--" for the reason [Client.RenameWindow] gives: it is
// positional, so without the separator a name beginning with a dash is read as
// a flag and the session keeps its old name. A '#' in it is doubled for the
// other reason that call gives: tmux expands the name as a format first.
//
// A ':' or a '.' is refused rather than sent, because tmux rewrites either to
// '_' and says nothing — see [checkSessionName]. A window name may contain
// both.
func (c *Client) RenameSession(ctx context.Context, id SessionID, name string) error {
	if err := id.check(); err != nil {
		return err
	}
	if err := checkSessionName(name); err != nil {
		return err
	}
	return c.runOK(ctx, "rename-session", "-t", string(id), "--", escapeFormat(name))
}
