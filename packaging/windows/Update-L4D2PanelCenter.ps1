#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$MsiPath,
    [Parameter(Mandatory)][ValidatePattern('^[A-Fa-f0-9]{64}$')][string]$Sha256,
    [string]$SignerThumbprint = '',
    [string]$InstallRoot = "$env:ProgramFiles\L4D2Panel\Control",
    [string]$DataRoot = "$env:ProgramData\L4D2Panel"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$msi = (Get-Item -LiteralPath $MsiPath).FullName
$actualHash = (Get-FileHash -LiteralPath $msi -Algorithm SHA256).Hash
if ($actualHash -ne $Sha256) { throw "MSI SHA-256 mismatch. Expected $Sha256, got $actualHash." }
if ($SignerThumbprint) {
    $signature = Get-AuthenticodeSignature -LiteralPath $msi
    if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Thumbprint -ne ($SignerThumbprint -replace ' ', '')) {
        throw 'MSI Authenticode signature validation failed.'
    }
} else {
    Write-Warning 'No signer thumbprint was supplied; only the required SHA-256 was verified.'
}

$configPath = "$DataRoot\config\control.json"
if (-not (Test-Path -LiteralPath $configPath)) { throw "Control configuration not found: $configPath" }
$config = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json
if ($config.schema_version -ne 1) { throw 'Unsupported Control configuration schema.' }

$systemDrive = Get-PSDrive -Name ([IO.Path]::GetPathRoot($DataRoot).Substring(0, 1))
if ($systemDrive.Free -lt 2GB) { throw 'At least 2 GiB free disk space is required for an upgrade checkpoint.' }
foreach ($name in @('l4d2panel-control', 'l4d2panel-caddy')) {
    if (-not (Get-Service -Name $name -ErrorAction SilentlyContinue)) { Write-Warning "Service $name is not installed." }
}

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$checkpoint = "$DataRoot\backups\center-upgrade-$stamp"
New-Item -ItemType Directory -Force -Path $checkpoint | Out-Null
Copy-Item -LiteralPath $configPath -Destination "$checkpoint\control.json" -Force
if (Test-Path -LiteralPath "$DataRoot\caddy") { Copy-Item -LiteralPath "$DataRoot\caddy" -Destination "$checkpoint\caddy" -Recurse -Force }
Get-ChildItem -LiteralPath $InstallRoot -File -ErrorAction SilentlyContinue |
    Select-Object Name, Length, @{Name='SHA256';Expression={(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash}} |
    ConvertTo-Json | Set-Content -LiteralPath "$checkpoint\binaries.json" -Encoding UTF8

$databaseUri = [Uri][string]$config.database_url
$managedPgDump = "$InstallRoot\postgresql\bin\pg_dump.exe"
$pgDump = if (Test-Path -LiteralPath $managedPgDump) { $managedPgDump } else { (Get-Command pg_dump.exe -ErrorAction Stop).Source }
$pgVersion = & $pgDump --version
if ($LASTEXITCODE -ne 0 -or $pgVersion -notmatch 'PostgreSQL\) 17\.') {
    throw "Only PostgreSQL 17.x is supported for automatic center upgrades. Detected: $pgVersion"
}

# The password is supplied through the process environment and never appears in
# the process list, MSI properties, or update logs.
$databaseUserInfo = $databaseUri.UserInfo.Split(':', 2)
if ($databaseUserInfo.Count -eq 2) { $env:PGPASSWORD = [Uri]::UnescapeDataString($databaseUserInfo[1]) }
$env:PGHOST = $databaseUri.Host
$env:PGPORT = [string]$databaseUri.Port
$env:PGUSER = [Uri]::UnescapeDataString($databaseUserInfo[0])
$env:PGDATABASE = $databaseUri.AbsolutePath.TrimStart('/')
foreach ($part in $databaseUri.Query.TrimStart('?').Split('&', [StringSplitOptions]::RemoveEmptyEntries)) {
    $pair = $part.Split('=', 2)
    if ($pair[0] -eq 'sslmode' -and $pair.Count -eq 2) { $env:PGSSLMODE = [Uri]::UnescapeDataString($pair[1]) }
}
try {
    $dumpArgs = @('--format=custom', '--file', "$checkpoint\database.dump")
    & $pgDump @dumpArgs
    if ($LASTEXITCODE -ne 0) { throw 'pg_dump failed; the MSI was not started.' }
} finally {
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
    foreach ($name in @('PGHOST', 'PGPORT', 'PGUSER', 'PGDATABASE', 'PGSSLMODE')) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
}

Stop-Service -Name l4d2panel-caddy -Force -ErrorAction SilentlyContinue
Stop-Service -Name l4d2panel-control -Force -ErrorAction SilentlyContinue
if (Get-Service -Name l4d2panel-postgres -ErrorAction SilentlyContinue) {
    Stop-Service -Name l4d2panel-postgres -Force
}

$msiArguments = @('/i', $msi, '/qn', '/norestart', 'SKIP_START=$true', 'SKIP_MIGRATE=$true', "UPGRADE_CHECKPOINT=$checkpoint")
$installer = Start-Process -FilePath "$env:SystemRoot\System32\msiexec.exe" -ArgumentList $msiArguments -Wait -PassThru
if ($installer.ExitCode -notin @(0, 3010)) {
    Write-Error "MSI failed with exit code $($installer.ExitCode). Services remain stopped. Checkpoint: $checkpoint"
    exit $installer.ExitCode
}

$controlExe = "$InstallRoot\l4d2panel-control.exe"
try {
    & $controlExe migrate --config $configPath
    if ($LASTEXITCODE -ne 0) { throw 'Database migration failed.' }
    Start-Service -Name l4d2panel-control
    (Get-Service -Name l4d2panel-control).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    $ready = Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:8080/readyz' -TimeoutSec 15
    if ($ready.StatusCode -ne 200) { throw 'Control readiness check failed.' }
    $caddyService = Get-Service -Name l4d2panel-caddy -ErrorAction SilentlyContinue
    if ($caddyService) {
        Start-Service -Name l4d2panel-caddy
        $caddyService.WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    }
} catch {
    Stop-Service -Name l4d2panel-control -Force -ErrorAction SilentlyContinue
    Stop-Service -Name l4d2panel-postgres -Force -ErrorAction SilentlyContinue
    Write-Error @"
Upgrade stopped after the database checkpoint was created. Old Control is not restarted after a committed migration.
Checkpoint: $checkpoint
For managed PostgreSQL, start l4d2panel-postgres before inspection.
Inspect: "$controlExe" doctor --config "$configPath"
Database restore (destructive, explicit): pg_restore --clean --if-exists --dbname <target> "$checkpoint\database.dump"
Error: $_
"@
    exit 1
}

Write-Output "Upgrade completed. Checkpoint retained at $checkpoint"
