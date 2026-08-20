#!/usr/bin/env bash
# Probe what this tmux does to a value between setting it and reading it back.
#
# Every question here is the same question: send a value in through a tmux
# command, ask tmux for it again, and see whether the two are the same string.
# On 3.2a they are not, in five separate ways, none of them documented:
#
#   1. show-options escapes a value with vis(3), and "$" is in the set — so a
#      value read, edited and written back gains a backslash every cycle.
#   2. rename-window, rename-session and new-session -s escape the name the
#      same way, and it is the escaped form that "#{window_name}" expands to.
#      new-session -n is the one that does not, so the package escapes that one
#      itself and the two paths agree.
#   3. show-hooks prints an array index on every hook, not only on one set at
#      an index, so the name printed is never the name that was set.
#   4. a raw newline in a value ends the -F line, which no column ordering can
#      recover from — only a substitution can.
#   5. the name argument of rename-window, rename-session, new-session -s and
#      new-session -n is expanded as a format before it is stored — and so is
#      new-session -c, which is a path rather than a name and fails as a wrong
#      directory instead of a wrong name.
#
# Five things below are assertions rather than reports, and the script exits
# non-zero if any of them stops holding, because each one is a way this package
# would hand back a value that is not the one that went in:
#
#   A1  every byte show-options alters is one the decoder undoes; a new escape
#       is a value returned wrong.
#   A2  the five expanded arguments really are format-expanded, and "##"
#       really does stop it — if a tmux stopped expanding, doubling would
#       corrupt every name containing a '#' instead of protecting it. The
#       command vector is asserted from the other side: it must stay
#       unexpanded, since expansion there is a shell running caller data.
#   A3  rename-window escapes a name, so that decoding it is right.
#   A4  new-session -n does not, so that escaping it here is right rather than
#       double.
#   A5  a substitution takes a raw newline out of a value, which is the only
#       thing standing between one pane in an oddly named directory and a
#       failed listing of every pane on the server.
#
#   ./scripts/probe-roundtrip.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-rtprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")
TAB=$(printf '\t')
NL=$'\n'
WORK=$(mktemp -d)
FAIL=0

