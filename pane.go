package gotmucks

import (
	"context"
	"fmt"
	"strconv"
)

// Pane is a tmux pane.
type Pane struct {
	// ID is the stable identifier, "%0".
	ID PaneID
	// Window and Session are the pane's containers.
	Window  WindowID
	Session SessionID
	// Index is the pane's position in its window. Indexes renumber.
	Index int
	// Active reports whether this is the window's current pane.
	Active bool
	// Dead reports that the pane's process has exited and the pane is being
	// kept by the remain-on-exit option.
	Dead bool
	// PID is the process id of the pane's immediate child.
	PID int
	// Width and Height are the pane's dimensions in cells.
	Width, Height int
	// CurrentCommand is the name of the running foreground command.
	CurrentCommand string
	// CurrentPath is the working directory of the running command, when tmux
	// can determine it.
	CurrentPath string
	// Title is the pane title.
	Title string
}

var paneSpec = FormatSpec{
	"pane_id",
	"window_id",
	"session_id",
	"pane_index",
	"pane_active",
	"pane_dead",
	"pane_pid",
	"pane_width",
	"pane_height",
	"pane_current_command",
	"pane_current_path",
	// Last: a pane title is caller-controlled text and the most likely field
	// to contain something unexpected.
	"pane_title",
}

func paneFromRow(r Row) (Pane, error) {
	var p Pane
	var err error
	if p.ID, err = r.PaneID("pane_id"); err != nil {
		return Pane{}, err
	}
	if p.Window, err = r.WindowID("window_id"); err != nil {
		return Pane{}, err
	}
	if p.Session, err = r.SessionID("session_id"); err != nil {
		return Pane{}, err
	}
	if p.Index, err = r.Int("pane_index"); err != nil {
		return Pane{}, err
	}
	if p.Active, err = r.Bool("pane_active"); err != nil {
		return Pane{}, err
	}
	if p.Dead, err = r.Bool("pane_dead"); err != nil {
		return Pane{}, err
	}
	if p.PID, err = r.Int("pane_pid"); err != nil {
		return Pane{}, err
	}
	if p.Width, err = r.Int("pane_width"); err != nil {
		return Pane{}, err
	}
	if p.Height, err = r.Int("pane_height"); err != nil {
		return Pane{}, err
	}
	p.CurrentCommand = r.Get("pane_current_command")
	p.CurrentPath = r.Get("pane_current_path")
	p.Title = r.Get("pane_title")
	return p, nil
}

// ListPanes returns every pane on the server.
//
// No server running is not an error: the result is an empty slice.
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	return c.panes(ctx, "list-panes", "-a")
}

// ListSessionPanes returns every pane in one session, across all its windows.
func (c *Client) ListSessionPanes(ctx context.Context, id SessionID) ([]Pane, error) {
	return c.panes(ctx, "list-panes", "-s", "-t", string(id))
}

// ListWindowPanes returns the panes of one window.
func (c *Client) ListWindowPanes(ctx context.Context, id WindowID) ([]Pane, error) {
	return c.panes(ctx, "list-panes", "-t", string(id))
}

func (c *Client) panes(ctx context.Context, args ...string) ([]Pane, error) {
	rows, err := c.QueryArgs(ctx, paneSpec, args...)
	if err != nil {
		return nil, err
	}
	panes := make([]Pane, 0, len(rows))
	for _, r := range rows {
		p, err := paneFromRow(r)
		if err != nil {
			return nil, err
		}
		panes = append(panes, p)
	}
	return panes, nil
}

// Pane returns one pane by identifier.
func (c *Client) Pane(ctx context.Context, id PaneID) (*Pane, error) {
	panes, err := c.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range panes {
		if panes[i].ID == id {
			p := panes[i]
			return &p, nil
		}
	}
	return nil, fmt.Errorf("gotmucks: pane %s: %w", id, ErrNoSession)
}

// CaptureOptions configures [Client.CapturePane].
type CaptureOptions struct {
	// Escapes includes the escape sequences for text and background
	// attributes in the output, tmux's capture-pane -e. Without it the
	// capture is plain text.
	Escapes bool

	// Join preserves trailing spaces and joins wrapped lines, tmux's -J.
	// Use it when the capture is going to be diffed or parsed, since it makes
	// a wrapped line indistinguishable from a hard-wrapped one.
	Join bool

	// PreserveTrailingSpaces keeps trailing spaces on each line, tmux's -N.
	PreserveTrailingSpaces bool

	// Start and End bound the captured region in lines. Negative values index
	// into the scrollback: -1 is the line immediately above the visible area.
	// Both are inclusive.
	//
	// Leave both nil to capture the visible pane only.
	Start, End *int

	// FullHistory captures the entire scrollback plus the visible area. It
	// overrides Start.
	FullHistory bool
}

// Line is a convenience for building the Start and End fields of
// [CaptureOptions].
func Line(n int) *int { return &n }

func (o CaptureOptions) args() []string {
	// -p writes the capture to stdout instead of a paste buffer.
	args := []string{"-p"}
	if o.Escapes {
		args = append(args, "-e")
	}
	if o.Join {
		args = append(args, "-J")
	}
	if o.PreserveTrailingSpaces {
		args = append(args, "-N")
	}
	switch {
	case o.FullHistory:
		// "-" is tmux's spelling for "the start of the history".
		args = append(args, "-S", "-")
	case o.Start != nil:
		args = append(args, "-S", strconv.Itoa(*o.Start))
	}
	if o.End != nil {
		args = append(args, "-E", strconv.Itoa(*o.End))
	}
	return args
}

// CapturePane returns the contents of a pane.
//
// The result is raw bytes rather than a string because with
// [CaptureOptions.Escapes] set it contains terminal escape sequences, and
// because pane contents are not guaranteed to be valid UTF-8.
func (c *Client) CapturePane(ctx context.Context, id PaneID, opts CaptureOptions) ([]byte, error) {
	args := append([]string{"capture-pane"}, opts.args()...)
	args = append(args, "-t", string(id))

	out, _, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}
