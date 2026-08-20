package gotmucks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/counterflow/gotmucks/internal/faketmux"
)

func TestSocketFlags(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want []string
	}{
		{"default socket", nil, nil},
		{"socket name", []Option{WithSocketName("proj")}, []string{"-L", "proj"}},
		{"socket path", []Option{WithSocketPath("/tmp/s")}, []string{"-S", "/tmp/s"}},
		{
			// The explicit path is the more specific statement, so it wins.
			name: "path beats name",
			opts: []Option{WithSocketName("proj"), WithSocketPath("/tmp/s")},
			want: []string{"-S", "/tmp/s"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{})
			c := f.client(tt.opts...)

			if _, err := c.ServerRunning(context.Background()); err != nil {
				t.Fatalf("ServerRunning: %v", err)
			}
			got := f.onlyArgv()

			want := append(append([]string(nil), tt.want...), "list-sessions", "-F", "#{session_id}")
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("argv\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	f := newFake(t, faketmux.Script{Version: "tmux 3.2a"})
	c := f.client()

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Major != 3 || v.Minor != 2 || v.Suffix != "a" {
		t.Errorf("got %+v, want 3.2a", v)
	}
	f.wantArgv(0, "-V")

	if err := c.CheckVersion(context.Background()); err != nil {
		t.Errorf("CheckVersion on 3.2a: %v", err)
	}
}

func TestCheckVersionTooOld(t *testing.T) {
	f := newFake(t, faketmux.Script{Version: "tmux 3.1c"})
	err := f.client().CheckVersion(context.Background())
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", err)
	}
}

func TestServerRunning(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		f := newFake(t, faketmux.Script{
			Responses: map[string]faketmux.Response{
				"list-sessions": {Stdout: "$0\n"},
			},
		})
		got, err := f.client().ServerRunning(context.Background())
		if err != nil || !got {
			t.Fatalf("got (%v, %v), want (true, nil)", got, err)
		}
	})

	// tmux exits 1 when there is no server, the same as for a real failure.
	// Telling the two apart is the whole point.
	t.Run("no server", func(t *testing.T) {
		f := newFake(t, faketmux.Script{NoServer: true})
		got, err := f.client().ServerRunning(context.Background())
		if err != nil {
			t.Fatalf("no server reported as an error: %v", err)
		}
		if got {
			t.Error("got true, want false")
		}
	})

	t.Run("real failure is still an error", func(t *testing.T) {
		f := newFake(t, faketmux.Script{
			Responses: map[string]faketmux.Response{
				"list-sessions": {Stderr: "something else went wrong\n", Exit: 1},
			},
		})
		if _, err := f.client().ServerRunning(context.Background()); err == nil {
			t.Fatal("a genuine failure was swallowed")
		}
	})
}

// TestNoServerIsNotAnError walks every read path with no server running. All
// of them must report emptiness rather than failure.
func TestNoServerIsNotAnError(t *testing.T) {
	ctx := context.Background()

	checks := []struct {
		name string
		run  func(*Client) error
	}{
		{"ListSessions", func(c *Client) error {
			s, err := c.ListSessions(ctx)
			if err == nil && len(s) != 0 {
				t.Errorf("ListSessions returned %d sessions", len(s))
			}
			return err
		}},
		{"ListWindows", func(c *Client) error {
			w, err := c.ListWindows(ctx)
			if err == nil && len(w) != 0 {
				t.Errorf("ListWindows returned %d windows", len(w))
			}
			return err
		}},
		{"ListPanes", func(c *Client) error {
			p, err := c.ListPanes(ctx)
			if err == nil && len(p) != 0 {
				t.Errorf("ListPanes returned %d panes", len(p))
			}
			return err
		}},
		{"HasSession", func(c *Client) error {
			ok, err := c.HasSession(ctx, "$0")
			if err == nil && ok {
				t.Error("HasSession returned true with no server")
			}
			return err
		}},
		{"ServerRunning", func(c *Client) error {
			ok, err := c.ServerRunning(ctx)
			if err == nil && ok {
				t.Error("ServerRunning returned true with no server")
			}
			return err
		}},
		{"KillSession", func(c *Client) error { return c.KillSession(ctx, "$0") }},
		{"KillServer", func(c *Client) error { return c.KillServer(ctx) }},
		{"ShowOptions", func(c *Client) error {
			_, err := c.ShowOptions(ctx, Global, ScopeGlobal)
			return err
		}},
		{"ShowHooks", func(c *Client) error {
			_, err := c.ShowGlobalHooks(ctx)
			return err
		}},
		{"Query", func(c *Client) error {
			rows, err := c.Query(ctx, "list-sessions", FormatSpec{"session_id"})
			if err == nil && len(rows) != 0 {
				t.Errorf("Query returned %d rows", len(rows))
			}
			return err
		}},
	}

	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{NoServer: true})
			if err := check.run(f.client()); err != nil {
				t.Errorf("no server reported as an error: %v", err)
			}
		})
	}
}

