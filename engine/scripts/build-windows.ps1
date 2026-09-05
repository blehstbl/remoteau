#requires -Version 5.1
<#
.SYNOPSIS
  Builds the Windows release binaries for remote-au: the Opus-enabled pair
  (client + tray) and a PCM-only fallback pair.

.DESCRIPTION
  Outputs:
    engine\bin\release\      remote-au.exe + remote-au-tray.exe   (-tags "opus,nolibopusfile")
    engine\bin\release-pcm\  remote-au.exe + remote-au-tray.exe   (default tags, no Opus)

  Opus builds link libopus via cgo (github.com/hraban/opus). The wrapper
  resolves the C library through pkg-config (an `opus.pc` is written into
  the lib tree automatically if the build did not install one) and the
  `nolibopusfile` tag drops the wrapper's opusfile-dependent files, so only
  libopus itself is required.

  Requirements:
    - Go (on PATH)
    - MinGW-w64 gcc (on PATH). Install via MSYS2:
        pacman -S mingw-w64-x86_64-gcc mingw-w64-x86_64-pkgconf make
      or `choco install mingw pkgconfiglite`.
    - A libopus build at -OpusRoot (default %USERPROFILE%\libopus-x64) with:
        lib\libopus.a + include{,\opus}\opus.h          (static, default)
        lib\libopus.dll.a or libopus.dll + bin\*.dll    (with -Dynamic)

  Usage:
    .\build-windows.ps1                       # Opus release + PCM fallback
    .\build-windows.ps1 -OpusRoot C:\libs\opus-x64
    .\build-windows.ps1 -Dynamic              # link libopus.dll, copy it next to the exes
    .\build-windows.ps1 -SkipOpus             # PCM-only dev build, no libopus needed

  The script fails with exit code 1 if the Opus build is requested but
  libopus, gcc or pkg-config are missing.
#>
param(
    # Root of the libopus install (contains include\ and lib\).
    [string]$OpusRoot = (Join-Path $env:USERPROFILE "libopus-x64"),
    # Link the dynamic libopus.dll instead of the static libopus.a and copy
    # the DLL next to the executables.
    [switch]$Dynamic,
    # Skip the Opus build entirely (PCM-only dev builds).
    [switch]$SkipOpus,
    # Override the engine root (defaults to the directory above this script).
    [string]$EngineRoot = ""
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

# --- Toolchain checks -------------------------------------------------------

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) { Fail "go not found on PATH - install https://go.dev/dl/" }

$gcc = Get-Command gcc -ErrorAction SilentlyContinue
if (-not $gcc) {
    Fail "MinGW-w64 gcc not found on PATH. Install it via MSYS2 (pacman -S mingw-w64-x86_64-gcc) or 'choco install mingw', then re-run."
}
Write-Host "Using gcc: $($gcc.Source)"

# Any build here uses cgo (WASAPI/miniaudio backends), so CGO must be on.
$env:CGO_ENABLED = "1"

# --- Opus build -------------------------------------------------------------

$tagsOpus = "opus,nolibopusfile"

