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
# Five questions, answered against the binary rather than the documentation:
#
#   1. Which "%name" format strings does this tmux binary contain? That is the
#      complete set it can possibly write, including names none of the
#      operations below happen to reach.
#   2. Which of them does a live server actually emit, with a control client
#      attached to one session while a second client mutates both that session
#      and another? The second session is what distinguishes the linked forms
#      from the unlinked ones.
#   3. Where do those two answers and the library's table disagree?
#   4. What does a notification carry for a *name*? The reader decodes one,
#      because tmux writes the stored, vis(3)-escaped form into the line — the
#      same claim probe-roundtrip.sh makes about a format row, asked of the
#      other pipe. The two halves of the package must agree or a caller holds
#      one object under two names.
#   5. What does a raw newline in the last field of a notification do? It ends
#      the line, and the remainder is dispatched as something tmux said.
#
# Unlike most of the other probes this one has an exit status worth reading: a
# name tmux can write that the table does not have is a lost event, so either
# of the two diffs being non-empty exits 1, as does a name that stops being
# escaped or a substitution that stops taking a newline out of a subscription
# value. The rest of the output is still just printed.
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

status=0

if [ ! -s "$WORK/in-table" ]; then
	echo
	echo "  (could not read the notification table out of the source; the diffs below are meaningless)" >&2
	status=1
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

# The two below are assertions, which makes this script different in kind from
# its neighbours: the rest print what a release does and leave the reading to a
# person, and CI runs them with "|| true" for that reason. A name here is a lost
# event, so this one is run for its exit status as well as its output. Deleting
# three real names from the table made it print exactly the right thing and
# still exit 0, which is how that was noticed.
#
# The binary half can in principle raise a false alarm: it is a regex over every
# string in the executable, and something that is not a notification could match
# it. That looks like a name in the first list that the live server never emits
# and that no tmux changelog mentions, and the answer then is to narrow the
# regex rather than to add the name to the table.
must_be_empty() {
	local heading="$1" file="$2"
	echo
	echo "--- $heading (must be empty) ---"
	sed 's/^/  /' "$file"
	if [ -s "$file" ]; then
		echo "  ^^ tmux has notification names the library's table does not; each one is an event lost" >&2
		status=1
	fi
}

comm -13 "$WORK/in-table" "$WORK/in-binary" >"$WORK/missing-from-binary"
must_be_empty "in the binary, absent from the library's table" "$WORK/missing-from-binary"

comm -13 "$WORK/in-table" "$WORK/emitted" >"$WORK/missing-from-emitted"
must_be_empty "emitted by the live server, absent from the table" "$WORK/missing-from-emitted"

echo
echo "--- window notifications as they arrived, verbatim ---"
grep -E '^%[a-z-]*window[a-z-]*' "$WORK/raw" | sed 's/^/  /'

# --- 4. what a notification carries for a name ----------------------------
# The reader decodes every name it delivers, because tmux writes the *stored*
# form into the notification and a name is stored escaped with vis(3). That is
# the same claim probe-roundtrip.sh makes about "#{window_name}" in a format
# row, asked of the other pipe: the two must agree, or a caller that lists once
# and then follows the events holds the same object under two names.
#
# Both lines below are assertions. If a tmux stopped escaping here, the decode
# would corrupt every name containing a backslash instead of restoring it.
"${T[@]}" kill-server 2>/dev/null
sleep 0.3
SID=$("${T[@]}" new-session -d -P -F "#{session_id}" -s names -x 80 -y 24 -- sleep 300 2>/dev/null)
sleep 0.4
{
	sleep 0.8
	printf 'rename-window -t %s:0 -- %s\n' "$SID" "'a"$'\t'"b'" ; sleep 0.4
	printf 'rename-session -t %s -- %s\n' "$SID" "'r\$HOMEs'"   ; sleep 0.6
} | "${T[@]}" -C attach-session -t "$SID" >"$WORK/names" 2>&1

echo
echo "--- a name as a notification carries it ---"
grep -aE '^%(unlinked-)?(window-renamed|session-renamed)' "$WORK/names" | sed 's/^/  /'

name_escaped() {
	local what="$1" pattern="$2"
	if grep -aqE "$pattern" "$WORK/names"; then
		printf '  ok: %s\n' "$what"
	else
		printf '  ASSERTION FAILED: %s\n' "$what" >&2
		status=1
	fi
}
# A literal backslash-t and backslash-dollar, which is vis(3)'s C style — not
# a raw tab and not a bare '$'.
name_escaped "a tab in a window name arrives as backslash-t" \
	'^%(unlinked-)?window-renamed @[0-9]+ a\\tb$'
name_escaped "a dollar in a session name arrives as backslash-dollar" \
	'^%session-renamed (\$[0-9]+ )?r\\\$HOMEs$'

# --- 5. a raw newline in the last field of a notification -----------------
# tmux ends a notification at the newline and delimits nothing, so any value it
# writes into one that can contain a raw newline becomes a second protocol
# line — outside any block, with nothing on it to say it is not tmux speaking.
# Two of those values are reachable through this package's Subscribe: the name,
# which is the caller's and is therefore checked (see checkSubscribeName), and
# the expanded value, which is tmux's and cannot be.
#
# The remedy documented on Subscribe is the substitution FormatSpec.Arg uses on
# a row, so the last check here is an assertion: if it stopped taking a newline
# out of a subscription value, that documentation would be wrong.
"${T[@]}" set-option -t "$SID" -- @nlprobe "v1"$'\n'"%exit forged-by-a-value"
sub() {
	{
		sleep 0.8
		printf 'refresh-client -B %s\n' "$1"
		sleep 1.6
	} | "${T[@]}" -C attach-session -t "$SID" 2>&1
}

echo
echo "--- a newline in a subscription name, unchecked ---"
# 'sN'"\n"'%exit ...' — tmux's own escape for a newline, spliced between
# single-quoted runs exactly as quoteArg splices one.
sub "'nl'\"\\n\"'%exit forged-by-a-name'::'#{session_id}'" |
	grep -aA1 '^%subscription-changed' | sed 's/^/  /'

echo
echo "--- and in the value, which the name check cannot reach ---"
sub "'v1::#{@nlprobe}'" | grep -aA1 '^%subscription-changed' | sed 's/^/  /'

echo
echo "--- the same value with the newline substituted out (must be one line) ---"
sub "'v2::#{s/'\"\\n\"'/ /:@nlprobe}'" >"$WORK/subfix" 2>&1
grep -aA1 '^%subscription-changed' "$WORK/subfix" | sed 's/^/  /'
if grep -aq '^%subscription-changed v2 .* : v1 %exit forged-by-a-value$' "$WORK/subfix"; then
	echo "  ok: the substitution kept the value on one line"
else
	echo "  ASSERTION FAILED: a substitution no longer takes a newline out of a" >&2
	echo "  subscription value, which is the remedy Subscribe's documentation gives" >&2
	status=1
fi

exit "$status"
