# Run the Go toolchain against this repository.
#
# Go and tmux live in WSL (tmux does not run on Windows, and Ubuntu's apt only
# offers Go 1.18, so the toolchain is installed unprivileged under ~/sdk).
# This wrapper hides the hop:
#
#   .\scripts\go.ps1 test ./...
#   .\scripts\go.ps1 build ./...
#   .\scripts\go.ps1 vet ./...

$ErrorActionPreference = 'Stop'

$repo = (Split-Path -Parent $PSScriptRoot) -replace '\\', '/'
$wslRepo = (wsl -d Ubuntu -- wslpath -a "$repo").Trim()

# Single-quote each argument for the remote shell, escaping embedded quotes.
$quoted = ($args | ForEach-Object { "'" + ($_ -replace "'", "'\''") + "'" }) -join ' '

# The Windows PATH is inherited into WSL and contains spaces and parentheses,
# so the toolchain is invoked by absolute path rather than by extending PATH.
wsl -d Ubuntu -- bash -lc "cd '$wslRepo' && exec `"`$HOME/sdk/go1.26.6/bin/go`" $quoted"
exit $LASTEXITCODE
