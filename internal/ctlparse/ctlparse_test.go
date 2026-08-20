package ctlparse

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKind  Kind
		wantName  string
		wantArgs  string
		wantNum   int
		wantTime  int64
		wantFlags int
		wantBad   bool
	}{
		{
			name: "begin", line: "%begin 1712345678 3 1",
			wantKind: KindBegin, wantTime: 1712345678, wantNum: 3, wantFlags: 1,
		},
		{
			name: "end", line: "%end 1712345678 3 1",
			wantKind: KindEnd, wantTime: 1712345678, wantNum: 3, wantFlags: 1,
		},
		{
			name: "error", line: "%error 1712345678 4 1",
			wantKind: KindError, wantTime: 1712345678, wantNum: 4, wantFlags: 1,
		},
		{
			name: "begin without flags", line: "%begin 1712345678 0",
			wantKind: KindBegin, wantTime: 1712345678, wantNum: 0, wantFlags: 0,
		},
		{
			name: "begin with command number zero", line: "%begin 100 0 1",
			wantKind: KindBegin, wantTime: 100, wantNum: 0, wantFlags: 1,
		},
		{
			name: "begin malformed", line: "%begin nope 1 1",
			wantKind: KindBegin, wantBad: true,
		},
		{
			name: "begin missing args", line: "%begin",
			wantKind: KindBegin, wantBad: true,
		},

		{
			name: "output", line: `%output %2 hello`,
			wantKind: KindNotification, wantName: "output", wantArgs: "%2 hello",
		},
		{
			name: "extended output", line: `%extended-output %2 15 : hello`,
			wantKind: KindNotification, wantName: "extended-output", wantArgs: "%2 15 : hello",
		},
		{
			name: "sessions changed", line: "%sessions-changed",
			wantKind: KindNotification, wantName: "sessions-changed", wantArgs: "",
		},
		{
			name: "exit with reason", line: "%exit server exited",
			wantKind: KindNotification, wantName: "exit", wantArgs: "server exited",
		},
		{
			name: "window add", line: "%window-add @3",
			wantKind: KindNotification, wantName: "window-add", wantArgs: "@3",
		},

		// The collision that makes prefix-only dispatch wrong: command output
		// can begin with '%' because that is what a pane id looks like.
		{name: "pane id is data", line: "%0", wantKind: KindData},
		{name: "pane id list is data", line: "%12", wantKind: KindData},
		{name: "percent alone is data", line: "%", wantKind: KindData},
		{name: "percent space is data", line: "% ", wantKind: KindData},
		{name: "format output is data", line: "%3\tbash\t1", wantKind: KindData},
		{name: "shell prompt is data", line: "$ echo hi", wantKind: KindData},
		{name: "empty is data", line: "", wantKind: KindData},
		{name: "percent digits then text is data", line: "%1 something", wantKind: KindData},

		// A notification this package predates should still be recognised as
		// a notification rather than mistaken for command output.
		{
			name: "unknown notification", line: "%some-future-thing a b c",
			wantKind: KindNotification, wantName: "some-future-thing", wantArgs: "a b c",
		},
		{name: "uppercase is not a notification", line: "%Output x", wantKind: KindData},
		{name: "underscore is not a notification", line: "%some_thing x", wantKind: KindData},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.line)
			if got.Kind != tt.wantKind {
				t.Fatalf("Kind = %v, want %v", got.Kind, tt.wantKind)
			}
			if got.Raw != tt.line {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.line)
			}
			if got.Malformed != tt.wantBad {
				t.Errorf("Malformed = %v, want %v", got.Malformed, tt.wantBad)
			}
			if tt.wantKind == KindNotification {
				if got.Name != tt.wantName {
					t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
				}
				if got.Args != tt.wantArgs {
					t.Errorf("Args = %q, want %q", got.Args, tt.wantArgs)
				}
			}
			if !tt.wantBad && (tt.wantKind == KindBegin || tt.wantKind == KindEnd || tt.wantKind == KindError) {
				if got.Number != tt.wantNum {
					t.Errorf("Number = %d, want %d", got.Number, tt.wantNum)
				}
				if got.Time != tt.wantTime {
					t.Errorf("Time = %d, want %d", got.Time, tt.wantTime)
				}
				if got.Flags != tt.wantFlags {
					t.Errorf("Flags = %d, want %d", got.Flags, tt.wantFlags)
				}
			}
		})
	}
}

