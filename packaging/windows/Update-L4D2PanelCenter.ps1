#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$MsiPath,
    [string]$InstallRoot = "$env:ProgramFiles\L4D2Panel\Control",
    [string]$DataRoot = "$env:ProgramData\L4D2Panel"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$msi = (Resolve-Path $MsiPath).Path
$control = Join-Path $InstallRoot 'l4d2panel-control.exe'
$config = Join-Path $DataRoot 'config\control.json'
$checkpoint = Join-Path $DataRoot ("backups\upgrade-{0}.sqlite3" -f (Get-Date -Format 'yyyyMMdd-HHmmss'))

foreach ($name in @('l4d2panel-caddy', 'l4d2panel-control')) {
    $service = Get-Service -Name $name -ErrorAction SilentlyContinue
    if ($service -and $service.Status -ne 'Stopped') {
        Stop-Service -Name $name -Force
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
}

New-Item -ItemType Directory -Force -Path (Split-Path $checkpoint) | Out-Null
& $control backup-database --config $config --output $checkpoint
if ($LASTEXITCODE -ne 0) { throw 'SQLite checkpoint failed; the MSI was not started.' }

$process = Start-Process msiexec.exe -ArgumentList @('/i', $msi, '/qn', '/norestart') -Wait -PassThru
if ($process.ExitCode -notin @(0, 3010)) {
    $instructions = Join-Path $DataRoot 'backups\UPGRADE-RECOVERY.txt'
    @"
The center upgrade failed with MSI exit code $($process.ExitCode).
The original database and checkpoint were preserved.
Checkpoint: $checkpoint
Stop l4d2panel-control before replacing the current SQLite database with a verified copy of this checkpoint.
"@ | Set-Content -LiteralPath $instructions -Encoding UTF8
    throw "MSI upgrade failed. Recovery instructions: $instructions"
}

foreach ($name in @('l4d2panel-control', 'l4d2panel-caddy')) {
    $service = Get-Service -Name $name -ErrorAction SilentlyContinue
    if ($service -and $service.Status -ne 'Running') {
        Start-Service $name
        $service.WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    }
}
Write-Output "Upgrade completed. Pre-upgrade SQLite checkpoint: $checkpoint"