// TestWriteRequiringServerFails is the other half of the rule: a write that
// cannot happen without a server must not be silently swallowed.
func TestWriteRequiringServerFails(t *testing.T) {
	f := newFake(t, faketmux.Script{NoServer: true})
	err := f.client().SendKeys(context.Background(), "%0", Literal("x"))
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("got %v, want an error wrapping ErrNoServer", err)
	}
}

func TestListSessions(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"list-sessions": {Stdout: tabbed(
				[]string{"$0", "main", "2", "1712345678", "1712345690", "1"},
				[]string{"$1", "build", "1", "1712345700", "1712345700", "0"},
			)},
		},
	})

	got, err := f.client().ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}

	want := Session{
		ID: "$0", Name: "main", Windows: 2,
		Created:  time.Unix(1712345678, 0),
		Activity: time.Unix(1712345690, 0),
		Attached: 1,
	}
	if got[0] != want {
		t.Errorf("session 0\n got %+v\nwant %+v", got[0], want)
	}
	if !got[0].IsAttached() {
		t.Error("session 0 should report as attached")
	}
	if got[1].IsAttached() {
		t.Error("session 1 should not report as attached")
	}

	f.wantArgv(0, "list-sessions", "-F", sessionSpec.Arg())
}

func TestListSessionsEmpty(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{"list-sessions": {Stdout: ""}},
	})
	got, err := f.client().ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d sessions, want 0", len(got))
	}
}

func TestNewSessionArgv(t *testing.T) {
	tests := []struct {
		name string
		opts NewSessionOptions
		want []string
	}{
		{
			name: "minimal",
			opts: NewSessionOptions{},
			want: []string{"new-session", "-d", "-P", "-F", "#{session_id}"},
		},
		{
			name: "named",
			opts: NewSessionOptions{Name: "build"},
			want: []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", "build"},
		},
		{
			name: "full",
			opts: NewSessionOptions{
				Name:       "build",
				WindowName: "main",
				StartDir:   "/src",
				Width:      132,
				Height:     43,
			},
			want: []string{
				"new-session", "-d", "-P", "-F", "#{session_id}",
				"-s", "build", "-n", "main", "-c", "/src", "-x", "132", "-y", "43",
			},
		},
		{
			// Sorted so the vector is deterministic and therefore assertable.
			name: "environment is sorted",
			opts: NewSessionOptions{Env: map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"}},
			want: []string{
				"new-session", "-d", "-P", "-F", "#{session_id}",
				"-e", "ALPHA=2", "-e", "MID=3", "-e", "ZED=1",
			},
		},
		{
			// The command goes after "--" as separate arguments, so tmux
			// executes it directly and metacharacters in it are inert.
			name: "command after double dash",
			opts: NewSessionOptions{Command: []string{"sh", "-c", "echo $HOME; rm -rf /"}},
			want: []string{
				"new-session", "-d", "-P", "-F", "#{session_id}",
				"--", "sh", "-c", "echo $HOME; rm -rf /",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{
				Responses: map[string]faketmux.Response{
					"new-session":   {Stdout: "$7\n"},
					"list-sessions": {Stdout: tabbed([]string{"$7", "build", "1", "1", "1", "0"})},
				},
			})

			s, err := f.client().NewSession(context.Background(), tt.opts)
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			if s.ID != "$7" {
				t.Errorf("session id = %q, want $7", s.ID)
			}
			f.wantArgv(0, tt.want...)
		})
	}
}

