#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [ValidateSet('Install', 'RemoveServices')]
    [string]$Action = 'Install',
    [ValidateSet('managed', 'external')]
    [string]$DatabaseMode = 'managed',
    [ValidateSet('caddy', 'external')]
    [string]$ProxyMode = 'caddy',
    [string]$PanelDomain = 'localhost',
    [ValidateRange(1, 65535)]
    [int]$ControlPort = 18080,
    [ValidateRange(1, 65535)]
    [int]$HttpPort = 8088,
    [ValidateRange(1, 65535)]
    [int]$HttpsPort = 8443,
    [string]$ResponseFile = '',
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

$script:PostgresService = 'l4d2panel-postgres'
$script:ControlService = 'l4d2panel-control'
$script:CaddyService = 'l4d2panel-caddy'

function Get-InstallerOption {
    param(
        [Parameter(Mandatory)]$Options,
        [Parameter(Mandatory)][string]$Name,
        $Default = $null
    )
    $property = $Options.PSObject.Properties[$Name]
    if ($null -eq $property) { return $Default }
    return $property.Value
}

function ConvertFrom-InstallerBoolean {
    param([Parameter(Mandatory)]$Value, [Parameter(Mandatory)][string]$Name)
    switch (([string]$Value).Trim().ToLowerInvariant()) {
        { $_ -in @('true', '1', '$true') } { return $true }
        { $_ -in @('false', '0', '$false', '') } { return $false }
        default { throw "$Name must be true or false." }
    }
}

if ($FromMsi) {
    $msiOptions = Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\L4D2Panel\Installer'
    $DatabaseMode = [string](Get-InstallerOption -Options $msiOptions -Name 'DatabaseMode' -Default 'managed')
    $ProxyMode = [string](Get-InstallerOption -Options $msiOptions -Name 'ProxyMode' -Default 'caddy')
    $PanelDomain = [string](Get-InstallerOption -Options $msiOptions -Name 'PanelDomain' -Default 'localhost')
    $ControlPort = [int](Get-InstallerOption -Options $msiOptions -Name 'ControlPort' -Default 18080)
    $HttpPort = [int](Get-InstallerOption -Options $msiOptions -Name 'HttpPort' -Default 8088)
    $HttpsPort = [int](Get-InstallerOption -Options $msiOptions -Name 'HttpsPort' -Default 8443)
    $ResponseFile = [string](Get-InstallerOption -Options $msiOptions -Name 'ResponseFile' -Default '')
    $SkipStart = ConvertFrom-InstallerBoolean -Value (Get-InstallerOption -Options $msiOptions -Name 'SkipStart' -Default 'false') -Name 'SKIP_START'
    $SkipMigrate = ConvertFrom-InstallerBoolean -Value (Get-InstallerOption -Options $msiOptions -Name 'SkipMigrate' -Default 'false') -Name 'SKIP_MIGRATE'
    $InstallRoot = $PSScriptRoot
}

function Invoke-CheckedProcess {
    param([Parameter(Mandatory)][string]$FilePath, [Parameter(Mandatory)][string[]]$ArgumentList)
    & $FilePath @ArgumentList
    if ($LASTEXITCODE -ne 0) {
        throw "Process failed with exit code $LASTEXITCODE`: $FilePath"
    }
}

function Remove-ServiceIfPresent {
    param([Parameter(Mandatory)][string]$Name)
    $service = Get-Service -Name $Name -ErrorAction SilentlyContinue
    if ($null -eq $service) { return }
    if ($service.Status -ne 'Stopped') {
        Stop-Service -Name $Name -Force -ErrorAction SilentlyContinue
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
    Invoke-CheckedProcess -FilePath "$env:SystemRoot\System32\sc.exe" -ArgumentList @('delete', $Name)
    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        if (-not (Get-Service -Name $Name -ErrorAction SilentlyContinue)) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "Timed out deleting Windows service $Name."
}

function Remove-CenterServices {
    foreach ($name in @($script:CaddyService, $script:ControlService, $script:PostgresService)) {
        Remove-ServiceIfPresent -Name $name
    }
    Get-NetFirewallRule -Group 'L4D2 Panel Center' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    Write-Output 'L4D2 Panel services were removed. ProgramData was preserved.'
}

if ($Action -eq 'RemoveServices') {
    Remove-CenterServices
    exit 0
}

if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
    throw 'The native Control installer supports Windows x64 only.'
}
if ([Environment]::OSVersion.Version.Major -lt 10 -or [Environment]::OSVersion.Version.Build -lt 17763) {
    throw 'Windows 10 build 17763 or Windows Server 2019 and later are required.'
}
if ($PanelDomain -notmatch '^(localhost|(?=.{1,253}$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)$') {
    throw 'PANEL_DOMAIN must be localhost or a valid DNS host name.'
}

