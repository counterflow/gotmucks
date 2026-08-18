// Package escape implements the octal escaping tmux applies to pane output in
// control mode.
//
// tmux writes each %output line as a single line of text, so any byte that
// would break that — anything below ASCII space — is written as a backslash
// followed by exactly three octal digits. The backslash itself is escaped the
// same way and becomes \134.
//
// From tmux's control.c, the condition is `*buf < ' ' || *buf == '\\'`.
// Everything else, including DEL and every byte of a multi-byte UTF-8
// sequence, is written raw.
package escape

// Unescape decodes tmux's %output escaping.
//
// It is deliberately lenient about input it did not produce: a backslash that
// is not followed by three octal digits is passed through as a literal
// backslash rather than treated as an error. A parser that rejected those
// would turn a newer tmux, or a corrupted line, into a dropped connection,
// and the data is more useful than the objection.
//
// The result is a fresh slice; src is not modified.
func Unescape(src []byte) []byte {
	// Escaped output is never shorter than its decoding, so one allocation of
	// len(src) is always enough.
	dst := make([]byte, 0, len(src))
	for i := 0; i < len(src); {
		c := src[i]
		if c != '\\' {
			dst = append(dst, c)
			i++
			continue
		}
		if b, ok := octalTriple(src, i+1); ok {
			dst = append(dst, b)
			i += 4
			continue
		}
		// Not a valid escape: keep the backslash as data.
		dst = append(dst, '\\')
		i++
	}
	return dst
}

// UnescapeString is [Unescape] for a string.
func UnescapeString(s string) []byte { return Unescape([]byte(s)) }

// octalTriple decodes exactly three octal digits starting at i, reporting
// whether they were there and fit in a byte.
func octalTriple(src []byte, i int) (byte, bool) {
	if i+3 > len(src) {
		return 0, false
	}
	v := 0
	for j := i; j < i+3; j++ {
		d := src[j]
		if d < '0' || d > '7' {
			return 0, false
		}
		v = v*8 + int(d-'0')
	}
	// Three octal digits reach 0777; only values that fit in a byte can have
	// come from tmux.
	if v > 0xFF {
		return 0, false
	}
	return byte(v), true
}

// Escape encodes bytes the way tmux does for %output.
//
// This exists so that the decoder can be tested by round trip and so that
// test fixtures can be written as raw bytes rather than hand-escaped strings.
// Nothing in the library sends escaped output to tmux — escaping is
// tmux's side of the protocol.
func Escape(src []byte) []byte {
	dst := make([]byte, 0, len(src))
	for _, c := range src {
		if c < ' ' || c == '\\' {
			dst = append(dst, '\\',
				'0'+(c>>6)&7,
				'0'+(c>>3)&7,
				'0'+c&7)
			continue
		}
		dst = append(dst, c)
	}
	return dst
}

// EscapeString is [Escape] for a string, returning a string.
func EscapeString(s string) string { return string(Escape([]byte(s))) }