func TestNewSessionRejectsBadEnv(t *testing.T) {
	f := newFake(t, faketmux.Script{})
	_, err := f.client().NewSession(context.Background(), NewSessionOptions{
		Env: map[string]string{"BAD=NAME": "x"},
	})
	if err == nil {
		t.Fatal("an environment name containing '=' was accepted")
	}
	if len(f.argv()) != 0 {
		t.Error("tmux was invoked despite invalid input")
	}
}

// TestNewSessionCommandSafety pins the one place tmux will not keep this
// package's promise that a command vector is executed directly. Verified
// against tmux 3.2a by scripts/probe-argv.sh: two or more arguments go to
// execvp, but a lone argument goes to the shell.
func TestNewSessionCommandSafety(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		wantErr bool
	}{
		{"bare word", []string{"htop"}, false},
		{"absolute path", []string{"/usr/bin/htop"}, false},
		{"explicit shell", []string{"sh", "-c", "make && ./run"}, false},
		{"multi element with metacharacters", []string{"printf", "one;two three"}, false},

		{"lone argument with a semicolon", []string{"touch x; touch y"}, true},
		{"lone argument with a space", []string{"echo hi"}, true},
		{"lone argument with a pipe", []string{"a|b"}, true},
		{"lone argument with backticks", []string{"a`b`"}, true},
		{"empty element", []string{"sh", ""}, true},
		{"newline in an element", []string{"sh", "-c", "a\nb"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{
				Responses: map[string]faketmux.Response{
					"new-session":   {Stdout: "$0\n"},
					"list-sessions": {Stdout: tabbed([]string{"$0", "s", "1", "1", "1", "0"})},
				},
			})
			_, err := f.client().NewSession(context.Background(), NewSessionOptions{Command: tt.command})

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Command %q was accepted; the shell would interpret it", tt.command)
				}
				if len(f.argv()) != 0 {
					t.Error("tmux was invoked despite invalid input")
				}
				return
			}
			if err != nil {
				t.Fatalf("Command %q was rejected: %v", tt.command, err)
			}
		})
	}
}

func TestHasSession(t *testing.T) {
	tests := []struct {
		name string
		resp faketmux.Response
		want bool
	}{
		{"present", faketmux.Response{}, true},
		{"absent", faketmux.Response{Stderr: "can't find session: $9\n", Exit: 1}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{
				Responses: map[string]faketmux.Response{"has-session": tt.resp},
			})
			got, err := f.client().HasSession(context.Background(), "$9")
			if err != nil {
				t.Fatalf("HasSession: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
			f.wantArgv(0, "has-session", "-t", "$9")
		})
	}
}

func TestKillSessionIsIdempotent(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"kill-session": {Stderr: "can't find session: $4\n", Exit: 1},
		},
	})
	if err := f.client().KillSession(context.Background(), "$4"); err != nil {
		t.Fatalf("killing an absent session should succeed, got %v", err)
	}
}

func TestRenameSession(t *testing.T) {
	f := newFake(t, faketmux.Script{})
	if err := f.client().RenameSession(context.Background(), "$0", "renamed"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	f.wantArgv(0, "rename-session", "-t", "$0", "renamed")
}

func TestListPanes(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"list-panes": {Stdout: tabbed(
				[]string{"%0", "@0", "$0", "0", "1", "0", "4242", "80", "24", "bash", "shell", "/home/u"},
				[]string{"%1", "@0", "$0", "1", "0", "1", "4243", "80", "24", "vim", "editor", "/src"},
			)},
		},
	})

	got, err := f.client().ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d panes, want 2", len(got))
	}

	want := Pane{
		ID: "%0", Window: "@0", Session: "$0", Index: 0,
		Active: true, Dead: false, PID: 4242, Width: 80, Height: 24,
		CurrentCommand: "bash", CurrentPath: "/home/u", Title: "shell",
	}
	if got[0] != want {
		t.Errorf("pane 0\n got %+v\nwant %+v", got[0], want)
	}
	if !got[1].Dead {
		t.Error("pane 1 should be dead")
	}
	f.wantArgv(0, "list-panes", "-a", "-F", paneSpec.Arg())
}

func TestListWindows(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"list-windows": {Stdout: tabbed(
				[]string{"@0", "$0", "0", "editor", "1", "2", "80", "24", "bb62,80x24,0,0,0"},
			)},
		},
	})

	got, err := f.client().ListWindows(context.Background())
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	want := Window{
		ID: "@0", Session: "$0", Index: 0, Name: "editor",
		Active: true, Panes: 2, Width: 80, Height: 24,
		Layout: "bb62,80x24,0,0,0",
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %+v, want [%+v]", got, want)
	}
	f.wantArgv(0, "list-windows", "-a", "-F", windowSpec.Arg())
}

