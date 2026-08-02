#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [ValidateSet('Install', 'RemoveServices')][string]$Action = 'Install',
    [string]$InstallRoot = "$env:ProgramFiles\L4D2Panel\Control",
    [string]$DataRoot = "$env:ProgramData\L4D2Panel",
    [switch]$SkipStart,
    [switch]$SkipMigrate,
    [switch]$FromMsi
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$script:ControlService = 'l4d2panel-control'
$script:ControlPort = 19090

function Invoke-CheckedProcess {
    param([Parameter(Mandatory)][string]$FilePath, [Parameter(Mandatory)][string[]]$ArgumentList)
    & $FilePath @ArgumentList
    if ($LASTEXITCODE -ne 0) { throw "Process failed with exit code $LASTEXITCODE`: $FilePath" }
}

function ConvertFrom-InstallerBoolean {
    param($Value, [string]$Name)
    switch (([string]$Value).Trim().ToLowerInvariant()) {
        { $_ -in @('true', '1', '$true') } { return $true }
        { $_ -in @('false', '0', '$false', '') } { return $false }
        default { throw "$Name must be true or false." }
    }
}

function Get-InstallerOption {
    param($Options, [string]$Name, $Default)
    $property = $Options.PSObject.Properties[$Name]
    if ($null -eq $property) { return $Default }
    return $property.Value
}

function Remove-ServiceIfPresent {
    param([string]$Name)
    $service = Get-Service -Name $Name -ErrorAction SilentlyContinue
    if ($null -eq $service) { return }
    if ($service.Status -ne 'Stopped') {
        Stop-Service -Name $Name -Force -ErrorAction SilentlyContinue
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
    Invoke-CheckedProcess "$env:SystemRoot\System32\sc.exe" @('delete', $Name)
    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        if (-not (Get-Service -Name $Name -ErrorAction SilentlyContinue)) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "Timed out deleting Windows service $Name."
}

function Remove-CenterServices {
    Remove-ServiceIfPresent $script:ControlService
    Get-NetFirewallRule -Group 'L4D2 Panel Center' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    Write-Output 'L4D2 Panel services were removed. ProgramData was preserved.'
}

function Set-RestrictedDirectoryAcl {
    param([string]$Path, [string[]]$ServiceNames, [switch]$ReadOnlyService)
    New-Item -ItemType Directory -Force -Path $Path | Out-Null
    $arguments = @($Path, '/inheritance:r', '/grant:r', '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F')
    foreach ($serviceName in $ServiceNames) {
        $rights = if ($ReadOnlyService) { '(OI)(CI)RX' } else { '(OI)(CI)M' }
        $arguments += @('/grant', "NT SERVICE\$serviceName`:$rights")
    }
    Invoke-CheckedProcess "$env:SystemRoot\System32\icacls.exe" $arguments
}

function Set-RestrictedFileAcl {
    param([string]$Path, [string]$ServiceName)
    Invoke-CheckedProcess "$env:SystemRoot\System32\icacls.exe" @(
        $Path, '/inheritance:r', '/grant:r', '*S-1-5-18:F', '*S-1-5-32-544:F', '/grant', "NT SERVICE\$ServiceName`:R")
}

function Wait-ControlReady {
    param([int]$Port)
    $deadline = [DateTime]::UtcNow.AddSeconds(90)
    do {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "http://127.0.0.1:$Port/readyz"
            if ($response.StatusCode -eq 200) { return }
        } catch { Start-Sleep -Milliseconds 500 }
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'Control readiness check timed out.'
}

if ($FromMsi) {
    $options = Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\L4D2Panel\Installer'
    $SkipStart = ConvertFrom-InstallerBoolean (Get-InstallerOption $options 'SkipStart' 'false') 'SKIP_START'
    $SkipMigrate = ConvertFrom-InstallerBoolean (Get-InstallerOption $options 'SkipMigrate' 'false') 'SKIP_MIGRATE'
    $InstallRoot = $PSScriptRoot
}

if ($Action -eq 'RemoveServices') { Remove-CenterServices; exit 0 }
if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
    throw 'The native Control installer supports Windows x64 only.'
}
if ([Environment]::OSVersion.Version.Build -lt 17763) {
    throw 'Windows 10 build 17763 or Windows Server 2019 and later are required.'
}
$controlExe = Join-Path $InstallRoot 'l4d2panel-control.exe'
$controlConfigPath = Join-Path $DataRoot 'config\control.json'
$databasePath = Join-Path $DataRoot 'data\l4d2panel.sqlite3'
$firstInstall = -not (Test-Path -LiteralPath $controlConfigPath)
foreach ($directory in @('config', 'data', 'logs', 'backups')) {
    New-Item -ItemType Directory -Force -Path (Join-Path $DataRoot $directory) | Out-Null
}