function Assert-PortAvailable {
    param([int]$Port)
    $listeners = Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue
    if ($listeners) { throw "TCP port $Port is already in use." }
}

$controlConfigPath = "$DataRoot\config\control.json"
$installStatePath = "$DataRoot\config\install.json"
$firstInstall = -not (Test-Path -LiteralPath $controlConfigPath)
if (-not $firstInstall -and (Test-Path -LiteralPath $installStatePath)) {
    $installState = Get-Content -LiteralPath $installStatePath -Raw | ConvertFrom-Json
    if ($installState.schema_version -notin @(1, 2)) { throw 'Unsupported Windows install-state schema.' }
    $DatabaseMode, $ProxyMode, $PanelDomain = [string]$installState.database_mode, [string]$installState.proxy_mode, [string]$installState.panel_domain
    $HttpPort, $HttpsPort = [int]$installState.http_port, [int]$installState.https_port
    if ($installState.schema_version -eq 2) {
        $ControlPort = [int]$installState.control_port
    } else {
        $existingPortConfig = Get-Content -LiteralPath $controlConfigPath -Raw | ConvertFrom-Json
        if ([string]$existingPortConfig.listen_address -notmatch '^127\.0\.0\.1:([0-9]{1,5})$') {
            throw 'Existing Control listen_address must use an IPv4 loopback port.'
        }
        $ControlPort = [int]$Matches[1]
    }
} elseif (-not $firstInstall) {
    $legacyConfig = Get-Content -LiteralPath $controlConfigPath -Raw | ConvertFrom-Json
    if ([string]$legacyConfig.listen_address -notmatch '^127\.0\.0\.1:([0-9]{1,5})$') {
        throw 'Existing Control listen_address must use an IPv4 loopback port.'
    }
    $ControlPort = [int]$Matches[1]
    $legacyPublicUri = [Uri][string]$legacyConfig.public_url
    $PanelDomain = $legacyPublicUri.Host
    $HttpsPort = if ($legacyPublicUri.IsDefaultPort) { 443 } else { $legacyPublicUri.Port }
    $ProxyMode = if (Test-Path -LiteralPath "$DataRoot\caddy\Caddyfile") { 'caddy' } else { 'external' }
}
if ($DatabaseMode -notin @('managed', 'external') -or $ProxyMode -notin @('caddy', 'external')) {
    throw 'Windows install state contains an invalid database or proxy mode.'
}
if ($ControlPort -lt 1 -or $ControlPort -gt 65535 -or $HttpPort -lt 1 -or $HttpPort -gt 65535 -or $HttpsPort -lt 1 -or $HttpsPort -gt 65535) {
    throw 'Windows install state contains invalid or conflicting ports.'
}
$assignedPorts = [ordered]@{
    CONTROL_PORT = $ControlPort
    HTTP_PORT = $HttpPort
    HTTPS_PORT = $HttpsPort
}
if ($DatabaseMode -eq 'managed') { $assignedPorts['POSTGRESQL_PORT'] = 55432 }
if ($ProxyMode -eq 'caddy') { $assignedPorts['CADDY_ADMIN_PORT'] = 2019 }
$conflictingPorts = $assignedPorts.GetEnumerator() | Group-Object Value | Where-Object Count -gt 1
if ($conflictingPorts) {
    $details = $conflictingPorts | ForEach-Object { "port $($_.Name): $(($_.Group.Name) -join ', ')" }
    throw "Windows install state contains conflicting ports: $($details -join '; ')."
}
if ($PanelDomain -notmatch '^(localhost|(?=.{1,253}$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)$') {
    throw 'Windows install state contains an invalid panel domain.'
}
$dataDrive = Get-PSDrive -Name ([IO.Path]::GetPathRoot($DataRoot).Substring(0, 1))
if ($dataDrive.Free -lt 2GB) { throw 'At least 2 GiB free disk space is required.' }
if ($firstInstall) {
    Assert-PortAvailable -Port $ControlPort
    if ($DatabaseMode -eq 'managed') { Assert-PortAvailable -Port 55432 }
    if ($ProxyMode -eq 'caddy') {
        Assert-PortAvailable -Port 2019
        Assert-PortAvailable -Port $HttpPort
        Assert-PortAvailable -Port $HttpsPort
    }
}

