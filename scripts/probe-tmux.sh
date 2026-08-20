#!/usr/bin/env bash
# Probe the installed tmux for the behaviours this library depends on.
#
# Several of the library's decisions rest on how a particular tmux release
# handles a particular argument shape, and those have drifted over time. This
# script asks the binary rather than assuming, and prints one line per
# question. Run it against each tmux in the support matrix when adding a
# version.
#
#   ./scripts/probe-tmux.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-probe-$$"
T=("$TMUX_BIN" -L "$SOCKET")

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

say() { printf '%-38s %s\n' "$1" "$2"; }

echo "=== $("$TMUX_BIN" -V) ==="
echo

# --- execvp versus the shell: see probe-argv.sh ---------------------------
# Whether a command vector after "--" is exec'd or handed to a shell is asked
# by scripts/probe-argv.sh, not here. It was asked here too, and answered
# wrongly on every run: the two checks did start-server, set remain-on-exit,
# then ran a pane command that exits at once, and screen-scraped the pane. A
# server with no sessions exits immediately (exit-empty defaults to on), so the
# option never landed and the server was gone again before capture-pane ran;
# capture-pane's "no server running" went to /dev/null and the empty capture
# was reported as "NOT execvp" and "not shell" — the opposite of the truth, and
# the opposite of what probe-argv.sh printed five lines later in the same CI
# log. probe-argv.sh holds the server open with a keeper session and observes
# through the filesystem rather than the screen, which is what a pane whose
# command exits at once requires.
say "execvp vs shell after --" "asked by scripts/probe-argv.sh"

# --- send-keys -- ---------------------------------------------------------
"${T[@]}" kill-server 2>/dev/null
"${T[@]}" new-session -d -s keys -x 80 -y 24 -- cat 2>/dev/null
sleep 0.3
if "${T[@]}" send-keys -l -t keys -- '--force' 2>/dev/null; then
	say "send-keys -l -t X -- --force" "accepted"
else
	say "send-keys -l -t X -- --force" "REJECTED"
fi
if "${T[@]}" send-keys -H -t keys -- 41 42 2>/dev/null; then
	say "send-keys -H -t X -- 41 42" "accepted"
else
	say "send-keys -H -t X -- 41 42" "REJECTED"
fi

# --- set-option scopes ----------------------------------------------------
pane=$("${T[@]}" list-panes -t keys -F '#{pane_id}' 2>/dev/null | head -1)
if "${T[@]}" set-option -p -t "$pane" remain-on-exit on 2>/dev/null; then
	say "set-option -p remain-on-exit" "accepted (pane option exists)"
else
	say "set-option -p remain-on-exit" "REJECTED (window option only)"
fi
if "${T[@]}" set-option -w -t keys remain-on-exit on 2>/dev/null; then
	say "set-option -w remain-on-exit" "accepted"
else
	say "set-option -w remain-on-exit" "REJECTED"
fi

# --- set-option -- separator ---------------------------------------------
if "${T[@]}" set-option -g -- history-limit 5000 2>/dev/null; then
	say "set-option -g -- name value" "accepted"
else
	say "set-option -g -- name value" "REJECTED"
fi

# --- new-session -e (the 3.2 floor) --------------------------------------
"${T[@]}" kill-server 2>/dev/null
if "${T[@]}" new-session -d -s envtest -e GOTMUCKS_PROBE=yes 2>/dev/null; then
	val=$("${T[@]}" show-environment -t envtest GOTMUCKS_PROBE 2>/dev/null)
	say "new-session -e" "accepted, show-environment: [$val]"
else
	say "new-session -e" "REJECTED (tmux older than 3.2)"
fi

# --- list-panes -a and format separator ----------------------------------
out=$("${T[@]}" list-panes -a -F '#{pane_id}	#{window_id}	#{session_id}' 2>/dev/null | head -1)
say "list-panes -a tab format" "[$(printf '%s' "$out" | tr '\t' '|')]"

# --- stderr for a socket with no server behind it -------------------------
# tmux exits 1 for this the same as for a real failure, so errors.go tells the
# two apart by the wording, and the wording has drifted across releases. The
# classification is load-bearing in the direction that fails quietly:
# ErrNoServer is what makes KillSession report success and HasSession report
# false, so a message wrongly classified as "no server" turns a failure into an
# answer.
#
# Printed here rather than asserted, because what is wanted is the exact text
# to match against — and, just as much, the text of failures that are NOT this
# one. "No such file or directory" is the phrase tmux writes for any missing
# file, so it says nothing on its own about the socket.
noserver=$("$TMUX_BIN" -L "gotmucks-absent-$$" list-sessions 2>&1 >/dev/null)
say "no such socket (-L)" "[$noserver]"

noserver=$("$TMUX_BIN" -S "/tmp/gotmucks-absent-$$/sock" list-sessions 2>&1 >/dev/null)
say "socket path in a missing dir" "[$noserver]"

"${T[@]}" kill-server 2>/dev/null
"${T[@]}" new-session -d -s stderr -x 80 -y 24 2>/dev/null
for cmd in "source-file /gotmucks-absent-$$.conf" \
	"load-buffer /gotmucks-absent-$$.txt" \
	"save-buffer -b nosuchbuffer /tmp/gotmucks-out-$$" \
	"pipe-pane -t nosuchpane cat"; do
	# shellcheck disable=SC2086
	msg=$("${T[@]}" $cmd 2>&1 >/dev/null)
	say "${cmd%% *} on a live server" "[$msg]"
done

# --- control mode: see probe-control.sh -----------------------------------
# Control mode is asked by scripts/probe-control.sh, which runs -C and -CC
# against both attach-session and new-session and prints each transcript. It
# was asked here too, with -CC, and -CC fails outright with piped stdio —
# "tcgetattr failed: Inappropriate ioctl for device" and nothing else. The two
# lines this section printed were counts of "%error" in that transcript, so a
# session that never started reported "control mode errors: none" and
# "refresh-client -C WxH: ok". Failing to run is not the same answer as running
# without error, and only one of the two probes was in a position to tell.
say "control mode" "asked by scripts/probe-control.sh"
