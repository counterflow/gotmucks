#!/usr/bin/env bash
# Probe the installed tmux for the behaviours this library depends on.
#
# Several of the library's decisions rest on how a particular tmux release
# handles a particular argument shape, and those have drifted over time. This
# script asks the binary rather than assuming, and prints one line per
# question. Run it against each tmux in the support matrix when adding a
# version.
#
#   ./scripts/probe-tmux.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-probe-$$"
T=("$TMUX_BIN" -L "$SOCKET")

cleanup() { "${T[@]}" kill-server 2>/dev/null; }
trap cleanup EXIT

say() { printf '%-38s %s\n' "$1" "$2"; }

echo "=== $("$TMUX_BIN" -V) ==="
echo

# --- multi-argument command after -- ------------------------------------
# If tmux execs the vector directly, printf prints the semicolon literally.
# If it hands the line to a shell, the shell splits on ';' and fails.
"${T[@]}" kill-server 2>/dev/null
# remain-on-exit has to be set before the pane starts: the command exits
# immediately and the pane would be gone before the option was applied.
"${T[@]}" start-server 2>/dev/null
"${T[@]}" set-option -g -w remain-on-exit on 2>/dev/null
"${T[@]}" new-session -d -s multi -x 80 -y 24 -- printf 'one;two three' 2>/dev/null
sleep 0.4
out=$("${T[@]}" capture-pane -p -t multi 2>/dev/null | head -1)
if [ "$out" = "one;two three" ]; then
	say "multi-arg command after --" "execvp (metacharacters inert)"
else
	say "multi-arg command after --" "NOT execvp: got [$out]"
fi

# --- single-argument command --------------------------------------------
# tmux is documented to hand a lone argument to the shell.
"${T[@]}" kill-server 2>/dev/null
"${T[@]}" start-server 2>/dev/null
"${T[@]}" set-option -g -w remain-on-exit on 2>/dev/null
"${T[@]}" new-session -d -s single -x 80 -y 24 -- 'printf single-via-shell' 2>/dev/null
sleep 0.4
out=$("${T[@]}" capture-pane -p -t single 2>/dev/null | head -1)
if [ "$out" = "single-via-shell" ]; then
	say "single-arg command after --" "shell (as documented)"
else
	say "single-arg command after --" "not shell: got [$out]"
fi

# --- send-keys -- ---------------------------------------------------------
"${T[@]}" kill-server 2>/dev/null
"${T[@]}" new-session -d -s keys -x 80 -y 24 -- cat 2>/dev/null
sleep 0.3
if "${T[@]}" send-keys -l -t keys -- '--force' 2>/dev/null; then
	say "send-keys -l -t X -- --force" "accepted"
else
	say "send-keys -l -t X -- --force" "REJECTED"
fi
if "${T[@]}" send-keys -H -t keys -- 41 42 2>/dev/null; then
	say "send-keys -H -t X -- 41 42" "accepted"
else
	say "send-keys -H -t X -- 41 42" "REJECTED"
fi

# --- set-option scopes ----------------------------------------------------
pane=$("${T[@]}" list-panes -t keys -F '#{pane_id}' 2>/dev/null | head -1)
if "${T[@]}" set-option -p -t "$pane" remain-on-exit on 2>/dev/null; then
	say "set-option -p remain-on-exit" "accepted (pane option exists)"
else
	say "set-option -p remain-on-exit" "REJECTED (window option only)"
fi
if "${T[@]}" set-option -w -t keys remain-on-exit on 2>/dev/null; then
	say "set-option -w remain-on-exit" "accepted"
else
	say "set-option -w remain-on-exit" "REJECTED"
fi

# --- set-option -- separator ---------------------------------------------
if "${T[@]}" set-option -g -- history-limit 5000 2>/dev/null; then
	say "set-option -g -- name value" "accepted"
else
	say "set-option -g -- name value" "REJECTED"
fi

# --- new-session -e (the 3.2 floor) --------------------------------------
"${T[@]}" kill-server 2>/dev/null
if "${T[@]}" new-session -d -s envtest -e GOTMUCKS_PROBE=yes 2>/dev/null; then
	val=$("${T[@]}" show-environment -t envtest GOTMUCKS_PROBE 2>/dev/null)
	say "new-session -e" "accepted, show-environment: [$val]"
else
	say "new-session -e" "REJECTED (tmux older than 3.2)"
fi

# --- list-panes -a and format separator ----------------------------------
out=$("${T[@]}" list-panes -a -F '#{pane_id}	#{window_id}	#{session_id}' 2>/dev/null | head -1)
say "list-panes -a tab format" "[$(printf '%s' "$out" | tr '\t' '|')]"

# --- control mode ---------------------------------------------------------
"${T[@]}" kill-server 2>/dev/null
"${T[@]}" new-session -d -s ctl -x 80 -y 24 2>/dev/null
ctl_out=$(mktemp)
{
	# Give tmux a moment between commands so replies are not batched.
	printf 'refresh-client -C 132x43\n'
	sleep 0.3
	printf 'refresh-client -B probe:%%*:#{pane_title}\n'
	sleep 0.3
	printf 'refresh-client -f pause-after=1\n'
	sleep 0.3
	printf 'list-panes -F #{pane_id}\n'
	sleep 0.5
	printf '\n'
} | "${T[@]}" -CC attach-session -t ctl >"$ctl_out" 2>&1
sleep 0.2

if grep -q '%error' "$ctl_out"; then
	say "control mode errors" "SOME COMMAND FAILED"
	grep -n -A2 '%begin\|%error' "$ctl_out" | head -30
else
	say "control mode errors" "none"
fi

say "refresh-client -C WxH" "$(grep -c '%error' "$ctl_out" | sed 's/^0$/ok/')"
echo
echo "--- control transcript (escaped) ---"
cat -v "$ctl_out" | head -40
echo "--- end transcript ---"
rm -f "$ctl_out"