func TestCapturePaneArgv(t *testing.T) {
	tests := []struct {
		name string
		opts CaptureOptions
		want []string
	}{
		{"plain", CaptureOptions{}, []string{"capture-pane", "-p", "-t", "%2"}},
		{"escapes", CaptureOptions{Escapes: true}, []string{"capture-pane", "-p", "-e", "-t", "%2"}},
		{
			name: "join and trailing",
			opts: CaptureOptions{Join: true, PreserveTrailingSpaces: true},
			want: []string{"capture-pane", "-p", "-J", "-N", "-t", "%2"},
		},
		{
			name: "range",
			opts: CaptureOptions{Start: Line(-10), End: Line(5)},
			want: []string{"capture-pane", "-p", "-S", "-10", "-E", "5", "-t", "%2"},
		},
		{
			name: "full history overrides start",
			opts: CaptureOptions{FullHistory: true, Start: Line(3)},
			want: []string{"capture-pane", "-p", "-S", "-", "-t", "%2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{
				Responses: map[string]faketmux.Response{
					"capture-pane": {Stdout: "line one\nline two\n"},
				},
			})
			out, err := f.client().CapturePane(context.Background(), "%2", tt.opts)
			if err != nil {
				t.Fatalf("CapturePane: %v", err)
			}
			if string(out) != "line one\nline two\n" {
				t.Errorf("capture = %q", out)
			}
			f.wantArgv(0, tt.want...)
		})
	}
}

// TestCapturePaneKeepsRawBytes checks that a capture is handed back
// untouched: with escapes included it is not text, and trimming or decoding
// it would corrupt it.
func TestCapturePaneKeepsRawBytes(t *testing.T) {
	raw := "\x1b[1;31mred\x1b[0m\n\n  trailing  \n"
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{"capture-pane": {Stdout: raw}},
	})
	out, err := f.client().CapturePane(context.Background(), "%0", CaptureOptions{Escapes: true})
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if string(out) != raw {
		t.Errorf("capture altered\n got %q\nwant %q", out, raw)
	}
}

