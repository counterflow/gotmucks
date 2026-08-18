// Package faketmux is a stand-in for the tmux binary, used by the test suite.
//
// It exists so that argument assembly, output parsing and error handling can
// be tested exactly and without a tmux installation: the fake records the
// argument vector it was given and replies with whatever the test scripted,
// byte for byte, including exit status and standard error.
//
// It is not a tmux emulator and does not try to be. Behaviour against real
// tmux is covered by the integration tests.
package faketmux

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Response is what the fake writes for one command.
type Response struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

// Script configures the fake. Tests write one to a file and point the fake at
// it with the FAKETMUX_SCRIPT environment variable.
type Script struct {
	// Version is what "tmux -V" prints. Defaults to "tmux 3.4".
	Version string `json:"version"`

	// NoServer makes every command fail the way tmux does when there is no
	// server on the socket. It takes precedence over Responses.
	NoServer bool `json:"no_server"`

	// Responses maps a tmux subcommand to its reply.
	Responses map[string]Response `json:"responses"`

	// Default is used for a subcommand with no entry in Responses. When nil,
	// an unscripted command succeeds silently.
	Default *Response `json:"default"`
}

// Environment variables the fake reads.
const (
	// EnvScript names a file holding a JSON-encoded [Script].
	EnvScript = "FAKETMUX_SCRIPT"
	// EnvArgvLog names a file the fake appends each invocation's argument
	// vector to, one JSON array per line.
	EnvArgvLog = "FAKETMUX_ARGV_LOG"
)

// noServerMessage is the shape tmux uses when the socket has no server.
const noServerMessage = "no server running on /tmp/tmux-1000/default"

// globalFlagsWithValue are the tmux global flags that consume the next
// argument, which must be skipped when looking for the subcommand.
var globalFlagsWithValue = map[string]bool{
	"-S": true, "-L": true, "-f": true, "-c": true,
}

// Run executes the fake against args, which are the arguments tmux would have
// been given, excluding the binary name. It returns the process exit status.
func Run(args []string, stdout, stderr io.Writer) int {
	if path := os.Getenv(EnvArgvLog); path != "" {
		logArgv(path, args)
	}

	script, err := loadScript(os.Getenv(EnvScript))
	if err != nil {
		fmt.Fprintf(stderr, "faketmux: %v\n", err)
		return 2
	}

	if hasVersionFlag(args) {
		version := script.Version
		if version == "" {
			version = "tmux 3.4"
		}
		fmt.Fprintln(stdout, version)
		return 0
	}

	sub := subcommand(args)
	if sub == "" {
		fmt.Fprintln(stderr, "faketmux: no subcommand")
		return 2
	}

	if script.NoServer {
		fmt.Fprintln(stderr, noServerMessage)
		return 1
	}

	resp, ok := script.Responses[sub]
	if !ok {
		if script.Default == nil {
			return 0
		}
		resp = *script.Default
	}

	if resp.Stdout != "" {
		io.WriteString(stdout, resp.Stdout)
	}
	if resp.Stderr != "" {
		io.WriteString(stderr, resp.Stderr)
	}
	return resp.Exit
}

func loadScript(path string) (Script, error) {
	if path == "" {
		return Script{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Script{}, fmt.Errorf("reading script: %w", err)
	}
	var s Script
	if err := json.Unmarshal(b, &s); err != nil {
		return Script{}, fmt.Errorf("parsing script %s: %w", path, err)
	}
	return s, nil
}

// hasVersionFlag reports whether -V appears among the global flags, which
// tmux answers from the binary without contacting a server.
func hasVersionFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-V" {
			return true
		}
		if globalFlagsWithValue[a] {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return false // the subcommand; -V after it is not a global flag
		}
	}
	return false
}

// subcommand returns the tmux subcommand, skipping global flags and their
// values.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if globalFlagsWithValue[a] {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// logArgv appends one invocation to the log, as a JSON array on its own line.
// Appends are O_APPEND so concurrent invocations do not interleave.
func logArgv(path string, args []string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	line, err := json.Marshal(args)
	if err != nil {
		return
	}
	f.Write(append(line, '\n'))
}

// ReadArgvLog parses a log written by the fake into one entry per invocation.
func ReadArgvLog(path string) ([][]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			return nil, fmt.Errorf("parsing argv log line %q: %w", line, err)
		}
		out = append(out, args)
	}
	return out, nil
}

// WriteScript encodes a script to a file for the fake to read.
func WriteScript(path string, s Script) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// NoServerStderr is the message the fake writes when [Script.NoServer] is
// set, exported so tests can assert on classification of it.
const NoServerStderr = noServerMessage
