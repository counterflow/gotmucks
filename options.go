package gotmucks

import (
	"os"
	"time"
)

// Option configures a [Client] or a [ControlClient]. The two share a
// configuration type so that a socket chosen for one-shot commands can be
// reused verbatim for a control connection.
//
// Options that only make sense for one of the two are documented as such and
// are ignored by the other.
type Option func(*config)

type config struct {
	binary     string
	socketPath string
	socketName string
	env        []string
	inheritEnv bool

	// Control-mode only.
	controlArgs   []string
	attach        SessionID
	eventBuffer   int
	outputBuffer  int
	closeTimeout  time.Duration
	skipVersionCk bool
}

// Defaults chosen to be unsurprising rather than clever.
const (
	defaultBinary       = "tmux"
	defaultEventBuffer  = 256
	defaultOutputBuffer = 64
	defaultCloseTimeout = 5 * time.Second
)

func newConfig(opts []Option) config {
	c := config{
		binary:       defaultBinary,
		inheritEnv:   true,
		eventBuffer:  defaultEventBuffer,
		outputBuffer: defaultOutputBuffer,
		closeTimeout: defaultCloseTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	return c
}

// globalArgs returns the tmux flags that must precede any subcommand.
//
// -S and -L are mutually exclusive in practice; if both are set the explicit
// path wins, because it is the more specific statement.
func (c config) globalArgs() []string {
	switch {
	case c.socketPath != "":
		return []string{"-S", c.socketPath}
	case c.socketName != "":
		return []string{"-L", c.socketName}
	default:
		return nil
	}
}

// environ returns the environment for a tmux subprocess.
func (c config) environ() []string {
	if !c.inheritEnv {
		return append([]string(nil), c.env...)
	}
	if len(c.env) == 0 {
		return nil // nil means "inherit", per os/exec.
	}
	return append(os.Environ(), c.env...)
}

// WithBinary sets the tmux executable to run. It may be a bare name resolved
// through PATH or an absolute path. Defaults to "tmux".
//
// The test suite uses this to point the client at a stand-in binary.
func WithBinary(path string) Option {
	return func(c *config) {
		if path != "" {
			c.binary = path
		}
	}
}

// WithSocketPath sets the server socket path, tmux's -S flag. It takes
// precedence over [WithSocketName].
func WithSocketPath(path string) Option {
	return func(c *config) { c.socketPath = path }
}

// WithSocketName sets the server socket name, tmux's -L flag. The socket is
// created in tmux's own directory.
//
// Tests and any program that must not disturb a developer's own sessions
// should always set this or [WithSocketPath].
func WithSocketName(name string) Option {
	return func(c *config) { c.socketName = name }
}

// WithEnv adds "KEY=VALUE" entries to the environment of every tmux process
// this client starts. By default the parent environment is inherited and
// these are appended to it; see [WithoutParentEnv].
func WithEnv(env ...string) Option {
	return func(c *config) { c.env = append(c.env, env...) }
}

// WithoutParentEnv runs tmux with only the entries given to [WithEnv],
// rather than inheriting this process's environment.
func WithoutParentEnv() Option {
	return func(c *config) { c.inheritEnv = false }
}

// WithControlArgs replaces the tmux command a control connection issues on
// startup. The default is "new-session", which creates a session and attaches
// the control client to it.
//
// Control-mode only.
func WithControlArgs(args ...string) Option {
	return func(c *config) { c.controlArgs = append([]string(nil), args...) }
}

// WithAttach makes a control connection attach to an existing session rather
// than create one. Equivalent to WithControlArgs("attach-session", "-t", id).
//
// Control-mode only.
func WithAttach(id SessionID) Option {
	return func(c *config) { c.attach = id }
}

// WithEventBuffer sets the capacity of the channel returned by
// [ControlClient.Events]. A larger buffer tolerates a slower consumer before
// events start being dropped. Values below 1 are ignored.
//
// Control-mode only.
func WithEventBuffer(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.eventBuffer = n
		}
	}
}

// WithOutputBuffer sets the capacity of each per-pane channel returned by
// [ControlClient.Output]. Values below 1 are ignored.
//
// Control-mode only.
func WithOutputBuffer(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.outputBuffer = n
		}
	}
}

// WithCloseTimeout bounds how long [ControlClient.Close] waits for tmux to
// exit after the connection is detached before the process is killed.
//
// Control-mode only.
func WithCloseTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.closeTimeout = d
		}
	}
}

// WithoutVersionCheck skips the tmux version check performed when a control
// connection is opened. Intended for tests against stand-in binaries.
func WithoutVersionCheck() Option {
	return func(c *config) { c.skipVersionCk = true }
}
