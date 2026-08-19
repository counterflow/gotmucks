package gotmucks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// fieldSep separates fields within a formatted line.
//
// tmux expands a -F template verbatim, so the separator has to be a byte the
// expansion will not itself produce. Tab is the conventional choice and is
// what this package uses; a value that does contain a tab produces a field
// count mismatch and a loud error from [ParseRows] rather than silently
// misaligned columns.
const fieldSep = "\t"

// FormatSpec is an ordered list of tmux format variables to request.
//
// Entries are normally bare variable names ("session_id", "pane_active").
// An entry containing a '#' is already a format expression and is used
// verbatim, which allows conditionals, modifiers and the single-character
// forms:
//
//	FormatSpec{"pane_id", "#{?pane_dead,dead,live}", "#H"}
//
// The name a value is looked up by in a [Row] is the entry as written.
//
// Order matters for more than presentation. tmux escapes a tab in most fields
// but passes some through raw, and a raw tab is an extra field as far as
// [ParseRows] is concerned; put any field that can contain one last, where the
// overflow belongs to it rather than shifting every column after it.
type FormatSpec []string

// Arg renders the spec as the value of tmux's -F flag.
func (s FormatSpec) Arg() string {
	parts := make([]string, len(s))
	for i, f := range s {
		parts[i] = formatVar(f)
	}
	return strings.Join(parts, fieldSep)
}

// formatVar wraps a bare variable name in #{...}, leaving anything that
// already looks like a format expression alone.
//
// The test is a bare '#' rather than "#{" or "#(": tmux also has
// single-character forms such as #H and #S, and wrapping one of those would
// produce "#{#H}", which expands to nothing useful. No tmux variable name
// contains a '#', so nothing that needs wrapping is missed.
func formatVar(f string) string {
	if strings.Contains(f, "#") {
		return f
	}
	return "#{" + f + "}"
}

// Row is one line of format output, addressed by the spec entries that
// produced it.
//
// Accessors that convert return an error rather than panicking or yielding a
// zero value silently, because a conversion failure means tmux returned
// something this package did not predict, and that is worth surfacing.
type Row struct {
	spec FormatSpec
	vals []string
}

// Len reports the number of fields in the row.
func (r Row) Len() int { return len(r.vals) }

// At returns the field at index i, or "" if i is out of range.
func (r Row) At(i int) string {
	if i < 0 || i >= len(r.vals) {
		return ""
	}
	return r.vals[i]
}

// Lookup returns the value for a spec entry and whether it was present.
func (r Row) Lookup(name string) (string, bool) {
	for i, f := range r.spec {
		if f == name && i < len(r.vals) {
			return r.vals[i], true
		}
	}
	return "", false
}

// Get returns the value for a spec entry, or "" if there is no such entry.
func (r Row) Get(name string) string {
	v, _ := r.Lookup(name)
	return v
}

// Map returns the row as a map from spec entry to value.
func (r Row) Map() map[string]string {
	m := make(map[string]string, len(r.spec))
	for i, f := range r.spec {
		if i < len(r.vals) {
			m[f] = r.vals[i]
		}
	}
	return m
}

// Int returns a field parsed as a decimal integer. A missing or empty field
// is zero without error, because tmux writes an empty string for a variable
// that does not apply to the object being listed.
func (r Row) Int(name string) (int, error) {
	v, ok := r.Lookup(name)
	if !ok || v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("gotmucks: field %q: %q is not an integer", name, v)
	}
	return n, nil
}

// Bool returns a field parsed as a tmux flag. tmux writes "1" and "0" for
// boolean formats; "on"/"off"/"yes"/"no" are also accepted because option
// values use those spellings. A missing or empty field is false.
func (r Row) Bool(name string) (bool, error) {
	v, ok := r.Lookup(name)
	if !ok || v == "" {
		return false, nil
	}
	switch strings.ToLower(v) {
	case "1", "on", "yes", "true":
		return true, nil
	case "0", "off", "no", "false":
		return false, nil
	default:
		return false, fmt.Errorf("gotmucks: field %q: %q is not a boolean", name, v)
	}
}

// Time returns a field parsed as Unix epoch seconds, which is how tmux writes
// its *_created and *_activity variables. A missing or empty field is the
// zero Time.
func (r Row) Time(name string) (time.Time, error) {
	v, ok := r.Lookup(name)
	if !ok || v == "" {
		return time.Time{}, nil
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("gotmucks: field %q: %q is not a timestamp", name, v)
	}
	return time.Unix(secs, 0), nil
}

// SessionID returns a field parsed as a session identifier.
func (r Row) SessionID(name string) (SessionID, error) {
	v, _ := r.Lookup(name)
	return ParseSessionID(v)
}

// WindowID returns a field parsed as a window identifier.
func (r Row) WindowID(name string) (WindowID, error) {
	v, _ := r.Lookup(name)
	return ParseWindowID(v)
}

// PaneID returns a field parsed as a pane identifier.
func (r Row) PaneID(name string) (PaneID, error) {
	v, _ := r.Lookup(name)
	return ParsePaneID(v)
}

// ParseRows splits raw format output into rows against a spec.
//
// It is exported so that output captured elsewhere — a control-mode reply,
// for instance — can be parsed with the same rules as a one-shot command.
//
// Too few fields is an error: the row cannot be aligned with the spec at all,
// and guessing which column is missing would be worse than saying so. Too
// many are folded into the last field, because tmux does not escape the tab
// in every value it expands — verified on 3.2a, where a pane whose working
// directory contains one puts a raw tab in pane_current_path while escaping
// it in session_name and window_name and refusing it outright in pane_title.
// A spec that keeps such a field last therefore reads back the value intact;
// one that does not silently mixes two columns, which is why [FormatSpec]
// says to put it last.
func ParseRows(spec FormatSpec, lines []string) ([]Row, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	rows := make([]Row, 0, len(lines))
	for i, line := range lines {
		if line == "" {
			continue
		}
		vals := strings.Split(line, fieldSep)
		if len(vals) < len(spec) {
			return nil, fmt.Errorf(
				"gotmucks: format line %d has %d fields, want %d (spec %v, line %q)",
				i+1, len(vals), len(spec), []string(spec), line)
		}
		if len(vals) > len(spec) {
			last := len(spec) - 1
			vals = append(vals[:last], strings.Join(vals[last:], fieldSep))
		}
		rows = append(rows, Row{spec: spec, vals: vals})
	}
	return rows, nil
}

// Query runs a tmux command with a format specification and returns its
// output as typed rows. Callers never write -F by hand and never parse tmux's
// human-readable output.
//
// cmd is a bare subcommand name such as "list-sessions". Use [Client.QueryArgs]
// when the command needs arguments of its own.
//
// No server running is not an error: the result is an empty slice.
func (c *Client) Query(ctx context.Context, cmd string, spec FormatSpec) ([]Row, error) {
	return c.QueryArgs(ctx, spec, cmd)
}

// QueryArgs is [Client.Query] for commands that take arguments. The -F flag
// built from spec is appended after args.
func (c *Client) QueryArgs(ctx context.Context, spec FormatSpec, args ...string) ([]Row, error) {
	if len(spec) == 0 {
		return nil, errors.New("gotmucks: empty format spec")
	}
	full := append(append([]string(nil), args...), "-F", spec.Arg())

	lines, err := c.runLines(ctx, full...)
	if err != nil {
		// Both "no server" and "no such target" mean the answer is "nothing",
		// which for a listing is an empty result rather than a failure.
		if errors.Is(err, ErrNoServer) || isMissingTarget(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseRows(spec, lines)
}
