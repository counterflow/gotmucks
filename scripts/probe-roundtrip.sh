#!/usr/bin/env bash
# Probe what this tmux does to a value between setting it and reading it back.
#
# Every question here is the same question: send a value in through a tmux
# command, ask tmux for it again, and see whether the two are the same string.
# On 3.2a they are not, in six separate ways, none of them documented:
#
#   1. show-options escapes a value with vis(3), and "$" is in the set — so a
#      value read, edited and written back gains a backslash every cycle. It
#      also puts a bare backslash in front of a value beginning with "~" and of
#      a value that is one punctuation character, which is not vis at all and
#      is why every sweep here asks each byte in four positions rather than
#      one: the middle-byte shape this script used to test alone cannot see a
#      prefix.
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
#   6. a ':' or a '.' in a *session* name is rewritten to '_' before the name
#      is stored, because both are tmux's own target separators. It is not an
#      encoding: nothing read back undoes it, which is why the package refuses
#      the two bytes rather than reporting a name nobody asked for. Windows are
#      untouched.
#
# And one thing that is not about a value at all but about the argument
# carrying it: tmux's own argv parser takes a trailing ';' off an element and
# ends the command there, so an argument vector is a boundary everywhere except
# at the last byte of an element.
#
# Seven things below are assertions rather than reports, and the script exits
# non-zero if any of them stops holding, because each one is a way this package
# would hand back a value that is not the one that went in:
#
#   A1  every value show-options alters comes back in a shape the package
#       undoes — a vis escape the decoder knows, or the one-backslash prefix
#       unquoteOptionValue takes off. Anything else is a value returned wrong.
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
#   A6  a session name really does lose a ':' and a '.', and a window name
#       really does not. A tmux that stopped would leave the package refusing
#       a name it could perfectly well have set.
#   A7  "\;" is the exact escape for a trailing ';', for every value including
#       one that already ends in backslashes. The package applies it to every
#       argument of every one-shot command.
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

# The escape the package applies to every argument of every one-shot command,
# reproduced here so that the sweeps below can send the values tmux would
# otherwise eat. See section 8, which is what says this is the right rule.
argesc() { case "$1" in *\;) printf '%s\\;' "${1%;}" ;; *) printf '%s' "$1" ;; esac; }

