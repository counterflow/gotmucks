#!/usr/bin/env bash
# Probe which format variables this tmux hands back with a raw tab in them.
#
# A -F template is expanded verbatim, so the tab this package separates fields
# with is only a separator for values that cannot contain one. ParseRows folds
# an overflowing field into the last column, which recovers the value when the
# last column is the one that overflowed and silently shifts every column after
# it when some other one did. Which variables can overflow is therefore what
# says how a FormatSpec may be ordered, and it is not written down anywhere: it
# depends on whether the value passed through a tmux command on its way in
# (those are escaped with vis(3)) or came from the operating system or from the
# program running in the pane (those are not).
#
#   ./scripts/probe-tabs.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-tabprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")
TAB=$(printf '\t')
WORK=$(mktemp -d)

cleanup() {
	"${T[@]}" kill-server 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

# Show a value with its tabs made visible, and say whether it had any.
show() {
	local label="$1" target="$2" fmt="$3" val raw=no
	val=$("${T[@]}" display-message -p -t "$target" "$fmt" 2>&1)
	case "$val" in *"$TAB"*) raw=yes ;; esac
	printf '  %-38s raw-tab=%-4s [%s]\n' "$label" "$raw" "$(printf '%s' "$val" | sed "s/$TAB/<TAB>/g")"
}

echo "=== $("$TMUX_BIN" -V) ==="

"${T[@]}" kill-server 2>/dev/null

mkdir -p "$WORK/bin" "$WORK/dir${TAB}name"
cp "$(command -v sleep)" "$WORK/bin/ab${TAB}cd"

# Names go in through a tmux command, so this is the escaping side of the
# question.
"${T[@]}" new-session -d -s "sess${TAB}name" -n "win${TAB}name" -x 80 -y 24 -- sleep 300
sleep 0.3
SESS=$("${T[@]}" list-sessions -F '#{session_id}' | head -1)
WIN=$("${T[@]}" list-windows -t "$SESS" -F '#{window_id}' | head -1)

# A pane whose working directory contains a tab, and one whose binary's own
# name does. Both values come from the operating system by way of /proc.
"${T[@]}" new-window -t "$SESS" -c "$WORK/dir${TAB}name" -- sleep 300
PANE_PATH=$("${T[@]}" list-panes -s -t "$SESS" -F '#{pane_id}' | sed -n 2p)
"${T[@]}" new-window -t "$SESS" -- "$WORK/bin/ab${TAB}cd" 300
PANE_CMD=$("${T[@]}" list-panes -s -t "$SESS" -F '#{pane_id}' | sed -n 3p)

# A pane whose program sets its own title with OSC 2, which is the vector that
# matters for pane_title: the title is whatever the program in the pane says it
# is. The space-only title is the control — it says the escape sequence
# arrived, so that a missing tab means tmux dropped it rather than that the
# probe never sent one.
"${T[@]}" new-window -t "$SESS" -- sh -c "printf '\033]2;osc${TAB}title\033\\'; sleep 300"
PANE_OSC=$("${T[@]}" list-panes -s -t "$SESS" -F '#{pane_id}' | sed -n 4p)
"${T[@]}" new-window -t "$SESS" -- sh -c "printf '\033]2;osc title\033\\'; sleep 300"
PANE_OSC2=$("${T[@]}" list-panes -s -t "$SESS" -F '#{pane_id}' | sed -n 5p)
sleep 0.5

echo
echo "--- values as tmux expands them ---"
show "session_name"                    "$SESS"       '#{session_name}'
show "window_name"                     "$WIN"        '#{window_name}'
show "pane_current_path"               "$PANE_PATH"  '#{pane_current_path}'
show "pane_current_command"            "$PANE_CMD"   '#{pane_current_command}'
show "pane_title (OSC 2, with a tab)"  "$PANE_OSC"   '#{pane_title}'
show "pane_title (OSC 2, control)"     "$PANE_OSC2"  '#{pane_title}'

echo
echo "--- select-pane -T with a tab: refused, or accepted and dropped? ---"
"${T[@]}" select-pane -t "$PANE_PATH" -T 'plain'
show "after -T 'plain'"                "$PANE_PATH"  '#{pane_title}'
"${T[@]}" select-pane -t "$PANE_PATH" -T "aa${TAB}bb"
printf '  select-pane -T with a tab exited %s\n' "$?"
show "after -T 'aa<TAB>bb'"            "$PANE_PATH"  '#{pane_title}'
"${T[@]}" select-pane -t "$PANE_PATH" -T 'aa bb'
show "after -T 'aa bb'"                "$PANE_PATH"  '#{pane_title}'

echo
echo "--- the substitution modifier that takes a tab out ---"
# A character class is not available: the ':' inside "[[:cntrl:]]" ends the
# modifier as far as the format parser is concerned, and the whole expression
# then expands to nothing rather than failing. A literal tab in the pattern
# works. Both forms are printed so the difference is on the record.
show "s/<TAB>/ /  command"             "$PANE_CMD"   "#{s/${TAB}/ /:pane_current_command}"
show "s/<TAB>/ /  path"                "$PANE_PATH"  "#{s/${TAB}/ /:pane_current_path}"
show "s/<TAB>/ /  title"               "$PANE_OSC"   "#{s/${TAB}/ /:pane_title}"
show "s/[[:cntrl:]]/ /  command"       "$PANE_CMD"   '#{s/[[:cntrl:]]/ /:pane_current_command}'
show "s/\t/ /  command"                "$PANE_CMD"   '#{s/\t/ /:pane_current_command}'

echo
echo "--- field counts on the pane spec, raw and as this package sends it ---"
COMMON='#{pane_id}	#{window_id}	#{session_id}	#{pane_index}	#{pane_active}	#{pane_dead}	#{pane_pid}	#{pane_width}	#{pane_height}'
RAW="$COMMON	#{pane_current_command}	#{pane_title}	#{pane_current_path}"
SAFE="$COMMON	#{s/${TAB}/ /:pane_current_command}	#{s/${TAB}/ /:pane_title}	#{pane_current_path}"
echo "  the spec has 12 fields; a row with more has had one overflow"
"${T[@]}" list-panes -s -t "$SESS" -F "$RAW" |
	awk -F'\t' '{printf "  raw       pane %-4s fields=%-3d command=[%s] title=[%s]\n", $1, NF, $10, $11}'
"${T[@]}" list-panes -s -t "$SESS" -F "$SAFE" |
	awk -F'\t' '{printf "  sanitised pane %-4s fields=%-3d command=[%s] title=[%s]\n", $1, NF, $10, $11}'

echo
echo "--- and on the window spec, where the name is the field at risk ---"
WCOMMON='#{window_id}	#{session_id}	#{window_index}'
WTAIL='#{window_active}	#{window_panes}	#{window_width}	#{window_height}	#{window_layout}'
echo "  the spec has 9 fields"
"${T[@]}" list-windows -t "$SESS" -F "$WCOMMON	#{window_name}	$WTAIL" |
	awk -F'\t' '{printf "  raw       window %-4s fields=%-3d name=[%s]\n", $1, NF, $4}'
"${T[@]}" list-windows -t "$SESS" -F "$WCOMMON	#{s/${TAB}/ /:window_name}	$WTAIL" |
	awk -F'\t' '{printf "  sanitised window %-4s fields=%-3d name=[%s]\n", $1, NF, $4}'
