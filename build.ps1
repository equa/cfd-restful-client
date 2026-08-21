<#
.SYNOPSIS
    Build the cfd-client Windows executable and smoke-test it.

.DESCRIPTION
    Windows counterpart of `make build`: compiles a static, dependency-free
    cfd-client.exe (CGO disabled, symbols stripped), then runs a few smoke
    tests so a build isn't declared good until the binary actually runs:
      1. --help          prints usage and exits 0
      2. config          resolves a backend and emits the JSON envelope (ok:true)
      3. bogus command   fails cleanly (ok:false, exit 1) -- the error contract

    The `config` test uses -ServerUrl so it needs no config file and no running
    server; if a cfd_client_config.json is present it is additionally exercised.

.PARAMETER Output
    Output binary name (default: cfd-client.exe, next to this script).

.PARAMETER ServerUrl
    Server URL used for the (server-less) config smoke test.

.EXAMPLE
    ./build.ps1
#>
param(
    [string]$Output = "cfd-client.exe",
    [string]$ServerUrl = "http://localhost:5001"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
Set-Location -Path $PSScriptRoot

# --- prerequisites --------------------------------------------------------- #
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Error "Go toolchain not found on PATH. Install Go from https://go.dev/dl/ and retry."
    exit 1
}
Write-Host ("Go: " + (go version)) -ForegroundColor DarkGray

# --- build ----------------------------------------------------------------- #
$exe = Join-Path $PSScriptRoot $Output
Write-Host "Building $Output ..." -ForegroundColor Cyan
$env:CGO_ENABLED = "0"     # static binary; no C toolchain needed
go build -ldflags "-s -w" -o $exe .
if ($LASTEXITCODE -ne 0) { Write-Error "go build failed."; exit 1 }
Write-Host ("Built {0} ({1:N0} bytes)" -f $Output, (Get-Item $exe).Length) -ForegroundColor Green

# --- smoke tests ----------------------------------------------------------- #
$failures = 0
function Test-Step {
    param([string]$Name, [scriptblock]$Body)
    try {
        & $Body
        Write-Host "  PASS  $Name" -ForegroundColor Green
    } catch {
        $script:failures++
        Write-Host "  FAIL  $Name -- $($_.Exception.Message)" -ForegroundColor Red
    }
}

Write-Host "Smoke tests:" -ForegroundColor Cyan

# 1. --help prints usage and exits 0
Test-Step "--help prints usage (exit 0)" {
    $out = & $exe --help 2>&1
    if ($LASTEXITCODE -ne 0) { throw "exit $LASTEXITCODE" }
    if (($out -join "`n") -notmatch "usage:") { throw "no 'usage:' in output" }
}

# 2. config resolves and emits the JSON envelope (ok:true), server-less
Test-Step "config emits ok:true (--server-url)" {
    $out = & $exe --server-url $ServerUrl config
    if ($LASTEXITCODE -ne 0) { throw "exit $LASTEXITCODE" }
    $j = $out | ConvertFrom-Json
    if (-not $j.ok) { throw "ok was not true: $out" }
    if ($j.result.server_url -ne $ServerUrl) { throw "server_url mismatch: $($j.result.server_url)" }
}

# 3. a bogus command fails cleanly: ok:false, exit 1 (the error contract)
Test-Step "bad command fails cleanly (ok:false, exit 1)" {
    $out = & $exe __nope__ 2>&1
    if ($LASTEXITCODE -ne 1) { throw "expected exit 1, got $LASTEXITCODE" }
    $j = $out | ConvertFrom-Json
    if ($j.ok) { throw "ok should be false" }
}

# 4. optional: exercise a real config file if one is present
$cfg = Join-Path $PSScriptRoot "cfd_client_config.json"
if (Test-Path $cfg) {
    Test-Step "config loads cfd_client_config.json (default backend)" {
        $out = & $exe config
        if ($LASTEXITCODE -ne 0) { throw "exit $LASTEXITCODE" }
        $j = $out | ConvertFrom-Json
        if (-not $j.ok) { throw "ok was not true: $out" }
    }
} else {
    Write-Host "  SKIP  cfd_client_config.json test (no config file present)" -ForegroundColor DarkGray
}

# --- summary --------------------------------------------------------------- #
if ($failures -gt 0) {
    Write-Host "$failures test(s) failed." -ForegroundColor Red
    exit 1
}
Write-Host "All good: $Output built and smoke-tested." -ForegroundColor Green
exit 0
