package gotmucks

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/counterflow/gotmucks/internal/faketmux"
)

// Three exported Options had no test at all, which the coverage profile is
// what said: WithoutParentEnv, WithDoubleControlMode and WithoutVersionCheck
// were the only 0.0% entries in this package that were not marker methods or
// String helpers. Each is a one-line setter, so what is worth asserting is not
// that the field changes but that the thing reading the field behaves — which
// for two of them is also the uncovered branch of a helper.

// TestWithoutParentEnv covers the branch of config.environ that decides
// whether tmux sees this process's environment.
//
// The distinction is not cosmetic and neither half is the obvious default. A
// nil return means "inherit", per os/exec, so the option cannot be expressed by
// clearing a slice: an empty environment and an inherited one are the same
// value unless the flag is read.
func TestWithoutParentEnv(t *testing.T) {
	const marker = "GOTMUCKS_TEST_MARKER=1"

	inherited := newConfig([]Option{WithEnv(marker)}).environ()
	if len(inherited) <= 1 {
		t.Fatalf("environ() with the parent environment returned %d entries, want the "+
			"process environment plus the one added", len(inherited))
	}
	if inherited[len(inherited)-1] != marker {
		t.Errorf("the added entry is %q, want it last so it wins over an inherited one",
			inherited[len(inherited)-1])
	}

	alone := newConfig([]Option{WithEnv(marker), WithoutParentEnv()}).environ()
	if !reflect.DeepEqual(alone, []string{marker}) {
		t.Errorf("environ() without the parent environment = %q, want only %q", alone, marker)
	}

	// The empty case is the one the nil convention is about: with the option,
	// tmux must get an empty environment rather than this process's.
	empty := newConfig([]Option{WithoutParentEnv()}).environ()
	if empty == nil {
		t.Error("environ() with no entries and no parent returned nil, which os/exec reads " +
			"as \"inherit\" — the opposite of what the option asks for")
	}
	if len(empty) != 0 {
		t.Errorf("environ() = %q, want empty", empty)
	}

	// And the default, which must stay nil rather than become a copy of the
	// process environment: os/exec is what should read it.
	if got := newConfig(nil).environ(); got != nil {
		t.Errorf("environ() by default = %q, want nil so os/exec inherits", got)
	}
}

// TestWithDoubleControlMode covers the other half of config.controlFlag.
//
// -C is the default for a reason measured on 3.2a and recorded on the option:
// -CC makes tmux call tcgetattr on its standard input, which fails against a
// pipe and exits the server at once. The flag is still offered for a caller
// who has arranged a pty, so which one each setting produces is worth pinning.
func TestWithDoubleControlMode(t *testing.T) {
	if got := newConfig(nil).controlFlag(); got != "-C" {
		t.Errorf("controlFlag() by default = %q, want %q — -CC fails against a pipe", got, "-C")
	}
	if got := newConfig([]Option{WithDoubleControlMode()}).controlFlag(); got != "-CC" {
		t.Errorf("controlFlag() with WithDoubleControlMode = %q, want %q", got, "-CC")
	}
}

// TestWithoutVersionCheck asserts the option through Connect rather than
// through the field, since the field is only interesting for what Connect does
// with it.
//
// The fake reports a tmux older than the floor. Without the option Connect
// refuses it, which is the check working; with the option Connect gets past
// the check and fails later for its own reasons, since a stand-in that answers
// -V is not a control server. "Fails, but not with this error" is the whole
// assertion, and it is the only one available without a real tmux.
func TestWithoutVersionCheck(t *testing.T) {
	script := faketmux.Script{Version: "tmux 3.1c"}

	f := newFake(t, script)
	_, err := Connect(context.Background(), f.options()...)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Connect against tmux 3.1c: %v, want ErrUnsupportedVersion", err)
	}

	g := newFake(t, script)
	_, err = Connect(context.Background(), append(g.options(), WithoutVersionCheck())...)
	if errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("Connect with WithoutVersionCheck still refused the version: %v", err)
	}
	if err == nil {
		t.Error("Connect against a stand-in that is not a control server succeeded")
	}
}

// TestSocketOptionsAreExclusive pins which of the two socket flags wins.
//
// tmux's -S takes a path and -L a name it resolves under the socket directory.
// They are alternatives rather than a pair, so a config carrying both has to
// pick one; globalArgs picks the path, and picking silently is only safe if it
// is written down somewhere that fails when it changes. This matters more than
// its size suggests, because every integration test in this package relies on
// a socket flag being set to stay off a developer's own server.
func TestSocketOptionsAreExclusive(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want []string
	}{
		{"neither", nil, nil},
		{"path", []Option{WithSocketPath("/tmp/s")}, []string{"-S", "/tmp/s"}},
		{"name", []Option{WithSocketName("work")}, []string{"-L", "work"}},
		{
			"both, path last",
			[]Option{WithSocketName("work"), WithSocketPath("/tmp/s")},
			[]string{"-S", "/tmp/s"},
		},
		{
			"both, name last",
			[]Option{WithSocketPath("/tmp/s"), WithSocketName("work")},
			[]string{"-S", "/tmp/s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newConfig(tt.opts).globalArgs(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("globalArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClientReportsWhatItWillRun covers the two exported accessors, which had
// no caller in the tests at all.
//
// They exist so a caller can log or reproduce what the package is about to do,
// which makes agreeing with the flags actually sent the whole of their
// contract.
func TestClientReportsWhatItWillRun(t *testing.T) {
	c := New(WithBinary("/opt/tmux"), WithSocketName("work"))
	if got := c.Binary(); got != "/opt/tmux" {
		t.Errorf("Binary() = %q, want %q", got, "/opt/tmux")
	}
	if got := c.SocketArgs(); !reflect.DeepEqual(got, []string{"-L", "work"}) {
		t.Errorf("SocketArgs() = %q, want [-L work]", got)
	}

	if got := New().Binary(); got != "tmux" {
		t.Errorf("Binary() by default = %q, want %q", got, "tmux")
	}
	if got := New().SocketArgs(); got != nil {
		t.Errorf("SocketArgs() on the default socket = %q, want nil", got)
	}
}

// TestFakeHelperEnvIsNotInherited is a guard on the test harness rather than
// on the package, and it belongs beside WithoutParentEnv because that is what
// it would interact with. The fake identifies its helper process by an
// environment variable; if that variable ever leaked into an ordinary run the
// test binary would re-enter faketmux instead of running tests.
func TestFakeHelperEnvIsNotInherited(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		t.Fatalf("%s is set in the test process; every re-exec would be a helper", helperEnv)
	}
}
