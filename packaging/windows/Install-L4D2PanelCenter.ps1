#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [ValidateSet('Install', 'RemoveServices')][string]$Action = 'Install',
    [ValidateSet('caddy', 'external')][string]$ProxyMode = 'caddy',
    [string]$PanelDomain = 'localhost',
    [ValidateRange(1, 65535)][int]$ControlPort = 18080,
    [ValidateRange(1, 65535)][int]$HttpPort = 8088,
    [ValidateRange(1, 65535)][int]$HttpsPort = 8443,
    [string]$InstallRoot = "$env:ProgramFiles\L4D2Panel\Control",
    [string]$DataRoot = "$env:ProgramData\L4D2Panel",
    [switch]$SkipStart,
    [switch]$SkipMigrate,
    [switch]$FromMsi,
    [switch]$OpenSetup
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$script:ControlService = 'l4d2panel-control'
$script:CaddyService = 'l4d2panel-caddy'

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
    foreach ($name in @($script:CaddyService, $script:ControlService)) { Remove-ServiceIfPresent $name }
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
    $ProxyMode = [string](Get-InstallerOption $options 'ProxyMode' 'caddy')
    $PanelDomain = [string](Get-InstallerOption $options 'PanelDomain' 'localhost')
    $ControlPort = [int](Get-InstallerOption $options 'ControlPort' 18080)
    $HttpPort = [int](Get-InstallerOption $options 'HttpPort' 8088)
    $HttpsPort = [int](Get-InstallerOption $options 'HttpsPort' 8443)
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
if ($PanelDomain -notmatch '^(localhost|(?=.{1,253}$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)$') {
    throw 'PANEL_DOMAIN must be localhost or a valid DNS host name.'
}

$controlExe = Join-Path $InstallRoot 'l4d2panel-control.exe'
$controlConfigPath = Join-Path $DataRoot 'config\control.json'
$databasePath = Join-Path $DataRoot 'data\l4d2panel.sqlite3'
$firstInstall = -not (Test-Path -LiteralPath $controlConfigPath)
foreach ($directory in @('config', 'data', 'logs', 'backups', 'caddy')) {
    New-Item -ItemType Directory -Force -Path (Join-Path $DataRoot $directory) | Out-Null
}

foreach ($name in @($script:CaddyService, $script:ControlService)) {
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
$publicUrl = if ($HttpsPort -eq 443) { "https://$PanelDomain" } else { "https://$PanelDomain`:$HttpsPort" }
$controlConfig = [ordered]@{
    schema_version = 2
    listen_address = "127.0.0.1:$ControlPort"
    database_path = $databasePath
    migrations_dir = (Join-Path $InstallRoot 'migrations\sqlite')
    master_key = $masterKey
    cookie_secure = $true
    session_ttl = '12h'
    public_url = $publicUrl
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
if ($ProxyMode -eq 'caddy') {
    $caddyExe = Join-Path $InstallRoot 'caddy\caddy.exe'
    $caddyConfig = Join-Path $DataRoot 'caddy\Caddyfile'
    $siteAddress = if ($HttpsPort -eq 443) { "https://$PanelDomain" } else { "https://$PanelDomain`:$HttpsPort" }
    [IO.File]::WriteAllText($caddyConfig, @"
{
    admin 127.0.0.1:2019
    auto_https disable_redirects
    storage file_system {
        root $DataRoot\caddy\data
    }
}

:$HttpPort {
    redir $publicUrl{uri} permanent
}

$siteAddress {
    encode zstd gzip
    reverse_proxy 127.0.0.1:$ControlPort
}
"@, [Text.UTF8Encoding]::new($false))
    Invoke-CheckedProcess $caddyExe @('validate', '--config', $caddyConfig)
    Remove-ServiceIfPresent $script:CaddyService
    $command = '"{0}" run --environ --config "{1}"' -f $caddyExe, $caddyConfig
    Invoke-CheckedProcess "$env:SystemRoot\System32\sc.exe" @('create', $script:CaddyService, 'binPath=', $command, 'start=', 'delayed-auto', 'obj=', "NT SERVICE\$($script:CaddyService)", 'depend=', $script:ControlService)
    Invoke-CheckedProcess "$env:SystemRoot\System32\sc.exe" @('failure', $script:CaddyService, 'reset=', '86400', 'actions=', 'restart/10000/restart/30000/none/0')
    Set-RestrictedDirectoryAcl $InstallRoot @($script:ControlService, $script:CaddyService) -ReadOnlyService
    Set-RestrictedDirectoryAcl (Join-Path $DataRoot 'caddy') @($script:CaddyService)
    foreach ($port in @($HttpPort, $HttpsPort)) {
        New-NetFirewallRule -DisplayName "L4D2 Panel Center TCP $port" -Group 'L4D2 Panel Center' -Direction Inbound -Action Allow -Protocol TCP -LocalPort $port | Out-Null
    }
} else {
    Remove-ServiceIfPresent $script:CaddyService
}

if (-not $SkipStart) {
    Start-Service $script:ControlService
    (Get-Service $script:ControlService).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    Wait-ControlReady $ControlPort
    if ($ProxyMode -eq 'caddy') {
        Start-Service $script:CaddyService
        (Get-Service $script:CaddyService).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    }
}
if ($OpenSetup) { Start-Process "$publicUrl/setup" }
Write-Output "L4D2 Panel Control installed. Complete setup at $publicUrl/setup"
