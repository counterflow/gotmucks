//go:build integration

// Integration tests run against a real tmux binary.
//
//	go test -tags integration ./...
//
// Every test gets its own server on its own socket, named with the process id
// and a counter, and kills it on the way out. Nothing here can touch a
// developer's own sessions: the socket name is always set, and a test that
// somehow lost it would talk to a server that does not exist rather than to
// the default one.
package gotmucks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/counterflow/gotmucks/internal/ctlparse"
)

var socketSeq atomic.Int64

// tmuxBinary is the binary under test. Override it to test another build.
func tmuxBinary() string {
	if b := os.Getenv("GOTMUCKS_TMUX"); b != "" {
		return b
	}
	return "tmux"
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(tmuxBinary()); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
}

// testSocket returns a socket name unique to this test.
func testSocket(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("gotmucks-t%d-%d", os.Getpid(), socketSeq.Add(1))
}

// testOptions configures a client against a private server that is destroyed
// when the test ends.
func testOptions(t *testing.T) []Option {
	t.Helper()
	requireTmux(t)

	socket := testSocket(t)
	opts := []Option{WithBinary(tmuxBinary()), WithSocketName(socket)}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := New(opts...).KillServer(ctx); err != nil {
			t.Logf("killing test server on socket %s: %v", socket, err)
		}
	})
	return opts
}

func testClient(t *testing.T) (*Client, []Option) {
	opts := testOptions(t)
	return New(opts...), opts
}

