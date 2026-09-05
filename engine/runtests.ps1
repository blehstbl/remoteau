# Builds every Go test binary to ONE fixed path and runs it, so Windows
# Firewall only ever asks about .testbin\rau.test.exe (click Allow once).
#
# Usage:
#   .\runtests.ps1                     # all packages
#   .\runtests.ps1 -Packages ./internal/discovery
param(
    [string[]]$Packages = @(),
    [string[]]$GoTestArgs = @()
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
Set-Location $root
$env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path", "User")
$bin = Join-Path $root ".testbin"
New-Item -ItemType Directory -Force -Path $bin | Out-Null
$testExe = Join-Path $bin "rau.test.exe"

if ($Packages.Count -eq 0) {
    $Packages = @("./...")
}

$failed = @()
foreach ($pkg in $Packages) {
    if ($pkg -eq "./...") {
        # List packages, then build+run each with the fixed binary path.
        $list = go list $pkg
        foreach ($p in $list) {
            Write-Host "== $p =="
            go test -c -o $testExe $p
            if ($LASTEXITCODE -ne 0) { $failed += $p; continue }
            & $testExe @GoTestArgs
            if ($LASTEXITCODE -ne 0) { $failed += $p }
        }
    } else {
        Write-Host "== $pkg =="
        go test -c -o $testExe $pkg
        if ($LASTEXITCODE -ne 0) { $failed += $pkg; continue }
        & $testExe @GoTestArgs
        if ($LASTEXITCODE -ne 0) { $failed += $pkg }
    }
}

if ($failed.Count -gt 0) {
    Write-Host "FAILED packages:" $failed
    exit 1
}
Write-Host "All tests passed."
