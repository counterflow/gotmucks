// Package gotmucks drives tmux from Go.
//
// tmux presents two interfaces and this package covers both.
//
// # Commands
//
// [Client] issues one-shot tmux invocations: create a session, list what
// exists, send keys, capture a pane, set an option or a hook. Arguments are
// typed, results are parsed from tmux format strings into typed rows, and no
// call goes through a shell.
//
//	c := gotmucks.New(gotmucks.WithSocketName("myapp"))
//	s, err := c.NewSession(ctx, gotmucks.NewSessionOptions{
//		Name:    "build",
//		Command: []string{"go", "test", "./..."},
//	})
//
// # Control mode
//
// [ControlClient] holds a persistent "tmux -C" connection: commands go down
// one pipe, and replies, live pane output and asynchronous notifications come
// back up another, interleaved. This is the half no other Go package covers
// and the reason this one exists. (The -CC form documented for applications
// wants a terminal on standard input and exits at once against a pipe; see
// [WithDoubleControlMode].)
//
//	cc, err := gotmucks.Connect(ctx, gotmucks.WithSocketName("myapp"))
//	defer cc.Close()
//
//	for ev := range cc.Events() {
//		switch e := ev.(type) {
//		case gotmucks.PaneOutput:
//			os.Stdout.Write(e.Data)
//		case gotmucks.Exited:
//			return
//		}
//	}
//
// # Addressing
//
// tmux objects are addressed by identifier — [SessionID] "$0", [WindowID]
// "@1", [PaneID] "%2" — and never by name or index. Names change and indexes
// renumber; identifiers do not. The three are distinct Go types so that a
// pane cannot be passed where a window is wanted.
//
// They are string types, so the compiler cannot also stop SessionID("work")
// being written, and tmux would resolve that as a name. Every call that acts
// on an identifier therefore checks its shape first and reports [ErrInvalidID]
// rather than proceeding: the ones that build a -t, and [ControlClient.Output],
// which builds none but would otherwise register a tap that matches nothing.
// Identifiers come back from tmux — [Session.ID], [Row.PaneID] — or from
// [ParseSessionID] and its siblings; a name is not an address.
//
// # No server running
//
// tmux exits non-zero when no server is listening, but for a read that is an
// answer rather than a failure. [Client.ListSessions] returns an empty slice,
// [Client.HasSession] and [Client.ServerRunning] return false, and none of
// them return an error. Only writes that cannot start a server report
// [ErrNoServer].
//
// # Backpressure
//
// A control connection has one goroutine reading the pipe, and it never
// blocks on a consumer. If a caller stops reading [ControlClient.Events], the
// events are dropped and the loss is reported through [EventsDropped] —
// stalling the reader would stall command replies and every other pane too. A
// per-pane tap from [ControlClient.Output] has a buffer of its own and so a
// loss of its own: reported as [OutputDropped], and counted by
// [ControlClient.DroppedOutput] for a pane that fell quiet before any report
// could be attached to it.
//
// Separately, tmux itself offers flow control:
// [ControlClient.PauseAfter] makes tmux pause a pane whose output is not
// being consumed and say so with [PanePaused], so a slow consumer can never
// block a pane's process indefinitely. [ControlClient.Resume] restarts it.
//
// # Requirements
//
// tmux 3.2 or newer, for "new-session -e". Go 1.22 or newer. No dependencies
// outside the standard library. Builds with CGO_ENABLED=0.
//
// tmux does not run on Windows, so neither does this package's subject; the
// code compiles anywhere but only functions where tmux does.
package gotmucks
