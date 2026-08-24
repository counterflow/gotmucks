package gotmucks

import (
	"errors"
	"strings"
	"testing"
)

// The pure parsers get their own file. Nothing here starts a process or a
// connection: these are the functions that turn what tmux wrote into what the
// caller asked for, and they are cheap enough to cover by table.

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    Version
		unknown bool
	}{
		{in: "3.2a", want: Version{Major: 3, Minor: 2, Suffix: "a"}},
		{in: "tmux 3.2a", want: Version{Major: 3, Minor: 2, Suffix: "a"}},
		{in: "tmux 3.4\n", want: Version{Major: 3, Minor: 4}},
		{in: "next-3.5", want: Version{Major: 3, Minor: 5, Next: true}},
		{in: "tmux next-3.5a", want: Version{Major: 3, Minor: 5, Suffix: "a", Next: true}},
		// Some distributions prefix an OS name.
		{in: "openbsd-7.4", want: Version{Major: 7, Minor: 4}},
		// A release candidate used to be discarded entirely: stripping at the
		// last hyphen first left "rc1", which parses as nothing.
		{in: "3.4-rc1", want: Version{Major: 3, Minor: 4, Suffix: "-rc1"}},
		{in: "tmux 3.5a-rc", want: Version{Major: 3, Minor: 5, Suffix: "a-rc"}},
		{in: "master", unknown: true},
		{in: "tmux master", unknown: true},
		{in: "3.x", unknown: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseVersion(tt.in)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", tt.in, err)
			}
			if got.Unknown != tt.unknown {
				t.Fatalf("Unknown = %v, want %v (got %+v)", got.Unknown, tt.unknown, got)
			}
			if tt.unknown {
				return
			}
			if got.Major != tt.want.Major || got.Minor != tt.want.Minor ||
				got.Suffix != tt.want.Suffix || got.Next != tt.want.Next {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}

	if _, err := ParseVersion("  "); err == nil {
		t.Error("an empty version string should be an error")
	}
}

func TestVersionCompare(t *testing.T) {
	v := func(s string) Version {
		t.Helper()
		parsed, err := ParseVersion(s)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", s, err)
		}
		return parsed
	}

	older := []struct{ a, b string }{
		{"3.1", "3.2"},
		{"2.9", "3.0"},
		{"3.2", "3.2a"},
		{"3.2a", "3.2b"},
		// A hyphenated suffix is a pre-release of the version it hangs off,
		// not a revision of it.
		{"3.4-rc1", "3.4"},
		{"3.4-rc1", "3.4a"},
		// "next-" is the tree heading towards a release, so it precedes it
		// for the same reason a release candidate does.
		{"next-3.2", "3.2"},
		{"next-3.2", "3.2a"},
		{"3.1", "next-3.2"},
		{"next-3.2", "next-3.4"},
	}
	for _, tt := range older {
		if got := v(tt.a).Compare(v(tt.b)); got != -1 {
			t.Errorf("%s.Compare(%s) = %d, want -1", tt.a, tt.b, got)
		}
		if got := v(tt.b).Compare(v(tt.a)); got != 1 {
			t.Errorf("%s.Compare(%s) = %d, want 1", tt.b, tt.a, got)
		}
	}

	if got := v("3.2a").Compare(v("3.2a")); got != 0 {
		t.Errorf("3.2a.Compare(3.2a) = %d, want 0", got)
	}
	// An unnumbered build is the tip, so it must not be refused as too old.
	if !v("master").AtLeast(MinimumVersion()) {
		t.Error("an unknown version should sort newest")
	}
	if !v("3.4-rc1").AtLeast(MinimumVersion()) {
		t.Error("3.4-rc1 is newer than the minimum")
	}
	// next-3.2 is the tree on its way to 3.2 and may predate new-session -e,
	// which is the whole reason the floor is 3.2.
	if v("next-3.2").AtLeast(MinimumVersion()) {
		t.Error("next-3.2 should not satisfy a floor of 3.2")
	}
	if !v("next-3.4").AtLeast(MinimumVersion()) {
		t.Error("next-3.4 is past the floor")
	}
}

