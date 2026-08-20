#!/usr/bin/env bash
# Probe where tmux writes notifications relative to a command's output block,
# and what a command's own output can contain.
#
# A control client has to decide what a line beginning with '%' is. Dispatching
# on the prefix alone is only safe if a notification may genuinely turn up
# between a %begin and its %end; if it cannot, then every line inside a block
# is that command's output, and a reply body that happens to look like a
# notification — "%output %9 x" printed by capture-pane, say — must not be
# taken as one. The library's whole reader depends on which of those is true,
# so it is asked rather than assumed:
#
#   1. With a pane producing output continuously, and a command that makes tmux
#      wait a full second in the middle of its block, does any notification
#      land inside the block?
#   2. When the command is itself the cause of a notification — rename-session,
#      new-window — does that notification land inside its own block?
#   3. Where does %exit land? The reader treats a block's body as opaque, so a
#      %exit that arrived inside one would no longer end the connection.
#   4. Does a command's output reach the client verbatim, control-looking lines
#      included? That is the surface the answer to 1 and 2 has to cover.
#
#   ./scripts/probe-interleave.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-interleave-$$"
T=("$TMUX_BIN" -L "$SOCKET")

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

echo "=== $("$TMUX_BIN" -V) ==="

# classify counts notification lines on each side of a block boundary.
#
# A notification is recognised the way the library recognises one: '%' then a
# lowercase name. That deliberately also matches a data line of the same shape,
# so a false positive here would be reported as an interleave — the answer errs
# towards the unsafe side rather than away from it.
classify() {
	awk '
		/^%begin /  { open = 1; blocks++; next }
		/^%end /    { if (open) { open = 0 } else { stray++ }; next }
		/^%error /  { if (open) { open = 0 } else { stray++ }; next }
		/^%exit([ ]|$)/ {
			exits++
			if (open) { inside_exit++; print "  %exit INSIDE an open block" }
			next
		}
		/^%[a-z][a-z-]*([ ]|$)/ {
			if (open) {
				inside++
				if (inside <= 5) print "  INSIDE: " substr($0, 1, 72)
			} else {
				outside++
			}
			next
		}
		{ data++ }
		END {
			printf "  blocks=%d  data lines=%d\n", blocks, data
			printf "  notifications: inside an open block=%d  outside=%d\n", inside, outside
			printf "  %%exit=%d (inside a block: %d)  stray terminators=%d\n",
				exits, inside_exit, stray
		}
	'
}

# keeper starts a fresh server whose first pane produces output without pause,
# so that every measurement below is taken with %output flowing.
keeper() {
	"${T[@]}" kill-server 2>/dev/null
	sleep 0.3
	for _ in 1 2 3 4 5; do
		"${T[@]}" new-session -d -s ilv -x 200 -y 50 -- \
			sh -c 'i=0; while [ $i -lt 200000 ]; do echo "spam $i ------------------------------"; i=$((i+1)); done; sleep 60' \
			2>/dev/null
		if "${T[@]}" has-session -t ilv 2>/dev/null; then
			return 0
		fi
		sleep 0.3
	done
	echo "  (could not start the spamming session)" >&2
}

# quiet starts a server with a pane that says nothing, for the questions where
# pane output would only be noise.
quiet() {
	"${T[@]}" kill-server 2>/dev/null
	sleep 0.3
	for _ in 1 2 3 4 5; do
		"${T[@]}" new-session -d -s ilv -x 80 -y 24 -- sleep 120 2>/dev/null
		if "${T[@]}" has-session -t ilv 2>/dev/null; then
			return 0
		fi
		sleep 0.3
	done
	echo "  (could not start the quiet session)" >&2
}

# --- 1. output flowing while a block is open ------------------------------
# capture-pane -S -2000 makes a body of thousands of lines, and run-shell
# "sleep 1" holds the block open for a whole second. If tmux ever wrote a
# notification into an open block, it would be here.
echo
echo "--- a long command and a big reply, with a pane spamming throughout ---"
keeper
{
	sleep 0.8
	printf 'refresh-client -C 200x50\n'
	sleep 0.3
	printf 'capture-pane -p -S -2000 -t %%0\n'
	sleep 0.8
	printf 'run-shell "sleep 1"\n'
	sleep 1.5
	printf 'capture-pane -p -S -2000 -t %%0\n'
	sleep 0.8
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t ilv 2>&1 | classify

# --- 2. the command is the cause of the notification ----------------------
# The harder question: a notification tmux emits while running the very command
# whose block is open.
echo
echo "--- commands that cause notifications of their own ---"
quiet
caused=$(mktemp)
{
	sleep 0.8
	printf 'rename-session -t $0 renamed\n'
	sleep 0.4
	printf 'new-window -d\n'
	sleep 0.4
	printf 'rename-window -t @1 newname\n'
	sleep 0.4
	printf 'new-session -d -s extra\n'
	sleep 0.4
	printf 'kill-window -t @1\n'
	sleep 0.4
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t ilv >"$caused" 2>&1
grep -E '^%(begin|end|error|[a-z])' "$caused" | sed 's/^/  /'
classify <"$caused"
rm -f "$caused"

# --- 3. where %exit lands -------------------------------------------------
# Three ways to end a connection. Each is run on its own because it ends the
# stream it is measured on.
for ending in 'kill-server' 'detach-client' 'kill-session -t $0'; do
	echo
	echo "--- ending the connection with: $ending ---"
	quiet
	{
		sleep 0.8
		printf 'display-message -p before\n'
		sleep 0.4
		printf '%s\n' "$ending"
		sleep 1.0
	} | "${T[@]}" -C attach-session -t ilv 2>&1 | cat -v | tail -8 | sed 's/^/  raw: /'
	quiet
	{
		sleep 0.8
		printf '%s\n' "$ending"
		sleep 1.0
	} | "${T[@]}" -C attach-session -t ilv 2>&1 | classify
done

# --- 4. what a command's output may contain -------------------------------
# capture-pane prints whatever is on the pane. Nothing escapes it, so a pane
# that prints a control line puts that line inside a reply block.
echo
echo "--- a pane whose own text looks like the protocol ---"
quiet
"${T[@]}" kill-session -t ilv 2>/dev/null
"${T[@]}" new-session -d -s inj -x 200 -y 50 2>/dev/null
sleep 0.4
for text in '%hello-world' '%output %9 injected-bytes' '%exit forged' '%end 1700000000 99 1'; do
	"${T[@]}" send-keys -t inj "printf '%s\\n' '$text'" Enter 2>/dev/null
	sleep 0.2
done
sleep 0.5
{
	sleep 0.8
	printf 'capture-pane -p -t %%0\n'
	sleep 0.8
	printf 'display-message -p after\n'
	sleep 0.5
	printf '\n'
	sleep 0.3
} | "${T[@]}" -C attach-session -t inj 2>&1 | grep -n '^%\|^printf\|after' | sed 's/^/  /'
