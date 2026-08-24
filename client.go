package gotmucks

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
)

// Client issues one-shot tmux commands. Each call starts a tmux process,
// waits for it and parses its output; there is no persistent connection. For
// live pane output and asynchronous notifications use [Connect] and
// [ControlClient].
//
// A Client holds no state beyond its configuration and one cached answer to
// "tmux -V", and is safe for concurrent use. The version is remembered because
// the readers consult it per value to undo a fault in one tmux release, and
// the binary a client runs is fixed when it is made.
type Client struct {
	cfg config

	// versionOnce caches "tmux -V" for the whole life of the client. The
	// readers ask it on every call to know whether this tmux is the one that
	// escapes a '$' into storage, and a subprocess per row would be absurd.
	// The binary cannot change under a client: cfg is fixed at New.
	versionOnce sync.Once
	version     Version
	versionErr  error
}

// tmuxVersion reports the version of the binary this client runs, asking it
// once and remembering the answer.
//
// A failure is remembered too, and is not returned to the caller of a read:
// the version is wanted here only to decide whether to undo a known 3.4 fault,
// and a client that cannot say what it is gets the behaviour of a tmux without
// the fault. Callers that want the version itself, and any error getting it,
// use [Client.Version].
func (c *Client) tmuxVersion(ctx context.Context) Version {
	c.versionOnce.Do(func() {
		out, _, err := c.run(ctx, "-V")
		if err != nil {
			c.versionErr = err
			return
		}
		c.version, c.versionErr = ParseVersion(string(out))
	})
	return c.version
}

// decodeStored undoes what tmux did to a value between being given it and
// handing it back.
//
// Every reader already undoes the escaping tmux applies on the way out. This
// is the second pass that 3.4 alone needs, because it escapes on the way in as
// well; see [Version.EscapesDollarOnWrite]. It is a no-op on every other
// release, so the readers can call it unconditionally.
func (c *Client) decodeStored(ctx context.Context, v string) string {
	if !c.tmuxVersion(ctx).EscapesDollarOnWrite() {
		return v
	}
	return undoDollarEscape(v)
}

