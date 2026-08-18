#!/usr/bin/env bash
# Probe whether tmux executes a command given after "--" directly or through a
# shell. The answer decides whether shell metacharacters in a caller-supplied
# command vector are inert.
#
# The probe is filesystem-observable rather than screen-scraped: a pane whose
# command exits immediately is not reliably capturable, but the file it
# created either exists or does not.
#
#   ./scripts/probe-argv.sh [tmux-binary]

set -uo pipefail

TMUX_BIN="${1:-tmux}"
SOCKET="gotmucks-argvprobe-$$"
T=("$TMUX_BIN" -L "$SOCKET")
WORK=$(mktemp -d)

cleanup() {
	"${T[@]}" kill-server 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

echo "=== $("$TMUX_BIN" -V) ==="

start_server() {
	"${T[@]}" kill-server 2>/dev/null
	sleep 0.2
	# A server with no sessions exits at once (exit-empty defaults to on), so
	# a keeper session holds it open for the probe.
	"${T[@]}" new-session -d -s keeper -x 80 -y 24 -- sleep 60
}

echo
echo "--- multiple arguments: touch 'a;b' ---"
start_server
rm -f "$WORK/a" "$WORK/a;b"
"${T[@]}" new-session -d -s multi -x 80 -y 24 -- touch "$WORK/a;b"
sleep 0.5
if [ -e "$WORK/a;b" ]; then
	echo "result: EXECVP -- the argument vector was passed through, metacharacters inert"
elif [ -e "$WORK/a" ]; then
	echo "result: SHELL -- the line was split on ';'"
else
	echo "result: NEITHER -- nothing was created"
fi
ls -a "$WORK" | sed 's/^/  /'

echo
echo "--- single argument: 'touch single' ---"
start_server
rm -f "$WORK/single"
"${T[@]}" new-session -d -s single -x 80 -y 24 -- "touch $WORK/single"
sleep 0.5
if [ -e "$WORK/single" ]; then
	echo "result: SHELL -- a lone argument is interpreted, as tmux documents"
else
	echo "result: NOT SHELL -- a lone argument was not interpreted"
fi

echo
echo "--- single argument with metacharacters: 'touch x; touch y' ---"
start_server
rm -f "$WORK/x" "$WORK/y"
"${T[@]}" new-session -d -s meta -x 80 -y 24 -- "touch $WORK/x; touch $WORK/y"
sleep 0.5
if [ -e "$WORK/x" ] && [ -e "$WORK/y" ]; then
	echo "result: SHELL -- both commands ran; a single-element vector is NOT safe"
elif [ -e "$WORK/x" ]; then
	echo "result: partial -- only the first ran"
else
	echo "result: NOT SHELL -- neither ran"
fi

echo
echo "--- two elements with metacharacters in the second ---"
start_server
rm -f "$WORK/p" "$WORK/q"
"${T[@]}" new-session -d -s two -x 80 -y 24 -- touch "$WORK/p; touch $WORK/q"
sleep 0.5
if [ -e "$WORK/q" ]; then
	echo "result: SHELL -- a two-element vector is still interpreted"
elif [ -e "$WORK/p; touch $WORK/q" ]; then
	echo "result: EXECVP -- the whole string became one filename, as intended"
else
	echo "result: unclear"
fi
ls -a "$WORK" | sed 's/^/  /'
