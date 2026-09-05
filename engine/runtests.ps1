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

# powershell -File binds ONE positional argument per declared parameter, so
#   runtests.ps1 -Packages ./a ./b
# puts "./a" in $Packages and "./b" in $GoTestArgs. Go test arguments always
# start with "-", so fold flag-less leftovers back into the package list.
if ($Packages.Count -le 1 -and $GoTestArgs.Count -gt 0 -and -not $GoTestArgs[0].StartsWith("-")) {
    $Packages = @($Packages) + $GoTestArgs
    $GoTestArgs = @()
}

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
            # go test -c builds nothing for packages without test files;
            # running the stale binary here would report false results.
            $hasTests = go list -f "{{if or .TestGoFiles .XTestGoFiles}}1{{else}}0{{end}}" $p
            if ($hasTests -ne "1") { Write-Host "(no test files - nothing to run)"; continue }
            & $testExe @GoTestArgs
            if ($LASTEXITCODE -ne 0) { $failed += $p }
        }
    } else {
        Write-Host "== $pkg =="
        go test -c -o $testExe $pkg
        if ($LASTEXITCODE -ne 0) { $failed += $pkg; continue }
        $hasTests = go list -f "{{if or .TestGoFiles .XTestGoFiles}}1{{else}}0{{end}}" $pkg
        if ($hasTests -ne "1") { Write-Host "(no test files - nothing to run)"; continue }
        & $testExe @GoTestArgs
        if ($LASTEXITCODE -ne 0) { $failed += $pkg }
    }
}

if ($failed.Count -gt 0) {
    Write-Host "FAILED packages:" $failed
    exit 1
}
Write-Host "All tests passed."
