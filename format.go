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
// what this package uses. Some values can contain one all the same — a working
// directory, the name of the binary a pane is running — so [FormatSpec.Arg]
// asks tmux to take it out of every column but the last, which is the only one
// [ParseRows] can attribute an extra field to.
const fieldSep = "\t"

// lineSep separates rows, and is the one byte no column ordering can survive:
// a raw newline in a value does not overflow into the next field, it ends the
// line, and the row is two rows before [ParseRows] ever sees it. Putting the
// value that carries one last does not help, so [FormatSpec.Arg] asks tmux to
// take a newline out of every column including the last.
//
// It is as reachable as the tab and worse. A single pane whose working
// directory contains a newline — legal on every filesystem this runs on —
// split the row for that pane and failed ListPanes for the whole server,
// taking Pane with it for every pane on it.
const lineSep = "\n"

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
// Order matters for more than presentation. tmux hands some values back with a
// raw tab in them, and a raw tab is an extra field as far as [ParseRows] is
// concerned, so [FormatSpec.Arg] requests every entry but the last through a
// substitution that replaces one with a space. The last entry keeps its tabs,
// because an extra field there can be folded back into it: put the field whose
// value must come back byte for byte at the end, and expect a tab anywhere
// else to arrive as a space.
//
// A raw newline is not an ordering question, because no position survives one:
// it ends the line rather than adding a field, so the row is already two rows
// by the time [ParseRows] sees it. Every entry is therefore requested through
// a substitution that takes a newline out, the last one included, and no
// column can come back carrying one.
type FormatSpec []string

// Arg renders the spec as the value of tmux's -F flag.
//
// Every entry is wrapped in tmux's substitution modifier so that a raw newline
// in the value cannot split the row, and every entry but the last so that a
// raw tab cannot split the column. Only a plain variable name is wrapped.
// Anything else the caller wrote — a format expression, a prefixed expansion
// such as "T:status-left" — is rendered as it stands and is the caller's own
// business: a substitution's operand is itself expanded, so "#{...}" nests
// inside one, but "#{s/<tab>/ /:#H}" expands to nothing at all on 3.2a, and
// turning a working column into an empty one is the worse trade.
func (s FormatSpec) Arg() string {
	parts := make([]string, len(s))
	for i, f := range s {
		if isBareVar(f) {
			parts[i] = rowSafeVar(f, i < len(s)-1)
			continue
		}
		parts[i] = formatVar(f)
	}
	return strings.Join(parts, fieldSep)
}

// isBareVar reports whether an entry is a plain variable or option name, which
// is what may be wrapped in a modifier without changing what it means. tmux
// variables are letters, digits and underscores; option names may contain a
// hyphen, and a user option begins with '@'.
func isBareVar(f string) bool {
	if f == "" {
		return false
	}
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '@':
		default:
			return false
		}
	}
	return true
}

// rowSafeVar wraps a bare variable name in the substitutions that stop its
// value breaking the row it is a field of: always the one that replaces a raw
// newline with a space, and for every column but the last the one that
// replaces a raw tab.
//
// Both patterns are the literal byte. tmux substitutes with a POSIX extended
// regular expression, where "\t" matches a t and nothing else, and a character
// class is worse than useless: the ':' inside "[[:cntrl:]]" ends the modifier
// as far as the format parser is concerned and the whole expression then
// expands to nothing rather than failing. scripts/probe-tabs.sh prints all
// three forms against a real tmux, and says which variables need this.
//
// Two substitutions go in one modifier separated by ';' rather than nested one
// inside the other. Both work on 3.2a — scripts/probe-roundtrip.sh runs the
// pair against a pane whose working directory contains a tab and a newline —
// and the flat form is the shorter template.
func rowSafeVar(f string, dropTab bool) string {
	subs := "s/" + lineSep + "/ /"
	if dropTab {
		subs = "s/" + fieldSep + "/ /;" + subs
	}
	return "#{" + subs + ":" + f + "}"
}

