package gotmucks_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/counterflow/gotmucks"
)

// Create a detached session running a command, and read back what it printed.
//
// The command is an argument vector, not a shell string, so the semicolon in
// it is inert.
func ExampleClient_NewSession() {
	ctx := context.Background()
	c := gotmucks.New(gotmucks.WithSocketName("example"))

	s, err := c.NewSession(ctx, gotmucks.NewSessionOptions{
		Name:    "build",
		Env:     map[string]string{"CI": "1"},
		Command: []string{"sh", "-c", "make; sleep 60"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.KillSession(ctx, s.ID)

	panes, err := c.ListSessionPanes(ctx, s.ID)
	if err != nil {
		log.Fatal(err)
	}

	out, err := c.CapturePane(ctx, panes[0].ID, gotmucks.CaptureOptions{})
	if err != nil {
		log.Fatal(err)
	}
	os.Stdout.Write(out)
}

// A server that is not running is not an error for a read. There is no need to
// check for it separately, and no error to distinguish from a real failure.
func ExampleClient_ListSessions() {
	ctx := context.Background()
	c := gotmucks.New(gotmucks.WithSocketName("example"))

	sessions, err := c.ListSessions(ctx)
	if err != nil {
		log.Fatal(err) // a genuine failure, not "nothing is running"
	}
	for _, s := range sessions {
		fmt.Printf("%s %q %d windows\n", s.ID, s.Name, s.Windows)
	}
}

// Send a command to a pane and wait for its output to appear.
func ExampleClient_SendKeys() {
	ctx := context.Background()
	c := gotmucks.New(gotmucks.WithSocketName("example"))

	pane := gotmucks.PaneID("%0")

	// Literal text and named keys cannot share one tmux invocation, so the
	// library issues one per run. The caller does not have to care.
	err := c.SendKeys(ctx, pane,
		gotmucks.Literal("go test ./..."),
		gotmucks.Enter(),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Ctrl-C as a raw byte.
	if err := c.SendKeys(ctx, pane, gotmucks.Hex(0x03)); err != nil {
		log.Fatal(err)
	}
}

// Stream one pane's output live.
func ExampleControlClient_Output() {
	ctx := context.Background()

	cc, err := gotmucks.Connect(ctx,
		gotmucks.WithSocketName("example"),
		gotmucks.WithAttach("$0"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer cc.Close()

	// The channel carries unescaped bytes as tmux framed them, which is not
	// line-oriented: one receive is one notification, not one line.
	pane, err := cc.Output("%0")
	if err != nil {
		log.Fatal(err)
	}
	for chunk := range pane {
		os.Stdout.Write(chunk)
	}
}

// Handle the full notification stream. Exited is terminal and the channel
// closes after it; this library does not reconnect on its own, because only
// the caller knows whether that is wanted.
func ExampleControlClient_Events() {
	ctx := context.Background()

	cc, err := gotmucks.Connect(ctx, gotmucks.WithSocketName("example"))
	if err != nil {
		log.Fatal(err)
	}
	defer cc.Close()

	if err := cc.SetSize(ctx, 132, 43); err != nil {
		log.Fatal(err)
	}

	for ev := range cc.Events() {
		switch e := ev.(type) {
		case gotmucks.PaneOutput:
			fmt.Printf("%s: %d bytes\n", e.Pane, len(e.Data))
		case gotmucks.WindowAdded:
			fmt.Printf("window %s appeared\n", e.Window)
		case gotmucks.PanePaused:
			// tmux stopped this pane because we fell behind. Nothing is lost
			// until we say we are ready.
			if err := cc.Resume(ctx, e.Pane); err != nil {
				log.Print(err)
			}
		case gotmucks.EventsDropped:
			log.Printf("fell behind; %d events discarded", e.Count)
		case gotmucks.Exited:
			log.Printf("connection ended: %v", e)
			return
		}
	}
}

// Track tmux state without polling. tmux expands the format once per matching
// object and reports a change at most once a second.
func ExampleControlClient_Subscribe() {
	ctx := context.Background()

	cc, err := gotmucks.Connect(ctx, gotmucks.WithSocketName("example"))
	if err != nil {
		log.Fatal(err)
	}
	defer cc.Close()

	err = cc.Subscribe(ctx, "cmd", gotmucks.SubscribeAllPanes, "#{pane_current_command}")
	if err != nil {
		log.Fatal(err)
	}

	for ev := range cc.Events() {
		if s, ok := ev.(gotmucks.SubscriptionChanged); ok {
			fmt.Printf("%s is now running %s\n", s.Pane, s.Value)
		}
	}
}

// Let tmux apply backpressure rather than building it yourself. With flow
// control on, a pane whose output nobody is consuming is paused instead of
// blocking its process indefinitely.
func ExampleControlClient_PauseAfter() {
	ctx := context.Background()

	cc, err := gotmucks.Connect(ctx, gotmucks.WithSocketName("example"))
	if err != nil {
		log.Fatal(err)
	}
	defer cc.Close()

	if err := cc.PauseAfter(ctx, 2*time.Second); err != nil {
		log.Fatal(err)
	}

	// Output now arrives as %extended-output, carrying how far behind it is.
	for ev := range cc.Events() {
		if o, ok := ev.(gotmucks.PaneOutput); ok && o.Age > time.Second {
			log.Printf("%s is %s behind", o.Pane, o.Age)
		}
	}
}

// Run an arbitrary command over the connection and parse its output with the
// same format machinery the one-shot client uses.
func ExampleControlClient_Do() {
	ctx := context.Background()

	cc, err := gotmucks.Connect(ctx, gotmucks.WithSocketName("example"))
	if err != nil {
		log.Fatal(err)
	}
	defer cc.Close()

	spec := gotmucks.FormatSpec{"pane_id", "pane_current_command"}

	// DoArgs quotes each argument for tmux's parser. It matters: unquoted, the
	// '#' in a format starts a comment and the command loses its argument.
	reply, err := cc.DoArgs(ctx, "list-panes", "-a", "-F", spec.Arg())
	if err != nil {
		var cerr *gotmucks.ControlError
		if errors.As(err, &cerr) {
			log.Fatalf("tmux refused the command: %s", cerr.Message)
		}
		log.Fatal(err)
	}

	rows, err := reply.Rows(spec)
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range rows {
		fmt.Printf("%s %s\n", r.Get("pane_id"), r.Get("pane_current_command"))
	}
}
