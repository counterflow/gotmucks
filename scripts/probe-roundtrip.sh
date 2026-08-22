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
#      an index, so the name printed is never the name that was set. And it
#      reads one option table per invocation while set-hook writes to whichever
#      table the hook's *name* belongs to, so "show-hooks -t" alone cannot see
#      a hook it just watched being set: on 3.2a every pane-* and window-* name
#      goes to the window table.
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
# And one thing that is not about a value either but about the *name* of one.
# A reply from show-options or show-hooks is "name value" separated by a space,
# and the name is the single field on those lines that tmux does not escape —
# so a name containing the separator is read as a shorter name and a longer
# value, and one containing a newline arrives as two lines of which the second
# is an option that does not exist. That is why the package refuses those bytes
# in a name rather than decoding them, and A9 is what says the refusal is still
# the right shape.
#
# And one thing that is about neither, but about the third argument of the same
# call: the SCOPE. Which table tmux keeps something in is a claim of its own,
# and asking it of a hook name is how round eleven found that show-hooks reads
# one table while set-hook writes by the name. A plain option does exactly the
# same thing — set-option and the named form of show-options both follow the
# name, while the listing form and a user option follow the flag — so an option
# set at one scope is missing from the listing of that scope, silently. That is
# A10, and it is why the section numbering here goes to ten.
#
# Ten things below are assertions rather than reports, and the script exits
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
#   A8  every hook name this tmux knows is reported by exactly one of the three
#       show-hooks scopes after being set. Exactly one, both ways: none is
#       what ShowHooks reading a single table did to every window hook, and
#       more than one would make merging the three ambiguous.
#   A9  an option name comes back as itself. Every printable byte in the four
#       positions, and the only ones that break the "name value" split are the
#       ones checkOptionName refuses — so a tmux that started escaping a name,
#       or one that stopped, is caught here rather than in a caller's map.
#   A10 a known option name ignores the scope flag, in both the write and the
#       named read, while the listing form and a user option obey it. Swept
#       over every name in this binary's tables for the read half. A tmux that
#       started honouring the flag would leave the three doc comments that now
#       describe this wrong the other way round.
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

echo
echo "  being a command line rather than a value is also why the trailing-';'"
echo "  escape does not reach a hook body: the byte arrives intact and"
echo "  set-hook's own parser then reads it as the command separator. And the"
echo "  body is lexed when the hook is SET, so a '~' in double quotes is"
echo "  expanded then rather than when it fires."
for body in 'display-message hi;' 'display-message hi ;' 'display-message "~/bin"' "display-message '~/bin'"; do
	"${T[@]}" set-hook -t "$SESS" -- alert-bell "$(argesc "$body")"
	printf '    set [%-28s] -> [%s]\n' "$(vis "$body")" \
		"$(vis "$("${T[@]}" show-hooks -t "$SESS" | head -1)")"
done
"${T[@]}" set-hook -u -t "$SESS" -- alert-bell

echo
echo "  which option table does each hook name land in? (A8)"
echo "  set-hook writes to the table the NAME belongs to, whatever target it"
echo "  is given, and show-hooks reads one table per invocation — so reading"
echo "  only the session table reported nothing for every window hook, however"
echo "  successfully it had been set. The names come from tmux itself: the"
echo "  global tables print every hook this binary knows."
hook_names() {
	{
		"${T[@]}" show-hooks -g 2>/dev/null
		"${T[@]}" show-hooks -g -w 2>/dev/null
		"${T[@]}" show-hooks -g -p 2>/dev/null
	} | sed 's/ .*//; s/\[[0-9]*\]$//' | sort -u
}
NAMES=$(hook_names)
printf '  %d hook names, in tables:\n' "$(printf '%s\n' "$NAMES" | wc -l)"
SESSION_HOOKS=0
WINDOW_HOOKS=0
PANE_HOOKS=0
for h in $NAMES; do
	[ -n "$h" ] || continue
	if ! "${T[@]}" set-hook -t "$SESS" -- "$h" 'display-message probe' 2>/dev/null; then
		fail A8 "set-hook refused [$h], which this tmux printed as a hook name"
		continue
	fi
	where=""
	count=0
	for scope in "" "-w" "-p"; do
		# shellcheck disable=SC2086
		if "${T[@]}" show-hooks $scope -t "$SESS" 2>/dev/null | grep -q "^$h\["; then
			where="$where ${scope:--t}"
			count=$((count + 1))
		fi
	done
	case "$count" in
	0) fail A8 "hook [$h] was set and no show-hooks scope reports it; ShowHooks would lose it" ;;
	1) : ;;
	*) fail A8 "hook [$h] is reported by more than one scope ($where); merging the three is ambiguous" ;;
	esac
	case "$where" in
	*' -t'*) SESSION_HOOKS=$((SESSION_HOOKS + 1)) ;;
	*' -w'*) WINDOW_HOOKS=$((WINDOW_HOOKS + 1)) ;;
	*' -p'*) PANE_HOOKS=$((PANE_HOOKS + 1)) ;;
	esac
	"${T[@]}" set-hook -u -t "$SESS" -- "$h" 2>/dev/null