// testCtx bounds a test's tmux calls so a hang fails rather than stalls.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// eventually polls until check passes or the deadline expires.
func eventually(t *testing.T, what string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func TestIntegrationVersionMeetsMinimum(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	v, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Logf("tmux version: %s", v)

	if !v.AtLeast(MinimumVersion()) {
		t.Fatalf("tmux %s is below the supported minimum %s", v, MinimumVersion())
	}
	if err := c.CheckVersion(ctx); err != nil {
		t.Errorf("CheckVersion: %v", err)
	}
}

// TestIntegrationNoServer is the whole no-server contract against a socket
// that has never had a server on it.
func TestIntegrationNoServer(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	running, err := c.ServerRunning(ctx)
	if err != nil {
		t.Fatalf("ServerRunning: %v", err)
	}
	if running {
		t.Fatal("a fresh socket reported a running server")
	}

	sessions, err := c.ListSessions(ctx)
	if err != nil || len(sessions) != 0 {
		t.Errorf("ListSessions = (%v, %v), want (empty, nil)", sessions, err)
	}
	windows, err := c.ListWindows(ctx)
	if err != nil || len(windows) != 0 {
		t.Errorf("ListWindows = (%v, %v), want (empty, nil)", windows, err)
	}
	panes, err := c.ListPanes(ctx)
	if err != nil || len(panes) != 0 {
		t.Errorf("ListPanes = (%v, %v), want (empty, nil)", panes, err)
	}
	has, err := c.HasSession(ctx, "$0")
	if err != nil || has {
		t.Errorf("HasSession = (%v, %v), want (false, nil)", has, err)
	}
	if err := c.KillSession(ctx, "$0"); err != nil {
		t.Errorf("KillSession with no server: %v", err)
	}
	if err := c.KillServer(ctx); err != nil {
		t.Errorf("KillServer with no server: %v", err)
	}
}

func TestIntegrationSessionLifecycle(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{
		Name:    "first",
		Width:   100,
		Height:  30,
		Command: []string{"sleep", "600"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !s.ID.Valid() {
		t.Fatalf("new session has an invalid id %q", s.ID)
	}
	if s.Name != "first" {
		t.Errorf("Name = %q, want %q", s.Name, "first")
	}
	if s.Created.IsZero() {
		t.Error("Created is zero")
	}

	// Size lives on the window, not the session: tmux's session_width and
	// session_height are deprecated and expand to nothing.
	windows, err := c.ListSessionWindows(ctx, s.ID)
	if err != nil || len(windows) != 1 {
		t.Fatalf("ListSessionWindows = (%v, %v)", windows, err)
	}
	if windows[0].Width != 100 || windows[0].Height != 30 {
		t.Errorf("window size = %dx%d, want 100x30", windows[0].Width, windows[0].Height)
	}

	running, err := c.ServerRunning(ctx)
	if err != nil || !running {
		t.Errorf("ServerRunning after NewSession = (%v, %v)", running, err)
	}

	has, err := c.HasSession(ctx, s.ID)
	if err != nil || !has {
		t.Errorf("HasSession(%s) = (%v, %v), want (true, nil)", s.ID, has, err)
	}

	// A second session, to check listing and that ids stay distinct.
	s2, err := c.NewSession(ctx, NewSessionOptions{Name: "second", Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("second NewSession: %v", err)
	}
	if s2.ID == s.ID {
		t.Fatalf("both sessions have id %s", s.ID)
	}

	sessions, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(sessions), sessions)
	}

	// Renaming must not change the identifier. That is the entire reason
	// this package addresses sessions by id.
	if err := c.RenameSession(ctx, s.ID, "renamed"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	got, err := c.Session(ctx, s.ID)
	if err != nil {
		t.Fatalf("Session after rename: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("id changed on rename: %s -> %s", s.ID, got.ID)
	}
	if got.Name != "renamed" {
		t.Errorf("Name = %q, want %q", got.Name, "renamed")
	}

	if err := c.KillSession(ctx, s.ID); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	has, err = c.HasSession(ctx, s.ID)
	if err != nil || has {
		t.Errorf("HasSession after kill = (%v, %v), want (false, nil)", has, err)
	}

	// Killing it again is still success.
	if err := c.KillSession(ctx, s.ID); err != nil {
		t.Errorf("second KillSession: %v", err)
	}

	if _, err := c.Session(ctx, s.ID); !errors.Is(err, ErrNoSession) {
		t.Errorf("Session on a dead session = %v, want ErrNoSession", err)
	}
}

// TestIntegrationSessionEnvironment exercises new-session -e, which is the
// reason tmux 3.2 is this package's floor.
func TestIntegrationSessionEnvironment(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")

	_, err := c.NewSession(ctx, NewSessionOptions{
		Name: "envtest",
		Env:  map[string]string{"GOTMUCKS_ONE": "first", "GOTMUCKS_TWO": "second"},
		// Reading the variable back requires a shell, so ask for one
		// explicitly rather than relying on tmux to supply it.
		Command: []string{"sh", "-c", "printf '%s,%s' \"$GOTMUCKS_ONE\" \"$GOTMUCKS_TWO\" > " + out + "; sleep 60"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	eventually(t, "the session environment to reach the pane's process", 10*time.Second, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && len(b) > 0
	})

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading %s: %v", out, err)
	}
	if got := string(b); got != "first,second" {
		t.Errorf("session environment = %q, want %q", got, "first,second")
	}
}

// TestIntegrationCommandIsExecuted checks the promise that a multi-element
// command vector is executed directly, so shell metacharacters in it are
// inert. If tmux passed the line to a shell, the semicolon would split it and
// the file would be named differently.
func TestIntegrationCommandIsExecuted(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	dir := t.TempDir()
	tricky := filepath.Join(dir, "a;b c")

	if _, err := c.NewSession(ctx, NewSessionOptions{
		Name:    "argv",
		Command: []string{"touch", tricky},
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	eventually(t, "the command to run", 10*time.Second, func() bool {
		_, err := os.Stat(tricky)
		return err == nil
	})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "a;b c" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %q; a shell interpreted the command", names)
	}
}

func TestIntegrationWindowsAndPanes(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{
		Name:       "wp",
		WindowName: "the-window",
		Width:      90,
		Height:     25,
		Command:    []string{"sleep", "600"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	windows, err := c.ListSessionWindows(ctx, s.ID)
	if err != nil {
		t.Fatalf("ListSessionWindows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	w := windows[0]
	if !w.ID.Valid() {
		t.Errorf("window id %q is malformed", w.ID)
	}
	if w.Session != s.ID {
		t.Errorf("window session = %s, want %s", w.Session, s.ID)
	}
	if w.Name != "the-window" {
		t.Errorf("window name = %q, want %q", w.Name, "the-window")
	}
	if !w.Active {
		t.Error("the only window is not active")
	}
	if w.Layout == "" {
		t.Error("window layout is empty")
	}

	panes, err := c.ListWindowPanes(ctx, w.ID)
	if err != nil {
		t.Fatalf("ListWindowPanes: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("got %d panes, want 1", len(panes))
	}
	p := panes[0]
	if !p.ID.Valid() {
		t.Errorf("pane id %q is malformed", p.ID)
	}
	if p.Window != w.ID || p.Session != s.ID {
		t.Errorf("pane containers = (%s, %s), want (%s, %s)", p.Window, p.Session, w.ID, s.ID)
	}
	if p.PID <= 0 {
		t.Errorf("pane pid = %d", p.PID)
	}
	if p.Width != 90 || p.Height != 25 {
		t.Errorf("pane size = %dx%d, want 90x25", p.Width, p.Height)
	}
	if p.CurrentCommand != "sleep" {
		t.Errorf("pane current command = %q, want %q", p.CurrentCommand, "sleep")
	}

	// The by-id lookups must agree with the listings.
	gotPane, err := c.Pane(ctx, p.ID)
	if err != nil {
		t.Fatalf("Pane: %v", err)
	}
	if gotPane.ID != p.ID {
		t.Errorf("Pane returned %s, want %s", gotPane.ID, p.ID)
	}
	gotWindow, err := c.Window(ctx, w.ID)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if gotWindow.ID != w.ID {
		t.Errorf("Window returned %s, want %s", gotWindow.ID, w.ID)
	}

	// Renaming a window leaves its id alone, as for sessions.
	if err := c.RenameWindow(ctx, w.ID, "other"); err != nil {
		t.Fatalf("RenameWindow: %v", err)
	}
	gotWindow, err = c.Window(ctx, w.ID)
	if err != nil {
		t.Fatalf("Window after rename: %v", err)
	}
	if gotWindow.Name != "other" || gotWindow.ID != w.ID {
		t.Errorf("after rename: id %s name %q", gotWindow.ID, gotWindow.Name)
	}
}

func TestIntegrationSendKeysAndCapture(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	// cat echoes what it is given, which makes the round trip observable
	// without depending on a shell prompt.
	s, err := c.NewSession(ctx, NewSessionOptions{
		Name:    "keys",
		Width:   80,
		Height:  24,
		Command: []string{"cat"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	panes, err := c.ListSessionPanes(ctx, s.ID)
	if err != nil || len(panes) != 1 {
		t.Fatalf("ListSessionPanes = (%v, %v)", panes, err)
	}
	pane := panes[0].ID

	eventually(t, "cat to start", 10*time.Second, func() bool {
		p, err := c.Pane(ctx, pane)
		return err == nil && p.CurrentCommand == "cat"
	})

	const marker = "gotmucks-round-trip"
	if err := c.SendLine(ctx, pane, marker); err != nil {
		t.Fatalf("SendLine: %v", err)
	}

	var capture []byte
	eventually(t, "the sent text to appear in the pane", 10*time.Second, func() bool {
		capture, err = c.CapturePane(ctx, pane, CaptureOptions{})
		return err == nil && strings.Contains(string(capture), marker)
	})

	if !strings.Contains(string(capture), marker) {
		t.Errorf("capture does not contain %q:\n%s", marker, capture)
	}

	// A capture with escapes must still come back as bytes we did not mangle.
	withEscapes, err := c.CapturePane(ctx, pane, CaptureOptions{Escapes: true, FullHistory: true})
	if err != nil {
		t.Fatalf("CapturePane with escapes: %v", err)
	}
	if !strings.Contains(string(withEscapes), marker) {
		t.Errorf("escaped capture lost the marker:\n%q", withEscapes)
	}

	// Hex keys, last: 0x03 is interrupt and cat exits on it, which ends the
	// only session and so takes the server with it.
	if err := c.SendKeys(ctx, pane, Hex(0x03)); err != nil {
		t.Fatalf("SendKeys hex: %v", err)
	}
	eventually(t, "the interrupt to end the session", 10*time.Second, func() bool {
		has, err := c.HasSession(ctx, s.ID)
		return err == nil && !has
	})
}

func TestIntegrationOptionsAndHooks(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "opts", Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := c.SetOption(ctx, s.ID, "status", "off"); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	v, ok, err := c.ShowOption(ctx, s.ID, ScopeSession, "status")
	if err != nil {
		t.Fatalf("ShowOption: %v", err)
	}
	if !ok || v != "off" {
		t.Errorf("status = (%q, %v), want (off, true)", v, ok)
	}

	// A value with spaces has to survive tmux's own quoting on the way back.
	const statusLeft = "[#S] left side"
	if err := c.SetOption(ctx, s.ID, "status-left", statusLeft); err != nil {
		t.Fatalf("SetOption status-left: %v", err)
	}
	v, ok, err = c.ShowOption(ctx, s.ID, ScopeSession, "status-left")
	if err != nil {
		t.Fatalf("ShowOption status-left: %v", err)
	}
	if !ok || v != statusLeft {
		t.Errorf("status-left = %q, want %q", v, statusLeft)
	}

	opts, err := c.ShowOptions(ctx, s.ID, ScopeSession)
	if err != nil {
		t.Fatalf("ShowOptions: %v", err)
	}
	if opts["status"] != "off" {
		t.Errorf("ShowOptions status = %q, want off", opts["status"])
	}

	if err := c.UnsetOption(ctx, s.ID, ScopeSession, "status"); err != nil {
		t.Fatalf("UnsetOption: %v", err)
	}

	// remain-on-exit, on both the scopes this package selects between.
	panes, err := c.ListSessionPanes(ctx, s.ID)
	if err != nil || len(panes) == 0 {
		t.Fatalf("ListSessionPanes = (%v, %v)", panes, err)
	}
	if err := c.SetRemainOnExit(ctx, panes[0].ID, true); err != nil {
		t.Fatalf("SetRemainOnExit on a pane: %v", err)
	}
	if err := c.SetRemainOnExit(ctx, panes[0].Window, true); err != nil {
		t.Fatalf("SetRemainOnExit on a window: %v", err)
	}

	// Global hooks round-trip through show-hooks.
	if err := c.SetGlobalHook(ctx, "session-created", "display-message made"); err != nil {
		t.Fatalf("SetGlobalHook: %v", err)
	}
	globals, err := c.ShowGlobalHooks(ctx)
	if err != nil {
		t.Fatalf("ShowGlobalHooks: %v", err)
	}
	found := false
	for name := range globals {
		if strings.HasPrefix(name, "session-created") {
			found = true
		}
	}
	if !found {
		t.Errorf("session-created hook missing from the global hooks")
	}

	if err := c.UnsetGlobalHook(ctx, "session-created"); err != nil {
		t.Fatalf("UnsetGlobalHook: %v", err)
	}
}

// TestIntegrationHookFires checks a per-target hook by the only means that
// actually proves it: making it fire.
//
// show-hooks -t reports nothing on tmux 3.2a even for a hook that was set
// successfully, so asserting on introspection would test tmux's reporting
// rather than this package's set-hook.
func TestIntegrationHookFires(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	dir := t.TempDir()
	marker := filepath.Join(dir, "fired")

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "hooks", Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := c.SetHook(ctx, s.ID, "pane-exited", "run-shell 'touch "+marker+"'"); err != nil {
		t.Fatalf("SetHook: %v", err)
	}

	// Give the session a second window whose command exits at once.
	if err := c.runOK(ctx, "new-window", "-t", string(s.ID), "--", "true"); err != nil {
		t.Fatalf("new-window: %v", err)
	}

	eventually(t, "the pane-exited hook to fire", 15*time.Second, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
}

// --- control mode ---------------------------------------------------------

// testControl opens a control connection against a private server.
func testControl(t *testing.T, extra ...Option) (*ControlClient, *Client) {
	t.Helper()

	opts := testOptions(t)
	c := New(opts...)

	// Attach to a session created up front so the connection's lifetime and
	// the session's are independent.
	s, err := c.NewSession(testCtx(t), NewSessionOptions{
		Name:    "control",
		Width:   80,
		Height:  24,
		Command: []string{"cat"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	cc, err := Connect(testCtx(t), append(append(opts, WithAttach(s.ID)), extra...)...)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		if err := cc.Close(); err != nil {
			t.Logf("closing control connection: %v", err)
		}
	})
	return cc, c
}

// TestIntegrationPaneWithTabInItsPath is H1 against real tmux: tmux hands
// pane_current_path back with a raw tab in it, and a single such pane used to
// make every pane listing on the whole server fail.
func TestIntegrationPaneWithTabInItsPath(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	dir := filepath.Join(t.TempDir(), "a\tb")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skipf("this filesystem will not hold a directory with a tab in its name: %v", err)
	}

	if _, err := c.NewSession(ctx, NewSessionOptions{
		Name: "tabbed", Width: 80, Height: 24, StartDir: dir, Command: []string{"cat"},
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	panes, err := c.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) == 0 {
		t.Fatal("no panes")
	}

	var found bool
	for _, p := range panes {
		if strings.Contains(p.CurrentPath, "\t") {
			found = true
		}
	}
	if !found {
		// Worth saying which it was: the alternative to a raw tab is that
		// this tmux escapes the field, which would be a change worth knowing.
		t.Errorf("no pane reported a path containing a tab; paths were %v", pathsOf(panes))
	}
}

func pathsOf(panes []Pane) []string {
	out := make([]string, len(panes))
	for i, p := range panes {
		out[i] = p.CurrentPath
	}
	return out
}

// TestIntegrationSessionOrderIsTmuxsOwn is F1. ListSessions promised an order
// by identifier and imposed none; what tmux prints is ordered by name, which
// for sessions created in any order but alphabetical is a different sequence
// entirely. The doc now promises nothing and records this, so this is what
// says the record is still true of the tmux under test.
func TestIntegrationSessionOrderIsTmuxsOwn(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	// Created newest-first alphabetically, so name order and identifier order
	// are exact opposites and neither can be mistaken for the other.
	for _, name := range []string{"zulu", "mike", "alpha"} {
		if _, err := c.NewSession(ctx, NewSessionOptions{Name: name}); err != nil {
			t.Fatalf("NewSession %s: %v", name, err)
		}
	}

	sessions, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}

	var names, ids []string
	for _, s := range sessions {
		names = append(names, s.Name)
		ids = append(ids, string(s.ID))
	}
	if want := []string{"alpha", "mike", "zulu"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names came back %v, want %v: tmux orders sessions by name", names, want)
	}
	if want := []string{"$2", "$1", "$0"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("identifiers came back %v, want %v", ids, want)
	}
}

// TestIntegrationEmptyColumnRowsSurvive is F3 against real tmux. A one-column
// row whose value is empty is a line with nothing on it, and tmux prints one
// per object like any other: a listing that dropped them returned fewer rows
// than there were sessions, with no error and nothing to say which was gone.
func TestIntegrationEmptyColumnRowsSurvive(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	// A user option nobody has set expands to the empty string, which is a
	// value no release can change out from under this test the way a default
	// could.
	spec := FormatSpec{"@gotmucks_absent"}

	first, err := c.NewSession(ctx, NewSessionOptions{Name: "one"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	rows, err := c.Query(ctx, "list-sessions", spec)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for one session, want 1", len(rows))
	}
	if v, ok := rows[0].Lookup("@gotmucks_absent"); !ok || v != "" {
		t.Errorf("row 0 = (%q, %v), want the empty value", v, ok)
	}

	second, err := c.NewSession(ctx, NewSessionOptions{Name: "two"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if rows, err = c.Query(ctx, "list-sessions", spec); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows for two sessions, want 2", len(rows))
	}

	// And the column is being read rather than the lines merely counted.
	if err := c.SetOption(ctx, first.ID, "@gotmucks_absent", "one"); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	if err := c.SetOption(ctx, second.ID, "@gotmucks_absent", "two"); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	if rows, err = c.Query(ctx, "list-sessions", spec); err != nil {
		t.Fatalf("Query: %v", err)
	}
	var values []string
	for _, r := range rows {
		v, _ := r.Lookup("@gotmucks_absent")
		values = append(values, v)
	}
	// tmux orders sessions by name, so "one" comes before "two".
	if want := []string{"one", "two"}; strings.Join(values, ",") != strings.Join(want, ",") {
		t.Errorf("values = %v, want %v", values, want)
	}
}

// TestIntegrationRawTabsDoNotShiftColumns is N2 against real tmux.
//
// Folding an overflowing field into the last column recovers the value when
// the last column is the one that overflowed, and shifts every field between
// when some earlier one did. The path is not the only field that can overflow:
// pane_current_command carries a raw tab whenever the running binary's own
// file name does, and window_name carries one whenever the window was named
// through new-session -n or new-window -n. Both were middle columns, and both
// turned a listing that used to fail loudly into three wrong values and no
// error.
//
// So this asks for a row with a raw tab available in two places at once and
// looks at every column of it. The fields either side of the sanitised one are
// the assertion that matters: a shift moves them, and their values here are
// ones the test chose.
func TestIntegrationRawTabsDoNotShiftColumns(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	dir := filepath.Join(t.TempDir(), "dir\tname")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skipf("this filesystem will not hold a directory with a tab in its name: %v", err)
	}
	prog := tabbedCopyOf(t, "cat")

	s, err := c.NewSession(ctx, NewSessionOptions{
		Name:       "tabs",
		WindowName: "win\tname",
		Width:      80,
		Height:     24,
		StartDir:   dir,
		// Two elements, so tmux execs this directly and the tab in the path is
		// inert. "-" is cat's own spelling for standard input, which in a pane
		// is the terminal, so it stays in the foreground.
		Command: []string{prog, "-"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	panes, err := c.ListSessionPanes(ctx, s.ID)
	if err != nil {
		t.Fatalf("ListSessionPanes: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("got %d panes, want 1 — a pane whose command exited leaves nothing to look at", len(panes))
	}
	p := panes[0]

	// The columns before the one that could overflow.
	if p.Session != s.ID {
		t.Errorf("Session = %q, want %q", p.Session, s.ID)
	}
	if p.Index != 0 {
		t.Errorf("Index = %d, want 0", p.Index)
	}
	if p.PID <= 0 {
		t.Errorf("PID = %d, want the pane's child", p.PID)
	}
	if p.Width != 80 || p.Height != 24 {
		t.Errorf("size = %dx%d, want 80x24", p.Width, p.Height)
	}
	// The one that could, and the two after it.
	if p.CurrentCommand != "ab cd" {
		t.Errorf("CurrentCommand = %q, want %q with its tab replaced", p.CurrentCommand, "ab\tcd")
	}
	if strings.Contains(p.Title, "\t") {
		t.Errorf("Title = %q, which has taken a raw tab", p.Title)
	}
	if !strings.HasSuffix(p.CurrentPath, "dir\tname") {
		t.Errorf("CurrentPath = %q, want it to end in the tabbed directory unaltered", p.CurrentPath)
	}

	// The same question of the window row, where the name is the field that
	// can overflow and four columns sit after it.
	windows, err := c.ListSessionWindows(ctx, s.ID)
	if err != nil {
		t.Fatalf("ListSessionWindows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	w := windows[0]
	if w.Name != "win name" {
		t.Errorf("Name = %q, want %q with its tab replaced", w.Name, "win\tname")
	}
	if !w.Active || w.Panes != 1 || w.Index != 0 {
		t.Errorf("active=%v panes=%d index=%d, want true/1/0", w.Active, w.Panes, w.Index)
	}
	if w.Width != 80 || w.Height != 24 {
		t.Errorf("window size = %dx%d, want 80x24", w.Width, w.Height)
	}
	if !strings.Contains(w.Layout, "80x24") {
		t.Errorf("Layout = %q, want tmux's layout string for an 80x24 window", w.Layout)
	}
}

// tabbedCopyOf copies a program to a file called "ab<tab>cd" and returns the
// path, so that a pane running it reports a raw tab in pane_current_command.
// It has to be a copy of a real binary: tmux takes the name from the operating
// system, so a wrapper script would report whatever the wrapper ran.
func tabbedCopyOf(t *testing.T, name string) string {
	t.Helper()

	src, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH: %v", name, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("reading %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "ab\tcd")
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Skipf("this filesystem will not hold a file with a tab in its name: %v", err)
	}
	return dst
}

// TestIntegrationShowOption is H2 against real tmux. The unit test for this
// scripted a stderr string ("unknown option") that no tmux emits, so the
// branch it covered could never fire; 3.2a says "invalid option". The other
// half is that an option which is simply not set in the table exits 0 with no
// output, which is what makes the bool meaningful.
func TestIntegrationShowOption(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "opts", Width: 80, Height: 24, Command: []string{"cat"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := c.SetOption(ctx, s.ID, "status-left", "[#S] "); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	v, ok, err := c.ShowOption(ctx, s.ID, ScopeSession, "status-left")
	if err != nil {
		t.Fatalf("ShowOption: %v", err)
	}
	if !ok || v != "[#S] " {
		t.Errorf("got (%q, %v), want (%q, true)", v, ok, "[#S] ")
	}

	// A value tmux has to escape on the way out must come back intact.
	if err := c.SetOption(ctx, s.ID, "status-right", "a\tb\\c"); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	if v, ok, err = c.ShowOption(ctx, s.ID, ScopeSession, "status-right"); err != nil {
		t.Fatalf("ShowOption: %v", err)
	} else if !ok || v != "a\tb\\c" {
		t.Errorf("got (%q, %v), want (%q, true)", v, ok, "a\tb\\c")
	}

	if err := c.UnsetOption(ctx, s.ID, ScopeSession, "status-left"); err != nil {
		t.Fatalf("UnsetOption: %v", err)
	}
	if v, ok, err = c.ShowOption(ctx, s.ID, ScopeSession, "status-left"); err != nil {
		t.Fatalf("ShowOption after unset: %v", err)
	} else if ok || v != "" {
		t.Errorf("got (%q, %v) for an option that is not set, want (\"\", false)", v, ok)
	}

	if v, ok, err = c.ShowOption(ctx, s.ID, ScopeSession, "no-such-option-at-all"); err != nil {
		t.Fatalf("an unknown option name is an absence, not a failure: %v", err)
	} else if ok || v != "" {
		t.Errorf("got (%q, %v) for an unknown option name, want (\"\", false)", v, ok)
	}
}

// TestIntegrationShowHooksReportsPerTargetHooks is M4: the comment on
// ShowHooks used to warn that "show-hooks -t" reports nothing on 3.2a even for
// a hook that was set successfully. It does report it.
func TestIntegrationShowHooksReportsPerTargetHooks(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "hooks", Width: 80, Height: 24, Command: []string{"cat"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := c.SetHook(ctx, s.ID, "alert-bell", "display-message hooked"); err != nil {
		t.Fatalf("SetHook: %v", err)
	}

	hooks, err := c.ShowHooks(ctx, s.ID)
	if err != nil {
		t.Fatalf("ShowHooks: %v", err)
	}
	if !hasHookNamed(hooks, "alert-bell") {
		t.Errorf("show-hooks -t did not report a hook that was set on the target: %v", hooks)
	}

	// A window of that session reports its session's hooks as its own, which
	// is why the doc says this reports what would fire rather than where it
	// was set.
	windows, err := c.ListSessionWindows(ctx, s.ID)
	if err != nil || len(windows) == 0 {
		t.Fatalf("ListSessionWindows: %v (%d windows)", err, len(windows))
	}
	if hooks, err = c.ShowHooks(ctx, windows[0].ID); err != nil {
		t.Fatalf("ShowHooks on a window: %v", err)
	}
	if !hasHookNamed(hooks, "alert-bell") {
		t.Errorf("a window did not report its session's hook: %v", hooks)
	}
}

// hasHookNamed matches a hook regardless of the array index tmux appends,
// which turns "alert-bell" into "alert-bell[0]".
func hasHookNamed(hooks map[string]string, name string) bool {
	for k := range hooks {
		if k == name || strings.HasPrefix(k, name+"[") {
			return true
		}
	}
	return false
}

// TestIntegrationMissingTargetErrors is M5: a missing window and a missing
// pane are distinguishable from a missing session.
func TestIntegrationMissingTargetErrors(t *testing.T) {
	c, _ := testClient(t)
	ctx := testCtx(t)

	if _, err := c.NewSession(ctx, NewSessionOptions{Name: "targets", Width: 80, Height: 24, Command: []string{"cat"}}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if _, err := c.Window(ctx, WindowID("@999")); !errors.Is(err, ErrNoWindow) {
		t.Errorf("Window on a missing window = %v, want ErrNoWindow", err)
	}
	if _, err := c.Pane(ctx, PaneID("%999")); !errors.Is(err, ErrNoPane) {
		t.Errorf("Pane on a missing pane = %v, want ErrNoPane", err)
	}
	if _, err := c.Session(ctx, SessionID("$999")); !errors.Is(err, ErrNoSession) {
		t.Errorf("Session on a missing session = %v, want ErrNoSession", err)
	}

	// And the same distinction as tmux itself reports it, through a command
	// that resolves the target rather than scanning a listing.
	_, err := c.ListWindowPanes(ctx, WindowID("@999"))
	if err != nil {
		t.Errorf("a listing of something that does not exist is empty, not an error: %v", err)
	}
}

// TestIntegrationControlBlockFlagsMarkOurCommands pins the assumption the
// reader's second orphan rule rests on, against whichever tmux is under test.
//
// tmux computes a block's flags word from CMDQ_STATE_CONTROL, which it sets
// only for a command line read from the control client's own input, so a block
// it opened for itself carries 0 and a reply to us carries 1. If a release
// ever stops doing that, the reader would start binding unsolicited blocks to
// commands again — silently — so this is checked rather than assumed. It talks
// to tmux directly instead of through [Connect], because the client absorbs
// the opening block and the point here is to look at it.
func TestIntegrationControlBlockFlagsMarkOurCommands(t *testing.T) {
	opts := testOptions(t)
	c := New(opts...)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "flags", Width: 80, Height: 24, Command: []string{"cat"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	cfg := newConfig(opts)
	args, _ := cfg.argv([]string{"-C", "attach-session", "-t", string(s.ID)})
	cmd := exec.Command(cfg.binary, args...)
	cmd.Env = cfg.environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting tmux -C: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Written straight away: tmux is expected to open its own block before it
	// reads any of this, which is the other half of what is being checked.
	if _, err := io.WriteString(stdin, "display-message -p gotmucks-marker\n"); err != nil {
		t.Fatalf("writing the command: %v", err)
	}

	type block struct {
		flags int
		body  []string
	}
	blocks := make(chan block, 4)
	go func() {
		sc := bufio.NewScanner(stdout)
		var open *block
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "%begin "):
				fields := strings.Fields(line)
				if len(fields) < 4 {
					continue
				}
				flags, _ := strconv.Atoi(fields[3])
				open = &block{flags: flags}
			case strings.HasPrefix(line, "%end "), strings.HasPrefix(line, "%error "):
				if open != nil {
					blocks <- *open
					open = nil
				}
			default:
				if open != nil {
					open.body = append(open.body, line)
				}
			}
		}
	}()

	next := func(what string) block {
		t.Helper()
		select {
		case b := <-blocks:
			return b
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return block{}
		}
	}

	opening := next("tmux's own opening block")
	if opening.flags != 0 {
		t.Errorf("the block tmux opened for its start command has flags %d, want 0;"+
			" beginBlock treats a zero flags word as the mark of a block this client did not ask for",
			opening.flags)
	}

	reply := next("the reply to our command")
	if reply.flags != 1 {
		t.Errorf("the reply to a command we sent has flags %d, want 1;"+
			" a reply that looked unsolicited would never reach the caller", reply.flags)
	}
	if len(reply.body) != 1 || reply.body[0] != "gotmucks-marker" {
		t.Errorf("reply body = %q, want the command's own output", reply.body)
	}
}

// TestIntegrationControlNotificationsNeverEnterABlock holds the rule the
// reader rests on against whatever tmux is under test.
//
// Inside an open block the reader treats every line as the command's output,
// because a reply body is whatever the command printed and capture-pane will
// happily print a line shaped like a notification. That is only safe while
// tmux keeps notifications out of an open block, so the matrix checks it
// rather than trusting one release's answer: a tmux that started interleaving
// would lose those notifications into a reply body, and this says so on the
// version that did it. scripts/probe-interleave.sh asks the same question by
// hand and prints the shape of the answer.
//
// It talks to tmux directly rather than through [Connect], because what is
// being measured is the raw stream the reader would see.
func TestIntegrationControlNotificationsNeverEnterABlock(t *testing.T) {
	opts := testOptions(t)
	c := New(opts...)
	ctx := testCtx(t)

	// A pane that produces output without pause: the question is meaningless
	// on a quiet server, where no notification wanted writing anyway.
	s, err := c.NewSession(ctx, NewSessionOptions{
		Name:   "interleave",
		Width:  200,
		Height: 50,
		Command: []string{"sh", "-c",
			`i=0; while [ $i -lt 200000 ]; do echo "spam $i ------------------------------"; ` +
				`i=$((i+1)); done; sleep 60`},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	panes, err := c.ListSessionPanes(ctx, s.ID)
	if err != nil || len(panes) != 1 {
		t.Fatalf("ListSessionPanes = (%v, %v)", panes, err)
	}
	pane := panes[0].ID

	cfg := newConfig(opts)
	args, _ := cfg.argv([]string{"-C", "attach-session", "-t", string(s.ID)})
	cmd := exec.Command(cfg.binary, args...)
	cmd.Env = cfg.environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting tmux -C: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// The last command prints the marker that ends the measurement. Before it:
	// a reply of thousands of lines, and a command that makes tmux hold a
	// block open for a whole second while the pane keeps producing output.
	const marker = "gotmucks-interleave-done"
	script := strings.Join([]string{
		"refresh-client -C 200x50",
		"capture-pane -p -S -2000 -t '" + string(pane) + "'",
		`run-shell "sleep 1"`,
		"capture-pane -p -S -2000 -t '" + string(pane) + "'",
		"display-message -p " + marker,
	}, "\n") + "\n"
	if _, err := io.WriteString(stdin, script); err != nil {
		t.Fatalf("writing the commands: %v", err)
	}

	type counts struct {
		blocks, inside, outside, body int
		examples                      []string
	}
	results := make(chan counts, 1)
	go func() {
		// ReadString rather than a Scanner, for the same reason the reader
		// uses one: a pane output line has no useful upper bound.
		r := bufio.NewReaderSize(stdout, 64<<10)
		var got counts
		open := false
		for {
			raw, err := r.ReadString('\n')
			line := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
			if line != "" {
				switch cl := ctlparse.Classify(line); cl.Kind {
				case ctlparse.KindBegin:
					open = true
					got.blocks++
				case ctlparse.KindEnd, ctlparse.KindError:
					open = false
				case ctlparse.KindNotification:
					if !open {
						got.outside++
						break
					}
					got.inside++
					if len(got.examples) < 3 {
						got.examples = append(got.examples, line)
					}
				default:
					if open {
						got.body++
						if line == marker {
							results <- got
							return
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
		results <- got
	}()

	var got counts
	select {
	case got = <-results:
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for the marker command's reply")
	}

	t.Logf("blocks=%d body lines=%d notifications: inside=%d outside=%d",
		got.blocks, got.body, got.inside, got.outside)

	if got.inside != 0 {
		t.Errorf("%d notifications arrived inside an open block, e.g. %q.\n"+
			"The reader treats a block's body as the command's own output, so on this tmux "+
			"those notifications are being delivered as reply lines instead of events",
			got.inside, got.examples)
	}

	// Guard against a green result that measured nothing: without notifications
	// flowing and a block held open across them, there was no question asked.
	if got.outside < 50 {
		t.Errorf("only %d notifications arrived at all; the pane was not producing enough "+
			"output for this to have tested anything", got.outside)
	}
	if got.blocks < 5 || got.body < 100 {
		t.Errorf("saw %d blocks and %d body lines, want the five commands and a large capture; "+
			"the measurement did not run to completion", got.blocks, got.body)
	}
}

// TestIntegrationControlFirstCommandGetsItsOwnReply is the regression test for
// the off-by-one this package used to have on every connection: Connect writes
// its probe as soon as the reader starts, and if that write won the race
// against tmux's opening block, the probe took the block's empty body and
// every later reply belonged to the command before it.
//
// It reconnects several times because the outcome was a race: the failure
// showed up in roughly one run in fifteen.
func TestIntegrationControlFirstCommandGetsItsOwnReply(t *testing.T) {
	opts := testOptions(t)
	c := New(opts...)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "first", Width: 80, Height: 24, Command: []string{"cat"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	for i := 0; i < 10; i++ {
		cc, err := Connect(ctx, append(opts, WithAttach(s.ID))...)
		if err != nil {
			t.Fatalf("connection %d: Connect: %v", i, err)
		}

		want := fmt.Sprintf("marker-%d", i)
		reply, err := cc.DoArgs(ctx, "display-message", "-p", want)
		if err != nil {
			t.Errorf("connection %d: DoArgs: %v", i, err)
		} else if len(reply.Output) != 1 || reply.Output[0] != want {
			t.Errorf("connection %d: reply = %q, want [%q]", i, reply.Output, want)
		}
		if err := cc.Close(); err != nil {
			t.Errorf("connection %d: Close: %v", i, err)
		}
	}
}

// TestIntegrationControlReapsWithoutClose covers the path the documentation
// does not push callers down: Done fires, the caller never calls Close, and
// the child still has to be reaped rather than left with its pipes open.
func TestIntegrationControlReapsWithoutClose(t *testing.T) {
	opts := testOptions(t)
	c := New(opts...)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "reap", Width: 80, Height: 24, Command: []string{"cat"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	cc, err := Connect(ctx, append(opts, WithAttach(s.ID))...)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	// Killing the session detaches the control client, which then exits.
	if err := c.KillSession(ctx, s.ID); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	select {
	case <-cc.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the connection never ended after its session was killed")
	}

	select {
	case <-cc.reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("the tmux process was never waited for, so it and its pipes are still held")
	}
	if cc.cmd.ProcessState == nil {
		t.Error("the process was not reaped")
	}
}

func TestIntegrationControlConnect(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	if v := cc.Version(); !v.AtLeast(MinimumVersion()) {
		t.Errorf("control client version = %s", v)
	}

	reply, err := cc.Do(ctx, "list-sessions -F '#{session_id}'")
	if err != nil {
		t.Fatalf("Do: %v (stderr: %s)", err, cc.Stderr())
	}
	if len(reply.Output) == 0 {
		t.Fatal("list-sessions returned no rows over the control connection")
	}
	for _, line := range reply.Output {
		if !SessionID(line).Valid() {
			t.Errorf("list-sessions returned %q, which is not a session id", line)
		}
	}

	eventually(t, "the attached session to be reported", 5*time.Second, func() bool {
		return cc.AttachedSession() != ""
	})
	if !cc.AttachedSession().Valid() {
		t.Errorf("AttachedSession = %q", cc.AttachedSession())
	}
}

// TestIntegrationControlQuoting is the counterpart to a finding from
// scripts/probe-control.sh: tmux parses the command line it is given, and an
// unquoted '#' starts a comment, so a format string must be quoted or the
// command silently loses its argument.
func TestIntegrationControlQuoting(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	reply, err := cc.DoArgs(ctx, "list-panes", "-F", "#{pane_id} #{pane_width}")
	if err != nil {
		t.Fatalf("DoArgs: %v", err)
	}
	if len(reply.Output) == 0 {
		t.Fatal("no panes returned")
	}
	first := strings.Fields(reply.Output[0])
	if len(first) != 2 {
		t.Fatalf("expected two fields from the format, got %q", reply.Output[0])
	}
	if !PaneID(first[0]).Valid() {
		t.Errorf("first field %q is not a pane id; the format was mangled", first[0])
	}
}

// TestIntegrationControlSpecArg sends a real [FormatSpec] over the connection.
//
// Arg now renders every column but the last inside a substitution, so what
// DoArgs has to quote is a string holding tabs, braces, slashes and a '#' —
// and tmux parses that line itself. This is the one place both halves are
// exercised together: quoteArg's output and tmux's parser.
func TestIntegrationControlSpecArg(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	reply, err := cc.DoArgs(ctx, "list-panes", "-a", "-F", paneSpec.Arg())
	if err != nil {
		t.Fatalf("DoArgs: %v", err)
	}
	rows, err := reply.Rows(paneSpec)
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no panes returned")
	}
	p, err := paneFromRow(rows[0])
	if err != nil {
		t.Fatalf("paneFromRow: %v (row %q)", err, reply.Output[0])
	}
	// A substitution tmux did not understand expands to nothing rather than
	// failing, so an empty column is the failure mode to look for.
	if p.CurrentCommand == "" || p.Width == 0 || p.Height == 0 {
		t.Errorf("command=%q size=%dx%d from %q; a column expanded to nothing",
			p.CurrentCommand, p.Width, p.Height, reply.Output[0])
	}
}

func TestIntegrationControlErrorReply(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	_, err := cc.Do(ctx, "kill-session -t $999")
	var cerr *ControlError
	if !errors.As(err, &cerr) {
		t.Fatalf("got %v (%T), want *ControlError", err, err)
	}
	if cerr.Message == "" {
		t.Error("ControlError carries no message")
	}
	t.Logf("tmux said: %s", cerr.Message)
}

func TestIntegrationControlConcurrentDo(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("marker-%d", i)
			reply, err := cc.DoArgs(ctx, "display-message", "-p", want)
			if err != nil {
				errs <- fmt.Errorf("command %d: %w", i, err)
				return
			}
			if len(reply.Output) != 1 || reply.Output[0] != want {
				errs <- fmt.Errorf("command %d got %q, want %q", i, reply.Output, want)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestIntegrationControlPaneOutput(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	// Find the pane running cat.
	reply, err := cc.DoArgs(ctx, "list-panes", "-F", "#{pane_id}")
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	if len(reply.Output) == 0 {
		t.Fatal("no panes")
	}
	pane := PaneID(reply.Output[0])
	tap, err := cc.Output(pane)
	if err != nil {
		t.Fatalf("Output(%s): %v", pane, err)
	}

	const marker = "gotmucks-live-output"
	if _, err := cc.DoArgs(ctx, "send-keys", "-t", string(pane), "-l", marker); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	if _, err := cc.DoArgs(ctx, "send-keys", "-t", string(pane), "Enter"); err != nil {
		t.Fatalf("send-keys Enter: %v", err)
	}

	var got strings.Builder
	deadline := time.After(15 * time.Second)
	for !strings.Contains(got.String(), marker) {
		select {
		case b, ok := <-tap:
			if !ok {
				t.Fatalf("pane tap closed; got %q", got.String())
			}
			got.Write(b)
		case <-deadline:
			t.Fatalf("timed out waiting for pane output; got %q", got.String())
		}
	}
}

// TestIntegrationControlPaneTextIsNotProtocol is G1 against real tmux.
//
// A reply body is not tmux speaking: it is whatever the command printed, and
// capture-pane prints whatever is on the pane. When every line was dispatched
// by its prefix, a pane containing "%output %999 ..." had those bytes
// delivered to whoever tapped %999, and a pane containing "%exit" ended the
// connection and reported a reason the pane had invented — with Err() and
// Wait() both nil, since %exit is a clean end.
func TestIntegrationControlPaneTextIsNotProtocol(t *testing.T) {
	cc, c := testControl(t)
	ctx := testCtx(t)

	reply, err := cc.DoArgs(ctx, "list-panes", "-F", "#{pane_id}")
	if err != nil || len(reply.Output) == 0 {
		t.Fatalf("list-panes = (%v, %v)", reply.Output, err)
	}
	pane := PaneID(reply.Output[0])

	// A tap on a pane that does not exist, so that the only bytes that could
	// ever arrive on it are forged ones.
	victim, err := cc.Output("%999")
	if err != nil {
		t.Fatalf("Output(%%999): %v", err)
	}

	// The pane runs cat, so a line sent to it is a line printed by it.
	injected := []string{
		"%hello-world",
		"%output %999 injected-bytes",
		"%exit forged",
		"%end 1700000000 99 1",
	}
	for _, line := range injected {
		if err := c.SendLine(ctx, pane, line); err != nil {
			t.Fatalf("SendLine(%q): %v", line, err)
		}
	}

	has := func(out []string, want string) bool {
		for _, line := range out {
			if line == want {
				return true
			}
		}
		return false
	}

	var capture Reply
	eventually(t, "the injected lines to reach the pane", 15*time.Second, func() bool {
		capture, err = cc.DoArgs(ctx, "capture-pane", "-p", "-t", string(pane))
		return err == nil && has(capture.Output, injected[len(injected)-1])
	})

	for _, line := range injected {
		if !has(capture.Output, line) {
			t.Errorf("the reply is missing %q, so it was read as protocol\nreply: %q",
				line, capture.Output)
		}
	}

	// The connection outlived the captured %exit and still answers.
	if err := cc.Err(); err != nil {
		t.Errorf("Err after capturing a pane containing %%exit: %v", err)
	}
	if _, err := cc.DoArgs(ctx, "display-message", "-p", "ok"); err != nil {
		t.Errorf("the connection did not survive the captured %%exit: %v", err)
	}

	select {
	case b := <-victim:
		t.Errorf("pane %%999 was handed %q, which was another pane's text", b)
	default:
	}
	if n := cc.DroppedOutput("%999"); n != 0 {
		t.Errorf("DroppedOutput(%%999) = %d, want 0", n)
	}
}

// TestIntegrationControlWindowNotifications is the check the notification
// table never had: its names asserted against tmux instead of against itself.
//
// The unit suite feeds the reader lines the tests themselves spell, so a name
// spelled wrong in the table is spelled the same wrong way in the test and the
// two agree forever. That is how the reader came to watch for
// "%unlinked-window-rename" — tmux writes "%unlinked-window-renamed" — and for
// "%linked-window-add" and "%linked-window-close", which tmux has never
// written at all. Here every line comes from tmux.
//
// Each of the three window notifications has two spellings, and which one
// arrives is a fact about the receiving client rather than about the window:
// tmux writes the plain form for a window the client's own session has a link
// to and the "unlinked-" form for any other. So each operation is done twice,
// once in the session this connection is attached to and once in a session it
// is not, and both are required to produce the same event.
func TestIntegrationControlWindowNotifications(t *testing.T) {
	cc, c := testControl(t)
	ctx := testCtx(t)

	attached := cc.AttachedSession()
	if !attached.Valid() {
		t.Fatalf("AttachedSession = %q, want the session testControl attached to", attached)
	}

	// A second session, whose windows this connection has no link to.
	other, err := c.NewSession(ctx, NewSessionOptions{
		Name:    "unlinked",
		Width:   80,
		Height:  24,
		Command: []string{"cat"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	var (
		mu     sync.Mutex
		events []Event
	)
	go func() {
		for ev := range cc.Events() {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
	}()
	seen := func(match func(Event) bool) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if match(ev) {
				return true
			}
		}
		return false
	}

	// newWindow makes a window in one session and returns its identifier. -P
	// -F is what turns the command into a question; without it the reply is
	// empty and the identifier would have to be guessed from a listing.
	newWindow := func(s SessionID) WindowID {
		t.Helper()
		reply, err := cc.DoArgs(ctx, "new-window", "-d", "-P", "-F", "#{window_id}", "-t", string(s))
		if err != nil || len(reply.Output) != 1 {
			t.Fatalf("new-window -t %s = (%q, %v)", s, reply.Output, err)
		}
		w := WindowID(strings.TrimSpace(reply.Output[0]))
		if !w.Valid() {
			t.Fatalf("new-window printed %q, which is not a window id", reply.Output[0])
		}
		return w
	}

	for _, tc := range []struct {
		what    string
		session SessionID
		name    string
	}{
		{"linked", attached, "linked-rename"},
		{"unlinked", other.ID, "unlinked-rename"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			w := newWindow(tc.session)
			eventually(t, "a WindowAdded for "+string(w), 15*time.Second, func() bool {
				return seen(func(ev Event) bool {
					e, ok := ev.(WindowAdded)
					return ok && e.Window == w
				})
			})

			if _, err := cc.DoArgs(ctx, "rename-window", "-t", string(w), tc.name); err != nil {
				t.Fatalf("rename-window: %v", err)
			}
			eventually(t, "a WindowRenamed for "+string(w), 15*time.Second, func() bool {
				return seen(func(ev Event) bool {
					e, ok := ev.(WindowRenamed)
					return ok && e.Window == w && e.Name == tc.name
				})
			})

			if _, err := cc.DoArgs(ctx, "kill-window", "-t", string(w)); err != nil {
				t.Fatalf("kill-window: %v", err)
			}
			eventually(t, "a WindowClosed for "+string(w), 15*time.Second, func() bool {
				return seen(func(ev Event) bool {
					e, ok := ev.(WindowClosed)
					return ok && e.Window == w
				})
			})
		})
	}

	// The other half of the same question. Everything above says the names the
	// table knows are tmux's; this says the table knows all of the names tmux
	// used. An UnknownNotification here is either a name spelled wrong, as
	// "unlinked-window-rename" was, or one a newer tmux has added — and both
	// are worth being told about, since only a name in the table gets an
	// event of its own.
	mu.Lock()
	for _, ev := range events {
		if u, ok := ev.(UnknownNotification); ok {
			t.Errorf("tmux sent %%%s, which the notification table does not know: %q",
				u.Name, u.Args)
		}
		if pe, ok := ev.(*ProtocolError); ok {
			t.Errorf("protocol error during window operations: %v", pe)
		}
	}
	mu.Unlock()
}

func TestIntegrationControlSetSize(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	if err := cc.SetSize(ctx, 132, 43); err != nil {
		t.Fatalf("SetSize: %v", err)
	}

	eventually(t, "the window to take the client's size", 10*time.Second, func() bool {
		reply, err := cc.DoArgs(ctx, "list-windows", "-F", "#{window_width}x#{window_height}")
		return err == nil && len(reply.Output) > 0 && reply.Output[0] == "132x43"
	})
}

func TestIntegrationControlSubscribe(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	if err := cc.Subscribe(ctx, "panetitle", SubscribeAllPanes, "#{pane_title}"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Provoke a change so a value is certain to be reported.
	go func() {
		for i := 0; i < 10; i++ {
			_, _ = cc.DoArgs(ctx, "select-pane", "-T", fmt.Sprintf("title-%d", i))
			time.Sleep(500 * time.Millisecond)
		}
	}()

	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-cc.Events():
			if !ok {
				t.Fatal("event channel closed while waiting for a subscription value")
			}
			sub, isSub := ev.(SubscriptionChanged)
			if !isSub {
				continue
			}
			if sub.Name != "panetitle" {
				t.Errorf("subscription name = %q, want %q", sub.Name, "panetitle")
			}
			t.Logf("subscription: %+v", sub)
			if sub.Pane != "" && !sub.Pane.Valid() {
				t.Errorf("subscription pane %q is malformed", sub.Pane)
			}
			return
		case <-deadline:
			t.Fatal("no SubscriptionChanged event arrived")
		}
	}
}

// TestIntegrationControlFlowControl checks that enabling flow control is
// accepted and that tmux then switches to %extended-output, which is the
// observable consequence a parser has to cope with.
//
// Provoking an actual %pause is not attempted: the library's reader consumes
// the pipe as fast as tmux fills it, so it never falls far enough behind. The
// pause and resume paths themselves are covered by the unit tests.
func TestIntegrationControlFlowControl(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	if err := cc.PauseAfter(ctx, 2*time.Second); err != nil {
		t.Fatalf("PauseAfter: %v", err)
	}

	reply, err := cc.DoArgs(ctx, "list-panes", "-F", "#{pane_id}")
	if err != nil || len(reply.Output) == 0 {
		t.Fatalf("list-panes = (%v, %v)", reply.Output, err)
	}
	pane := PaneID(reply.Output[0])

	// Resume must be accepted whether or not the pane is currently paused.
	if err := cc.Resume(ctx, pane); err != nil {
		t.Errorf("Resume: %v", err)
	}

	if _, err := cc.DoArgs(ctx, "send-keys", "-t", string(pane), "-l", "extended-output-probe"); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	if _, err := cc.DoArgs(ctx, "send-keys", "-t", string(pane), "Enter"); err != nil {
		t.Fatalf("send-keys Enter: %v", err)
	}

	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-cc.Events():
			if !ok {
				t.Fatal("event channel closed before any pane output")
			}
			out, isOutput := ev.(PaneOutput)
			if !isOutput {
				continue
			}
			if !out.Extended {
				t.Errorf("with flow control enabled, output arrived as plain %%output: %+v", out)
			}
			t.Logf("extended output: pane %s age %s, %d bytes", out.Pane, out.Age, len(out.Data))
			return
		case <-deadline:
			t.Fatal("no pane output arrived")
		}
	}
}

func TestIntegrationControlCloseIsClean(t *testing.T) {
	opts := testOptions(t)
	c := New(opts...)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "closer", Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	cc, err := Connect(ctx, append(opts, WithAttach(s.ID))...)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if err := cc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Idempotent.
	if err := cc.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	select {
	case <-cc.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the connection did not finish after Close")
	}

	// Detaching a control client must not take the session with it.
	has, err := c.HasSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Error("closing the control connection destroyed the session")
	}

	if _, err := cc.Do(ctx, "list-sessions"); err == nil {
		t.Error("Do succeeded after Close")
	}
}

func TestIntegrationControlAttachMissingSessionFails(t *testing.T) {
	opts := testOptions(t)
	requireTmux(t)

	// Attaching to a session that does not exist must fail at Connect rather
	// than yield a connection that never works.
	cc, err := Connect(testCtx(t), append(opts, WithAttach("$999"))...)
	if err == nil {
		cc.Close()
		t.Fatal("Connect succeeded against a session that does not exist")
	}
	t.Logf("Connect failed as expected: %v", err)
}

// TestIntegrationNameIsNotAnAddress is the identifier rule against a tmux that
// really would honour a name. The session is named so that tmux could resolve
// it, and the package must still refuse to ask.
func TestIntegrationNameIsNotAnAddress(t *testing.T) {
	c, opts := testClient(t)
	ctx := testCtx(t)

	s, err := c.NewSession(ctx, NewSessionOptions{Name: "work", Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// tmux itself resolves the name; that is what makes the refusal necessary
	// rather than pedantic.
	if _, _, err := c.run(ctx, "has-session", "-t", "work"); err != nil {
		t.Fatalf("tmux did not resolve the session name, so this test proves nothing: %v", err)
	}

	if err := c.KillSession(ctx, SessionID("work")); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("KillSession by name = %v, want an error wrapping ErrInvalidID", err)
	}
	has, err := c.HasSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("the session was killed by name")
	}

	// WithAttach cannot report anything itself, so Connect checks it.
	cc, err := Connect(ctx, append(opts, WithAttach("work"))...)
	if err == nil {
		cc.Close()
		t.Fatal("Connect attached to a session by name")
	}
	if !errors.Is(err, ErrInvalidID) {
		t.Errorf("Connect(WithAttach(\"work\")) = %v, want an error wrapping ErrInvalidID", err)
	}

	// The identifier still works, which is the other half of the claim.
	if err := c.KillSession(ctx, s.ID); err != nil {
		t.Errorf("KillSession by id: %v", err)
	}
}

// TestIntegrationControlRefusesCommandBlocks is G6 against real tmux: a brace
// block earns a second reply block, so the connection would be one reply out
// of step from then on. The command must be refused, and the connection must
// still be in step afterwards.
func TestIntegrationControlRefusesCommandBlocks(t *testing.T) {
	cc, _ := testControl(t)
	ctx := testCtx(t)

	for _, cmd := range []string{
		`if-shell "true" { list-sessions }`,
		`if-shell "true" {list-sessions}`,
		`list-sessions ; list-windows`,
	} {
		if _, err := cc.Do(ctx, cmd); err == nil {
			t.Errorf("Do(%q) was accepted", cmd)
		}
	}

	// Still in step: this reply is this command's, not a leftover.
	reply, err := cc.Do(ctx, "display-message -p in-step")
	if err != nil {
		t.Fatalf("Do after the refusals: %v (stderr: %s)", err, cc.Stderr())
	}
	if reply.String() != "in-step" {
		t.Errorf("reply = %q, want %q", reply.String(), "in-step")
	}
}