if (-not $SkipOpus) {
    $pkgconfig = Get-Command pkg-config -ErrorAction SilentlyContinue
    if (-not $pkgconfig) { $pkgconfig = Get-Command pkgconf -ErrorAction SilentlyContinue }
    if (-not $pkgconfig) {
        Fail "pkg-config not found on PATH (required by the hraban/opus cgo wrapper). Install via MSYS2 (pacman -S mingw-w64-x86_64-pkgconf) or 'choco install pkgconfiglite'."
    }
    # The go tool runs the literal `pkg-config` binary unless PKG_CONFIG
    # points elsewhere; msys2 sometimes only ships `pkgconf`.
    if ($pkgconfig.Name -ne "pkg-config.exe") {
        $env:PKG_CONFIG = $pkgconfig.Name
    }

    $root = (Resolve-Path $OpusRoot -ErrorAction SilentlyContinue)
    if (-not $root) {
        Fail "libopus not found at '$OpusRoot'. Build it first (see .github/workflows/release-windows.yml) or pass -OpusRoot / -SkipOpus."
    }
    $root = $root.Path
    $libDir = Join-Path $root "lib"
    $binDir = Join-Path $root "bin"

    # Locate opus.h: autotools installs it to include\opus\, some prebuilt
    # bundles put it directly into include\.
    $incDir = $null
    foreach ($cand in @((Join-Path $root "include\opus"), (Join-Path $root "include"))) {
        if (Test-Path (Join-Path $cand "opus.h")) { $incDir = $cand; break }
    }
    if (-not $incDir) { Fail "opus.h not found under '$root\include' (looked in include\opus and include)." }

    # Static is preferred: lib\libopus.a, nothing to ship alongside the exes.
    $staticLib = Join-Path $libDir "libopus.a"
    $importLib = $null
    foreach ($cand in @("libopus.dll.a", "opus.dll.a", "libopus.dll")) {
        if (Test-Path (Join-Path $libDir $cand)) { $importLib = Join-Path $libDir $cand; break }
    }

    if ($Dynamic) {
        if (-not $importLib) {
            Fail "-Dynamic requested but no import library (libopus.dll.a / opus.dll) found in '$libDir'."
        }
        $dll = $null
        foreach ($cand in @("libopus-0.dll", "opus.dll", "libopus.dll")) {
            foreach ($dir in @($binDir, $libDir)) {
                $p = Join-Path $dir $cand
                if (Test-Path $p) { $dll = $p; break }
            }
            if ($dll) { break }
        }
        if (-not $dll) { Fail "-Dynamic requested but no libopus-0.dll / opus.dll found under '$root'." }
        $copyDll = $dll
    } else {
        if (-not (Test-Path $staticLib)) {
            Fail "static libopus not found: '$staticLib' (build with --disable-shared --enable-static, or re-run with -Dynamic)."
        }
    }

    # Make sure pkg-config can answer `pkg-config --cflags/--libs opus`.
    # Source builds (make install) ship their own opus.pc; write one if absent.
    $pcDir = Join-Path $libDir "pkgconfig"
    $pcFile = Join-Path $pcDir "opus.pc"
    if (-not (Test-Path $pcFile)) {
        New-Item -ItemType Directory -Force -Path $pcDir | Out-Null
        $rootFwd = $root -replace "\\", "/"
        $incFwd = $incDir -replace "\\", "/"
        $pcContent = @'
prefix=PREFIX_PLACEHOLDER
libdir=PREFIX_PLACEHOLDER/lib
includedir=INCLUDEDIR_PLACEHOLDER

Name: opus
Description: Opus IETF audio codec (local build)
Version: 1.4
Libs: -L${libdir} -lopus
Cflags: -I${includedir}
'@
        $pcContent = $pcContent.Replace("PREFIX_PLACEHOLDER", $rootFwd).Replace("INCLUDEDIR_PLACEHOLDER", $incFwd)
        $pcContent | Set-Content -Path $pcFile -Encoding ASCII
        Write-Host "Wrote $pcFile"
    }
    if ($env:PKG_CONFIG_PATH) {
        $env:PKG_CONFIG_PATH = "$pcDir;$env:PKG_CONFIG_PATH"
    } else {
        $env:PKG_CONFIG_PATH = $pcDir
    }

    # Static linking is preferred; for dynamic builds -lopus picks up the
    # import library from -L.
    $env:CGO_CFLAGS = "-I" + ($incDir -replace "\\", "/")
    $env:CGO_LDFLAGS = "-L" + ($libDir -replace "\\", "/") + " -lopus"

    Write-Host "`n=== Building Opus release -> bin\release (tags: $tagsOpus) ==="
    New-Item -ItemType Directory -Force -Path (Join-Path $EngineRoot "bin\release") | Out-Null
    go build -trimpath -tags $tagsOpus -o bin\release\remote-au.exe ./cmd/remote-au
    Assert-LastExit "go build remote-au (opus)"
    go build -trimpath -tags $tagsOpus -o bin\release\remote-au-tray.exe ./cmd/remote-au-tray
    Assert-LastExit "go build remote-au-tray (opus)"

    # Guard against a silent PCM-only "release": the binary must actually
    # link the opus module.
    $buildInfo = (go version -m bin\release\remote-au.exe | Out-String)
    if ($buildInfo -notmatch "hraban/opus") {
        Fail "bin\release\remote-au.exe was not built with Opus (no hraban/opus in build info)."
    }

    if ($Dynamic) {
        Copy-Item $copyDll (Join-Path $EngineRoot "bin\release\") -Force
        Write-Host "Copied $(Split-Path -Leaf $copyDll) next to the executables."
    } else {
        Write-Host "Static Opus link - no DLL to ship."
    }
} else {
    Write-Host "Skipping Opus build (-SkipOpus)."
}

# --- PCM-only fallback ------------------------------------------------------

# Clear the Opus cgo overrides so the fallback build is the plain default.
Remove-Item Env:CGO_CFLAGS -ErrorAction SilentlyContinue
Remove-Item Env:CGO_LDFLAGS -ErrorAction SilentlyContinue

Write-Host "`n=== Building PCM-only fallback -> bin\release-pcm (default tags) ==="
New-Item -ItemType Directory -Force -Path (Join-Path $EngineRoot "bin\release-pcm") | Out-Null
go build -trimpath -o bin\release-pcm\remote-au.exe ./cmd/remote-au
Assert-LastExit "go build remote-au (pcm)"
go build -trimpath -o bin\release-pcm\remote-au-tray.exe ./cmd/remote-au-tray
Assert-LastExit "go build remote-au-tray (pcm)"

$pcmInfo = (go version -m bin\release-pcm\remote-au.exe | Out-String)
if ($pcmInfo -match "hraban/opus") {
    Fail "bin\release-pcm\remote-au.exe unexpectedly links Opus."
}

Write-Host "`nDone:"
Write-Host "  Opus release : $EngineRoot\bin\release$(if ($SkipOpus) { ' (skipped)' })"
Write-Host "  PCM fallback : $EngineRoot\bin\release-pcm"