done
printf '    session table %d   window table %d   pane table %d\n' \
	"$SESSION_HOOKS" "$WINDOW_HOOKS" "$PANE_HOOKS"
[ "$WINDOW_HOOKS" -gt 0 ] ||
	echo "    (no window hooks on this tmux; the -w scope costs a process and finds nothing)"

echo
echo "  a hook set to an empty command is printed exactly as an unset one is,"
echo "  which is why the package refuses one — a bare name can then only mean"
echo "  'not set', and that is what makes the global tables readable at all:"
"${T[@]}" set-hook -t "$SESS" -- alert-bell '' 2>/dev/null
printf '    set-hook alert-bell ""  -> [%s]\n' "$("${T[@]}" show-hooks -t "$SESS" | head -1)"
"${T[@]}" set-hook -u -t "$SESS" -- alert-bell 2>/dev/null
printf '    show-hooks -g   prints %s names, %s of them set\n' \
	"$("${T[@]}" show-hooks -g | wc -l)" "$("${T[@]}" show-hooks -g | grep -c ' ')"
printf '    show-hooks -g -w prints %s names, %s of them set\n' \
	"$("${T[@]}" show-hooks -g -w | wc -l)" "$("${T[@]}" show-hooks -g -w | grep -c ' ')"

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

# ---------------------------------------------------------------------------
echo
echo "--- 9. the same sweep asked of a NAME rather than a value (A9) ---"
echo "  everything above asks what tmux does to a byte in a value. The twin"
echo "  question is what happens to the DELIMITER a reply is parsed by when"
echo "  the name contains it: show-options prints 'name value', escapes the"
echo "  value and prints the name raw, and a user option's name is whatever"
echo "  the caller passed. So the question is not what tmux stores but where"
echo "  the first space in the printed line falls."

NAMEBAD=()
for code in $(seq 32 126); do
	byte=$(printf "\\$(printf '%03o' "$code")")
	for name in "@a${byte}b" "@${byte}ab" "@ab${byte}" "@${byte}"; do
		"${T[@]}" set-option -t "$SESS" -- "$(argesc "$name")" V 2>/dev/null || continue
		line=$("${T[@]}" show-options -t "$SESS" -- "$(argesc "$name")" 2>/dev/null)
		"${T[@]}" set-option -t "$SESS" -u -- "$(argesc "$name")" 2>/dev/null
		# The parse both readers do: everything up to the first space is the
		# name. Anything else means the caller's name is not the map's key.
		[ "${line%% *}" = "$name" ] && [ "${line#* }" = "V" ] && continue
		printf '  byte %3d %-6s name [%s] printed [%s]\n' \
			"$code" "$(vis "$byte")" "$(vis "$name")" "$(vis "$line")"
		NAMEBAD+=("$name")
	done
done

echo
echo "  names the split cannot recover, which must be exactly the ones"
echo "  checkOptionName refuses — a byte at or below space:"
for n in "${NAMEBAD[@]-}"; do
	[ -n "$n" ] || continue
	printf '    [%s]\n' "$(vis "$n")"
	case "$n" in
	*' '*) : ;; # the space, which the package refuses
	*) fail A9 "option name [$(vis "$n")] is not read back as itself and contains no byte checkOptionName refuses" ;;
	esac
done

echo
echo "  and the newline, which is not a split but a second line — the"
echo "  remainder becomes a row of show-options output that was never an"
echo "  option, keyed as the caller chose:"
"${T[@]}" set-option -t "$SESS" -- "@nl${NL}injected value" V 2>/dev/null
"${T[@]}" show-options -t "$SESS" | grep -n 'injected\|^@nl' | sed 's/^/    | /'
lines=$("${T[@]}" show-options -t "$SESS" | grep -c 'injected')
[ "$lines" -ge 1 ] ||
	echo "    (this tmux no longer prints an option name raw; the refusal could be narrowed)"
"${T[@]}" set-option -t "$SESS" -u -- "@nl${NL}injected value" 2>/dev/null

echo
echo "  a hook name is the same field on the same kind of line, and tmux"
echo "  refuses most of them itself because the name has to be one it knows:"
"${T[@]}" set-hook -t "$SESS" -- 'alert-bell x' 'display-message x' 2>&1 | sed 's/^/    /'
echo "  the exception is a '@' name, which it files as a user option and never"
echo "  fires — nothing reads it back as a hook:"
"${T[@]}" set-hook -t "$SESS" -- '@nothook' 'display-message x' 2>/dev/null
printf '    show-hooks   -> [%s]\n' "$("${T[@]}" show-hooks -t "$SESS" | tr '\n' '|')"
printf '    show-options -> [%s]\n' "$("${T[@]}" show-options -t "$SESS" -- '@nothook')"
"${T[@]}" set-option -t "$SESS" -u -- '@nothook' 2>/dev/null

