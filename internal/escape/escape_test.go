package escape

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnescape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []byte
	}{
		{"empty", "", []byte{}},
		{"plain ascii", "hello world", []byte("hello world")},

		// The case the protocol makes most awkward: backslash is itself
		// escaped, so a literal backslash arrives as four characters.
		{"backslash", `\134`, []byte{'\\'}},
		{"two backslashes", `\134\134`, []byte{'\\', '\\'}},
		{"backslash amid text", `a\134b`, []byte(`a\b`)},

		{"newline", `\012`, []byte{'\n'}},
		{"carriage return", `\015`, []byte{'\r'}},
		{"crlf", `\015\012`, []byte{'\r', '\n'}},
		{"tab", `\011`, []byte{'\t'}},
		{"nul", `\000`, []byte{0}},
		{"escape", `\033`, []byte{0x1b}},
		{"unit separator", `\037`, []byte{0x1f}},

		{"escape sequence", `\033[1;31mred\033[0m`, []byte("\x1b[1;31mred\x1b[0m")},
		{"prompt then newline", `$ ls\015\012`, []byte("$ ls\r\n")},

		// Space is 0x20 and is not escaped; 0x1f is the last that is.
		{"space stays literal", " ", []byte(" ")},

		// UTF-8 passes through raw: tmux escapes only bytes below space and
		// the backslash, so every continuation byte is untouched.
		{"utf8 two byte", "café", []byte("café")},
		{"utf8 four byte", "🐙", []byte("🐙")},
		{"utf8 with control", "🐙\\012", []byte("🐙\n")},

		// DEL and high bytes are above the escaping threshold.
		{"del literal", "\x7f", []byte{0x7f}},
		{"high byte literal", "\xc3\xa9", []byte{0xc3, 0xa9}},

		// Lenient handling of things tmux would not emit. A backslash that is
		// not a complete escape is data.
		{"trailing lone backslash", `abc\`, []byte(`abc\`)},
		{"short octal at end", `abc\01`, []byte(`abc\01`)},
		{"non-octal digits", `\189`, []byte(`\189`)},
		{"eight is not octal", `\008`, []byte(`\008`)},
		{"octal out of byte range", `\400`, []byte(`\400`)},
		{"largest valid octal", `\377`, []byte{0xff}},
		{"escaped backslash then digits", `\134012`, []byte(`\012`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Unescape([]byte(tt.in))
			if !bytes.Equal(got, tt.want) {
				t.Errorf("Unescape(%q)\n got %q (% x)\nwant %q (% x)", tt.in, got, got, tt.want, tt.want)
			}
		})
	}
}

func TestEscape(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", []byte{}, ""},
		{"plain", []byte("hi"), "hi"},
		{"backslash", []byte{'\\'}, `\134`},
		{"newline", []byte{'\n'}, `\012`},
		{"nul", []byte{0}, `\000`},
		{"escape", []byte{0x1b}, `\033`},
		{"boundary 0x1f", []byte{0x1f}, `\037`},
		{"boundary 0x20", []byte{0x20}, " "},
		{"del untouched", []byte{0x7f}, "\x7f"},
		{"utf8 untouched", []byte("héllo"), "héllo"},
		{"mixed", []byte("a\nb\\c"), `a\012b\134c`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(Escape(tt.in)); got != tt.want {
				t.Errorf("Escape(% x) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRoundTrip is the property that matters: whatever bytes a pane produced,
// escaping and unescaping them must give them back exactly.
func TestRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("plain text"),
		[]byte("with\nnewlines\r\nand\ttabs"),
		[]byte(`back\slash`),
		[]byte("\x1b[31mcolour\x1b[0m"),
		[]byte("unicode: αβγ 日本語 🐙"),
		allBytes(),
		bytes.Repeat([]byte{'\\'}, 64),
		bytes.Repeat([]byte{0}, 64),
	}

	for i, in := range cases {
		enc := Escape(in)
		dec := Unescape(enc)
		if !bytes.Equal(dec, in) {
			t.Errorf("case %d: round trip mismatch\n in %q\nenc %q\ndec %q", i, in, enc, dec)
		}
		// The encoding must survive being a single line, which is the whole
		// reason it exists.
		if strings.ContainsAny(string(enc), "\n\r") {
			t.Errorf("case %d: encoded form contains a line break: %q", i, enc)
		}
	}
}

// TestEscapedFormIsSingleLine checks the invariant over every byte value
// individually, since a single offending byte would break line-oriented
// parsing of the whole stream.
func TestEscapedFormIsSingleLine(t *testing.T) {
	for b := 0; b < 256; b++ {
		enc := Escape([]byte{byte(b)})
		if bytes.ContainsAny(enc, "\n\r") {
			t.Errorf("byte %#02x encodes to %q, which spans lines", b, enc)
		}
		if dec := Unescape(enc); len(dec) != 1 || dec[0] != byte(b) {
			t.Errorf("byte %#02x round tripped to % x", b, dec)
		}
	}
}

// TestUnescapeStringMatchesUnescape keeps the two entry points honest: they
// share an implementation precisely so they cannot disagree.
func TestUnescapeStringMatchesUnescape(t *testing.T) {
	for _, in := range []string{
		"",
		"plain",
		`a\012b`,
		`\134`,
		`trailing\`,
		`\99 not an escape`,
		string(Escape(allBytes())),
	} {
		if got, want := UnescapeString(in), Unescape([]byte(in)); !bytes.Equal(got, want) {
			t.Errorf("UnescapeString(%q) = %q, Unescape = %q", in, got, want)
		}
	}
}

func TestUnescapeDoesNotAliasInput(t *testing.T) {
	src := []byte(`a\012b`)
	out := Unescape(src)
	out[0] = 'z'
	if src[0] != 'a' {
		t.Fatalf("Unescape aliased its input: src is now %q", src)
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add([]byte{0, 1, 2, '\\', 0x1b})
	f.Add([]byte("🐙\n\\134"))
	f.Add(allBytes())

	f.Fuzz(func(t *testing.T, in []byte) {
		enc := Escape(in)
		if bytes.ContainsAny(enc, "\n\r") {
			t.Fatalf("escaped form spans lines: %q", enc)
		}
		if dec := Unescape(enc); !bytes.Equal(dec, in) {
			t.Fatalf("round trip mismatch\n in %q\nenc %q\ndec %q", in, enc, dec)
		}
	})
}

// FuzzUnescapeArbitrary asserts only that decoding never panics and never
// grows its input. Arbitrary bytes are not necessarily a valid encoding, so
// there is nothing else to require of them.
func FuzzUnescapeArbitrary(f *testing.F) {
	f.Add(`\134`)
	f.Add(`\`)
	f.Add(`\99`)
	f.Add(`\777\400\377`)

	f.Fuzz(func(t *testing.T, in string) {
		out := Unescape([]byte(in))
		if len(out) > len(in) {
			t.Fatalf("Unescape(%q) grew from %d to %d bytes", in, len(in), len(out))
		}
	})
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func BenchmarkUnescape(b *testing.B) {
	in := Escape([]byte("\x1b[1;32muser@host\x1b[0m:\x1b[1;34m~/src\x1b[0m$ go test ./...\r\n"))
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Unescape(in)
	}
}

// BenchmarkUnescapeString measures what the library actually runs. %output
// reaches the reader as a string, so this is the hot path; benchmarking only
// the []byte entry point hid the conversion this one used to make.
func BenchmarkUnescapeString(b *testing.B) {
	in := EscapeString("\x1b[1;32muser@host\x1b[0m:\x1b[1;34m~/src\x1b[0m$ go test ./...\r\n")
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = UnescapeString(in)
	}
}