func TestSendKeysBatching(t *testing.T) {
	tests := []struct {
		name string
		keys []Key
		want [][]string
	}{
		{
			name: "named only",
			keys: []Key{Named("C-c"), Enter()},
			want: [][]string{{"send-keys", "-t", "%0", "--", "C-c", "Enter"}},
		},
		{
			name: "literal only",
			keys: []Key{Literal("echo hi")},
			want: [][]string{{"send-keys", "-l", "-t", "%0", "--", "echo hi"}},
		},
		{
			name: "hex only",
			keys: []Key{Hex(0x1b, 0x5b, 0x41)},
			want: [][]string{{"send-keys", "-H", "-t", "%0", "--", "1b", "5b", "41"}},
		},
		{
			// -l and -H are flags on the invocation, not properties of a key,
			// so a mixed sequence has to be split into runs.
			name: "mixed splits into runs",
			keys: []Key{Literal("echo hi"), Enter(), Hex(0x03)},
			want: [][]string{
				{"send-keys", "-l", "-t", "%0", "--", "echo hi"},
				{"send-keys", "-t", "%0", "--", "Enter"},
				{"send-keys", "-H", "-t", "%0", "--", "03"},
			},
		},
		{
			name: "consecutive same kind coalesce",
			keys: []Key{Literal("a"), Literal("b"), Named("Enter"), Named("Escape")},
			want: [][]string{
				{"send-keys", "-l", "-t", "%0", "--", "a", "b"},
				{"send-keys", "-t", "%0", "--", "Enter", "Escape"},
			},
		},
		{
			// A literal beginning with a dash must not be read as a flag.
			name: "leading dash is not a flag",
			keys: []Key{Literal("--force")},
			want: [][]string{{"send-keys", "-l", "-t", "%0", "--", "--force"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{})
			if err := f.client().SendKeys(context.Background(), "%0", tt.keys...); err != nil {
				t.Fatalf("SendKeys: %v", err)
			}
			got := f.argv()
			if len(got) != len(tt.want) {
				t.Fatalf("got %d invocations, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				f.wantArgv(i, tt.want[i]...)
			}
		})
	}
}

func TestSendKeysRejectsEmpty(t *testing.T) {
	f := newFake(t, faketmux.Script{})
	if err := f.client().SendKeys(context.Background(), "%0", Literal("")); err == nil {
		t.Fatal("an empty literal was accepted")
	}
	if len(f.argv()) != 0 {
		t.Error("tmux was invoked despite invalid input")
	}
}

func TestSendLine(t *testing.T) {
	f := newFake(t, faketmux.Script{})
	if err := f.client().SendLine(context.Background(), "%0", "go test ./..."); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	f.wantArgv(0, "send-keys", "-l", "-t", "%0", "--", "go test ./...")
	f.wantArgv(1, "send-keys", "-t", "%0", "--", "Enter")
}

func TestSetOptionArgv(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Client) error
		want []string
	}{
		{
			name: "session option",
			run:  func(c *Client) error { return c.SetOption(context.Background(), SessionID("$0"), "status", "off") },
			want: []string{"set-option", "-t", "$0", "--", "status", "off"},
		},
		{
			name: "global has no target",
			run: func(c *Client) error {
				return c.SetOptionScoped(context.Background(), Global, ScopeGlobal, "history-limit", "10000")
			},
			want: []string{"set-option", "-g", "--", "history-limit", "10000"},
		},
		{
			name: "window scope",
			run: func(c *Client) error {
				return c.SetOptionScoped(context.Background(), WindowID("@1"), ScopeWindow, "automatic-rename", "off")
			},
			want: []string{"set-option", "-w", "-t", "@1", "--", "automatic-rename", "off"},
		},
		{
			name: "unset",
			run: func(c *Client) error {
				return c.UnsetOption(context.Background(), SessionID("$0"), ScopeSession, "status")
			},
			want: []string{"set-option", "-u", "-t", "$0", "--", "status"},
		},
		{
			// remain-on-exit is a pane option for a pane and a window option
			// otherwise; getting that wrong makes the call quietly do nothing.
			name: "remain-on-exit on a pane",
			run:  func(c *Client) error { return c.SetRemainOnExit(context.Background(), PaneID("%3"), true) },
			want: []string{"set-option", "-p", "-t", "%3", "--", "remain-on-exit", "on"},
		},
		{
			name: "remain-on-exit on a window",
			run:  func(c *Client) error { return c.SetRemainOnExit(context.Background(), WindowID("@3"), false) },
			want: []string{"set-option", "-w", "-t", "@3", "--", "remain-on-exit", "off"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{})
			if err := tt.run(f.client()); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			f.wantArgv(0, tt.want...)
		})
	}
}

func TestShowOptions(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"show-options": {Stdout: "history-limit 2000\nstatus off\nstatus-left \"[#S] \"\nflagonly\n" +
				"tabbed has\\ttab\nlined a\\nb\nslashed a\\\\b\nocted a\\033b\nquoted 'a\"b'\n"},
		},
	})
	got, err := f.client().ShowOptions(context.Background(), Global, ScopeGlobal)
	if err != nil {
		t.Fatalf("ShowOptions: %v", err)
	}
	// tmux escapes an option value with vis(3) whether or not it also quotes
	// it, so undoing the quotes alone is not enough. Verified against 3.2a,
	// which prints a value containing a tab unquoted as "has\ttab".
	want := map[string]string{
		"history-limit": "2000",
		"status":        "off",
		"status-left":   "[#S] ",
		"flagonly":      "",
		"tabbed":        "has\ttab",
		"lined":         "a\nb",
		"slashed":       `a\b`,
		"octed":         "a\x1bb",
		"quoted":        `a"b`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("option %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestShowOptionUnknownIsAbsent pins both spellings tmux has used for an
// option name it does not know. The first fixture here said "unknown option",
// which no tmux emits: it was written from the implementation rather than from
// the binary, and the branch it was checking could never fire. 3.2a says
// "invalid option".
func TestShowOptionUnknownIsAbsent(t *testing.T) {
	for _, stderr := range []string{"invalid option: nope\n", "unknown option: nope\n"} {
		t.Run(strings.Fields(stderr)[0], func(t *testing.T) {
			f := newFake(t, faketmux.Script{
				Responses: map[string]faketmux.Response{
					"show-options": {Stderr: stderr, Exit: 1},
				},
			})
			v, ok, err := f.client().ShowOption(context.Background(), Global, ScopeGlobal, "nope")
			if err != nil {
				t.Fatalf("ShowOption: %v", err)
			}
			if ok || v != "" {
				t.Errorf("got (%q, %v), want (\"\", false)", v, ok)
			}
		})
	}
}

// TestShowOptionUnsetIsAbsent is the case show-options -v cannot answer: an
// option that is not set in the requested table exits 0 and prints nothing,
// which -v renders as an empty value indistinguishable from one set to "".
func TestShowOptionUnsetIsAbsent(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"show-options": {Stdout: ""},
		},
	})
	v, ok, err := f.client().ShowOption(context.Background(), Global, ScopeGlobal, "status-left")
	if err != nil {
		t.Fatalf("ShowOption: %v", err)
	}
	if ok || v != "" {
		t.Errorf("got (%q, %v), want (\"\", false) for an option that is not set", v, ok)
	}
	// -v would defeat the whole point of the bool, so its absence is pinned.
	f.wantArgv(0, "show-options", "-g", "--", "status-left")
}

