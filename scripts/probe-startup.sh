#!/usr/bin/env bash
# Probe how a control-mode connection starts, and what distinguishes a block
# tmux opened by itself from a block that answers one of our commands.
#
# The library binds a %begin to the front of its own queue of written-but-
# unanswered commands. That rule is only safe if tmux never opens a block for
# something we did not send while one of our commands is outstanding, so the
# two questions here are load-bearing:
#
#   1. Does the unsolicited startup block always arrive before tmux processes
#      anything from stdin, even when the command was already in the pipe?
#   2. Is the flags word of that block distinguishable from the flags word of
#      a reply to one of our commands?
#
#   ./scripts/probe-startup.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-startprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

echo "=== $("$TMUX_BIN" -V) ==="

# keeper starts a fresh server with one long-lived session. A server with no
# sessions exits at once, and kill-server does not release the socket
# instantly, so the session is waited for rather than assumed.
keeper() {
	"${T[@]}" kill-server 2>/dev/null
	sleep 0.3
	for _ in 1 2 3 4 5; do
		"${T[@]}" new-session -d -s keeper -x 80 -y 24 -- sleep 120 2>/dev/null
		if "${T[@]}" has-session -t keeper 2>/dev/null; then
			return 0
		fi
		sleep 0.3
	done
	echo "  (could not start the keeper session)" >&2
}

# show numbers each line of a control stream so the ordering of the startup
# block against the first reply is visible at a glance.
show() { cat -v | grep -n '' | head -30; }

# --- 1. the command is already in the pipe when tmux starts ---------------
# This is the race the library loses if it writes before the reader has
# absorbed the startup block: if tmux still emits its own block first, a
# barrier on the first %end is enough to make the common case deterministic.
echo
echo "--- attach, command already buffered in stdin ---"
keeper
{
	printf "list-sessions -F '#{session_id}'\n"
	sleep 1.0
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t keeper 2>&1 | show

# --- 2. the command is written only after tmux has settled ---------------
echo
echo "--- attach, command written after a delay ---"
keeper
{
	sleep 1.0
	printf "list-sessions -F '#{session_id}'\n"
	sleep 1.0
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t keeper 2>&1 | show

# --- 3. the same for a connection that creates its own session -----------
echo
echo "--- new-session, command already buffered in stdin ---"
keeper
{
	printf "list-sessions -F '#{session_id}'\n"
	sleep 1.0
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C new-session 2>&1 | show

# --- 4. the flags word across several replies, including an error --------
# If unsolicited blocks carry a different flags word from command replies,
# that is a discriminator the queue-order rule can use for blocks arriving at
# any time, not just at startup.
echo
echo "--- flags word: startup block, three replies, one error ---"
keeper
{
	sleep 0.6
	printf "list-sessions -F '#{session_id}'\n"
	sleep 0.4
	printf 'display-message -p ok\n'
	sleep 0.4
	printf 'kill-session -t $999\n'
	sleep 0.4
	printf 'refresh-client -C 132x43\n'
	sleep 0.4
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t keeper 2>&1 | grep -E '^%(begin|end|error)' | \
	awk '{ printf "  %-8s number=%-6s flags=%s\n", $1, $3, $4 }'

# --- 5. how many blocks does one written line produce? -------------------
# The queue-order rule assumes one block per line written. A command list and
# a braced block both break that assumption, and the leftover blocks are
# marked exactly like replies, so they cannot be told apart after the fact.
echo
echo "--- blocks produced by one written line ---"
keeper
{
	sleep 0.6
	printf 'list-sessions -F ONE\n'
	sleep 0.4
	printf 'list-sessions -F TWO ; list-sessions -F THREE\n'
	sleep 0.4
	printf "if-shell 'true' { list-sessions -F FOUR ; list-sessions -F FIVE }\n"
	sleep 0.6
	printf "list-sessions -F 'SIX;SEVEN'\n"
	sleep 0.4
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t keeper 2>&1 | grep -E '^%(begin|end|error)|^[A-Z]' | sed 's/^/  /'

# --- 6. a NUL byte inside a quoted control-command argument --------------
# tmux reads the command line as a C string, so a NUL truncates it. The
# command then succeeds while doing something other than what was asked.
echo
echo "--- NUL inside a control command argument ---"
keeper
{
	sleep 0.6
	printf 'rename-session -t $0 %b\n' "'a\000b'"
	sleep 0.6
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t keeper >/dev/null 2>&1
sleep 0.3
printf '  session name after rename: [%s]\n' \
	"$("${T[@]}" display-message -p -t '$0' '#{session_name}' 2>&1)"
