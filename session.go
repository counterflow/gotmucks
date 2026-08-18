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
	// Name is the current session name.
	Name string
	// Windows is the number of windows in the session.
	Windows int
	// Created is when the session was created.
	Created time.Time
	// Activity is the time of the last activity in the session.
	Activity time.Time
	// Attached is the number of clients attached to the session.
	Attached int
	// Width and Height are the dimensions of the session's smallest client,
	// or the session's own size when detached.
	Width, Height int
}

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
	"session_width",
	"session_height",
}

func sessionFromRow(r Row) (Session, error) {
	var s Session
	var err error
	if s.ID, err = r.SessionID("session_id"); err != nil {
		return Session{}, err
	}
	s.Name = r.Get("session_name")
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
	if s.Width, err = r.Int("session_width"); err != nil {
		return Session{}, err
	}
	if s.Height, err = r.Int("session_height"); err != nil {
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
	Name string

	// StartDir is the working directory for the session's first window.
	StartDir string

	// Env is set in the session environment, tmux's new-session -e. It
	// requires tmux 3.2 or newer, which is this package's floor.
	Env map[string]string

	// Command is the program to run in the first window, as an argument
	// vector. It is passed to tmux after a literal "--" so the elements
	// arrive as separate arguments.
	//
	// A vector of two or more elements is executed directly, so shell
	// metacharacters in it are inert. A single element is handled by tmux
	// itself and, per tmux's documented behaviour, is run through the shell —
	// pass {"sh", "-c", script} if you want that explicitly, or add a second
	// element if you do not.
	Command []string

	// WindowName names the session's first window.
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
		args = append(args, "-s", o.Name)
	}
	if o.WindowName != "" {
		args = append(args, "-n", o.WindowName)
	}
	if o.StartDir != "" {
		args = append(args, "-c", o.StartDir)
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
	if err := validateEnv(opts.Env); err != nil {
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

	s, err := c.Session(ctx, id)
	if err != nil {
		// The session exists — we have its id — but reading it back failed.
		// Return what is known rather than nothing.
		if errors.Is(err, ErrNoSession) {
			return &Session{ID: id, Name: opts.Name}, nil
		}
		return nil, err
	}
	return s, nil
}

// validateEnv rejects environment keys tmux cannot express. tmux splits -e on
// the first '=', so a key containing '=' would silently become part of the
// value, and an empty key is meaningless.
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

// ListSessions returns every session on the server, ordered by identifier.
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
func (c *Client) HasSession(ctx context.Context, id SessionID) (bool, error) {
	_, _, err := c.run(ctx, "has-session", "-t", string(id))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNoServer), errors.Is(err, ErrNoSession):
		return false, nil
	default:
		return false, err
	}
}

// KillSession destroys a session.
//
// It is idempotent: a session that is already gone, or a server that is not
// running, is success.
func (c *Client) KillSession(ctx context.Context, id SessionID) error {
	err := c.runOK(ctx, "kill-session", "-t", string(id))
	if err != nil && (errors.Is(err, ErrNoServer) || errors.Is(err, ErrNoSession)) {
		return nil
	}
	return err
}

// RenameSession gives a session a new name. The session's identifier is
// unchanged, which is why identifiers rather than names are the addressing
// scheme here.
func (c *Client) RenameSession(ctx context.Context, id SessionID, name string) error {
	return c.runOK(ctx, "rename-session", "-t", string(id), name)
}
