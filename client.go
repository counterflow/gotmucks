package gotmucks

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Client issues one-shot tmux commands. Each call starts a tmux process,
// waits for it and parses its output; there is no persistent connection. For
// live pane output and asynchronous notifications use [Connect] and
// [ControlClient].
//
// A Client holds no state beyond its configuration and is safe for concurrent
// use.
type Client struct {
	cfg config
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
// caller-supplied values is required or performed.
//
// A non-zero exit yields an [*ExitError]. Two exit conditions are classified
// further and wrap a sentinel so callers can test them with errors.Is: a
// missing server wraps [ErrNoServer], and an unresolvable target wraps
// [ErrNoSession].
func (c *Client) run(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	full, tmuxArgs := c.cfg.argv(args)

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

	switch {
	case isNoServerStderr(xerr.Stderr):
		xerr.Err = ErrNoServer
	case isNoSessionStderr(xerr.Stderr):
		xerr.Err = ErrNoSession
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
func splitLines(b []byte) []string {
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" {
		return nil
	}
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
