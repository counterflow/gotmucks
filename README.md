# gotmucks

[![CI](https://github.com/counterflow/gotmucks/actions/workflows/ci.yml/badge.svg)](https://github.com/counterflow/gotmucks/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/counterflow/gotmucks.svg)](https://pkg.go.dev/github.com/counterflow/gotmucks)

A Go library for driving tmux programmatically.

```
go get github.com/counterflow/gotmucks
```

tmux presents two interfaces, and this library covers both.

| Half | What it does |
|---|---|
| **Commands** | One-shot tmux invocations: create, list, kill, send keys, capture panes, set options and hooks. Typed arguments, typed results parsed from format strings |
| **Control mode** | A persistent `tmux -C` client: send commands on one connection, receive live pane output and asynchronous notifications |

Existing Go tmux packages cover parts of the first half only. The control-mode
half is the reason this exists.

---

## Commands

```go
c := gotmucks.New(gotmucks.WithSocketName("myapp"))

s, err := c.NewSession(ctx, gotmucks.NewSessionOptions{
    Name:    "build",
    Env:     map[string]string{"CI": "1"},
    Command: []string{"go", "test", "./..."},
})

panes, err := c.ListSessionPanes(ctx, s.ID)
out, err := c.CapturePane(ctx, panes[0].ID, gotmucks.CaptureOptions{})
```

Options and hooks are typed the same way, and are read back decoded:

```go
c.SetOption(ctx, s.ID, "@deploy-target", "staging")
v, ok, err := c.ShowOption(ctx, s.ID, gotmucks.ScopeSession, "@deploy-target")

c.SetHook(ctx, s.ID, "alert-bell", "display-message rang")
hooks, err := c.ShowHooks(ctx, s.ID)
```

## Control mode

```go
cc, err := gotmucks.Connect(ctx, gotmucks.WithSocketName("myapp"),
    gotmucks.WithAttach(s.ID))
defer cc.Close()

cc.SetSize(ctx, 132, 43)
cc.Subscribe(ctx, "cmd", gotmucks.SubscribeAllPanes, "#{pane_current_command}")

for ev := range cc.Events() {
    switch e := ev.(type) {
    case gotmucks.PaneOutput:
        os.Stdout.Write(e.Data)
    case gotmucks.SubscriptionChanged:
        log.Printf("%s %s = %s", e.Pane, e.Name, e.Value)
    case gotmucks.Exited:
        return
    }
}
```

`Do` is safe to call from many goroutines: more than one command may be
outstanding, and each reply is bound to the command that earned it by the
order the commands were written in. Not by tmux's command number — those
neither start at zero nor run contiguously, and tmux opens blocks of its own
that no command asked for.

Because replies are bound by order, a line must earn exactly one reply block.
`Do` refuses the ways a line earns more than one — an unquoted `;`, a `{`
beginning a token — and the way it earns none, a leading `#`, which is a
comment tmux answers with nothing at all and which would hand this command the
*next* one's reply. What cannot be detected is a command that makes tmux run
further commands as an ordinary quoted string: `if-shell`, `bind-key` and
`source-file` are documented rather than refused. `DoArgs` quotes for you.

---

## Design

**Address by identifier, never by name or index.** Sessions are `$0`, windows
`@1`, panes `%2`. Names change and indexes renumber. `SessionID`, `WindowID`
and `PaneID` are distinct types, so a pane cannot be passed where a window is
wanted — and since they are string types the compiler cannot stop
`SessionID("work")` on its own, every call that acts on an identifier checks
the shape too and fails with `ErrInvalidID` rather than letting tmux resolve a
name.

**`context.Context` on everything.** These are subprocess calls and they can
hang.

**Format strings, never human-readable output.** `Query` takes a field spec,
builds the `-F` argument and parses the result into typed rows. Callers never
write `-F` by hand and never parse tmux prose.

**No shell.** Commands are assembled as an argument vector.
`NewSessionOptions.Command` is passed after a literal `--`, so shell
metacharacters in it are inert. The one case where tmux will not honour that —
a single-element vector, which tmux hands to the shell — is rejected rather
than silently interpreted.

**The vector is the boundary, with one exception.** tmux's own argv parser
takes a trailing `;` off an element and ends the command there, so
`RenameWindow("a;")` named the window `a` and an element of exactly `;`
vanished — turning `SetOption(status, ";")` into a `set-option` with no value,
which tmux reads as turning the flag *on*, with a nil error. `Client` escapes
that one byte on the way out. Every other metacharacter survives an element
intact.

**A name is data, and tmux does not hand it back as given.** It expands a name
as a format before storing it, so `v#{host}` would name a window after the
host; and it stores names, and prints option values, escaped with vis(3), so a
session called `$HOME` reads back as `\$HOME`. The writers escape and the
readers decode — rows and notifications alike — so what was set is what comes
back. One rewrite is not an encoding and cannot be undone by reading: tmux
turns a `:` or a `.` in a *session* name into `_`, exits 0 and says nothing, so
`web.example.com` would be a session called `web_example_com`. Those two bytes
are refused rather than reported as a name nobody asked for. Windows keep both.

**A name printed back is a delimiter.** `show-options` and `show-hooks` print
`name value`, escape the value and print the name exactly as stored — and a
user option's name is whatever the caller passed, since tmux validates only the
leading `@`. A name containing a space comes back indistinguishable from a
shorter name and a longer value; one containing a newline arrives as a second
line that was never an option. Those bytes are refused in the readers as well
as the writers.

**Which table a thing is kept in follows its name, not the scope you ask for.**
tmux files a known option or hook under the table its name belongs to and
ignores the scope flag. `ShowHooks` therefore reads all three tables and
merges, which is unambiguous because a hook name belongs to exactly one.
Options cannot be merged — the same name legitimately exists in several tables
with different values — so `SetOption` and `ShowOptions` are *not* inverses: an
option set at `ScopeSession` that tmux files under window is found by
`ShowOption` and is absent from `ShowOptions` at that scope. `OptionScope` says
which reader to believe about what.

**"No server running" is not an error.** tmux exits non-zero for it, but for a
read that is an answer: `ListSessions` returns an empty slice, `HasSession` and
`ServerRunning` return false, and none of them return an error.

**The reader never blocks.** A control connection has one goroutine on the
pipe. If a consumer stops reading `Events()`, events are dropped and the loss
is reported as an `EventsDropped` event and through `Dropped()` — stalling the
reader would stall command replies and every other pane too. A per-pane tap
from `Output()` has its own buffer and its own losses: `OutputDropped` on the
event stream, and `DroppedOutput(pane)` for a pane that fell quiet before a
report could be attached to it.

**Backpressure is tmux's job, not yours.** `PauseAfter` enables tmux's own flow
control: tmux pauses a pane whose output is not being consumed and says so with
`PanePaused`, so a slow consumer can never block a pane's process
indefinitely. `Resume` restarts it.

---

## Requirements

- **tmux ≥ 3.2**, for `new-session -e`. Checked at connect time; CI runs the
  integration suite against 3.2a, 3.4 and 3.5a.
- **Go ≥ 1.22**, built and tested in CI on 1.22 and stable.
- **No dependencies outside the standard library.** CI fails if `go.sum` is
  not empty.
- Builds with `CGO_ENABLED=0`.

tmux does not run on Windows. The package compiles anywhere but only functions
where tmux does.

---

## What this library does not do

Layout management, copy mode, key tables and plugin systems are out of scope.
Windows and panes are exposed for reading and for addressing other commands,
but this library does not create them or manipulate layouts — adding that later
is additive, whereas removing it would not be.

There is no CLI. This is a library.

---

## Testing

```
go test ./...                        # hermetic; no tmux needed
go test -tags integration ./...      # against the real tmux on PATH
go test -race ./...
```

The unit suite runs against a stand-in tmux binary that records the argument
vector it was given and replies byte for byte, so argv assembly and output
parsing are both pinned without a tmux installation. Control-mode parsing is
driven over in-memory pipes from tables of raw protocol lines. Output escaping
and control-line assembly are covered by fuzzing over arbitrary bytes
(`FuzzRoundTrip`, `FuzzUnescapeArbitrary`, `FuzzDoArgsProducesASendableLine`).

Integration tests run against real tmux on a private socket per test
(`-L gotmucks-t<pid>-<n>`) and kill their server on the way out, so they can
never touch a developer's own sessions.

`scripts/` holds probe scripts that ask a tmux binary how it actually behaves
rather than what the manual says — whether a command vector is `execvp`'d,
whether control mode works over pipes, which format variables are populated,
which notification names are really written, how many reply blocks a line
earns, and what a value looks like after a round trip through tmux. They are
assertions and not reports: each exits non-zero when a tmux stops behaving the
way the package depends on. Several of this library's decisions came from
running them, and a few corrected what had been written from reading. Run them
against any tmux you add to the support matrix — which table an option or hook
name belongs to is exactly the kind of claim that moves between releases.

---

## Licence

MIT.
