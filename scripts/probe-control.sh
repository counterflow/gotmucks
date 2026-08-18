#!/usr/bin/env bash
# Probe how the installed tmux behaves in control mode with piped standard
# input and output, which is how a library must drive it.
#
#   ./scripts/probe-control.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-ctlprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

echo "=== $("$TMUX_BIN" -V) ==="

run_probe() {
	local label="$1" flag="$2" startcmd="$3"
	echo
	echo "--- $label ($flag $startcmd) ---"

	"${T[@]}" kill-server 2>/dev/null
	"${T[@]}" new-session -d -s ctl -x 80 -y 24 2>/dev/null

	local out
	out=$(mktemp)
	{
		printf 'list-panes -F #{pane_id}\n'
		sleep 0.4
		printf 'refresh-client -C 132x43\n'
		sleep 0.4
		printf 'refresh-client -B probe:%%*:#{pane_title}\n'
		sleep 0.4
		printf 'refresh-client -f pause-after=1\n'
		sleep 0.4
		printf 'display-message -p #{session_id}\n'
		sleep 0.6
		printf '\n'
		sleep 0.4
	} | "${T[@]}" "$flag" $startcmd >"$out" 2>&1

	echo "exit status: $?"
	echo "bytes: $(wc -c <"$out")"
	cat -v "$out" | head -40
	rm -f "$out"
}

run_probe "attach with -C"       -C  "attach-session -t ctl"
run_probe "new-session with -C"  -C  "new-session"
run_probe "attach with -CC"      -CC "attach-session -t ctl"
run_probe "new-session with -CC" -CC "new-session"
