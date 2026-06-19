#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Checks this machine can build and test this Go module — a quick onboarding
    check for contributors.

.DESCRIPTION
    POSIX counterpart is check-env.sh — use whichever matches your shell.

    Verifies the Go toolchain (`go`) is on PATH so `go build ./...` and
    `go test ./...` can run (gofmt and go vet ship with it). Prints "Environment
    ready" and exits 0 on success; if Go is missing it prints per-OS install
    commands and exits 1 — install it, then re-run:

        pwsh ./scripts/check-env.ps1
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$problems = @()

Write-Host "==> Checking environment for Go development" -ForegroundColor Cyan

# Required: go (build/test driver, formatter via `gofmt`, and the compiler).
if (Get-Command go -ErrorAction SilentlyContinue) {
    Write-Host "    $(go version)" -ForegroundColor DarkGray
} else {
    $problems += "the Go toolchain ('go' is not on PATH)"
}

# Soft: git drives the version-control workflow (jj is colocated with it), but is
# not required to build.
if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
    Write-Host "    note: git is not on PATH — needed for the version-control workflow." -ForegroundColor DarkGray
}

if ($problems.Count -eq 0) {
    Write-Host ""
    Write-Host "Environment ready. Next: go build ./... && go test ./..." -ForegroundColor Green
    exit 0
}

Write-Host ""
Write-Host "Environment NOT ready. Missing:" -ForegroundColor Red
foreach ($p in $problems) { Write-Host "  - $p" -ForegroundColor Red }
Write-Host ""
Write-Host "Install the Go toolchain, then re-run this check:" -ForegroundColor Yellow
Write-Host "  Windows : winget install GoLang.Go"
Write-Host "  macOS   : brew install go"
Write-Host "  Linux   : see https://go.dev/doc/install (or your distro's package, e.g. apt install golang-go)"
Write-Host "  (any OS): https://go.dev/dl"
exit 1