// undoDollarEscape removes the backslash tmux 3.4 inserts before a '$'.
//
// [visDecode] must not be used for this and was, for one round. What 3.4 does
// is not a vis(3) pass: it inserts a backslash before a '$' and touches
// nothing else, so decoding with vis also consumed escapes that were really in
// the value — a window named "a\b" came back holding a backspace, and one
// named "a\tb" came back holding a tab. The undo has to be as narrow as the
// fault.
//
// A '$' at the end of a value is left alone by 3.4, since nothing follows it
// to be read as a variable name, so a backslash before one of those is the
// caller's own and stays. That is also what makes this unambiguous where a
// value already contained a backslash: 3.4 inserts exactly one per escaped
// '$', so removing exactly one is its inverse — "a\$b" is stored as "a\\$b"
// and comes back "a\$b", not "a$b".
func undoDollarEscape(v string) string {
	if !strings.Contains(v, `\$`) {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		// i+2 < len(v) is the "something follows the '$'" test: without a
		// following byte tmux did not escape it, so the backslash is data.
		if v[i] == '\\' && i+2 < len(v) && v[i+1] == '$' {
			continue
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// New returns a Client configured by opts.
//
// With no options it runs "tmux" from PATH against the default socket, which
// is the same server an interactive tmux would use. Programs that must not
// disturb a user's own sessions should pass [WithSocketName] or
// [WithSocketPath].
func New(opts ...Option) *Client {
	return &Client{cfg: newConfig(opts)}
}

// Binary reports the tmux executable this client runs.
func (c *Client) Binary() string { return c.cfg.binary }

// SocketArgs reports the global socket flags this client prepends to every
// command, as ["-L", name] or ["-S", path], or nil for the default socket.
func (c *Client) SocketArgs() []string { return c.cfg.globalArgs() }

// run executes tmux with the client's global flags followed by args.
//
// It is the only place in the package that starts a subprocess. Arguments are
// passed as an argv, never through a shell, so no quoting or escaping of
// caller-supplied values is required — with the single exception
// [escapeTrailingSemicolon] covers, which is tmux's own argv parser rather
// than a shell.
//
// A non-zero exit yields an [*ExitError]. Two exit conditions are classified
// further and wrap a sentinel so callers can test them with errors.Is: a
// missing server wraps [ErrNoServer], and an unresolvable target wraps
// [ErrNoSession], [ErrNoWindow] or [ErrNoPane] according to what tmux said it
// could not find.
func (c *Client) run(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	full, tmuxArgs := c.cfg.argv(escapeTrailingSemicolon(args))

	cmd := exec.CommandContext(ctx, c.cfg.binary, full...)
	cmd.Env = c.cfg.environ()

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout, stderr = outBuf.Bytes(), errBuf.Bytes()
	if runErr == nil {
		return stdout, stderr, nil
	}

	// A cancelled or expired context is the caller's own doing; report it as
	// such rather than as a tmux failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout, stderr, &ExitError{
			Args:   tmuxArgs,
			Code:   exitCode(runErr),
			Stderr: strings.TrimSpace(string(stderr)),
			Err:    ctxErr,
		}
	}

	xerr := &ExitError{
		Args:   tmuxArgs,
		Code:   exitCode(runErr),
		Stderr: strings.TrimSpace(string(stderr)),
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		// Failed to start at all: binary missing, permissions, and so on.
		xerr.Err = runErr
		return stdout, stderr, xerr
	}

	if isNoServerStderr(xerr.Stderr) {
		xerr.Err = ErrNoServer
	} else if target := missingTargetErr(xerr.Stderr); target != nil {
		xerr.Err = target
	}
	return stdout, stderr, xerr
}

// runOK executes tmux and discards its standard output.
func (c *Client) runOK(ctx context.Context, args ...string) error {
	_, _, err := c.run(ctx, args...)
	return err
}

// runLines executes tmux and splits standard output into lines, dropping a
// trailing empty line.
func (c *Client) runLines(ctx context.Context, args ...string) ([]string, error) {
	out, _, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// escapeTrailingSemicolon disarms the one byte that crosses out of an argv
// element.
//
// "Commands are argument vectors, never a shell string" is true of this
// package and not quite true of tmux: cmd_parse_from_arguments takes a
// trailing ';' off an element and ends the command there. So an element that
// is exactly ";" vanishes, and everything after it becomes a second command
// whose name is the next whole element. Measured on 3.2a through this
// package's own calls: RenameWindow("a;") named the window "a";
// SetOption(status, ";") turned the option *on*, because tmux read a
// set-option with no value at all as setting a flag; SendText(p, "echo one;")
// typed "echo one"; and NewSession failed with "unknown command: -n", naming a
// flag the caller never passed. Every one of the first three had a nil error.
// Only a byte-final ';' does it — "a;b" and "a; " are untouched.
//
// tmux's own escape is "\;". Its parser strips the final ';' and then rewrites
// a backslash that has become last as a literal one, so inserting a single
// backslash is exact for every string, a run of backslashes included:
// "a\;" goes out as "a\\;" and is stored as "a\;". Verified byte for byte on
// 3.2a, which is the only way this could be got right — the rule reads as
// though it should need the backslashes doubled, and it does not.
//
// It belongs here, at the one place a process is started, rather than at each
// call site: it is a property of tmux's argv parser and not of any one
// command, so it applies to a name, an option value, a hook body and a format
// alike, and a guarantee spread across call sites is one that quietly stops
// holding. It is applied to the command arguments only — the socket flags are
// read by getopt before the command parser runs, so a socket named "a;" must
// go through untouched.
//
// The control path must not have this. [quoteArg] does not list ';' as safe,
// so [ControlClient.DoArgs] quotes the argument and tmux's control-mode lexer
// takes the byte as data; escaping it as well would store the backslash. The
// one argv this package builds that does not come through here is
// [WithControlArgs], which starts its own process and is documented as passing
// what it is given to tmux as written — a caller's own command line, where
// writing "\;" is the caller's to do.
func escapeTrailingSemicolon(args []string) []string {
	var out []string
	for i, a := range args {
		if !strings.HasSuffix(a, ";") {
			continue
		}
		if out == nil {
			// Copied rather than edited in place: the caller's slice is its
			// own, and most commands never reach this at all.
			out = append(out, args...)
		}
		out[i] = a[:len(a)-1] + `\;`
	}
	if out == nil {
		return args
	}
	return out
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// splitLines splits tmux output on newlines, tolerating CRLF and ignoring a
// trailing newline. Interior blank lines are preserved: capture-pane output
// legitimately contains them.
//
// No output at all is no lines, and a single newline is one empty line. The
// difference survives only until the trailing newline is taken off, which is
// why the emptiness test comes first: a one-column format query over a single
// object whose value is empty prints exactly one newline, and collapsing that
// to nothing lost the row instead of reporting an empty one.
func splitLines(b []byte) []string {
	s := string(b)
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// ServerRunning reports whether a tmux server is listening on the configured
// socket.
//
// No server is not an error: this returns false, nil.
func (c *Client) ServerRunning(ctx context.Context) (bool, error) {
	// list-sessions is the cheapest command that requires a live server and
	// says so distinctly when there is not one.
	_, _, err := c.run(ctx, "list-sessions", "-F", "#{session_id}")
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNoServer):
		return false, nil
	default:
		return false, err
	}
}

// KillServer terminates the tmux server on the configured socket.
//
// A server that was not running is success, not failure.
func (c *Client) KillServer(ctx context.Context) error {
	err := c.runOK(ctx, "kill-server")
	if err != nil && errors.Is(err, ErrNoServer) {
		return nil
	}
	return err
}