cleanup() {
	"${T[@]}" kill-server 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

# Render a string with its unprintable bytes visible, so that a backslash the
# probe is asking about cannot be confused with one a terminal invented.
vis() { printf '%s' "$1" | cat -v | sed "s/$TAB/<TAB>/g"; }

fail() {
	printf '  ASSERTION FAILED (%s): %s\n' "$1" "$2"
	FAIL=1
}

# A named format read back off one object.
fmt() { "${T[@]}" display-message -p -t "$1" "$2" 2>&1; }

echo "=== $("$TMUX_BIN" -V) ==="

"${T[@]}" kill-server 2>/dev/null
SESS=$("${T[@]}" new-session -d -P -F '#{session_id}' -s keeper -x 80 -y 24 -- sleep 600)
sleep 0.3
WIN=$("${T[@]}" list-windows -t "$SESS" -F '#{window_id}' | head -1)

# ---------------------------------------------------------------------------
echo
echo "--- 1. which bytes show-options alters on the way out (A1) ---"
echo "  every printable byte, set as a<byte>b, read back by name and by -v"

ALTERED=()
for code in $(seq 32 126); do
	byte=$(printf "\\$(printf '%03o' "$code")")
	want="a${byte}b"
	"${T[@]}" set-option -t "$SESS" -- @probe "$want" || continue
	named=$("${T[@]}" show-options -t "$SESS" -- @probe)
	named=${named#@probe }
	# Strip the quotes tmux adds when a value needs them. Which quote it picks
	# depends on what is inside, so both are accepted.
	case "$named" in
	\"*\" | \'*\') named=${named:1:${#named}-2} ;;
	esac
	vflag=$("${T[@]}" show-options -t "$SESS" -v -- @probe)
	if [ "$named" != "$want" ] || [ "$vflag" != "$want" ]; then
		printf '  byte %3d %-4s named=[%s] -v=[%s]\n' \
			"$code" "$(vis "$byte")" "$(vis "$named")" "$(vis "$vflag")"
		[ "$named" != "$want" ] && ALTERED+=("$byte")
	fi
done
printf '  altered in the named form: [%s]\n' "$(vis "${ALTERED[*]-}")"

echo
echo "  and the bytes below space, which cannot be compared as text:"
for code in 1 9 13 27 127; do
	byte=$(printf "\\$(printf '%03o' "$code")")
	"${T[@]}" set-option -t "$SESS" -- @probe "a${byte}b"
	printf '  byte %3d  named=[%s]\n' "$code" \
		"$(vis "$(v=$("${T[@]}" show-options -t "$SESS" -- @probe) && printf '%s' "${v#@probe }")")"
done
# A newline has to go in through a variable rather than $(), which eats it.
"${T[@]}" set-option -t "$SESS" -- @probe "a${NL}b"
printf '  byte  10  named=[%s]\n' \
	"$(vis "$(v=$("${T[@]}" show-options -t "$SESS" -- @probe) && printf '%s' "${v#@probe }")")"

# visDecode in vis.go knows these. Anything else this tmux writes is a byte the
# package hands back wrong.
echo
echo "  altered bytes the package's decoder does not undo (must be empty):"
for byte in "${ALTERED[@]-}"; do
	case "$byte" in
	'' | '\' | '"' | "'" | '$') ;;
	*) fail A1 "show-options escapes [$(vis "$byte")], which visDecode has no case for" ;;
	esac
done

# ---------------------------------------------------------------------------
echo
echo "--- 2. does the escape compound across read-modify-write? ---"
"${T[@]}" set-option -t "$SESS" -- @cycle 'PATH=$HOME/bin'
printf '  set     [%s]\n' 'PATH=$HOME/bin'
for i in 1 2 3 4; do
	v=$("${T[@]}" show-options -t "$SESS" -- @cycle)
	v=${v#@cycle }
	case "$v" in \"*\") v=${v:1:${#v}-2} ;; esac
	printf '  read %d  [%s]\n' "$i" "$(vis "$v")"
	# Write back what was read, which is what a caller editing one element of a
	# value has no choice but to do. This is the raw form, so it compounds
	# faster here than it did through the package, which undid "\\" already.
	"${T[@]}" set-option -t "$SESS" -- @cycle "$v"
done

echo
echo "  a set-option value is not itself format-expanded, which is why only"
echo "  the name arguments need escaping:"
"${T[@]}" set-option -t "$SESS" -- @fmt 'v#{host}'
printf '    set [v#{host}] reads back [%s]\n' "$("${T[@]}" show-options -t "$SESS" -v -- @fmt)"

# ---------------------------------------------------------------------------
echo
echo "--- 3. window and session names: what does the format expand to? (A3) ---"
printf '  %-14s %-22s %s\n' "sent" "#{window_name}" "#{session_name}"
probe_name() {
	"${T[@]}" rename-window -t "$WIN" -- "$2"
	"${T[@]}" rename-session -t "$SESS" -- "$2"
	printf '  %-14s %-22s %s\n' "$1" \
		"$(vis "$(fmt "$WIN" '#{window_name}')")" \
		"$(vis "$(fmt "$SESS" '#{session_name}')")"
}
probe_name 'a<TAB>b' "a${TAB}b"
probe_name 'a<NL>b' "a${NL}b"
probe_name 'a\b' 'a\b'
probe_name 'a<0x01>b' "$(printf 'a\001b')"
probe_name 'a$b' 'a$b'
probe_name '$HOME' '$HOME'
probe_name 'a b' 'a b'
probe_name 'plain' 'plain'

"${T[@]}" rename-window -t "$WIN" -- "a${TAB}b"
[ "$(fmt "$WIN" '#{window_name}')" = 'a\tb' ] ||
	fail A3 "rename-window no longer escapes a tab; visDecode would corrupt every name"
"${T[@]}" rename-window -t "$WIN" -- 'a\b'
[ "$(fmt "$WIN" '#{window_name}')" = 'a\\b' ] ||
	fail A3 "rename-window no longer escapes a backslash; visDecode would corrupt one"
"${T[@]}" rename-session -t "$SESS" -- keeper

echo
echo "  and through new-session -s and -n, where -n is the one that does not"
echo "  escape (A4):"
NS=$("${T[@]}" new-session -d -P -F '#{session_id}' -s "n${TAB}s" -n "w${TAB}n" -x 80 -y 24 -- sleep 600)
sleep 0.2
printf '    -s "n<TAB>s" -> [%s]\n' "$(vis "$(fmt "$NS" '#{session_name}')")"
printf '    -n "w<TAB>n" -> [%s]\n' "$(vis "$(fmt "$NS" '#{window_name}')")"
[ "$(fmt "$NS" '#{session_name}')" = 'n\ts' ] ||
	fail A4 "new-session -s no longer escapes a tab"
[ "$(fmt "$NS" '#{window_name}')" = "w${TAB}n" ] ||
	fail A4 "new-session -n now escapes a tab; visEncode would double the escaping"
"${T[@]}" kill-session -t "$NS"

# ---------------------------------------------------------------------------
echo
echo "--- 4. what does show-hooks print for a hook set with no index? ---"
"${T[@]}" set-hook -t "$SESS" -- alert-bell 'display-message hi'
"${T[@]}" set-hook -g -- session-created 'display-message made'
printf '  set-hook -t %s -- alert-bell    -> [%s]\n' "$SESS" \
	"$("${T[@]}" show-hooks -t "$SESS" | head -1)"
printf '  set-hook -g -- session-created  -> [%s]\n' \
	"$("${T[@]}" show-hooks -g | grep session-created | head -1)"
"${T[@]}" set-hook -a -t "$SESS" -- alert-bell 'display-message again'
echo "  and after set-hook -a, which is what makes the index load-bearing:"
"${T[@]}" show-hooks -t "$SESS" | grep alert-bell | sed 's/^/    /'
echo "  a hook command is a command line, not a value — tmux re-serialises it:"
"${T[@]}" set-hook -u -t "$SESS" -- alert-bell
"${T[@]}" set-hook -t "$SESS" -- alert-bell "display-message \"a${TAB}b\""
printf '    set [display-message "a<TAB>b"] -> [%s]\n' \
	"$(vis "$("${T[@]}" show-hooks -t "$SESS" | head -1)")"
"${T[@]}" set-hook -u -t "$SESS" -- alert-bell
"${T[@]}" set-hook -gu -- session-created

# ---------------------------------------------------------------------------
echo
echo "--- 5. a newline in pane_current_path splits the -F row (A5) ---"
mkdir -p "$WORK/two${NL}li${TAB}nes"
"${T[@]}" new-window -d -t "$SESS" -c "$WORK/two${NL}li${TAB}nes" -- sleep 600
sleep 0.3
echo "  asked for as it stands, one output line per row:"
"${T[@]}" list-panes -a -F "#{pane_id}${TAB}#{pane_current_path}" | sed 's/^/    | /'
echo "  with the newline substituted out, which is what FormatSpec.Arg does:"
"${T[@]}" list-panes -a -F "#{pane_id}${TAB}#{s/${NL}/ /:pane_current_path}" | sed 's/^/    | /'
echo "  and with the tab as well, which is every column but the last:"
"${T[@]}" list-panes -a -F "#{pane_id}${TAB}#{s/${TAB}/ /;s/${NL}/ /:pane_current_path}" | sed 's/^/    | /'

rows=$("${T[@]}" list-panes -a -F "#{pane_id}${TAB}#{s/${NL}/ /:pane_current_path}" | wc -l)
panes=$("${T[@]}" list-panes -a -F '#{pane_id}' | wc -l)
if [ "$rows" -ne "$panes" ]; then
	fail A5 "the newline substitution left $rows lines for $panes panes; every listing would fail"
fi

# ---------------------------------------------------------------------------
echo
echo "--- 6. is a name argument expanded as a format? (A2) ---"
expands() {
	local what="$1" got="$2" want="$3"
	if [ "$got" = "$want" ]; then
		printf '  ** %-34s -> [%s]  NOT EXPANDED\n' "$what" "$(vis "$got")"
		fail A2 "$what no longer expands its argument; doubling the '#' would corrupt it"
	else
		printf '     %-34s -> [%s]\n' "$what" "$(vis "$got")"
	fi
}

"${T[@]}" rename-window -t "$WIN" -- 'v#{host}'
expands "rename-window  'v#{host}'" "$(fmt "$WIN" '#{window_name}')" 'v#{host}'
"${T[@]}" rename-session -t "$SESS" -- 'v#{host}'
expands "rename-session 'v#{host}'" "$(fmt "$SESS" '#{session_name}')" 'v#{host}'
"${T[@]}" rename-session -t "$SESS" -- keeper

NS=$("${T[@]}" new-session -d -P -F '#{session_id}' -s 'n#{host}' -n 'w#{host}' -x 80 -y 24 -- sleep 600)
sleep 0.2
expands "new-session -s 'n#{host}'" "$(fmt "$NS" '#{session_name}')" 'n#{host}'
expands "new-session -n 'w#{host}'" "$(fmt "$NS" '#{window_name}')" 'w#{host}'
"${T[@]}" kill-session -t "$NS"

# The fifth argument, and the one round nine found missing: new-session -c is
# expanded too. A path is not a name, so the failure is not a wrong name but a
# wrong directory — tmux expands, finds no such place, falls back to the home
# directory and exits 0 with nothing on stderr.
mkdir -p "$WORK/dir#Hhere" "$WORK/dir##Hhere"
NS=$("${T[@]}" new-session -d -P -F '#{session_id}' -c "$WORK/dir#Hhere" -x 80 -y 24 -- sleep 600)
sleep 0.3
expands "new-session -c '...#Hhere'" "$(fmt "$NS" '#{pane_current_path}')" "$WORK/dir#Hhere"
"${T[@]}" kill-session -t "$NS"

NS=$("${T[@]}" new-session -d -P -F '#{session_id}' -c "$WORK/dir##Hhere" -x 80 -y 24 -- sleep 600)
sleep 0.3
got=$(fmt "$NS" '#{pane_current_path}')
printf '     %-34s -> [%s]\n' "new-session -c doubled" "$(vis "$got")"
[ "$got" = "$WORK/dir#Hhere" ] ||
	fail A2 "new-session -c with '##H' gave '$got', want '$WORK/dir#Hhere'"
"${T[@]}" kill-session -t "$NS"

echo
echo "  and doubling the '#' stops it, on every form:"
for pair in 'v##{host}:v#{host}' 'a##Hb:a#Hb' 'a#b:a#b' 'plain:plain'; do
	sent=${pair%%:*}
	want=${pair#*:}
	"${T[@]}" rename-window -t "$WIN" -- "$sent"
	got=$(fmt "$WIN" '#{window_name}')
	printf '     rename-window %-12s -> [%s]\n' "$sent" "$(vis "$got")"
	[ "$got" = "$want" ] || fail A2 "rename-window '$sent' gave '$got', want '$want'"
done

echo
echo "  the #() form, which hands the argument to a shell. Whether the job"
echo "  runs is a fact about the client rather than about the argument: a job"
echo "  belongs to the client that asked for the expansion, and a one-shot"
echo "  tmux can exit before its own job runs. So the one-shot answer says"
echo "  nothing about the control-mode one, and it is the control-mode one"
echo "  that matters — a control client stays alive. Round nine found this"
echo "  section testing only the case where the job does not run."
rm -f "$WORK/ran"
"${T[@]}" rename-window -t "$WIN" -- "#(touch $WORK/ran; echo RAN)"
for _ in 1 2 3 4 5; do
	fmt "$WIN" '#{window_name}' >/dev/null
	sleep 0.4
done
printf '     window_name = [%s]\n' "$(vis "$(fmt "$WIN" '#{window_name}')")"
ranmark() {
	if [ -e "$WORK/$1" ]; then
		printf '  ** %-30s THE JOB RAN\n' "$2:"
	else
		printf '     %-30s the job did not run\n' "$2:"
	fi
}
ranmark ran "one-shot rename-window"
"${T[@]}" rename-window -t "$WIN" -- keeper

# One-shot new-session -s, which on 3.2a does run it where rename-window does
# not — so "the job does not run one-shot" is not a property of the path.
rm -f "$WORK/ran-s"
NS=$("${T[@]}" new-session -d -P -F '#{session_id}' \
	-s "#(touch $WORK/ran-s; echo RAN)" -x 80 -y 24 -- sleep 600 2>/dev/null)
sleep 1.5
ranmark ran-s "one-shot new-session -s"
[ -n "$NS" ] && "${T[@]}" kill-session -t "$NS" 2>/dev/null

# The same three down a control connection, which is the client that stays
# alive. This is where an unescaped '#' in caller data is arbitrary command
# execution rather than a wrong name.
rm -f "$WORK/ran-ctl" "$WORK/ran-ctl-c" "$WORK/ran-ctl-s"
{
	sleep 0.6
	printf "rename-window -t %s -- '#(touch %s/ran-ctl; echo RAN)'\n" "$WIN" "$WORK"
	sleep 1.0
	printf "new-session -d -c '#(touch %s/ran-ctl-c; echo %s)' -x 80 -y 24 -- sleep 600\n" "$WORK" "$WORK"
	sleep 1.0
	printf "new-session -d -s '#(touch %s/ran-ctl-s; echo RAN)' -x 80 -y 24 -- sleep 600\n" "$WORK"
	sleep 2.0
} | "${T[@]}" -C attach-session -t "$SESS" >"$WORK/ctl-job.out" 2>&1
ranmark ran-ctl "control rename-window"
ranmark ran-ctl-c "control new-session -c"
ranmark ran-ctl-s "control new-session -s"
"${T[@]}" rename-window -t "$WIN" -- keeper
# The sessions those two new-session lines made are named after a job. Kill
# everything but the keeper rather than trying to name them.
for s in $("${T[@]}" list-sessions -F '#{session_id}' 2>/dev/null); do
	[ "$s" = "$SESS" ] || "${T[@]}" kill-session -t "$s" 2>/dev/null
done

echo
echo "  arguments that are NOT expanded, which is what bounds the escape to"
echo "  those five:"
"${T[@]}" set-option -t "$SESS" -- @noexpand 'v#{host}'
printf '     set-option value  -> [%s]\n' "$("${T[@]}" show-options -t "$SESS" -v -- @noexpand)"
"${T[@]}" set-hook -t "$SESS" -- alert-bell "display-message 'v#{host}'"
printf '     set-hook command  -> [%s]\n' "$("${T[@]}" show-hooks -t "$SESS" | head -1)"
"${T[@]}" set-hook -u -t "$SESS" -- alert-bell

# The other two arguments new-session takes from a caller. Neither is expanded
# on 3.2a, and the command vector is an assertion because it is the one where
# expansion would be a shell running a caller's data.
rm -f "$WORK/argv"
NS=$("${T[@]}" new-session -d -P -F '#{session_id}' -e 'PROBE=v#{host}' -x 80 -y 24 -- \
	sh -c 'printf "%s" "$1" >"$0"; sleep 600' "$WORK/argv" 'v#{host}')
sleep 0.6
printf '     new-session -e    -> [%s]\n' \
	"$(vis "$("${T[@]}" show-environment -t "$NS" PROBE 2>/dev/null)")"
printf '     the -- vector     -> [%s]\n' "$(vis "$(cat "$WORK/argv" 2>/dev/null)")"
[ "$(cat "$WORK/argv" 2>/dev/null)" = 'v#{host}' ] ||
	fail A2 "new-session's command vector is expanded on this tmux; it was not on 3.2a"
"${T[@]}" kill-session -t "$NS"

echo
if [ "$FAIL" -ne 0 ]; then
	echo "FAIL: this tmux does not behave the way the package assumes"
	exit 1
fi
echo "OK"
