#!/usr/bin/env bash
# Probe how many reply blocks tmux answers a control command line with.
#
# A control client binds each reply to the command that earned it by queue
# order, so the number has to be exactly one. Two pushes every later reply one
# place along and corrupts them all; zero is worse, because the command that
# earned nothing is then handed the *next* command's block with a nil error and
# the queue stays out of step for the life of the connection, with every health
# signal the package offers still reporting a healthy connection.
#
# ControlClient.Do refuses both, with commandBreak counting upwards and
# commentStart counting down; this is what says which lines are which.
#
# The block tmux opens for the command that started the connection carries
# flags 0 and every reply carries 1, so "ours" below counts only the blocks
# attributed to the command line under test.
#
# Three of the sections are assertions rather than reports, and the script
# exits non-zero if any of them stops holding:
#
#   B1  every line the package will send earns exactly one block. This is the
#       property the whole queue-order scheme rests on, and it had never been
#       stated: a malformed command, an unterminated quote, a lone '}' and a
#       '%if' all still earn theirs.
#   B2  a leading '#' is the only way to earn none. Swept over all ninety-five
#       printable bytes as the first byte of the line, since "is there a second
#       shape that makes tmux answer with nothing" is what decides whether
#       commentStart can afford to be this short.
#   B3  the forms Do refuses for earning too many really do earn too many, so a
#       tmux that stopped doing it would be found here rather than leaving the
#       package refusing lines it could send.
#
#   ./scripts/probe-blocks.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-blockprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")
FAIL=0

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

fail() {
	printf '  ASSERTION FAILED (%s): %s\n' "$1" "$2"
	FAIL=1
}

# Render a line with its unprintable bytes visible, so a tab in a probe cannot
# be mistaken for the spaces around it.
vis() { printf '%s' "$1" | cat -v; }

echo "=== $("$TMUX_BIN" -V) ==="

"${T[@]}" kill-server 2>/dev/null
"${T[@]}" new-session -d -s blk -x 80 -y 24 2>/dev/null

# ours COMMAND -> the number of blocks tmux attributed to this client.
ours() {
	local cmd="$1" out n
	out=$(mktemp)
	{
		printf '%s\n' "$cmd"
		sleep 0.5
		printf '\n'
		sleep 0.3
	} | "${T[@]}" -C attach-session -t blk >"$out" 2>&1
	n=$(grep '^%begin' "$out" | grep -cv ' 0$')
	printf '%s' "$n"
	rm -f "$out"
}

# probe reports one line in full, for the exploratory half.
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
	echo "--- $(vis "$cmd")"
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

# The line that earns nothing. '#' opens a comment, the line parses to an empty
# command list, and tmux writes not one byte in reply.
probe '#comment'
probe 'list-sessions -F #{session_id}'
probe "'#comment'"
probe 'list-sessions #tail'

# ---------------------------------------------------------------------------
echo
echo "--- every line the package will send earns exactly one block (B1) ---"
echo "  including every way of being wrong: an unknown command, an ambiguous"
echo "  one, an unterminated quote, a lone brace, a '%' the parser reads as a"
echo "  preprocessor directive. Being answered with an error is not the same"
echo "  as not being answered, and only the second one corrupts the queue."
for cmd in \
	'list-sessions' \
	'display-message -p one' \
	'nosuchcommand' \
	'a#b' \
	'list-sessions #tail' \
	'list-sessions -F #{session_id}' \
	"'#comment'" \
	'\#comment' \
	'"#comment"' \
	'display-message -p a{b' \
	"display-message -p '{'" \
	'display-message -p }' \
	'}' \
	"'" \
	'%if 1' \
	'%endif' \
	'%%' \
	'\' \
	'list-sessions \' \
	'""' \
	'-' \
	'--' \
	"set-option -- @probe 'a;b'" \
	"set-option -- @probe 'a{b'" \
	"set-option -- @probe 'a b'"; do
	n=$(ours "$cmd")
	printf '  %2s  [%s]\n' "$n" "$(vis "$cmd")"
	[ "$n" = 1 ] || fail B1 "[$(vis "$cmd")] earned $n blocks, not one"
done
"${T[@]}" set-option -u -- @probe 2>/dev/null

# ---------------------------------------------------------------------------
echo
echo "--- a leading '#' is the only line that earns none (B2) ---"
echo "  the shapes first, then every printable byte swept as the first of the"
echo "  line. The sweep is what says commentStart can look at one byte: if a"
echo "  second byte opened a comment the check would be silently incomplete,"
echo "  and the failure it lets through is invisible from inside the package."
for cmd in '#comment' '#' '#x' '  #x' "$(printf '#\tx')" "$(printf '\t#x')" \
	'#{session_id}' '#c ; list-sessions' '# { list-sessions }'; do
	n=$(ours "$cmd")
	printf '  %2s  [%s]\n' "$n" "$(vis "$cmd")"
	[ "$n" = 0 ] || fail B2 "[$(vis "$cmd")] earned $n blocks, not none; Do refuses a line tmux answers"
done

# The sweep goes down one connection, because ninety-five of the probes above
# would take a minute and a half. Each line is a command name of its own, so
# the expected count is one apiece and only the '#' is exempt; a total that
# does not match is unpicked byte by byte below.
SWEEP=$(mktemp)
{
	for code in $(seq 32 126); do
		printf "\\$(printf '%03o' "$code")x\n"
	done
	sleep 2
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t blk >"$SWEEP" 2>&1
got=$(grep '^%begin' "$SWEEP" | grep -cv ' 0$')
rm -f "$SWEEP"
printf '  sweep of 95 first bytes: %s blocks, want 94 (every byte but the hash)\n' "$got"
if [ "$got" != 94 ]; then
	fail B2 "the sweep earned $got blocks for 95 lines; naming the bytes one at a time"
	for code in $(seq 32 126); do
		byte=$(printf "\\$(printf '%03o' "$code")")
		n=$(ours "${byte}x")
		want=1
		[ "$byte" = '#' ] && want=0
		[ "$n" = "$want" ] ||
			printf '    byte %3d [%s] earned %s blocks, want %s\n' "$code" "$(vis "$byte")" "$n" "$want"
	done
fi

# ---------------------------------------------------------------------------
echo
echo "--- the forms Do refuses for earning too many still earn too many (B3) ---"
for cmd in \
	'list-sessions ; list-windows' \
	'if-shell "true" { list-sessions }' \
	'if-shell "true" {list-sessions}' \
	'if-shell "true" "list-sessions"'; do
	n=$(ours "$cmd")
	printf '  %2s  [%s]\n' "$n" "$(vis "$cmd")"
	[ "$n" -gt 1 ] || fail B3 "[$(vis "$cmd")] earned $n blocks; Do refuses a line that is now harmless"
done

# source-file is the same trap from the other side, and the one a caller
# reaches for: it needs a file on the server's own filesystem, so replaying a
# configuration line by line is the obvious alternative — and that hits the
# comment check on the first comment in the file.
CONF=$(mktemp)
printf '# a comment\nlist-sessions\nlist-windows\n' >"$CONF"
printf '  %2s  [source-file with two commands and a comment]\n' "$(ours "source-file $CONF")"
rm -f "$CONF"

echo
if [ "$FAIL" = 0 ]; then
	echo "all assertions hold"
else
	echo "one or more assertions failed"
fi
exit "$FAIL"
