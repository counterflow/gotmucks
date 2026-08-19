package gotmucks

import (
	"context"
	"fmt"
)

// Window is a tmux window.
//
// v1 exposes windows for reading and for addressing other commands. Creating
// windows and manipulating layouts are deliberately absent: layout management
// is out of scope, and adding those calls later is additive whereas removing
// them would not be.
type Window struct {
	// ID is the stable identifier, "@0".
	ID WindowID
	// Session is the session the window belongs to.
	Session SessionID
	// Index is the window's position in its session. Indexes renumber; do not
	// address windows by them.
	Index int
	// Name is the current window name.
	Name string
	// Active reports whether this is the session's current window.
	Active bool
	// Panes is the number of panes in the window.
	Panes int
	// Layout is tmux's layout string for the window.
	Layout string
	// Width and Height are the window's dimensions in cells.
	Width, Height int
}

var windowSpec = FormatSpec{
	"window_id",
	"session_id",
	"window_index",
	"window_name",
	"window_active",
	"window_panes",
	"window_width",
	"window_height",
	// Last: the layout string is the only field with a plausible claim to
	// containing surprising bytes, and a trailing field fails loudly rather
	// than shifting every column after it.
	"window_layout",
}

func windowFromRow(r Row) (Window, error) {
	var w Window
	var err error
	if w.ID, err = r.WindowID("window_id"); err != nil {
		return Window{}, err
	}
	if w.Session, err = r.SessionID("session_id"); err != nil {
		return Window{}, err
	}
	if w.Index, err = r.Int("window_index"); err != nil {
		return Window{}, err
	}
	w.Name = r.Get("window_name")
	if w.Active, err = r.Bool("window_active"); err != nil {
		return Window{}, err
	}
	if w.Panes, err = r.Int("window_panes"); err != nil {
		return Window{}, err
	}
	if w.Width, err = r.Int("window_width"); err != nil {
		return Window{}, err
	}
	if w.Height, err = r.Int("window_height"); err != nil {
		return Window{}, err
	}
	w.Layout = r.Get("window_layout")
	return w, nil
}

// ListWindows returns every window on the server.
//
// No server running is not an error: the result is an empty slice.
func (c *Client) ListWindows(ctx context.Context) ([]Window, error) {
	return c.windows(ctx, "list-windows", "-a")
}

// ListSessionWindows returns the windows of one session.
//
// A session that does not exist yields an empty slice rather than an error,
// matching the other list calls.
func (c *Client) ListSessionWindows(ctx context.Context, id SessionID) ([]Window, error) {
	if err := id.check(); err != nil {
		return nil, err
	}
	return c.windows(ctx, "list-windows", "-t", string(id))
}

func (c *Client) windows(ctx context.Context, args ...string) ([]Window, error) {
	rows, err := c.QueryArgs(ctx, windowSpec, args...)
	if err != nil {
		return nil, err
	}
	windows := make([]Window, 0, len(rows))
	for _, r := range rows {
		w, err := windowFromRow(r)
		if err != nil {
			return nil, err
		}
		windows = append(windows, w)
	}
	return windows, nil
}

// Window returns one window by identifier.
func (c *Client) Window(ctx context.Context, id WindowID) (*Window, error) {
	if err := id.check(); err != nil {
		return nil, err
	}
	windows, err := c.ListWindows(ctx)
	if err != nil {
		return nil, err
	}
	for i := range windows {
		if windows[i].ID == id {
			w := windows[i]
			return &w, nil
		}
	}
	return nil, fmt.Errorf("gotmucks: window %s: %w", id, ErrNoWindow)
}

// RenameWindow gives a window a new name.
func (c *Client) RenameWindow(ctx context.Context, id WindowID, name string) error {
	if err := id.check(); err != nil {
		return err
	}
	return c.runOK(ctx, "rename-window", "-t", string(id), name)
}
