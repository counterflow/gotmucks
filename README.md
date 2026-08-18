# gotmucks

A Go library for driving tmux programmatically.

```
go get github.com/counterflow/gotmucks
```

tmux presents two interfaces, and this library covers both.

| Half | What it does |
|---|---|
| **Commands** | One-shot tmux invocations: create, list, kill, send keys, set options, capture panes. Typed arguments, typed results parsed from format strings |
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

`Do` is safe to call from many goroutines: replies are correlated with their
commands by tmux's command number, so more than one may be outstanding.

---

## Design

**Address by identifier, never by name or index.** Sessions are `$0`, windows
`@1`, panes `%2`. Names change and indexes renumber. `SessionID`, `WindowID`
and `PaneID` are distinct types, so a pane cannot be passed where a window is
wanted.

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

**"No server running" is not an error.** tmux exits non-zero for it, but for a
read that is an answer: `ListSessions` returns an empty slice, `HasSession` and
`ServerRunning` return false, and none of them return an error.

**The reader never blocks.** A control connection has one goroutine on the
pipe. If a consumer stops reading `Events()`, events are dropped and the loss
is reported as an `EventsDropped` event and through `Dropped()` — stalling the
reader would stall command replies and every other pane too.

**Backpressure is tmux's job, not yours.** `PauseAfter` enables tmux's own flow
control: tmux pauses a pane whose output is not being consumed and says so with
`PanePaused`, so a slow consumer can never block a pane's process
indefinitely. `Resume` restarts it.

---

## Requirements

- **tmux ≥ 3.2**, for `new-session -e`. Checked at connect time and covered in CI.
- **Go ≥ 1.22.**
- **No dependencies outside the standard library.**
- Builds with `CGO_ENABLED=0`.

tmux does not run on Windows. The package compiles anywhere but only functions
where tmux does.

---

## What this library does not do

Layout management, copy mode, key tables and plugin systems are out of scope.
Windows and panes are exposed for reading and for addressing other commands,
but v1 does not create them or manipulate layouts — adding that later is
additive, whereas removing it would not be.

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
is covered by round-trip fuzzing over arbitrary bytes.

Integration tests run against real tmux on a private socket per test
(`-L gotmucks-t<pid>-<n>`) and kill their server on the way out, so they can
never touch a developer's own sessions.

`scripts/` holds probe scripts that ask a tmux binary how it actually behaves —
whether a command vector is `execvp`'d, whether control mode works over pipes,
which format variables are populated. Several of this library's decisions came
from running them rather than from reading the manual; run them against any
tmux you add to the support matrix.

---

## Licence

MIT.
