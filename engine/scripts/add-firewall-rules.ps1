# One-time (elevated) firewall whitelist for RemoteAU development.
# Right-click -> Run with PowerShell, or from an admin terminal:
#   powershell -ExecutionPolicy Bypass -File .\add-firewall-rules.ps1
#
# This stops Windows Firewall from prompting about every rebuilt Go test
# binary: all test runs go through engine\.testbin\rau.test.exe and the CLI
# through engine\bin\remote-au.exe, and rules are matched by path.

$ErrorActionPreference = "Stop"
$engine = Split-Path -Parent $PSScriptRoot
$paths = @(
    (Join-Path $engine ".testbin\rau.test.exe"),
    (Join-Path $engine "bin\remote-au.exe")
)

foreach ($p in $paths) {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $p) | Out-Null
    $name = "RemoteAU dev - " + (Split-Path -Leaf $p)
    Remove-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue
    New-NetFirewallRule -DisplayName $name -Program $p `
        -Direction Inbound -Action Allow -Protocol UDP -Profile Any | Out-Null
    New-NetFirewallRule -DisplayName $name -Program $p `
        -Direction Inbound -Action Allow -Protocol TCP -Profile Any | Out-Null
    Write-Host "Allowed: $p"
}
Write-Host "Done."