function Get-SecretHex {
    param([ValidateRange(16, 128)][int]$Bytes)
    $buffer = New-Object byte[] $Bytes
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $generator.GetBytes($buffer) } finally { $generator.Dispose() }
    return ([BitConverter]::ToString($buffer) -replace '-', '').ToLowerInvariant()
}

function Assert-ProtectedResponseFile {
    param([Parameter(Mandatory)][string]$Path)
    $item = Get-Item -LiteralPath $Path
    if ($item.PSIsContainer) { throw 'The response file path must refer to a regular file.' }
    $acl = Get-Acl -LiteralPath $item.FullName
    if (-not $acl.AreAccessRulesProtected) {
        throw 'The external database response file must have ACL inheritance disabled.'
    }
    $allowed = @('S-1-5-32-544', 'S-1-5-18', [Security.Principal.WindowsIdentity]::GetCurrent().User.Value)
    foreach ($entry in $acl.Access) {
        $identitySid = $entry.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value
        if ($entry.AccessControlType -eq 'Allow' -and $allowed -notcontains $identitySid) {
            throw "Response file grants access to an unapproved identity: $($entry.IdentityReference.Value)"
        }
    }
}

function Set-DirectoryAcl {
    param([string]$Path, [string[]]$ServiceNames, [switch]$ReadOnlyService)
    New-Item -ItemType Directory -Force -Path $Path | Out-Null
    $aclArguments = @($Path, '/inheritance:r', '/grant:r', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-18:(OI)(CI)F')
    foreach ($serviceName in $ServiceNames) {
        $rights = if ($ReadOnlyService) { '(OI)(CI)RX' } else { '(OI)(CI)M' }
        $aclArguments += "NT SERVICE\$serviceName`:$rights"
    }
    $aclArguments += '/Q'
    & "$env:SystemRoot\System32\icacls.exe" @aclArguments | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Failed to secure directory tree $Path" }
    if (Get-ChildItem -LiteralPath $Path -Force | Select-Object -First 1) {
        & "$env:SystemRoot\System32\icacls.exe" "$Path\*" '/reset' '/T' '/C' '/Q' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Failed to propagate the secured ACL beneath $Path" }
    }
    foreach ($serviceName in $ServiceNames) {
        & "$env:SystemRoot\System32\icacls.exe" $DataRoot '/grant:r' "NT SERVICE\$serviceName`:RX" '/Q' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Failed to grant $serviceName traverse access to $DataRoot" }
    }
}

function Grant-ServiceInstallRootAccess {
    param([Parameter(Mandatory)][string]$ServiceName)
    & "$env:SystemRoot\System32\icacls.exe" $InstallRoot '/grant:r' "NT SERVICE\$ServiceName`:(OI)(CI)RX" '/Q' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Failed to grant $ServiceName read/execute access to $InstallRoot" }
}

function Wait-ControlReady {
    param([Parameter(Mandatory)][int]$Port, [ValidateRange(1, 300)][int]$TimeoutSeconds = 45)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $lastError = $null
    do {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port/readyz" -TimeoutSec 5
            if ($response.StatusCode -eq 200) { return }
            $lastError = "HTTP $($response.StatusCode)"
        } catch {
            $lastError = $_.Exception.Message
        }
        Start-Sleep -Milliseconds 500
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Control readiness check timed out on 127.0.0.1:$Port. Last error: $lastError"
}

foreach ($path in @($DataRoot, "$DataRoot\config", "$DataRoot\logs", "$DataRoot\backups", "$DataRoot\postgres", "$DataRoot\caddy")) {
    New-Item -ItemType Directory -Force -Path $path | Out-Null
}
& "$env:SystemRoot\System32\icacls.exe" $DataRoot '/inheritance:r' '/grant:r' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' '/T' '/C' '/Q' | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Failed to establish the private ProgramData root: $DataRoot" }

$existingConfig = if ($firstInstall) { $null } else { Get-Content -LiteralPath $controlConfigPath -Raw | ConvertFrom-Json }
$databaseUrl = if ($firstInstall) { '' } else { [string]$existingConfig.database_url }
$databasePassword = ''
if (-not $firstInstall) {
    if ([string]::IsNullOrWhiteSpace($databaseUrl)) { throw 'Existing Control configuration has no database_url.' }
    $existingDatabaseUri = [Uri]$databaseUrl
    $DatabaseMode = if ($existingDatabaseUri.Port -eq 55432 -and (Test-Path -LiteralPath "$DataRoot\postgres\data\PG_VERSION")) { 'managed' } else { 'external' }
    if ($DatabaseMode -eq 'managed') {
        $databasePassword = [Uri]::UnescapeDataString($existingDatabaseUri.UserInfo.Split(':', 2)[1])
    }
} elseif ($DatabaseMode -eq 'external') {
    if ([string]::IsNullOrWhiteSpace($ResponseFile)) {
        throw 'External database mode requires RESPONSE_FILE. Database passwords are not accepted as MSI properties.'
    }
    Assert-ProtectedResponseFile -Path $ResponseFile
    $response = Get-Content -LiteralPath $ResponseFile -Raw | ConvertFrom-Json
    $allowedProperties = @('schema_version', 'database_url')
    foreach ($property in $response.PSObject.Properties.Name) {
        if ($allowedProperties -notcontains $property) { throw "Unknown response-file property: $property" }
    }
    if ($response.schema_version -ne 1 -or [string]::IsNullOrWhiteSpace($response.database_url)) {
        throw 'Response file requires schema_version 1 and database_url.'
    }
    $databaseUrl = [string]$response.database_url
    $uri = [Uri]$databaseUrl
    if ($uri.Scheme -notin @('postgres', 'postgresql')) { throw 'External database URL must use postgres or postgresql.' }
    if ($uri.Host -notin @('127.0.0.1', 'localhost', '::1') -and $uri.Query.TrimStart('?') -notmatch '(^|&)sslmode=(verify-full|verify-ca|require)(&|$)') {
        throw 'Remote external PostgreSQL requires TLS (sslmode=require, verify-ca, or verify-full).'
    }
} else {
    $databasePassword = Get-SecretHex -Bytes 32
    $encodedPassword = [Uri]::EscapeDataString($databasePassword)
    $databaseUrl = "postgres://l4d2panel:$encodedPassword@127.0.0.1:55432/l4d2panel?sslmode=disable"
}

$publicUrl = if ($HttpsPort -eq 443) {
    "https://$PanelDomain"
} else {
    "https://$PanelDomain`:$HttpsPort"
}

if ($firstInstall) {
    $controlConfig = [ordered]@{
        schema_version = 1
        listen_address = "127.0.0.1:$ControlPort"
        database_url = $databaseUrl
        migrations_dir = "$InstallRoot\migrations"
        master_key = Get-SecretHex -Bytes 32
        cookie_secure = $true
        session_ttl = '12h'
        public_url = $publicUrl
        trusted_proxies = @('127.0.0.1/32', '::1/128')
        timezone = 'Asia/Hong_Kong'
        log_file = "$DataRoot\logs\control.jsonl"
    }
    [IO.File]::WriteAllText($controlConfigPath, ($controlConfig | ConvertTo-Json -Depth 4), (New-Object Text.UTF8Encoding($false)))
    & "$env:SystemRoot\System32\icacls.exe" $controlConfigPath '/inheritance:r' '/grant:r' '*S-1-5-32-544:F' '*S-1-5-18:F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Failed to secure the generated Control configuration.' }
} else {
    $publicUrl = [string]$existingConfig.public_url
}
$installState = [ordered]@{
    schema_version = 2
    database_mode = $DatabaseMode
    proxy_mode = $ProxyMode
    panel_domain = $PanelDomain
    control_port = $ControlPort
    http_port = $HttpPort
    https_port = $HttpsPort
}
[IO.File]::WriteAllText($installStatePath, ($installState | ConvertTo-Json), (New-Object Text.UTF8Encoding($false)))
& "$env:SystemRoot\System32\icacls.exe" $installStatePath '/inheritance:r' '/grant:r' '*S-1-5-32-544:F' '*S-1-5-18:F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Failed to secure Windows install state.' }

