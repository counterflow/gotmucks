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
	// CurrentCommand is the name of the running foreground command. tmux takes
	// it from the operating system, so a binary whose file name contains a tab
	// puts one here; it arrives as a space, for the reason [FormatSpec] gives.
	CurrentCommand string
	// CurrentPath is the working directory of the running command, when tmux
	// can determine it. It is the one field here that keeps a raw tab, since
	// it is last in the spec and an overflowing final column folds back.
	//
	// A newline in it arrives as a space. Nothing can be last enough to
	// survive one — it ends the format line rather than adding a field — so
	// [FormatSpec.Arg] asks tmux to substitute it out of this column too. It
	// used to be left in, and one pane started in a directory whose name
	// contained a newline failed [Client.ListPanes] for every pane on the
	// server.
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
	"pane_title",
	// Last, which is the one place a raw tab survives: every other entry is
	// requested through a substitution that turns one into a space, and this
	// one keeps its tabs so that a working directory containing one reads back
	// byte for byte. Of the three fields here that could carry one it has the
	// best claim to the place — a tab-named directory is ordinary, and the
	// path is the value a caller is most likely to hand straight to the
	// operating system again. A newline is taken out of this column as well as
	// the others, for the reason [FormatSpec] gives.
	//
	// The other two, verified on 3.2a by scripts/probe-tabs.sh:
	// pane_current_command carries a raw tab whenever the binary's own file
	// name does, and pane_title cannot be given one at all — tmux discards
	// control bytes inside an OSC string, and select-pane -T exits 0 and
	// silently drops a title that is not printable ASCII rather than refusing
	// it. The substitution covers pane_title regardless, because the title is
	// whatever the program in the pane says it is and the only thing between
	// that program and this row is a filter in another process.
	"pane_current_path",
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
	if err := id.check(); err != nil {
		return nil, err
	}
	return c.panes(ctx, "list-panes", "-s", "-t", string(id))
}

// ListWindowPanes returns the panes of one window.
func (c *Client) ListWindowPanes(ctx context.Context, id WindowID) ([]Pane, error) {
	if err := id.check(); err != nil {
		return nil, err
	}
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
	if err := id.check(); err != nil {
		return nil, err
	}
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
	return nil, fmt.Errorf("gotmucks: pane %s: %w", id, ErrNoPane)
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
	if err := id.check(); err != nil {
		return nil, err
	}
	args := append([]string{"capture-pane"}, opts.args()...)
	args = append(args, "-t", string(id))

	out, _, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}
