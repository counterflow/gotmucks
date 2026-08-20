#!/usr/bin/env bash
# Probe which notification names a tmux really emits, and compare them with
# the set the library recognises.
#
# The reader's notification table is the one part of the protocol the unit
# suite cannot check: those tests feed the reader lines the tests themselves
# spell, so the table is only ever asserted against itself. A name that
# disagrees with tmux passes every test and then never matches a real line —
# which is how the rename of an unlinked window spent five review rounds
# spelled "unlinked-window-rename" for a notification tmux writes as
# "%unlinked-window-renamed", and how "linked-window-add" and
# "linked-window-close", which tmux has never emitted, got into the table at
# all.
#
# Three questions, answered against the binary rather than the documentation:
#
#   1. Which "%name" format strings does this tmux binary contain? That is the
#      complete set it can possibly write, including names none of the
#      operations below happen to reach.
#   2. Which of them does a live server actually emit, with a control client
#      attached to one session while a second client mutates both that session
#      and another? The second session is what distinguishes the linked forms
#      from the unlinked ones.
#   3. Where do those two answers and the library's table disagree?
#
#   ./scripts/probe-notify.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-notify-$$"
T=("$TMUX_BIN" -L "$SOCKET")
REPO="$(cd "$(dirname "$0")/.." && pwd)"

WORK="$(mktemp -d)"
cleanup() {
	"${T[@]}" kill-server 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

echo "=== $("$TMUX_BIN" -V) ==="

# --- 1. every name the binary can write -----------------------------------
# tmux writes most notifications through printf, where a literal percent is
# doubled: control_write(c, "%%window-renamed @%u %s", ...). The bytes in the
# binary are therefore "%%window-renamed". A few go through format expansion
# instead and keep a single percent — %layout-change is written
# "%layout-change #{window_id} #{window_layout} ..." — so both spellings are
# collected. The single-percent pass is restricted to hyphenated names because
# an unhyphenated "%word" matches ordinary prose all over the binary.
#
# grep -a rather than strings(1), which is not installed everywhere.
BIN_PATH="$(command -v "$TMUX_BIN" || echo "$TMUX_BIN")"
{
	grep -aoE '%%[a-z][a-z-]+' "$BIN_PATH" | sed 's/^%%//'
	grep -aoE '%[a-z][a-z-]*-[a-z-]*[a-z]' "$BIN_PATH" | sed 's/^%//'
} | sort -u >"$WORK/in-binary"

echo
echo "--- names present in $BIN_PATH ---"
sed 's/^/  /' "$WORK/in-binary"

# --- 2. names a live server emits -----------------------------------------
# The control client attaches to session "probe". Everything done to session
# "other" is done to windows this client has no winlink for, which is what
# makes tmux choose the unlinked spelling.
"${T[@]}" kill-server 2>/dev/null
sleep 0.3
"${T[@]}" new-session -d -s probe -x 80 -y 24 -- sleep 300 2>/dev/null
"${T[@]}" new-session -d -s other -x 80 -y 24 -- sleep 300 2>/dev/null
sleep 0.4

{
	sleep 0.8

	# Linked: every window below is in the session this client is attached to.
	printf 'new-window -d -t probe\n';              sleep 0.3
	printf 'rename-window -t probe:1 linked-new\n'; sleep 0.3
	printf 'split-window -d -t probe:1\n';          sleep 0.3
	printf 'select-pane -t probe:1.1\n';            sleep 0.3
	printf 'select-window -t probe:1\n';            sleep 0.3
	printf 'kill-window -t probe:1\n';              sleep 0.3

	# Unlinked: the same three operations against the other session.
	printf 'new-window -d -t other\n';                sleep 0.3
	printf 'rename-window -t other:1 unlinked-new\n'; sleep 0.3
	printf 'kill-window -t other:1\n';                sleep 0.3

	# Session-level, buffers, subscriptions, pane modes and pane output.
	printf 'rename-session -t probe renamed-probe\n';    sleep 0.3
	printf 'set-buffer -b probebuf hello\n';             sleep 0.3
	printf 'delete-buffer -b probebuf\n';                sleep 0.3
	printf 'refresh-client -B "sub:%%%%1:#{pane_id}"\n'; sleep 0.4
	printf 'copy-mode -t %%%%1\n';                       sleep 0.3
	printf 'send-keys -t %%%%1 q\n';                     sleep 0.3
	printf 'run-shell "echo hi"\n';                      sleep 0.3
	printf 'new-session -d -s third\n';                  sleep 0.3
	printf 'kill-session -t third\n';                    sleep 0.4

	printf 'kill-server\n'
	sleep 1.0
} | "${T[@]}" -C attach-session -t probe >"$WORK/raw" 2>&1

# A name is counted only where it is outside an open block. Inside one the
# line is that command's output, which is the whole point of probe-interleave.
awk '
	/^%begin /               { open = 1; next }
	/^%end /  || /^%error /  { open = 0; next }
	/^%[a-z][a-z-]*([ ]|$)/  { if (!open) { sub(/^%/, "", $1); print $1 } }
' "$WORK/raw" | sort -u >"$WORK/emitted"

echo
echo "--- names a live server emitted ---"
sed 's/^/  /' "$WORK/emitted"

# --- 3. where the library disagrees ---------------------------------------
# The table is read out of the source rather than restated here, so this
# cannot drift away from the thing it is checking.
sed -n '/^var notifications = map\[string\]bool{/,/^}/p' \
	"$REPO/internal/ctlparse/ctlparse.go" |
	grep -oE '"[a-z-]+"' | tr -d '"' | sort -u >"$WORK/in-table"

if [ ! -s "$WORK/in-table" ]; then
	echo
	echo "  (could not read the notification table out of the source; the diffs below are meaningless)" >&2
fi

# begin, end and error are block framing rather than notifications. tmux
# writes all three through one format — "%%%s %ld %u %u" with the word passed
# as an argument — so they are in the table but cannot be in the binary scan,
# and would otherwise show up here as three permanent false alarms.
printf 'begin\nend\nerror\n' | sort >"$WORK/framing"

# Only one direction is a defect. A name tmux sends that the table does not
# have loses the typed event for it, so the last two lists must be empty. The
# other way round costs nothing and is expected: the table runs ahead of the
# 3.2 floor on purpose, so an older binary is simply missing some of it.
echo
echo "--- in the library's table, absent from the binary (informational) ---"
comm -23 "$WORK/in-table" "$WORK/in-binary" | comm -23 - "$WORK/framing" | sed 's/^/  /'

echo
echo "--- in the binary, absent from the library's table (must be empty) ---"
comm -13 "$WORK/in-table" "$WORK/in-binary" | sed 's/^/  /'

echo
echo "--- emitted by the live server, absent from the table (must be empty) ---"
comm -13 "$WORK/in-table" "$WORK/emitted" | sed 's/^/  /'

echo
echo "--- window notifications as they arrived, verbatim ---"
grep -E '^%[a-z-]*window[a-z-]*' "$WORK/raw" | sed 's/^/  /'
