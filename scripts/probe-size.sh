#!/usr/bin/env bash
# What size does a window get when no client ever attaches?
#
# Three integration tests assert a size they asked for with new-session -x/-y
# and got 80x23 on a GitHub runner while passing on a developer's machine
# against the same tmux version. 80x23 is default-size less a status line,
# which is what a window gets when something other than -x/-y decided it.
#
# window-size has defaulted to "latest" since 2.9 — size the window from the
# most recently attached client — and falls back to default-size when there has
# never been one. Whether that overrides the session size -x/-y sets is the
# claim, and it is a claim about tmux rather than about the tests, so it is
# asked here rather than reasoned about. On tmux 3.2a under WSL with no
# controlling terminal, -x/-y sticks and default-size is not reduced by the
# status line; the runner disagrees and this is what will say how.
set -uo pipefail

tmux="${1:-tmux}"
sock="probe-size-$$"

cleanup() { "$tmux" -L "$sock" kill-server 2>/dev/null; }
trap cleanup EXIT

report() {
	printf '  %-28s session [%s]  window [%s]\n' "$1" \
		"$("$tmux" -L "$sock" display-message -p -t "$2" '#{session_width}x#{session_height}')" \
		"$("$tmux" -L "$sock" list-windows -t "$2" -F '#{window_width}x#{window_height}')"
}

printf '=== %s ===\n' "$("$tmux" -V)"
printf 'stdin is a tty: %s\n\n' "$([ -t 0 ] && echo yes || echo no)"

printf -- '--- 1. new-session -x 100 -y 30, default options ---\n'
"$tmux" -L "$sock" new-session -d -s a -x 100 -y 30 -- sleep 60
printf '  window-size   = %s\n' "$("$tmux" -L "$sock" show-options -gv window-size)"
printf '  default-size  = %s\n' "$("$tmux" -L "$sock" show-options -gv default-size)"
printf '  status        = %s\n' "$("$tmux" -L "$sock" show-options -gv status)"
report 'asked 100x30' a

printf -- '\n--- 2. the same with window-size manual ---\n'
"$tmux" -L "$sock" set-option -g window-size manual
"$tmux" -L "$sock" new-session -d -s b -x 100 -y 30 -- sleep 60
report 'asked 100x30' b

printf -- '\n--- 3. no -x/-y, default-size raised to 120x40 ---\n'
"$tmux" -L "$sock" set-option -g window-size latest
"$tmux" -L "$sock" set-option -g default-size 120x40
"$tmux" -L "$sock" new-session -d -s c -- sleep 60
report 'asked nothing' c

printf -- '\n--- 4. the exact shape the failing tests use ---\n'
"$tmux" -L "$sock" set-option -g default-size 80x24
"$tmux" -L "$sock" new-session -d -s d -x 90 -y 25 -- sleep 60
report 'asked 90x25' d

printf -- '\n--- 5. resize-window after the fact, window-size left at latest ---\n'
# This is what NewSession does. Setting window-size to "manual" also makes the
# request stick and is the wrong remedy: it pins the window for good, so a
# control client attaching later and calling SetSize never moves it again. A
# resize has to hold on its own for that to be avoidable, so it is asked here.
"$tmux" -L "$sock" resize-window -t d -x 90 -y 25
report 'resized to 90x25' d
printf '  window-size still = %s\n' "$("$tmux" -L "$sock" show-options -gv window-size)"
