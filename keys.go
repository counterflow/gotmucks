package gotmucks

import (
	"context"
	"fmt"
)

// keyKind distinguishes the three ways tmux can be told to send input. They
// are flags on send-keys rather than properties of an individual key, which
// is why [Client.SendKeys] batches by kind rather than issuing one command
// per key.
type keyKind int

const (
	keyNamed keyKind = iota
	keyLiteral
	keyHex
)

// Key is one thing to send to a pane.
//
// Build keys with [Named], [Literal] or [Hex]. The zero Key is an empty named
// key and is rejected by [Client.SendKeys].
type Key struct {
	kind keyKind
	text string
	hex  []byte
}

// Named is a tmux key name, looked up in tmux's key table: "Enter", "Escape",
// "C-c", "M-x", "Up", "F1". A single printable character is also a valid key
// name.
//
// Use [Literal] for text that should be sent as-is; a string like "C-c" sent
// as a named key is an interrupt, whereas as literal text it is three
// characters.
func Named(name string) Key { return Key{kind: keyNamed, text: name} }

// Literal is text sent exactly as written, with no key-name lookup. This is
// tmux's send-keys -l.
func Literal(s string) Key { return Key{kind: keyLiteral, text: s} }

// Hex sends raw bytes, tmux's send-keys -H. Use it for input that is not
// text: control bytes, or a specific encoding this package should not guess
// at.
func Hex(b ...byte) Key { return Key{kind: keyHex, hex: append([]byte(nil), b...)} }

// Bytes is [Hex] for a byte slice.
func Bytes(b []byte) Key { return Hex(b...) }

// Enter is the return key, the most common thing to send after text.
var Enter = Named("Enter")

// String renders the key for diagnostics.
func (k Key) String() string {
	switch k.kind {
	case keyLiteral:
		return fmt.Sprintf("literal(%q)", k.text)
	case keyHex:
		return fmt.Sprintf("hex(% x)", k.hex)
	default:
		return fmt.Sprintf("key(%s)", k.text)
	}
}

// args renders the key as one or more send-keys arguments.
func (k Key) args() []string {
	if k.kind == keyHex {
		out := make([]string, len(k.hex))
		for i, b := range k.hex {
			out[i] = fmt.Sprintf("%02x", b)
		}
		return out
	}
	return []string{k.text}
}

func (k Key) valid() bool {
	switch k.kind {
	case keyHex:
		return len(k.hex) > 0
	default:
		// A literal empty string sends nothing, and an empty key name is not
		// a key. Both are caller mistakes worth reporting.
		return k.text != ""
	}
}

// flag returns the send-keys flag that selects this kind of key.
func (k keyKind) flag() (string, bool) {
	switch k {
	case keyLiteral:
		return "-l", true
	case keyHex:
		return "-H", true
	default:
		return "", false
	}
}

// SendKeys sends keys to a pane.
//
// tmux selects literal and hex interpretation with a flag on send-keys, not
// per key, so a mixed sequence is issued as one send-keys per run of
// same-kind keys. The runs are sent in order and a failure part-way through
// leaves the earlier runs delivered — tmux has no transaction for this.
//
//	c.SendKeys(ctx, pane, gotmucks.Literal("echo hi"), gotmucks.Enter)
func (c *Client) SendKeys(ctx context.Context, id PaneID, keys ...Key) error {
	if len(keys) == 0 {
		return nil
	}
	for i, k := range keys {
		if !k.valid() {
			return fmt.Errorf("gotmucks: send-keys: key %d is empty", i)
		}
	}

	for _, run := range batchKeys(keys) {
		args := []string{"send-keys"}
		if flag, ok := run.kind.flag(); ok {
			args = append(args, flag)
		}
		args = append(args, "-t", string(id))
		// Terminate option parsing so that a key or literal beginning with a
		// dash is not mistaken for a flag.
		args = append(args, "--")
		for _, k := range run.keys {
			args = append(args, k.args()...)
		}
		if err := c.runOK(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// SendText sends a string to a pane literally, without a trailing newline.
func (c *Client) SendText(ctx context.Context, id PaneID, s string) error {
	if s == "" {
		return nil
	}
	return c.SendKeys(ctx, id, Literal(s))
}

// SendLine sends a string to a pane literally followed by Enter, which is
// what running a shell command amounts to.
//
// The text is not interpreted by this package, but it is by whatever is
// reading the pane. Sending untrusted input to a shell is the caller's risk
// to manage.
func (c *Client) SendLine(ctx context.Context, id PaneID, s string) error {
	if s == "" {
		return c.SendKeys(ctx, id, Enter)
	}
	return c.SendKeys(ctx, id, Literal(s), Enter)
}

// keyRun is a maximal run of consecutive keys of one kind.
type keyRun struct {
	kind keyKind
	keys []Key
}

func batchKeys(keys []Key) []keyRun {
	var runs []keyRun
	for _, k := range keys {
		if n := len(runs); n > 0 && runs[n-1].kind == k.kind {
			runs[n-1].keys = append(runs[n-1].keys, k)
			continue
		}
		runs = append(runs, keyRun{kind: k.kind, keys: []Key{k}})
	}
	return runs
}