func TestShowOptionValue(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"bare", "status-left value\n", "value"},
		{"quoted", "status-left \"[#S] \"\n", "[#S] "},
		{"set to the empty string", "status-left ''\n", ""},
		{"escaped tab", "status-left has\\ttab\n", "has\ttab"},
		{"escaped newline", "status-left a\\nb\n", "a\nb"},
		{"flag option with no value", "focus-events\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{
				Responses: map[string]faketmux.Response{
					"show-options": {Stdout: tt.stdout},
				},
			})
			v, ok, err := f.client().ShowOption(context.Background(), Global, ScopeGlobal, "status-left")
			if err != nil {
				t.Fatalf("ShowOption: %v", err)
			}
			if !ok {
				t.Error("an option tmux printed should be reported as set")
			}
			if v != tt.want {
				t.Errorf("value = %q, want %q", v, tt.want)
			}
		})
	}
}

// TestShowOptionArrayIsRefused: show-options prints one "name[index] value"
// line per element for an array option, and reading the first line as the
// whole answer returned element 0 with ok true and no hint that there was
// anything else. The fixture is 3.2a's own output for status-format.
func TestShowOptionArrayIsRefused(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"show-options": {Stdout: "status-format[0] \"#[align=left]\"\nstatus-format[1] \"#[align=centre]\"\n"},
		},
	})
	v, ok, err := f.client().ShowOption(context.Background(), Global, ScopeGlobal, "status-format")
	if err == nil {
		t.Fatalf("got (%q, %v, nil), want an error naming the array", v, ok)
	}
	if ok || v != "" {
		t.Errorf("got (%q, %v) beside the error, want (\"\", false)", v, ok)
	}
	if !strings.Contains(err.Error(), "2 elements") {
		t.Errorf("error does not say how many elements there were: %v", err)
	}
}

// TestIsArrayElement pins what separates an indexed name from an ordinary one,
// since the value of a plain option may well end in a bracket.
func TestIsArrayElement(t *testing.T) {
	tests := []struct {
		printed, name string
		want          bool
	}{
		{"status-format[0]", "status-format", true},
		{"command-alias[10]", "command-alias", true},
		{"status-format", "status-format", false},
		{"status-format[]", "status-format", false},
		{"status-format[x]", "status-format", false},
		{"status-format[0", "status-format", false},
		{"@user[0]", "@user", true},
		{"other[0]", "status-format", false},
	}
	for _, tt := range tests {
		if got := isArrayElement(tt.printed, tt.name); got != tt.want {
			t.Errorf("isArrayElement(%q, %q) = %v, want %v", tt.printed, tt.name, got, tt.want)
		}
	}
}