$controlExe = "$InstallRoot\l4d2panel-control.exe"
if (-not (Test-Path -LiteralPath $controlExe)) { throw "Control executable not found: $controlExe" }

if ($DatabaseMode -eq 'managed') {
    $pgBin = "$InstallRoot\postgresql\bin"
    $pgData = "$DataRoot\postgres\data"
    $passwordFile = "$DataRoot\config\.pg-init-password"
    [IO.File]::WriteAllText($passwordFile, $databasePassword, (New-Object Text.UTF8Encoding($false)))
    & "$env:SystemRoot\System32\icacls.exe" $passwordFile '/inheritance:r' '/grant:r' '*S-1-5-32-544:F' '*S-1-5-18:F' | Out-Null
    try {
        if (-not (Test-Path -LiteralPath "$pgData\PG_VERSION")) {
            Invoke-CheckedProcess -FilePath "$pgBin\initdb.exe" -ArgumentList @('-D', $pgData, '-U', 'l4d2panel', '--pwfile', $passwordFile, '--encoding=UTF8', '--locale=C', '--auth-host=scram-sha-256', '--auth-local=scram-sha-256')
            Add-Content -LiteralPath "$pgData\postgresql.conf" -Encoding ascii -Value @"
listen_addresses = '127.0.0.1'
port = 55432
password_encryption = 'scram-sha-256'
"@
            Set-Content -LiteralPath "$pgData\pg_hba.conf" -Encoding ascii -Value @"
local all all scram-sha-256
host all all 127.0.0.1/32 scram-sha-256
host all all ::1/128 reject
"@
        }
    } finally {
        Remove-Item -LiteralPath $passwordFile -Force -ErrorAction SilentlyContinue
    }
    Remove-ServiceIfPresent -Name $script:PostgresService
    $pgServiceCommand = '"{0}\pg_ctl.exe" runservice -N "{1}" -D "{2}"' -f $pgBin, $script:PostgresService, $pgData
    Invoke-CheckedProcess -FilePath "$env:SystemRoot\System32\sc.exe" -ArgumentList @('create', $script:PostgresService, 'binPath=', $pgServiceCommand, 'start=', 'delayed-auto', 'obj=', "NT SERVICE\$($script:PostgresService)")
    Invoke-CheckedProcess -FilePath "$env:SystemRoot\System32\sc.exe" -ArgumentList @('failure', $script:PostgresService, 'reset=', '86400', 'actions=', 'restart/10000/restart/30000/none/0')
    Grant-ServiceInstallRootAccess -ServiceName $script:PostgresService
    Set-DirectoryAcl -Path "$DataRoot\postgres" -ServiceNames @($script:PostgresService)
    Start-Service -Name $script:PostgresService
    (Get-Service -Name $script:PostgresService).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    $env:PGPASSWORD = $databasePassword
    try {
        $databaseExists = & "$pgBin\psql.exe" -h 127.0.0.1 -p 55432 -U l4d2panel -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='l4d2panel'"
        if ($LASTEXITCODE -ne 0) { throw 'Failed to inspect the managed PostgreSQL cluster.' }
        if (($databaseExists | Out-String).Trim() -ne '1') {
            Invoke-CheckedProcess -FilePath "$pgBin\createdb.exe" -ArgumentList @('-h', '127.0.0.1', '-p', '55432', '-U', 'l4d2panel', 'l4d2panel')
        }
    } finally {
        Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
    }
}

