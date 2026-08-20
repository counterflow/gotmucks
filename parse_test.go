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
			name: "bare names are wrapped, all but the last against a tab",
			spec: FormatSpec{"session_id", "session_name"},
			want: "#{s/\t/ /:session_id}\t#{session_name}",
		},
		{
			name: "a single entry is the last one",
			spec: FormatSpec{"session_id"},
			want: "#{session_id}",
		},
		{
			name: "an expression is used verbatim",
			spec: FormatSpec{"pane_id", "#{?pane_dead,dead,live}"},
			want: "#{s/\t/ /:pane_id}\t#{?pane_dead,dead,live}",
		},
		{
			// A substitution's operand is expanded as a format, so "#{...}"
			// would nest, but "#H" inside one expands to nothing at all —
			// verified on 3.2a. An entry the caller wrote is left as written
			// rather than risked.
			name: "an expression is left alone in a column that is not last",
			spec: FormatSpec{"#H", "#{?pane_dead,dead,live}", "pane_id"},
			want: "#H\t#{?pane_dead,dead,live}\t#{pane_id}",
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
			want: "#{T:status-left}\t#{pane_id}",
		},
		{
			name: "an option name may contain a hyphen",
			spec: FormatSpec{"@user-option", "pane_id"},
			want: "#{s/\t/ /:@user-option}\t#{pane_id}",
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
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := unquoteOptionValue(tt.in); got != tt.want {
				t.Errorf("unquoteOptionValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
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
