package gotmucks

import "strings"

// tmux escapes a string with vis(3) in its C style in two places: on the way
// out of show-options, and on the way *in* to a window or session name, which
// is stored escaped and read back that way through "#{window_name}". It is
// one encoding, so both halves of it live here.
//
// The set was measured rather than read. scripts/probe-roundtrip.sh sets every
// printable byte as an option value and as a window name on 3.2a and prints
// both back: of the printable bytes only '\' and '$' are touched, and '$'
// because tmux means the value to be feedable back through set-option, where a
// bare '$' is a variable. Below space it is vis(3)'s C style — "\a" "\b" "\t"
// "\n" "\v" "\f" "\r" — and three octal digits for the rest.
//
// The probe's last "must be empty" diff is the assertion that keeps this
// honest: a byte a tmux escapes that [visDecode] does not undo is a value the
// package hands back wrong, and it exits non-zero when it finds one.

// visDecode undoes that escaping.
//
// This is one left-to-right pass rather than a sequence of replacements. Two
// passes over the whole string — undoing "\\" and then "\t" or the other way
// round — is the shape that turns a literal backslash followed by a t into a
// tab, and it happens to be safe here only because of the order the escapes
// are written in.
func visDecode(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}

	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 >= len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch c := v[i]; c {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case 's':
			b.WriteByte(' ')
		case '\\', '"', '\'', '$':
			// '$' is the one that took eight review rounds to notice, because
			// it is the only escape here that a value picks up silently and
			// keeps: a caller that reads an option, edits it and writes it
			// back gained a backslash every cycle for as long as the case was
			// missing.
			b.WriteByte(c)
		default:
			if o, ok := octalByte(v, i); ok {
				b.WriteByte(o)
				i += 2
				continue
			}
			// Not an escape this package knows. Keep both bytes: the value is
			// worth more than the objection.
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String()
}

// octalByte decodes exactly three octal digits starting at i.
func octalByte(s string, i int) (byte, bool) {
	if i+3 > len(s) {
		return 0, false
	}
	n := 0
	for j := i; j < i+3; j++ {
		if s[j] < '0' || s[j] > '7' {
			return 0, false
		}
		n = n*8 + int(s[j]-'0')
	}
	if n > 0xFF {
		return 0, false
	}
	return byte(n), true
}

// visEncode applies the escaping, for the one call that would otherwise skip
// it.
//
// rename-window, rename-session and new-session -s all run a name through
// tmux's own vis before storing it. new-session -n does not — verified on
// 3.2a, where "-n" stores a raw tab and a raw backslash exactly as given while
// every other path stores "\t" and "\\". Without this, the same name would
// read back differently according to which call set it, and a name containing
// a backslash would come back as something else entirely once [visDecode] ran
// over it.
//
// It matches what 3.2a stores for every byte a name can carry, with one
// deliberate difference: a byte above 0x7f is left alone, where tmux writes
// three octal digits for one that is not part of a valid UTF-8 sequence. Both
// forms decode back to the same bytes, which is the only property that has to
// hold — nothing can observe the stored form except through the decoder.
//
// A NUL is encoded rather than passed through, which lets new-session -n carry
// one where rename-window cannot: an argument vector ends at a NUL, so
// rename-window truncates the name there instead.
func visEncode(s string) string {
	i := 0
	for ; i < len(s); i++ {
		if visEscape(s[i]) != "" {
			break
		}
	}
	if i == len(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + 8)
	b.WriteString(s[:i])
	for ; i < len(s); i++ {
		if e := visEscape(s[i]); e != "" {
			b.WriteString(e)
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// visEscape returns the escape tmux writes for a byte, or "" for a byte it
// writes as it stands.
func visEscape(c byte) string {
	switch c {
	case '\\':
		return `\\`
	case '$':
		return `\$`
	case '\a':
		return `\a`
	case '\b':
		return `\b`
	case '\t':
		return `\t`
	case '\n':
		return `\n`
	case '\v':
		return `\v`
	case '\f':
		return `\f`
	case '\r':
		return `\r`
	}
	if c >= 0x20 && c != 0x7f {
		return ""
	}
	// Three octal digits, which for a byte in this range never needs more
	// than the two bits the first digit can hold.
	return string([]byte{'\\', '0' + c>>6, '0' + (c>>3)&7, '0' + c&7})
}