Invoke-CheckedProcess -FilePath $controlExe -ArgumentList @('doctor', '--config', $controlConfigPath)
if (-not $SkipMigrate) {
    Invoke-CheckedProcess -FilePath $controlExe -ArgumentList @('migrate', '--config', $controlConfigPath)
}
if (Get-Service -Name $script:ControlService -ErrorAction SilentlyContinue) {
    Invoke-CheckedProcess -FilePath $controlExe -ArgumentList @('service', 'uninstall', '--config', $controlConfigPath)
}
Invoke-CheckedProcess -FilePath $controlExe -ArgumentList @('service', 'install', '--config', $controlConfigPath)
Grant-ServiceInstallRootAccess -ServiceName $script:ControlService
if ($DatabaseMode -eq 'managed') {
    Invoke-CheckedProcess -FilePath "$env:SystemRoot\System32\sc.exe" -ArgumentList @('config', $script:ControlService, 'depend=', $script:PostgresService)
}

Get-NetFirewallRule -Group 'L4D2 Panel Center' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
if ($ProxyMode -eq 'caddy') {
    $caddyExe = "$InstallRoot\caddy\caddy.exe"
    $caddyConfig = "$DataRoot\caddy\Caddyfile"
    $siteAddress = if ($HttpsPort -eq 443) { "https://$PanelDomain" } else { "https://$PanelDomain`:$HttpsPort" }
    $httpRedirect = ":$HttpPort"
    [IO.File]::WriteAllText($caddyConfig, @"
{
    admin 127.0.0.1:2019
    auto_https disable_redirects
    storage file_system {
        root $DataRoot\caddy\data
    }
}

$httpRedirect {
    redir $publicUrl{uri} permanent
}

$siteAddress {
    encode zstd gzip
    reverse_proxy 127.0.0.1:$ControlPort
}
"@, (New-Object Text.UTF8Encoding($false)))
    Invoke-CheckedProcess -FilePath $caddyExe -ArgumentList @('validate', '--config', $caddyConfig)
    Remove-ServiceIfPresent -Name $script:CaddyService
    $caddyServiceCommand = '"{0}" run --environ --config "{1}"' -f $caddyExe, $caddyConfig
    Invoke-CheckedProcess -FilePath "$env:SystemRoot\System32\sc.exe" -ArgumentList @('create', $script:CaddyService, 'binPath=', $caddyServiceCommand, 'start=', 'delayed-auto', 'obj=', "NT SERVICE\$($script:CaddyService)", 'depend=', $script:ControlService)
    Invoke-CheckedProcess -FilePath "$env:SystemRoot\System32\sc.exe" -ArgumentList @('failure', $script:CaddyService, 'reset=', '86400', 'actions=', 'restart/10000/restart/30000/none/0')
    Grant-ServiceInstallRootAccess -ServiceName $script:CaddyService
    Set-DirectoryAcl -Path "$DataRoot\caddy" -ServiceNames @($script:CaddyService)
    foreach ($port in @($HttpPort, $HttpsPort)) {
        New-NetFirewallRule -DisplayName "L4D2 Panel Center TCP $port" -Group 'L4D2 Panel Center' -Direction Inbound -Action Allow -Protocol TCP -LocalPort $port | Out-Null
    }
}

# Create service SIDs first, then lock secret configuration to the Control SID.
Set-DirectoryAcl -Path "$DataRoot\config" -ServiceNames @($script:ControlService) -ReadOnlyService
Set-DirectoryAcl -Path "$DataRoot\logs" -ServiceNames @($script:ControlService)
Set-DirectoryAcl -Path "$DataRoot\backups" -ServiceNames @($script:ControlService)

if (-not $SkipStart) {
    Start-Service -Name $script:ControlService
    (Get-Service -Name $script:ControlService).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    Wait-ControlReady -Port $ControlPort
    if ($ProxyMode -eq 'caddy') {
        try {
            Start-Service -Name $script:CaddyService
            (Get-Service -Name $script:CaddyService).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
        } catch {
            Write-Warning "Caddy could not become ready. Control and PostgreSQL remain installed. Diagnose with: '$InstallRoot\caddy\caddy.exe' validate --config '$DataRoot\caddy\Caddyfile'"
        }
    }
}

if ($OpenSetup) {
    Start-Process "$publicUrl/setup"
}
Write-Output "L4D2 Panel Control installed. Complete setup at $publicUrl/setup"
