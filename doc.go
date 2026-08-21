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
// The exceptions are the four calls that take a tmux command line rather than
// an identifier — [ControlClient.Do], [ControlClient.DoArgs],
// [Client.QueryArgs] and [WithControlArgs]. A command line is passed to tmux
// as written, -t included, so a name in one resolves as a name. DoArgs quotes
// each argument, which is about how tmux splits the line and not about what it
// addresses: a quoted -t still takes a name.
//
// # Names
//
// A name is not an address, but it is data, and tmux does not hand a value
// back the way it was given. It expands a name as a format before storing it,
// so "v#{host}" would name a window after the host; and it stores names and
// prints option values escaped with vis(3), so a session called "$HOME" reads
// back as "\$HOME". Both are undone here: [Client.RenameWindow],
// [Client.RenameSession] and [Client.NewSession] escape what they send, and
// [Window.Name], [Session.Name], [Client.ShowOption] and [Client.ShowOptions]
// decode what they read, so what was set is what comes back.
//
// tmux hands the same stored name back a second way, in a notification, and
// the control half decodes it in the same place: [WindowRenamed],
// [SessionRenamed], [SessionChanged] and [ClientSessionChanged] carry the name
// that was set, so a caller that lists once and then follows the events holds
// one name for an object rather than two. [SubscriptionChanged] is not one of
// these — its value is the caller's own format, expanded, and there is nothing
// to undo.
//
// The expansion is not confined to names. [NewSessionOptions.StartDir] is
// expanded too and is escaped for the same reason: an unescaped "#H" in a path
// put the pane in the home directory, silently and with a nil error.
//
// One alteration is not an encoding and so cannot be undone by reading: tmux
// rewrites a ':' or a '.' in a *session* name to '_' before storing it,
// because both are its own target separators. It exits 0 and says nothing, so
// "web.example.com" would be a session called "web_example_com".
// [Client.RenameSession] and [Client.NewSession] refuse those two bytes rather
// than report a name nobody asked for; window names take both, since tmux does
// this to sessions alone.
//
// The cost is that a caller who wants tmux to expand a format into a name
// cannot get one through these calls — a '#' is a '#' — and should send
// rename-window itself. The other edge is a window named by another program
// through "new-window -n", which is the one path tmux stores unescaped: a
// plain name is unaffected, but a backslash or a tab in one is not. It applies
// to the events as well as to the rows, since both read what tmux stored.
//
// # Arguments
//
// Commands are argument vectors and never shell strings, so a space, a quote
// or a metacharacter in a caller's value is inert. One byte is not: tmux's own
// argv parser takes a trailing ';' off an element and ends the command there,
// so "a;" arrived as "a" and an argument of exactly ";" vanished, changing
// what the command meant. [Client] escapes that one byte on the way out and
// tmux stores the semicolon, so the vector really is a boundary. A control
// connection quotes its arguments instead and never needed it.
//
// [Row] is the raw view and decodes nothing: it is what tmux wrote.
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