# ---------------------------------------------------------------------------
echo
echo "--- 10. the table question of section 4, asked of an option (A10) ---"
echo "  set-hook files a hook by its NAME rather than by the target, and"
echo "  section 4 is what pins that. Plain options do the same thing and it"
echo "  went unasked a round longer: set-option ignores the scope flag for a"
echo "  name tmux knows, and so does the NAMED form of show-options. Only the"
echo "  listing form and a user option obey it — which is why SetOption and"
echo "  ShowOptions are not inverses, silently."

echo
echo "  first the sweep: for a name it knows, does the flag change the answer?"
echo "  the global tables have every option set, so this asks the whole table"
echo "  without writing anything."
sess_names=$("${T[@]}" show-options -g | sed 's/\[[0-9]*\]//; s/ .*//' | sort -u)
win_names=$("${T[@]}" show-options -g -w | sed 's/\[[0-9]*\]//; s/ .*//' | sort -u)
srv_names=$("${T[@]}" show-options -s | sed 's/\[[0-9]*\]//; s/ .*//' | sort -u)
printf '    global session table %s names, global window table %s, server %s\n' \
	"$(printf '%s\n' "$sess_names" | wc -l)" \
	"$(printf '%s\n' "$win_names" | wc -l)" \
	"$(printf '%s\n' "$srv_names" | wc -l)"
IGNORED=0
for n in $sess_names $win_names; do
	[ -n "$n" ] || continue
	a=$("${T[@]}" show-options -g -- "$n" 2>&1 | head -1)
	b=$("${T[@]}" show-options -g -w -- "$n" 2>&1 | head -1)
	if [ "$a" = "$b" ]; then
		IGNORED=$((IGNORED + 1))
		continue
	fi
	fail A10 "named show-options for [$n] depends on the flag: -g [$a] -g -w [$b]"
done
for n in $srv_names; do
	[ -n "$n" ] || continue
	a=$("${T[@]}" show-options -s -- "$n" 2>&1 | head -1)
	b=$("${T[@]}" show-options -- "$n" 2>&1 | head -1)
	if [ "$a" = "$b" ]; then
		IGNORED=$((IGNORED + 1))
		continue
	fi
	fail A10 "named show-options for server option [$n] depends on the flag: -s [$a] plain [$b]"
done
printf '    %d names answer the same whatever flag they are asked with\n' "$IGNORED"

echo
echo "  then the write, which is the half a caller sees: a window option set"
echo "  through a session target, with no window flag anywhere."
"${T[@]}" set-option -t "$SESS" -- remain-on-exit on || fail A10 "set-option refused a window option at session scope"
printf '    show-options    -t -- remain-on-exit : [%s]\n' "$("${T[@]}" show-options -t "$SESS" -- remain-on-exit)"
printf '    show-options    -t   (listing)       : [%s]\n' "$("${T[@]}" show-options -t "$SESS" | grep '^remain-on-exit' || echo '<absent>')"
printf '    show-options -w -t   (listing)       : [%s]\n' "$("${T[@]}" show-options -w -t "$SESS" | grep '^remain-on-exit' || echo '<absent>')"
[ -n "$("${T[@]}" show-options -t "$SESS" -- remain-on-exit)" ] ||
	fail A10 "the named form cannot see an option set at session scope; ShowOption would report it unset"
[ -z "$("${T[@]}" show-options -t "$SESS" | grep '^remain-on-exit')" ] ||
	fail A10 "the session listing now holds a window option; ShowOptions' doc says it cannot"
[ -n "$("${T[@]}" show-options -w -t "$SESS" | grep '^remain-on-exit')" ] ||
	fail A10 "the window listing does not hold it either; the option is somewhere neither reader looks"

echo "  and set-option -u follows the name to the same table, so the two"
echo "  writers stay inverses however they are scoped:"
"${T[@]}" set-option -u -t "$SESS" -- remain-on-exit
printf '    show-options -w -t after -u          : [%s]\n' "$("${T[@]}" show-options -w -t "$SESS" | grep '^remain-on-exit' || echo '<absent>')"
[ -z "$("${T[@]}" show-options -w -t "$SESS" | grep '^remain-on-exit')" ] ||
	fail A10 "set-option -u at session scope did not reach the window table set-option wrote to"

echo
echo "  a USER option is the other way round: no name for tmux to follow, so"
echo "  the flag is the whole of where it lives."
"${T[@]}" set-option -w -t "$SESS" -- @scoped window-value
printf '    show-options    -t -- @scoped        : [%s]\n' "$("${T[@]}" show-options -t "$SESS" -- @scoped)"
printf '    show-options -w -t -- @scoped        : [%s]\n' "$("${T[@]}" show-options -w -t "$SESS" -- @scoped)"
[ -z "$("${T[@]}" show-options -t "$SESS" -- @scoped)" ] ||
	fail A10 "a user option set with -w is visible without it; the scope no longer places one"
[ -n "$("${T[@]}" show-options -w -t "$SESS" -- @scoped)" ] ||
	fail A10 "a user option set with -w is invisible with it"
"${T[@]}" set-option -u -w -t "$SESS" -- @scoped 2>/dev/null

echo
if [ "$FAIL" -ne 0 ]; then
	echo "FAIL: this tmux does not behave the way the package assumes"
	exit 1
fi
echo "OK"