// escapeFormat protects a caller's string from tmux's format expansion by
// doubling every '#', which is tmux's own escape for one.
//
// tmux runs five caller-data arguments through format_expand before using
// them: the name argument of rename-window, rename-session, new-session -s and
// new-session -n, and new-session's -c. A name containing "#{" is therefore
// not a name — verified on 3.2a, where renaming a window to "v#{host}" leaves
// it called "vianf-laptop" — and a directory containing one is not that
// directory: "-c /tmp/a#Hb" put the pane in the home directory, because tmux
// expanded the path, found no such place and fell back, exiting 0 with nothing
// on stderr. This is the command expanding its own argument rather than the
// control-mode lexer, so it happens through [ControlClient.DoArgs] too and
// quoteArg neither causes it nor prevents it.
//
// The "#(...)" form reaches tmux's job machinery, which leaves the placeholder
// "<'...' not ready>" behind as a name. Whether the job also runs depends on
// the argument and on the client, so the placeholder says nothing either way.
// Measured on 3.2a: one-shot, "new-session -s" ran the job and both
// "new-session -c" and "rename-window" did not; down a control connection — a
// client that stays alive — every one of the three ran. Anything that reaches
// a job is arbitrary command execution on some path, which is why the escape
// is not conditional on which one.
//
// Doubling covers the single-character forms as well as "#{": "a##Hb" gives
// "a#Hb" where "a#Hb" gives the hostname. It is correct for a value with no
// format in it, since "a#b" has no format sequence today and "a##b" expands
// back to "a#b" — but it is a deliberate behaviour change for a caller that
// was passing a format on purpose, which is why the five call sites that do it
// say so.
//
// The boundary is measured rather than assumed, and holds at five: on 3.2a a
// new-session -e environment value, the "--" command vector and send-keys -l
// are all stored or delivered verbatim. scripts/probe-roundtrip.sh asserts
// both halves of that.
func escapeFormat(s string) string {
	if !strings.Contains(s, "#") {
		return s
	}
	return strings.ReplaceAll(s, "#", "##")
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

// errEmptySpec is what the two entry points that need columns say when they
// were given none. [Client.QueryArgs] would otherwise ask tmux for an empty
// -F, and [ParseRows] has nothing to align a line against.
var errEmptySpec = errors.New("gotmucks: empty format spec")

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
// and guessing which column is missing would be worse than saying so. Too many
// are folded into the last field, because tmux does not escape the tab in
// every value it expands — verified on 3.2a, where a pane whose working
// directory contains one puts a raw tab in pane_current_path. Folding is only
// correct if no earlier column can overflow, which is what [FormatSpec.Arg]
// arranges; a caller who builds the -F template some other way and hands the
// output here owes itself the same discipline, or an earlier tab will shift
// every column after it without saying so.
//
// A short row is that caller's other way of arriving here. tmux writes a raw
// newline in a value out as it stands, which ends the line and leaves the
// remainder of the value as a row of its own with too few fields in it; the
// substitution [FormatSpec.Arg] wraps every column in is what keeps that from
// happening, and a template built by hand has to do the same.
//
// An empty spec is a caller error rather than an unusual line: there is
// nothing for a row to be aligned against.
//
// A blank line is skipped for a spec of two columns or more, where it cannot
// be a row: any real row carries its separators, so a row of nothing but
// empty values is a line of tabs rather than an empty line. For a
// single-column spec the same line is a row whose one value is empty — a pane
// with no title is an ordinary thing — and skipping it would hand back fewer
// rows than there are objects with no way to tell which one went missing,
// which is the count callers align everything else against. The cost is that
// a trailing blank line becomes a row for a one-column spec; tmux does not
// write one, and splitLines drops the trailing newline that would otherwise
// look like one.
func ParseRows(spec FormatSpec, lines []string) ([]Row, error) {
	if len(spec) == 0 {
		return nil, errEmptySpec
	}
	if len(lines) == 0 {
		return nil, nil
	}
	rows := make([]Row, 0, len(lines))
	for i, line := range lines {
		if line == "" && len(spec) > 1 {
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
		return nil, errEmptySpec
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