func TestSetHookArgv(t *testing.T) {
	f := newFake(t, faketmux.Script{})
	c := f.client()
	ctx := context.Background()

	if err := c.SetHook(ctx, SessionID("$0"), "pane-exited", "run-shell 'echo gone'"); err != nil {
		t.Fatalf("SetHook: %v", err)
	}
	f.wantArgv(0, "set-hook", "-t", "$0", "--", "pane-exited", "run-shell 'echo gone'")

	if err := c.SetGlobalHook(ctx, "session-created", "display-message hi"); err != nil {
		t.Fatalf("SetGlobalHook: %v", err)
	}
	f.wantArgv(1, "set-hook", "-g", "--", "session-created", "display-message hi")

	if err := c.UnsetHook(ctx, SessionID("$0"), "pane-exited"); err != nil {
		t.Fatalf("UnsetHook: %v", err)
	}
	f.wantArgv(2, "set-hook", "-u", "-t", "$0", "--", "pane-exited")
}

func TestExitErrorMessage(t *testing.T) {
	f := newFake(t, faketmux.Script{
		Responses: map[string]faketmux.Response{
			"rename-session": {Stderr: "duplicate session: build\n", Exit: 1},
		},
	})
	err := f.client().RenameSession(context.Background(), "$0", "build")
	if err == nil {
		t.Fatal("expected an error")
	}

	var xerr *ExitError
	if !errors.As(err, &xerr) {
		t.Fatalf("got %T, want *ExitError", err)
	}
	if xerr.Code != 1 {
		t.Errorf("Code = %d, want 1", xerr.Code)
	}
	if xerr.Stderr != "duplicate session: build" {
		t.Errorf("Stderr = %q", xerr.Stderr)
	}
	// The message should name the command and repeat tmux's own words.
	if !strings.Contains(err.Error(), "rename-session") ||
		!strings.Contains(err.Error(), "duplicate session") {
		t.Errorf("unhelpful error message: %v", err)
	}
	// The test-harness exec prefix must not leak into user-facing errors.
	for _, a := range xerr.Args {
		if strings.HasPrefix(a, "-test.") {
			t.Errorf("harness argument %q leaked into ExitError.Args: %q", a, xerr.Args)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	f := newFake(t, faketmux.Script{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := f.client().RenameSession(ctx, "$0", "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestMissingBinary(t *testing.T) {
	c := New(WithBinary("/nonexistent/tmux"))
	_, err := c.Version(context.Background())
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if errors.Is(err, ErrNoServer) {
		t.Error("a missing binary was misreported as a missing server")
	}
}

// TestIdentifiersMustBeIdentifiers is this package's central claim, and the one
// the types alone cannot keep. SessionID, WindowID and PaneID are string types,
// so SessionID("work") compiles; tmux then resolves "work" as a name and acts
// on whichever session answers to it, which is exactly the failure addressing
// by identifier exists to prevent.
//
// So every call that puts one in a -t argument refuses a name, and refuses it
// before tmux is started: a command that ran cannot be taken back.
func TestIdentifiersMustBeIdentifiers(t *testing.T) {
	ctx := context.Background()

	calls := []struct {
		name string
		run  func(*Client) error
	}{
		{"Session", func(c *Client) error { _, err := c.Session(ctx, "work"); return err }},
		{"HasSession", func(c *Client) error { _, err := c.HasSession(ctx, "work"); return err }},
		{"KillSession", func(c *Client) error { return c.KillSession(ctx, "work") }},
		{"RenameSession", func(c *Client) error { return c.RenameSession(ctx, "work", "x") }},
		{"ListSessionWindows", func(c *Client) error { _, err := c.ListSessionWindows(ctx, "work"); return err }},
		{"ListSessionPanes", func(c *Client) error { _, err := c.ListSessionPanes(ctx, "work"); return err }},
		{"Window", func(c *Client) error { _, err := c.Window(ctx, "editor"); return err }},
		{"RenameWindow", func(c *Client) error { return c.RenameWindow(ctx, "editor", "x") }},
		{"ListWindowPanes", func(c *Client) error { _, err := c.ListWindowPanes(ctx, "editor"); return err }},
		{"Pane", func(c *Client) error { _, err := c.Pane(ctx, "bash"); return err }},
		{"CapturePane", func(c *Client) error {
			_, err := c.CapturePane(ctx, "bash", CaptureOptions{})
			return err
		}},
		{"SendKeys", func(c *Client) error { return c.SendKeys(ctx, "bash", Literal("hi")) }},
		{"SendText", func(c *Client) error { return c.SendText(ctx, "bash", "hi") }},
		// The empty string sends nothing, but it must not be the one way in.
		{"SendText empty", func(c *Client) error { return c.SendText(ctx, "bash", "") }},
		{"SendLine", func(c *Client) error { return c.SendLine(ctx, "bash", "hi") }},
		{"SetOption", func(c *Client) error {
			return c.SetOption(ctx, SessionID("work"), "status", "off")
		}},
		{"SetOptionScoped", func(c *Client) error {
			return c.SetOptionScoped(ctx, WindowID("editor"), ScopeWindow, "automatic-rename", "off")
		}},
		{"UnsetOption", func(c *Client) error {
			return c.UnsetOption(ctx, SessionID("work"), ScopeSession, "status")
		}},
		{"ShowOption", func(c *Client) error {
			_, _, err := c.ShowOption(ctx, SessionID("work"), ScopeSession, "status")
			return err
		}},
		{"ShowOptions", func(c *Client) error {
			_, err := c.ShowOptions(ctx, SessionID("work"), ScopeSession)
			return err
		}},
		{"SetRemainOnExit", func(c *Client) error { return c.SetRemainOnExit(ctx, PaneID("bash"), true) }},
		{"SetHook", func(c *Client) error {
			return c.SetHook(ctx, SessionID("work"), "pane-exited", "display-message gone")
		}},
		{"UnsetHook", func(c *Client) error { return c.UnsetHook(ctx, SessionID("work"), "pane-exited") }},
		{"ShowHooks", func(c *Client) error { _, err := c.ShowHooks(ctx, SessionID("work")); return err }},
	}

	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{})

			err := call.run(f.client())
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("got %v, want an error wrapping ErrInvalidID", err)
			}
			if argv := f.argv(); len(argv) != 0 {
				t.Errorf("tmux ran anyway: %v", argv)
			}
		})
	}
}