func TestParseOutput(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantPane string
		wantData string
		wantOK   bool
	}{
		{"simple", `%2 hello`, "%2", "hello", true},
		{"escaped", `%0 $ ls\015\012`, "%0", `$ ls\015\012`, true},
		{"data with spaces", `%1 a b  c`, "%1", "a b  c", true},
		{"empty data", `%1 `, "%1", "", true},
		{"no data at all", `%1`, "%1", "", true},
		{"data starting with percent", `%1 %0`, "%1", "%0", true},
		{"large pane id", `%1234 x`, "%1234", "x", true},

		{"missing pane", ``, "", "", false},
		{"not a pane id", `@1 hello`, "", "", false},
		{"bare word", `foo bar`, "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseOutput(tt.args)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Pane != tt.wantPane || got.Data != tt.wantData {
				t.Errorf("got {%q, %q}, want {%q, %q}", got.Pane, got.Data, tt.wantPane, tt.wantData)
			}
			if got.Extended {
				t.Error("Extended set on a plain output notification")
			}
		})
	}
}

func TestParseExtendedOutput(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantPane string
		wantAge  int64
		wantData string
		wantOK   bool
	}{
		{"documented shape", `%2 150 : hello`, "%2", 150, "hello", true},
		{"zero age", `%0 0 : x`, "%0", 0, "x", true},
		{"extra reserved fields", `%0 12 34 56 : payload`, "%0", 12, "payload", true},
		{"data contains colon", `%0 5 : a : b`, "%0", 5, "a : b", true},
		{"escaped data", `%0 5 : line\012`, "%0", 5, `line\012`, true},
		{"no colon separator", `%0 5 payload`, "%0", 5, "payload", true},
		{"empty data after colon", `%0 5 : `, "%0", 5, "", true},

		{"not a pane", `@1 5 : x`, "", 0, "", false},
		{"missing age", `%0`, "", 0, "", false},
		{"age not a number", `%0 abc : x`, "", 0, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseExtendedOutput(tt.args)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Pane != tt.wantPane {
				t.Errorf("Pane = %q, want %q", got.Pane, tt.wantPane)
			}
			if got.AgeMS != tt.wantAge {
				t.Errorf("AgeMS = %d, want %d", got.AgeMS, tt.wantAge)
			}
			if got.Data != tt.wantData {
				t.Errorf("Data = %q, want %q", got.Data, tt.wantData)
			}
			if !got.Extended {
				t.Error("Extended not set on an extended-output notification")
			}
		})
	}
}

