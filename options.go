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
	doubleControl bool

	// execPrefix is inserted before tmux's own arguments. It exists so the
	// test suite can re-execute the test binary as a stand-in for tmux, which
	// is how the command layer is tested without a tmux installation.
	execPrefix []string
}

// attachTarget is the session the connection will actually attach to, which is
// none once [WithControlArgs] has replaced the startup command. Both the check
// in [Connect] and the command it builds ask this rather than reading attach
// directly, so that neither can disagree with the other about which option
// won.
func (c config) attachTarget() SessionID {
	if len(c.controlArgs) > 0 {
		return ""
	}
	return c.attach
}

// withExecPrefix inserts args between the binary and tmux's own arguments.
// Unexported: this is a testing seam, not part of the API.
func withExecPrefix(args ...string) Option {
	return func(c *config) { c.execPrefix = append([]string(nil), args...) }
}

// argv assembles the full argument vector for a tmux invocation, and returns
// alongside it the tmux-visible portion for use in error messages.
func (c config) argv(args []string) (full, tmuxArgs []string) {
	tmuxArgs = append(c.globalArgs(), args...)
	if len(c.execPrefix) == 0 {
		return tmuxArgs, tmuxArgs
	}
	full = append(append([]string(nil), c.execPrefix...), tmuxArgs...)
	return full, tmuxArgs
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
//
// nil and empty are different answers here, which is the whole difficulty:
// os/exec reads a nil Env as "inherit this process's environment" and an empty
// non-nil one as "no environment at all". So the slice returned for
// [WithoutParentEnv] has to be non-nil even when there is nothing in it —
// "append([]string(nil), nothing...)" gives back nil, which handed tmux the
// entire parent environment and made the option a no-op in exactly the case a
// caller reaches for it most plainly, WithoutParentEnv() with no WithEnv
// beside it. Both consumers assign the result straight to exec.Cmd.Env
// ([Client.run] and [Connect]), so there is nowhere else the distinction could
// be recovered.
func (c config) environ() []string {
	if !c.inheritEnv {
		env := make([]string, 0, len(c.env))
		return append(env, c.env...)
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
// rather than inheriting this process's environment. On its own, with no
// [WithEnv] beside it, that means an empty environment — see [config.environ],
// where the difference between an empty environment and an inherited one is
// the difference between an empty slice and a nil one.
func WithoutParentEnv() Option {
	return func(c *config) { c.inheritEnv = false }
}

// WithControlArgs replaces the tmux command a control connection issues on
// startup. The default is "new-session", which creates a session and attaches
// the control client to it.
//
// It replaces the whole command, so it also replaces the one [WithAttach]
// would have built: given both, this one wins and the attach is not made.
//
// These arguments are passed to tmux as written. That includes a -t, which
// makes this the one Option through which an object may be addressed by name:
// WithAttach("work") is refused with [ErrInvalidID], while
// WithControlArgs("attach-session", "-t", "work") attaches to whichever
// session that name reached. It is allowed for the same reason the three
// calls that take a command line are — [ControlClient.Do],
// [ControlClient.DoArgs] and [Client.QueryArgs] — a command line assembled by
// the caller is the caller's own, and parsing it here to disagree with it
// would be guesswork; but the addressing scheme is not enforced through it.
// Prefer [WithAttach], or build the -t from an identifier.
//
// "As written" includes the trailing ';' that tmux's argv parser reads as a
// command terminator, which [Client] escapes on the one-shot path and this
// does not: an element ending in one ends the command there, and an element
// that is exactly ";" separates two. Write "\;" to mean the byte.
//
// Control-mode only.
func WithControlArgs(args ...string) Option {
	return func(c *config) { c.controlArgs = append([]string(nil), args...) }
}

// WithAttach makes a control connection attach to an existing session rather
// than create one. Equivalent to WithControlArgs("attach-session", "-t", id).
//
// Being equivalent to one, it loses to one: a [WithControlArgs] anywhere in
// the same option list replaces the whole startup command, and the session
// given here is then neither attached to nor checked.
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
// connection is opened. Intended for tests against stand-in binaries, and for
// the one real build the check is deliberately strict about: a "next-" tmux
// sorts before the release it is heading for, so next-3.2 does not satisfy the
// 3.2 floor even though it may well have what the floor is there for. See
// [Version.Compare].
func WithoutVersionCheck() Option {
	return func(c *config) { c.skipVersionCk = true }
}

// WithDoubleControlMode starts the control connection with tmux's -CC rather
// than -C.
//
// -CC additionally turns off canonical mode on the terminal, which means tmux
// calls tcgetattr on its standard input. With a pipe there — which is how a
// library drives tmux — that call fails and tmux exits immediately:
//
//	tcgetattr failed: Inappropriate ioctl for device
//
// So -C is the default here, despite -CC being the flag documented for
// applications: -CC is for an application that has given tmux a terminal, such
// as a terminal emulator embedding a tmux session. Pass this option only if
// you have arranged a pty for tmux's standard input yourself.
//
// Control-mode only.
func WithDoubleControlMode() Option {
	return func(c *config) { c.doubleControl = true }
}

// controlFlag is the flag that puts tmux into control mode.
func (c config) controlFlag() string {
	if c.doubleControl {
		return "-CC"
	}
	return "-C"
}