// The global target addresses no object, so it is the one Target with nothing
// to check. It must keep working.
func TestGlobalTargetIsAlwaysAddressable(t *testing.T) {
	f := newFake(t, faketmux.Script{})
	if err := f.client().SetOptionScoped(context.Background(), Global, ScopeGlobal, "status", "off"); err != nil {
		t.Fatalf("SetOptionScoped on Global: %v", err)
	}
	f.wantArgv(0, "set-option", "-g", "--", "status", "off")
}

// TestSplitLinesCountsAnEmptyLine is the other half of F3: the row for an
// object whose only column is empty is a line with nothing on it, and tmux
// writes that as a single newline. Testing emptiness after the trailing
// newline was taken off made that indistinguishable from no output at all.
func TestSplitLinesCountsAnEmptyLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"no output at all", "", nil},
		{"one empty line", "\n", []string{""}},
		{"one empty line, CRLF", "\r\n", []string{""}},
		{"two empty lines", "\n\n", []string{"", ""}},
		{"one value", "a\n", []string{"a"}},
		{"a value then an empty line", "a\n\n", []string{"a", ""}},
		{"an empty line between values", "a\n\nb\n", []string{"a", "", "b"}},
		{"no trailing newline", "a\nb", []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLines([]byte(tt.in))
			if len(got) != len(tt.want) {
				t.Fatalf("got %q (%d lines), want %q (%d)", got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestQueryKeepsRowsWithAnEmptyColumn drives the same thing through the path a
// caller takes. The row count is what a caller aligns against everything else
// it knows about the server, so a missing one is worse than an empty value.
func TestQueryKeepsRowsWithAnEmptyColumn(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   []string
	}{
		{"no sessions", "", nil},
		{"one session, empty value", "\n", []string{""}},
		{"two sessions, both empty", "\n\n", []string{"", ""}},
		{"an empty value among others", "a\n\nb\n", []string{"a", "", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, faketmux.Script{
				Responses: map[string]faketmux.Response{
					"list-sessions": {Stdout: tt.stdout},
				},
			})
			// An unset user option expands to the empty string on every
			// supported tmux, which is why the integration test uses one too.
			rows, err := f.client().Query(
				context.Background(), "list-sessions", FormatSpec{"@gotmucks_absent"})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(rows) != len(tt.want) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tt.want))
			}
			for i := range rows {
				if v, ok := rows[i].Lookup("@gotmucks_absent"); !ok || v != tt.want[i] {
					t.Errorf("row %d = (%q, %v), want %q", i, v, ok, tt.want[i])
				}
			}
		})
	}
}