# Strip the quotes tmux adds when a value needs them. Which quote it picks
# depends on what is inside, so both are accepted.
unquote() {
	local v="$1"
	case "$v" in
	\"*\" | \'*\') printf '%s' "${v:1:${#v}-2}" ;;
	*) printf '%s' "$v" ;;
	esac
}

# The value show-options prints for @probe, with the name and the quoting off.
optraw() {
	local v
	v=$("${T[@]}" show-options -t "$SESS" -- @probe)
	unquote "${v#@probe }"
}

echo "=== $("$TMUX_BIN" -V) ==="

"${T[@]}" kill-server 2>/dev/null
SESS=$("${T[@]}" new-session -d -P -F '#{session_id}' -s keeper -x 80 -y 24 -- sleep 600)
sleep 0.3
WIN=$("${T[@]}" list-windows -t "$SESS" -F '#{window_id}' | head -1)

# ---------------------------------------------------------------------------
echo
echo "--- 1. which values show-options alters on the way out, and where (A1) ---"
echo "  every printable byte in four positions — a<b>b, <b>ab, ab<b>, and the"
echo "  byte alone — read back by name and by -v. The middle shape is the one"
echo "  this sweep used to test on its own, and five values are wrong in the"
echo "  other three: tmux prefixes a leading '~' whatever the length, and a"
echo "  value that is a single character needing quotes."

ALTERED=()
PREFIXED=()
UNKNOWN=()
for code in $(seq 32 126); do
	byte=$(printf "\\$(printf '%03o' "$code")")
	for want in "a${byte}b" "${byte}ab" "ab${byte}" "${byte}"; do
		if ! "${T[@]}" set-option -t "$SESS" -- @probe "$(argesc "$want")" 2>/dev/null; then
			fail A1 "set-option refused [$(vis "$want")], which the package can send"
			continue
		fi
		named=$(optraw)
		vflag=$("${T[@]}" show-options -t "$SESS" -v -- @probe)
		# -v prints the value unescaped, so it says what tmux is holding. If
		# that is already wrong the argument never arrived intact and the
		# escaping is not what is being measured.
		if [ "$vflag" != "$want" ]; then
			fail A1 "tmux holds [$(vis "$vflag")] for [$(vis "$want")]; the value did not arrive"
			continue
		fi
		[ "$named" = "$want" ] && continue

		printf '  byte %3d %-6s want [%s] named [%s]\n' \
			"$code" "$(vis "$byte")" "$(vis "$want")" "$(vis "$named")"
		case "$byte" in
		'\' | '"' | "'" | '$')
			# The vis pass, which visDecode undoes. Recorded per byte, since
			# it does not depend on where the byte sits.
			ALTERED+=("$byte")
			continue
			;;
		esac
		if [ "$named" = "\\$want" ]; then
			# args_escape's bare prefix, which unquoteOptionValue takes off.
			PREFIXED+=("$want")
			continue
		fi
		UNKNOWN+=("$want -> $named")
	done
done
printf '  altered by the vis pass: [%s]\n' "$(vis "${ALTERED[*]-}")"
printf '  given the bare prefix:   [%s]\n' "$(vis "${PREFIXED[*]-}")"

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

# visDecode in vis.go undoes the first list and unquoteOptionValue the second.
# Anything else this tmux writes is a value the package hands back wrong.
echo
echo "  alterations the package undoes neither way (must be empty):"
for u in "${UNKNOWN[@]-}"; do
	[ -n "$u" ] || continue
	fail A1 "show-options turned [$(vis "$u")], which is neither a vis escape visDecode knows nor the prefix unquoteOptionValue takes off"
done

echo
echo "  and the prefix in the shapes that carry it, spelled out — a leading"
echo "  tilde at any length, and a single character needing quotes:"
for want in '~/bin' '~' '~ x' 'a~b' '#' '{' '}' ';' '$HOME/bin'; do
	"${T[@]}" set-option -t "$SESS" -- @probe "$(argesc "$want")" 2>/dev/null
	printf '    set [%-9s] named [%-9s] -v [%s]\n' "$(vis "$want")" \
		"$(vis "$(optraw)")" "$(vis "$("${T[@]}" show-options -t "$SESS" -v -- @probe)")"
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

# ---------------------------------------------------------------------------
echo
echo "--- 7. what a session name loses that a window name does not (A6) ---"
echo "  tmux's session_check_name replaces ':' and '.' before the name is"
echo "  stored, because both are its own target separators. It is a rewrite"
echo "  rather than an encoding, so no decoding undoes it and the package"
echo "  refuses the two bytes instead."
printf '  %-18s %-20s %s\n' "sent" "#{session_name}" "#{window_name}"
for n in 'a:b' 'a.b' ':ab' 'ab.' ':' '.' 'web.example.com' 'db:5432' 'a_b'; do
	"${T[@]}" rename-session -t "$SESS" -- "$n" 2>/dev/null
	"${T[@]}" rename-window -t "$WIN" -- "$n" 2>/dev/null
	sname=$(fmt "$SESS" '#{session_name}')
	wname=$(fmt "$WIN" '#{window_name}')
	mark='  '
	[ "$sname" = "$n" ] || mark='**'
	printf '  %s %-16s %-20s %s\n' "$mark" "$(vis "$n")" "$(vis "$sname")" "$(vis "$wname")"
	want=$(printf '%s' "$n" | tr ':.' '__')
	[ "$sname" = "$want" ] ||
		fail A6 "rename-session '$n' stored '$sname', want '$want'"
	[ "$wname" = "$n" ] ||
		fail A6 "rename-window '$n' stored '$wname'; a window name is not a session name"
done
"${T[@]}" rename-session -t "$SESS" -- keeper
"${T[@]}" rename-window -t "$WIN" -- keeper

echo
echo "  new-session -s is the same, and -n is not:"
NS=$("${T[@]}" new-session -d -P -F '#{session_id}' -s 'n:s' -n 'w:n' -x 80 -y 24 -- sleep 600)
sleep 0.2
printf '    -s "n:s" -> [%s]   -n "w:n" -> [%s]\n' \
	"$(vis "$(fmt "$NS" '#{session_name}')")" "$(vis "$(fmt "$NS" '#{window_name}')")"
[ "$(fmt "$NS" '#{session_name}')" = 'n_s' ] || fail A6 "new-session -s no longer rewrites a colon"
[ "$(fmt "$NS" '#{window_name}')" = 'w:n' ] || fail A6 "new-session -n now rewrites a colon"
"${T[@]}" kill-session -t "$NS"

echo
echo "  two consequences that are invisible from the rename itself. A name"
echo "  that collapses onto one already taken fails against a name the caller"
echo "  never used:"
D1=$("${T[@]}" new-session -d -P -F '#{session_id}' -s 'dup_x' -x 80 -y 24 -- sleep 600)
printf '    first  "dup_x" -> [%s]\n' "$D1"
printf '    second "dup:x" -> [%s]\n' \
	"$("${T[@]}" new-session -d -P -F '#{session_id}' -s 'dup:x' -x 80 -y 24 -- sleep 600 2>&1)"
"${T[@]}" kill-session -t "$D1"

echo "  and a rename that collapses onto the name already stored emits no"
echo "  notification at all, since tmux compares the two and returns early —"
echo "  so a caller following the events sees one rename where it made two:"
{
	sleep 0.6
	printf "rename-session -t %s -- 'c:d'\n" "$SESS"
	sleep 0.5
	printf "rename-session -t %s -- 'c.d'\n" "$SESS"
	sleep 0.5
	printf "rename-session -t %s -- 'c-e'\n" "$SESS"
	sleep 0.8
} | "${T[@]}" -C attach-session -t "$SESS" >"$WORK/ren.out" 2>&1
grep -c '^%session-renamed' "$WORK/ren.out" |
	sed 's/^/    %session-renamed lines for three renames: /'
sed -n 's/^%session-renamed/    | %session-renamed/p' "$WORK/ren.out"
"${T[@]}" rename-session -t "$SESS" -- keeper

# ---------------------------------------------------------------------------
echo
echo "--- 8. the one byte that crosses an argv element (A7) ---"
echo "  cmd_parse_from_arguments takes a trailing ';' off an element and ends"
echo "  the command there, so an element that is exactly ';' vanishes and"
echo "  everything after it becomes a second command. Unescaped, through"
echo "  rename-window:"
for n in 'a;' 'a\;' 'a;b' 'a; ' 'a;;'; do
	"${T[@]}" rename-window -t "$WIN" -- "$n" 2>/dev/null
	got=$(fmt "$WIN" '#{window_name}')
	printf '    sent [%-6s] stored [%s]\n' "$(vis "$n")" "$(vis "$got")"
done
"${T[@]}" set-option -t "$SESS" -- status off
"${T[@]}" set-option -t "$SESS" -- status ';' 2>/dev/null
printf '    set-option status ";" left status [%s] — the element vanished and\n' \
	"$("${T[@]}" show-options -t "$SESS" -v -- status)"
printf '    tmux read a set-option with no value as setting the flag on\n'
[ "$("${T[@]}" show-options -t "$SESS" -v -- status)" = 'on' ] ||
	echo "    (this tmux no longer does that, which would make the lone-';' case merely a truncation)"
"${T[@]}" set-option -t "$SESS" -u -- status

echo
echo "  escaped as the package sends it. The rule reads as though a run of"
echo "  backslashes would need doubling, and it does not: tmux strips the"
echo "  final ';' and then rewrites whichever backslash has become last."
for want in 'a;' 'a\;' 'a\\;' 'a;;' ';' ';;' 'a\' 'a;b' 'a; ' 'plain'; do
	sent=$(argesc "$want")
	"${T[@]}" set-option -t "$SESS" -- @semi "$sent" 2>/dev/null
	got=$("${T[@]}" show-options -t "$SESS" -v -- @semi)
	if [ "$got" = "$want" ]; then
		printf '    ok  want [%-6s] sent [%-8s] stored [%s]\n' \
			"$(vis "$want")" "$(vis "$sent")" "$(vis "$got")"
	else
		printf '    **  want [%-6s] sent [%-8s] stored [%s]\n' \
			"$(vis "$want")" "$(vis "$sent")" "$(vis "$got")"
		fail A7 "'\\;' is not the escape for a trailing ';' on this tmux: [$want] stored as [$got]"
	fi
done
"${T[@]}" set-option -t "$SESS" -u -- @semi

echo
echo "  and the control path, which must NOT have the escape: quoteArg quotes"
echo "  a ';' and tmux's control-mode lexer takes it as data."
{
	sleep 0.6
	printf "set-option -t %s -- @ctl 'a;'\n" "$SESS"
	sleep 0.8
} | "${T[@]}" -C attach-session -t "$SESS" >"$WORK/ctl-semi.out" 2>&1
printf '    DoArgs-style quoting -> [%s]\n' \
	"$(vis "$("${T[@]}" show-options -t "$SESS" -v -- @ctl)")"
[ "$("${T[@]}" show-options -t "$SESS" -v -- @ctl)" = 'a;' ] ||
	fail A7 "a quoted ';' no longer survives the control-mode lexer; DoArgs would need the escape too"
"${T[@]}" set-option -t "$SESS" -u -- @ctl

echo
if [ "$FAIL" -ne 0 ]; then
	echo "FAIL: this tmux does not behave the way the package assumes"
	exit 1
fi
echo "OK"