foreach ($name in @($script:ControlService)) {
    $service = Get-Service -Name $name -ErrorAction SilentlyContinue
    if ($service -and $service.Status -ne 'Stopped') {
        Stop-Service -Name $name -Force
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
}

if (-not $firstInstall -and (Test-Path -LiteralPath $databasePath)) {
    $checkpoint = Join-Path $DataRoot ("backups\upgrade-{0}.sqlite3" -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
    Invoke-CheckedProcess $controlExe @('backup-database', '--config', $controlConfigPath, '--output', $checkpoint)
}

$masterKey = ''
if (Test-Path -LiteralPath $controlConfigPath) {
    $existing = Get-Content -LiteralPath $controlConfigPath -Raw | ConvertFrom-Json
    if ($existing.PSObject.Properties['master_key']) { $masterKey = [string]$existing.master_key }
}
if ([string]::IsNullOrWhiteSpace($masterKey)) {
    $bytes = New-Object byte[] 32
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $masterKey = [Convert]::ToBase64String($bytes)
}
$controlConfig = [ordered]@{
    schema_version = 2
    listen_address = "0.0.0.0:$($script:ControlPort)"
    database_path = $databasePath
    migrations_dir = (Join-Path $InstallRoot 'migrations\sqlite')
    master_key = $masterKey
    cookie_secure = $true
    session_ttl = '12h'
    public_url = ''
    trusted_proxies = @('127.0.0.1/32', '::1/128')
    timezone = 'Asia/Hong_Kong'
    log_file = (Join-Path $DataRoot 'logs\control.jsonl')
}
[IO.File]::WriteAllText($controlConfigPath, ($controlConfig | ConvertTo-Json -Depth 4), [Text.UTF8Encoding]::new($false))

Invoke-CheckedProcess $controlExe @('doctor', '--config', $controlConfigPath)
if (-not $SkipMigrate) { Invoke-CheckedProcess $controlExe @('migrate', '--config', $controlConfigPath) }

if (Get-Service -Name $script:ControlService -ErrorAction SilentlyContinue) {
    Invoke-CheckedProcess $controlExe @('service', 'uninstall', '--config', $controlConfigPath)
}
Invoke-CheckedProcess $controlExe @('service', 'install', '--config', $controlConfigPath)
Set-RestrictedDirectoryAcl $InstallRoot @($script:ControlService) -ReadOnlyService
Set-RestrictedDirectoryAcl (Join-Path $DataRoot 'config') @($script:ControlService) -ReadOnlyService
Set-RestrictedDirectoryAcl (Join-Path $DataRoot 'data') @($script:ControlService)
Set-RestrictedDirectoryAcl (Join-Path $DataRoot 'logs') @($script:ControlService)
Set-RestrictedDirectoryAcl (Join-Path $DataRoot 'backups') @($script:ControlService)
Set-RestrictedFileAcl $controlConfigPath $script:ControlService

Get-NetFirewallRule -Group 'L4D2 Panel Center' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName "L4D2 Panel Center TCP $($script:ControlPort)" -Group 'L4D2 Panel Center' `
    -Direction Inbound -Action Allow -Protocol TCP -LocalPort $script:ControlPort -Profile Any | Out-Null

if (-not $SkipStart) {
    Start-Service $script:ControlService
    (Get-Service $script:ControlService).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    Wait-ControlReady $script:ControlPort
}
Write-Output "L4D2 Panel Control installed on HTTP port $($script:ControlPort). Configure HTTPS, then run New-L4D2PanelSetupKey.ps1."
