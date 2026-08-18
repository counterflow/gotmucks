#!/usr/bin/env bash
# Run the Go toolchain against this repository from inside WSL (or any Linux box
# where Go was installed unprivileged under ~/sdk).
#
#   ./scripts/go.sh test ./...
#   ./scripts/go.sh test -tags integration ./...
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for candidate in "$HOME/sdk/go1.26.6/bin/go" "$(command -v go || true)" /usr/local/go/bin/go; do
	if [ -x "$candidate" ]; then
		go_bin="$candidate"
		break
	fi
done

if [ -z "${go_bin:-}" ]; then
	echo "no go toolchain found; expected \$HOME/sdk/go1.26.6/bin/go" >&2
	exit 1
fi

cd "$repo"
exec "$go_bin" "$@"
