#requires -Version 5.1
<#
.SYNOPSIS
  Builds the RemoteAU Windows tray binary with an embedded common-controls-6
  application manifest.

.DESCRIPTION
  walk (the settings-window toolkit) and the themed Win32 controls it relies on
  require a side-by-side Microsoft.Windows.Common-Controls 6.0.0.0 dependency in
  the executable's manifest. The Go linker embeds a `.syso` resource object that
  sits next to the package sources, so this script:

    1. regenerates cmd\remote-au-tray\rsrc_windows_amd64.syso from
       cmd\remote-au-tray\tray.manifest, then
    2. builds bin\remote-au-tray.exe.

  `rsrc` is a BUILD-TIME TOOL run through `go run github.com/akavel/rsrc@latest`.
  It is intentionally NOT a module dependency and never links into the binary;
  it is fetched on demand and cached by the Go toolchain.

  Regenerate whenever tray.manifest changes. A plain `go build ./...` (and the
  release build-windows.ps1) also picks up an existing .syso automatically, so
  this script only needs to run when the manifest itself is edited.

  Usage:
    .\build-tray.ps1
    .\build-tray.ps1 -NoManifest   # reuse the existing .syso, skip rsrc
#>
param(
    # Override the engine root (defaults to the directory above this script).
    [string]$EngineRoot = "",
    # Skip manifest regeneration and reuse the checked-in .syso.
    [switch]$NoManifest
)

$ErrorActionPreference = "Stop"

function Fail([string]$msg) {
    Write-Host "ERROR: $msg" -ForegroundColor Red
    exit 1
}

function Assert-LastExit([string]$what) {
    if ($LASTEXITCODE -ne 0) { Fail "$what failed (exit $LASTEXITCODE)" }
}

if (-not $EngineRoot) {
    $EngineRoot = Split-Path -Parent $PSScriptRoot
}
Set-Location $EngineRoot

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) { Fail "go not found on PATH - install https://go.dev/dl/" }

$manifest = "cmd\remote-au-tray\tray.manifest"
$syso = "cmd\remote-au-tray\rsrc_windows_amd64.syso"

if (-not $NoManifest) {
    if (-not (Test-Path $manifest)) { Fail "manifest not found: $manifest" }
    Write-Host "Generating $syso from $manifest ..."
    go run github.com/akavel/rsrc@latest -manifest $manifest -arch amd64 -o $syso
    Assert-LastExit "rsrc manifest embedding"
} elseif (-not (Test-Path $syso)) {
    Fail "-NoManifest requested but $syso does not exist."
}

New-Item -ItemType Directory -Force -Path "bin" | Out-Null
Write-Host "`n=== Building bin\remote-au-tray.exe ==="
go build -trimpath -o bin\remote-au-tray.exe ./cmd/remote-au-tray
Assert-LastExit "go build remote-au-tray"

Write-Host "`nDone: $EngineRoot\bin\remote-au-tray.exe (common-controls manifest embedded)."