func TestFormatSpecArg(t *testing.T) {
	tests := []struct {
		name string
		spec FormatSpec
		want string
	}{
		{
			// Every column but the last asks tmux to take a raw tab out of the
			// value, since only the last one can be given an extra field back.
			// Every column including the last asks it to take a newline out,
			// since no column can be given a whole extra row back.
			name: "bare names are wrapped, all but the last against a tab",
			spec: FormatSpec{"session_id", "session_name"},
			want: "#{s/\t/ /;s/\n/ /:session_id}\t#{s/\n/ /:session_name}",
		},
		{
			name: "a single entry is the last one",
			spec: FormatSpec{"session_id"},
			want: "#{s/\n/ /:session_id}",
		},
		{
			name: "an expression is used verbatim",
			spec: FormatSpec{"pane_id", "#{?pane_dead,dead,live}"},
			want: "#{s/\t/ /;s/\n/ /:pane_id}\t#{?pane_dead,dead,live}",
		},
		{
			// A substitution's operand is expanded as a format, so "#{...}"
			// would nest, but "#H" inside one expands to nothing at all —
			// verified on 3.2a. An entry the caller wrote is left as written
			// rather than risked.
			name: "an expression is left alone in a column that is not last",
			spec: FormatSpec{"#H", "#{?pane_dead,dead,live}", "pane_id"},
			want: "#H\t#{?pane_dead,dead,live}\t#{s/\n/ /:pane_id}",
		},
		{
			name: "a shell expansion is used verbatim",
			spec: FormatSpec{"#(hostname)"},
			want: "#(hostname)",
		},
		{
			// "#{#H}" is not a variable and expands to nothing useful, so the
			// single-character forms have to be left alone as well.
			name: "a single-character form is used verbatim",
			spec: FormatSpec{"#H", "#S"},
			want: "#H\t#S",
		},
		{
			// A prefixed expansion is not a name a modifier can be put in
			// front of, so it is rendered as written even though it is not
			// the last column.
			name: "a prefixed expansion is left alone",
			spec: FormatSpec{"T:status-left", "pane_id"},
			want: "#{T:status-left}\t#{s/\n/ /:pane_id}",
		},
		{
			name: "an option name may contain a hyphen",
			spec: FormatSpec{"@user-option", "pane_id"},
			want: "#{s/\t/ /;s/\n/ /:@user-option}\t#{s/\n/ /:pane_id}",
		},
		{
			name: "an empty spec renders nothing",
			spec: FormatSpec{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.Arg(); got != tt.want {
				t.Errorf("Arg() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseRows(t *testing.T) {
	spec := FormatSpec{"pane_id", "pane_title", "pane_current_path"}

	t.Run("exact", func(t *testing.T) {
		rows, err := ParseRows(spec, []string{"%0\ttitle\t/home/u", "", "%1\tother\t/src"})
		if err != nil {
			t.Fatalf("ParseRows: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want 2 (the blank line is not one)", len(rows))
		}
		if got := rows[1].Get("pane_current_path"); got != "/src" {
			t.Errorf("pane_current_path = %q", got)
		}
	})

	t.Run("too few fields is an error", func(t *testing.T) {
		_, err := ParseRows(spec, []string{"%0\ttitle"})
		if err == nil {
			t.Fatal("a short row should not be guessed at")
		}
		if !strings.Contains(err.Error(), "want 3") {
			t.Errorf("error does not say what was expected: %v", err)
		}
	})

	t.Run("overflow belongs to the last field", func(t *testing.T) {
		// tmux passes a tab in pane_current_path through unescaped, which is
		// why the spec ends with it. Before this, one such pane made every
		// pane listing on the server fail.
		rows, err := ParseRows(spec, []string{"%0\ttitle\t/home/u/a\tb"})
		if err != nil {
			t.Fatalf("ParseRows: %v", err)
		}
		if got := rows[0].Get("pane_current_path"); got != "/home/u/a\tb" {
			t.Errorf("pane_current_path = %q, want the whole path including its tab", got)
		}
		if got := rows[0].Get("pane_title"); got != "title" {
			t.Errorf("pane_title = %q; the overflow shifted a column", got)
		}
	})

	t.Run("no lines", func(t *testing.T) {
		rows, err := ParseRows(spec, nil)
		if err != nil || rows != nil {
			t.Errorf("got (%v, %v), want (nil, nil)", rows, err)
		}
	})

	t.Run("an empty spec is an error, not a panic", func(t *testing.T) {
		// Folding the overflow into the last field computed that field's index
		// as len(spec)-1, which for no spec at all is -1: any non-empty line
		// panicked on the slice bound. Both exported entry points reached it.
		for _, tt := range []struct {
			name string
			call func() ([]Row, error)
		}{
			{"ParseRows", func() ([]Row, error) {
				return ParseRows(FormatSpec{}, []string{"anything"})
			}},
			{"Reply.Rows", func() ([]Row, error) {
				return Reply{Output: []string{"$0"}}.Rows(nil)
			}},
			{"no lines either", func() ([]Row, error) {
				return ParseRows(nil, nil)
			}},
		} {
			rows, err := tt.call()
			if err == nil {
				t.Errorf("%s with an empty spec: got %v, want an error", tt.name, rows)
				continue
			}
			if !errors.Is(err, errEmptySpec) {
				t.Errorf("%s: err = %v, want the empty-spec error", tt.name, err)
			}
		}
	})
}

func TestUnquoteOptionValue(t *testing.T) {
	// tmux escapes an option value with vis(3) in its C style and then quotes
	// it only if it has to, so the escaping has to be undone either way. Every
	// expectation here was produced by tmux 3.2a's show-options.
	tests := []struct {
		in, want string
	}{
		{`plain`, `plain`},
		{`"has space"`, `has space`},
		{`'has"quote'`, `has"quote`},
		{`has\\backslash`, `has\backslash`},
		{`has\ttab`, "has\ttab"},
		{`has\nnewline`, "has\nnewline"},
		{`\033[1m`, "\x1b[1m"},
		{`''`, ``},
		{`""`, ``},
		// A trailing lone backslash is data, not the start of an escape.
		{`ends\`, `ends\`},
		// An escape this package does not know keeps both its bytes rather
		// than losing one of them.
		{`\q`, `\q`},
		// The case a two-pass ReplaceAll gets wrong: an escaped backslash
		// followed by a literal t must not become a tab.
		{`a\\tb`, `a\tb`},
		// tmux escapes a '$' so that the value it prints can be fed back
		// through set-option, where a bare one is a variable. It is the escape
		// this decoder was missing, and the only one a caller picked up
		// silently: read-modify-write of an ordinary PATH gained a backslash
		// every cycle. All four lines are 3.2a's own output.
		{`"a\$b"`, `a$b`},
		{`"PATH=\$HOME/bin"`, `PATH=$HOME/bin`},
		// A value that really contains a backslash before a '$' is
		// unambiguous, because tmux escapes the backslash too.
		{`"a\\\$b"`, `a\$b`},
		{`"a\\\\\$b"`, `a\\$b`},
		// tmux's args_escape quotes, prefixes and then runs the same vis, so
		// the prefix is positional and the sweep that found the '$' could not
		// see it: it set every byte as "a<byte>b", and the prefix only
		// happens at the front. A leading tilde gets one whatever the length,
		// quoted or not, since a tilde is a home directory to tmux's own
		// lexer. All of these are 3.2a's own output.
		{`\~/bin`, `~/bin`},
		{`\~`, `~`},
		{`"\~ x"`, `~ x`},
		{`a~b`, `a~b`},
		{`ab~`, `ab~`},
		// And a value that is a single character needing quotes.
		{`\#`, `#`},
		{`\{`, `{`},
		{`\}`, `}`},
		{`\;`, `;`},
		{`\%`, `%`},
		{`\'`, `'`},
		{`\"`, `"`},
		{`\$`, `$`},
		// Not the prefix: two bytes that are a vis escape, and a value that
		// really begins with a backslash — which is unambiguous because the
		// vis pass doubles that one. A stored "~x" prints as "\~x" and a
		// stored "\~x" as "\\~x".
		{`\t`, "\t"},
		{`\\`, `\`},
		{`\\~x`, `\~x`},
		{`\\~`, `\~`},
		// Longer than the single-character form, so the backslash is the vis
		// escape rather than the prefix.
		{`\#a`, `\#a`},
		{`"#a"`, `#a`},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := unquoteOptionValue(tt.in); got != tt.want {
				t.Errorf("unquoteOptionValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestOptionValueSurvivesReadModifyWrite is the shape of the bug rather than
// one of its values: a caller that reads an option, changes part of it and
// writes it back is the only way to edit one element of status-left or append
// to a user option, and every cycle used to add a backslash.
//
// tmuxEscape stands in for tmux here — it is what 3.2a does to a value on its
// way out of show-options, checked against the real thing by
// scripts/probe-roundtrip.sh — so that the loop can run further than an
// integration test would want to.
func TestOptionValueSurvivesReadModifyWrite(t *testing.T) {
	tmuxEscape := func(v string) string {
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, `$`, `\$`)
		return `"` + v + `"`
	}

	const want = `PATH=$HOME/bin:$HOME/.local/bin`
	stored := want
	for i := 1; i <= 20; i++ {
		got := unquoteOptionValue(tmuxEscape(stored))
		if got != want {
			t.Fatalf("after %d read-modify-write cycles the value is %q, want %q", i, got, want)
		}
		stored = got
	}
}

// TestVisEncodeMatchesTmux pins the encoder against what tmux 3.2a stores for
// the same name, measured by scripts/probe-roundtrip.sh. It exists because the
// encoder's whole job is to agree with tmux: new-session -n is the one name
// argument tmux does not escape itself, and a form that did not match would
// make the same name read back two different ways.
func TestVisEncodeMatchesTmux(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`plain`, `plain`},
		{`a b`, `a b`},
		{"a\tb", `a\tb`},
		{"a\nb", `a\nb`},
		{"a\rb", `a\rb`},
		{"a\ab", `a\ab`},
		{"a\bb", `a\bb`},
		{"a\vb", `a\vb`},
		{"a\fb", `a\fb`},
		{"a\x01b", `a\001b`},
		{"a\x1bb", `a\033b`},
		{"a\x7fb", `a\177b`},
		{`a\b`, `a\\b`},
		{`a$b`, `a\$b`},
		{`$HOME`, `\$HOME`},
		// tmux leaves these alone even though vis(3) has flags that would not.
		{`a"b`, `a"b`},
		{`a'b`, `a'b`},
		{"a`b", "a`b"},
		{`a#b`, `a#b`},
		// Valid UTF-8 passes through, which is what tmux does with it.
		{"aéb", "aéb"},
		{"a→b", "a→b"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := visEncode(tt.in); got != tt.want {
				t.Errorf("visEncode(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got := visDecode(tt.want); got != tt.in {
				t.Errorf("visDecode(%q) = %q, want %q", tt.want, got, tt.in)
			}
		})
	}
}

// TestVisRoundTrip is the property the pair exists for, over the bytes a name
// can be built from. Encoding then decoding has to be the identity, including
// for the strings that look like an escape already.
func TestVisRoundTrip(t *testing.T) {
	cases := []string{
		``, `plain`, `a\tb`, `a\\tb`, `a\$b`, `\`, `\\`, `$`, `$$`,
		"\t", "\n", "\\\t", "a\\\nb", `\001`, "\x01", "\x00", "\xff\xfe",
		"#{host}", "##{host}", "a b\tc\nd", "é\\→",
	}
	for _, s := range cases {
		if got := visDecode(visEncode(s)); got != s {
			t.Errorf("visDecode(visEncode(%q)) = %q", s, got)
		}
	}
}

// TestUndoDollarEscape pins the inverse of what tmux 3.4 does to a value on
// its way into storage: it inserts one backslash before a '$' that has a byte
// after it, and touches nothing else.
//
// The cases that matter are the ones that made visDecode the wrong tool for
// this. A backslash that is really in the value must survive, and a "\t" or a
// "\b" in it is data here rather than an escape — decoding with vis turned a
// window named "a\b" into one holding a backspace. The pairs below are the
// stored form on the left and what the caller set on the right, taken from
// scripts/probe-dollar.sh against a real 3.4.
func TestUndoDollarEscape(t *testing.T) {
	tests := []struct {
		stored, want string
	}{
		// What 3.4 escapes.
		{`a\$b`, `a$b`},
		{`\$ab`, `$ab`},
		{`x\$HOMEy`, `x$HOMEy`},
		{`PATH=\$HOME/bin:\$PATH`, `PATH=$HOME/bin:$PATH`},
		// A backslash already in the value keeps it: 3.4 inserts exactly one
		// per escaped '$', so removing exactly one is the inverse.
		{`a\\$b`, `a\$b`},
		{`a\\\$b`, `a\\$b`},
		// A '$' with nothing after it is not escaped by 3.4, so a backslash
		// before one of those is the caller's and stays.
		{`ab$`, `ab$`},
		{`$`, `$`},
		{`a\$`, `a\$`},
		// Everything else is untouched — this is the half visDecode got wrong.
		{`a\b`, `a\b`},
		{`a\tb`, `a\tb`},
		{`\ab`, `\ab`},
		{`a\\b`, `a\\b`},
		{``, ``},
		{`plain`, `plain`},
		{`a#{host}b`, `a#{host}b`},
	}

	for _, tt := range tests {
		t.Run(tt.stored, func(t *testing.T) {
			if got := undoDollarEscape(tt.stored); got != tt.want {
				t.Errorf("undoDollarEscape(%q) = %q, want %q", tt.stored, got, tt.want)
			}
		})
	}
}

// TestEscapesDollarOnWrite pins the fault to the one release that has it. A
// range would quietly claim a version nobody has asked.
func TestEscapesDollarOnWrite(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"tmux 3.4", true},
		{"3.4", true},
		{"tmux 3.2a", false},
		{"tmux 3.3a", false},
		{"tmux 3.5a", false},
		{"tmux 3.5", false},
		{"tmux 3.6", false},
		{"tmux 4.0", false},
		// A tree heading towards 3.4 is not 3.4, and a release candidate for
		// it precedes it; neither has been measured.
		{"tmux next-3.4", false},
		{"tmux 3.4-rc1", false},
		{"tmux master", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			v, err := ParseVersion(tt.in)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", tt.in, err)
			}
			if got := v.EscapesDollarOnWrite(); got != tt.want {
				t.Errorf("ParseVersion(%q).EscapesDollarOnWrite() = %v, want %v",
					tt.in, got, tt.want)
			}
		})
	}
}

// TestEscapeFormat pins the escape for the five arguments tmux expands as a
// format before it uses them: the four names, and new-session's -c.
func TestEscapeFormat(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`plain`, `plain`},
		{`v#{host}`, `v##{host}`},
		{`a#Hb`, `a##Hb`},
		{`a#b`, `a##b`},
		{`#(touch /tmp/x)`, `##(touch /tmp/x)`},
		{`###`, `######`},
		{``, ``},
		// Nothing else is touched: the expansion is the only thing being
		// defended against, and the name is otherwise the caller's.
		{`a$b\c`, `a$b\c`},
	}
	for _, tt := range tests {
		if got := escapeFormat(tt.in); got != tt.want {
			t.Errorf("escapeFormat(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSplitArrayElement pins the parse both ShowOption and ShowHooks lean on.
func TestSplitArrayElement(t *testing.T) {
	tests := []struct {
		printed, base string
		indexed       bool
	}{
		{"alert-bell[0]", "alert-bell", true},
		{"status-format[10]", "status-format", true},
		{"@user[0]", "@user", true},
		{"alert-bell", "alert-bell", false},
		{"alert-bell[]", "alert-bell[]", false},
		{"alert-bell[x]", "alert-bell[x]", false},
		{"alert-bell[0", "alert-bell[0", false},
		// A leading bracket is a name in its own right, not an index with no
		// name in front of it.
		{"[0]", "[0]", false},
	}
	for _, tt := range tests {
		base, indexed := splitArrayElement(tt.printed)
		if base != tt.base || indexed != tt.indexed {
			t.Errorf("splitArrayElement(%q) = (%q, %v), want (%q, %v)",
				tt.printed, base, indexed, tt.base, tt.indexed)
		}
	}
}

func TestSyncBufferIsCapped(t *testing.T) {
	var b syncBuffer

	b.Write([]byte("first\n"))
	// A tmux writing warnings for the life of a long connection must not be
	// able to grow this without bound.
	chunk := strings.Repeat("x", 8<<10)
	for i := 0; i < 32; i++ {
		n, err := b.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write = (%d, %v); a short write would make os/exec abandon the stream", n, err)
		}
	}

	got := b.String()
	if !strings.HasPrefix(got, "first\n") {
		t.Error("the earliest output, which is the useful part, was discarded")
	}
	if len(got) > maxStderr+128 {
		t.Errorf("buffer grew to %d bytes, want no more than %d plus a note", len(got), maxStderr)
	}
	if !strings.Contains(got, "discarded") {
		t.Error("the truncation is silent")
	}
}

func TestCleanExitCode(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		killed bool
		want   bool
	}{
		{"clean detach", 0, false, true},
		{"tmux failed", 1, false, false},
		// ExitCode is -1 for an exit by any signal. Only this package's own
		// Kill is forgiven; a tmux that died on SIGSEGV is news.
		{"killed by us", -1, true, true},
		{"signalled by something else", -1, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanExitCode(tt.code, tt.killed); got != tt.want {
				t.Errorf("cleanExitCode(%d, %v) = %v, want %v", tt.code, tt.killed, got, tt.want)
			}
		})
	}
}

// TestParseRowsKeepsAnEmptyOneColumnRow is F3. tmux prints one line per
// object, so for a single-column spec the empty line is a row whose one value
// is empty — an untitled pane, an option nobody set — and skipping it handed
// back fewer rows than there were objects with nothing to say which went
// missing.
func TestParseRowsKeepsAnEmptyOneColumnRow(t *testing.T) {
	rows, err := ParseRows(FormatSpec{"pane_title"}, []string{"a", "", "b"})
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if v, ok := rows[1].Lookup("pane_title"); !ok || v != "" {
		t.Errorf("row 1 = (%q, %v), want the empty value and ok", v, ok)
	}

	// With a second column an empty line cannot be a row at all: a row of
	// nothing but empty values still carries its separator.
	rows, err = ParseRows(
		FormatSpec{"pane_id", "pane_title"},
		[]string{"%0\tx", "", "%1\t"})
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: the blank line is not a row here", len(rows))
	}
	if v, _ := rows[1].Lookup("pane_title"); v != "" {
		t.Errorf("row 1 title = %q, want empty", v)
	}
}