func TestParseSubscriptionChanged(t *testing.T) {
	tests := []struct {
		name string
		args string
		want Subscription
		ok   bool
	}{
		{
			name: "pane subscription",
			args: `title $0 @1 1 %2 : bash`,
			want: Subscription{Name: "title", Session: "$0", Window: "@1", WindowIndex: 1, Pane: "%2", Value: "bash"},
			ok:   true,
		},
		{
			name: "session only",
			args: `count $0 : 3`,
			want: Subscription{Name: "count", Session: "$0", WindowIndex: -1, Value: "3"},
			ok:   true,
		},
		{
			name: "value with spaces and colons",
			args: `path $0 @1 0 %0 : /home/x : y`,
			want: Subscription{Name: "path", Session: "$0", Window: "@1", WindowIndex: 0, Pane: "%0", Value: "/home/x : y"},
			ok:   true,
		},
		{
			name: "empty value",
			args: `empty $0 : `,
			want: Subscription{Name: "empty", Session: "$0", WindowIndex: -1, Value: ""},
			ok:   true,
		},
		{
			name: "no separator",
			args: `bare somevalue`,
			want: Subscription{Name: "bare", WindowIndex: -1, Value: "somevalue"},
			ok:   true,
		},
		{
			name: "name only",
			args: `justname`,
			want: Subscription{Name: "justname", WindowIndex: -1},
			ok:   true,
		},
		{name: "empty", args: ``, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseSubscriptionChanged(tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseLayoutChange(t *testing.T) {
	tests := []struct {
		name string
		args string
		want Layout
		ok   bool
	}{
		{
			name: "window and layout",
			args: "@0 bb62,80x23,0,0,0",
			want: Layout{Window: "@0", Layout: "bb62,80x23,0,0,0"},
			ok:   true,
		},
		{
			name: "with visible and flags",
			args: "@0 bb62,80x23,0,0,0 bb62,80x23,0,0,0 *",
			want: Layout{Window: "@0", Layout: "bb62,80x23,0,0,0", Visible: "bb62,80x23,0,0,0", Flags: "*"},
			ok:   true,
		},
		{name: "no layout", args: "@0", ok: false},
		{name: "not a window id", args: "$0 layout", ok: false},
		{name: "empty", args: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseLayoutChange(tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseIDAndName(t *testing.T) {
	tests := []struct {
		args     string
		wantID   string
		wantName string
		ok       bool
	}{
		{"$0 mysession", "$0", "mysession", true},
		{"@1 a window with spaces", "@1", "a window with spaces", true},
		{"$0", "$0", "", true},
		{"", "", "", false},
	}
	for _, tt := range tests {
		id, name, ok := ParseIDAndName(tt.args)
		if ok != tt.ok || id != tt.wantID || name != tt.wantName {
			t.Errorf("ParseIDAndName(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.args, id, name, ok, tt.wantID, tt.wantName, tt.ok)
		}
	}
}

func TestParseTwoIDs(t *testing.T) {
	tests := []struct {
		args   string
		a, b   string
		wantOK bool
	}{
		{"@1 %2", "@1", "%2", true},
		{"$0 @3", "$0", "@3", true},
		{"@1", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		a, b, ok := ParseTwoIDs(tt.args)
		if ok != tt.wantOK || a != tt.a || b != tt.b {
			t.Errorf("ParseTwoIDs(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.args, a, b, ok, tt.a, tt.b, tt.wantOK)
		}
	}
}

// TestNotificationNamesAreKnown guards the set that separates a notification
// from command output. Every name tmux sends must be in it, because one that
// is not loses the typed event tmux sent it for.
//
// What this test cannot do is say whether these are tmux's spellings, since
// the names here and the names in the map were written by the same hand: it
// asserts the table against itself, and did so happily while the reader
// watched for "unlinked-window-rename" and for two "linked-" names tmux has
// never emitted. scripts/probe-notify.sh is what asks tmux, by scanning a
// binary's format strings and a live server's stream and diffing both against
// the map, and TestIntegrationControlWindowNotifications is what asserts the
// window names end to end. What is kept here is coverage of the three
// spellings that pair up, so that a future edit cannot quietly drop one.
func TestNotificationNamesAreKnown(t *testing.T) {
	documented := []string{
		"output", "extended-output", "pause", "continue", "exit",
		"sessions-changed", "session-changed", "session-renamed",
		"session-window-changed", "window-add", "window-close",
		"window-renamed", "unlinked-window-add", "unlinked-window-close",
		"unlinked-window-renamed", "window-pane-changed", "pane-mode-changed",
		"layout-change", "subscription-changed", "client-session-changed",
		"client-detached", "message", "config-error",
		"paste-buffer-changed", "paste-buffer-deleted",
	}
	for _, name := range documented {
		if !IsKnownNotification(name) {
			t.Errorf("%%%s is not in the known notification set", name)
		}
		if got := Classify("%" + name + " args"); got.Kind != KindNotification {
			t.Errorf("Classify(%%%s ...) = %v, want notification", name, got.Kind)
		}
	}

	// Names tmux does not have. They were in the table for three review
	// rounds, matched by two arms of the reader's switch, and every test
	// passed the whole time — see control-notify.c, which pairs window-add
	// with unlinked-window-add and window-close with unlinked-window-close
	// and has no "linked-" spelling of anything.
	for _, name := range []string{
		"linked-window-add", "linked-window-close", "unlinked-window-rename",
	} {
		if IsKnownNotification(name) {
			t.Errorf("%%%s is in the known notification set, but tmux does not send it", name)
		}
	}
}
