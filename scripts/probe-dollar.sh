#!/usr/bin/env bash
# Does this tmux STORE a backslash before '$', or only PRINT one?
#
# tmux 3.4 inserts one on the way in, into storage, where 3.2a and 3.5a store
# the bytes they were given. That is a fault in one release and it is corrected
# on the way back out — see Version.EscapesDollarOnWrite — so which side of it
# a release falls on has to be measured rather than assumed.
#
# The two cases need opposite handling and getting it backwards corrupts data
# in the other direction, so two readers are asked rather than one. show-options
# -v escapes least, and a "#{@x}" expansion is an independent opinion; if both
# show the backslash it is in storage, and if only the named form shows it then
# it is in the printing and the ordinary decoder already deals with it.
#
# Read as a report, not an assertion: it prints what this binary does and exits
# 0. What is asserted about it lives in TestUndoDollarEscape and in the
# integration round-trip tests, which run against each release in the matrix.
set -uo pipefail

tmux="${1:-tmux}"
sock="probe-dollar-$$"

cleanup() { "$tmux" -L "$sock" kill-server 2>/dev/null; }
trap cleanup EXIT

"$tmux" -L "$sock" new-session -d -s p -- sleep 60

printf '=== %s ===\n' "$("$tmux" -V)"
printf '%-24s | %-16s | %-18s | %s\n' 'set' 'show -v' 'show (named)' 'format #{@x}'
printf -- '-------------------------|------------------|--------------------|-------------\n'

# The four positions round ten established are not interchangeable, plus the
# two shapes that tell an inserted backslash from one the caller sent.
for v in 'a$b' '$ab' 'ab$' '$' 'a\$b' 'x$HOMEy' 'PATH=$HOME/bin:$PATH'; do
	"$tmux" -L "$sock" set-option -t p -- @x "$v"
	printf '%-24s | %-16s | %-18s | %s\n' "$v" \
		"$("$tmux" -L "$sock" show-options -t p -v -- @x)" \
		"$("$tmux" -L "$sock" show-options -t p -- @x | cut -d' ' -f2-)" \
		"$("$tmux" -L "$sock" display-message -p -t p '#{@x}')"
done

printf -- '\n--- the same question asked of a name rather than a value ---\n'
for v in 'a$b' 'x$HOMEy' 'a\b'; do
	"$tmux" -L "$sock" rename-window -t p -- "$v"
	printf '  set %-12s  #{window_name} = %s\n' "$v" \
		"$("$tmux" -L "$sock" display-message -p -t p '#{window_name}')"
done

printf -- '\n--- and of a hook body, which is a command line rather than a value ---\n'
"$tmux" -L "$sock" set-hook -t p -- alert-silence 'run-shell "echo \$HOME"'
printf '  set   %s\n' 'run-shell "echo \$HOME"'
printf '  shown %s\n' \
	"$("$tmux" -L "$sock" show-hooks -t p 2>/dev/null | grep alert-silence | cut -d' ' -f2-)"
