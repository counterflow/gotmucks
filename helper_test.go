package gotmucks

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/counterflow/gotmucks/internal/faketmux"
)

// The command layer is tested by re-executing the test binary as a stand-in
// for tmux. That is the standard os/exec testing idiom and it buys exactness:
// the fake records the argument vector verbatim and replies byte for byte, so
// argv assembly and output parsing are both pinned without a tmux
// installation or a build step.

const helperEnv = "GOTMUCKS_HELPER_PROCESS"

// TestHelperProcess is not a test. It is the entry point the fake tmux runs
// under when the test binary re-executes itself.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("not the helper process")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Exit(faketmux.Run(args, os.Stdout, os.Stderr))
}

// fake is a scripted stand-in tmux plus the options that point a client at it.
type fake struct {
	t          *testing.T
	scriptPath string
	argvPath   string
}

func newFake(t *testing.T, script faketmux.Script) *fake {
	t.Helper()

	dir := t.TempDir()
	f := &fake{
		t:          t,
		scriptPath: filepath.Join(dir, "script.json"),
		argvPath:   filepath.Join(dir, "argv.log"),
	}
	if err := faketmux.WriteScript(f.scriptPath, script); err != nil {
		t.Fatalf("writing fake tmux script: %v", err)
	}
	return f
}

// options point a client at the fake.
func (f *fake) options() []Option {
	f.t.Helper()

	self, err := os.Executable()
	if err != nil {
		f.t.Fatalf("locating test binary: %v", err)
	}
	return []Option{
		WithBinary(self),
		withExecPrefix("-test.run=^TestHelperProcess$", "--"),
		WithEnv(
			helperEnv+"=1",
			faketmux.EnvScript+"="+f.scriptPath,
			faketmux.EnvArgvLog+"="+f.argvPath,
		),
	}
}

func (f *fake) client(extra ...Option) *Client {
	return New(append(f.options(), extra...)...)
}

// argv returns every invocation the fake has seen, in order.
func (f *fake) argv() [][]string {
	f.t.Helper()

	got, err := faketmux.ReadArgvLog(f.argvPath)
	if err != nil {
		f.t.Fatalf("reading argv log: %v", err)
	}
	return got
}

// onlyArgv asserts that exactly one command ran and returns its arguments.
func (f *fake) onlyArgv() []string {
	f.t.Helper()

	all := f.argv()
	if len(all) != 1 {
		f.t.Fatalf("expected exactly one tmux invocation, got %d: %v", len(all), all)
	}
	return all[0]
}

// wantArgv asserts the full argument vector of the nth invocation.
func (f *fake) wantArgv(n int, want ...string) {
	f.t.Helper()

	all := f.argv()
	if n >= len(all) {
		f.t.Fatalf("no invocation %d; only %d ran: %v", n, len(all), all)
	}
	if !reflect.DeepEqual(all[n], want) {
		f.t.Errorf("invocation %d\n got %q\nwant %q", n, all[n], want)
	}
}

// tabbed joins fields with the separator the format parser expects, so that
// fixtures read as columns rather than as escapes.
func tabbed(rows ...[]string) string {
	var out string
	for _, r := range rows {
		for i, f := range r {
			if i > 0 {
				out += "\t"
			}
			out += f
		}
		out += "\n"
	}
	return out
}
