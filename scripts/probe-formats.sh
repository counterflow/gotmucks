#!/usr/bin/env bash
# Probe which format variables and introspection commands this tmux actually
# populates. Several have been deprecated or moved between releases, and a
# variable that expands to nothing is indistinguishable from a zero value
# unless you look.
#
#   ./scripts/probe-formats.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-fmtprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

echo "=== $("$TMUX_BIN" -V) ==="

"${T[@]}" kill-server 2>/dev/null
"${T[@]}" new-session -d -s probe -x 100 -y 30 -- sleep 60
sleep 0.3

echo
echo "--- session format variables ---"
for v in session_id session_name session_windows session_created session_activity \
	session_attached session_width session_height session_many_attached; do
	printf '  %-24s [%s]\n' "$v" "$("${T[@]}" display-message -p -t probe "#{$v}")"
done

echo
echo "--- window size for the same session ---"
"${T[@]}" list-windows -t probe -F '  window_width=#{window_width} window_height=#{window_height}'

echo
echo "--- hooks ---"
"${T[@]}" set-hook -t probe pane-exited "display-message gone"
echo "show-hooks -t probe:"
"${T[@]}" show-hooks -t probe 2>&1 | sed 's/^/  | /'
echo "show-hooks -g:"
"${T[@]}" show-hooks -g 2>&1 | sed 's/^/  | /' | head -5
echo "show-options -t probe (looking for the hook):"
"${T[@]}" show-options -t probe 2>&1 | grep -i 'pane-exited' | sed 's/^/  | /'

echo
echo "--- set-hook without -t, then show-hooks ---"
"${T[@]}" set-hook -g session-created "display-message made"
echo "show-hooks -g (grep):"
"${T[@]}" show-hooks -g 2>&1 | grep -i 'session-created' | sed 's/^/  | /'

echo
echo "--- does a per-target set-hook report success, and does the hook fire? ---"
"${T[@]}" set-hook -t probe pane-exited "display-message hook-fired"
echo "  set-hook -t exit status: $?"
echo "  show-hooks -t probe after: [$("${T[@]}" show-hooks -t probe 2>&1)]"

# A hook only proves itself by firing. Give the session a second window whose
# command exits, and see whether the hook's command ran.
"${T[@]}" set-hook -t probe pane-exited "run-shell 'echo fired > /tmp/gotmucks-hook-$$'"
"${T[@]}" new-window -t probe -- true
sleep 1
if [ -e "/tmp/gotmucks-hook-$$" ]; then
	echo "  per-target hook FIRED"
	rm -f "/tmp/gotmucks-hook-$$"
else
	echo "  per-target hook did NOT fire"
fi
