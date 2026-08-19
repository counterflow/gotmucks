#!/usr/bin/env bash
# Probe which command forms make tmux answer with more than one block.
#
# A control client binds each reply to the command that earned it by queue
# order, so any line that produces two blocks pushes every later reply one
# place along and corrupts them all. ControlClient.Do refuses the forms that do
# it; this is what says which those are.
#
# The block tmux opens for the command that started the connection carries
# flags 0 and every reply carries 1, so "ours" below counts only the blocks
# attributed to the command line under test.
#
#   ./scripts/probe-blocks.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-blockprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

echo "=== $("$TMUX_BIN" -V) ==="

"${T[@]}" kill-server 2>/dev/null
"${T[@]}" new-session -d -s blk -x 80 -y 24 2>/dev/null

probe() {
	local cmd="$1" out
	out=$(mktemp)
	{
		printf '%s\n' "$cmd"
		sleep 0.6
		printf '\n'
		sleep 0.3
	} | "${T[@]}" -C attach-session -t blk >"$out" 2>&1

	echo
	echo "--- $cmd"
	printf 'blocks: %s  ours: %s\n' \
		"$(grep -c '^%begin' "$out")" \
		"$(grep '^%begin' "$out" | grep -cv ' 0$')"
	grep -n '^%\(begin\|end\|error\)' "$out" | sed 's/^/  /'
	grep -v '^%' "$out" | sed 's/^/  out: /' | head -5
	rm -f "$out"
}

# One command, one block: the case everything else is measured against.
probe 'list-sessions'

# A brace block. tmux answers the outer command and each command inside it.
probe 'if-shell "true" { list-sessions }'
probe 'if-shell "true" { list-sessions ; list-windows }'

# The same hazard reached through a quoted string, which nothing on the wire
# distinguishes from an ordinary argument.
probe 'if-shell "true" "list-sessions"'

# Where the '{' has to be for tmux to read it as opening a block, which is what
# says how narrow a rejection rule can afford to be.
probe 'if-shell "true" {list-sessions}'
probe 'display-message -p {a}'
probe 'display-message -p a{b'
probe "display-message -p '{'"
probe 'display-message -p }'

# '#' opens a comment outside quotes, so an unquoted format is already broken
# for reasons of its own. Recorded so the rejection rule is not blamed for it.
probe 'list-sessions -F #{session_id}'
