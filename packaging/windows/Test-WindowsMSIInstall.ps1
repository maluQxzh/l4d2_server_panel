#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$MsiPath,
    [string]$InstallRoot = "$env:ProgramFiles\L4D2Panel\Control",
    [string]$DataRoot = "$env:ProgramData\L4D2Panel",
    [string]$ReportPath = '',
    [switch]$CleanupTestData
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$ControlPort = 19090

function Assert-True {
    param([bool]$Condition, [Parameter(Mandatory)][string]$Message)
    if (-not $Condition) { throw $Message }
}

function Wait-HttpStatus {
    param([Parameter(Mandatory)][string]$Uri, [ValidateRange(1, 300)][int]$TimeoutSeconds = 60)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $lastError = $null
    do {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri $Uri -TimeoutSec 5
            if ($response.StatusCode -eq 200) { return $response }
            $lastError = "HTTP $($response.StatusCode)"
        } catch {
            $lastError = $_.Exception.Message
        }
        Start-Sleep -Milliseconds 500
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for $Uri. Last error: $lastError"
}

function Invoke-MsiExec {
    param([Parameter(Mandatory)][string[]]$Arguments, [Parameter(Mandatory)][string]$Operation)
    $process = Start-Process -FilePath "$env:SystemRoot\System32\msiexec.exe" -ArgumentList $Arguments -Wait -PassThru
    if ($process.ExitCode -notin @(0, 3010)) { throw "$Operation failed with Windows Installer exit code $($process.ExitCode)." }
    return $process.ExitCode
}

$msi = (Get-Item -LiteralPath $MsiPath).FullName
$installRootFull = [IO.Path]::GetFullPath($InstallRoot).TrimEnd('\')
$dataRootFull = [IO.Path]::GetFullPath($DataRoot).TrimEnd('\')
if (-not $ReportPath) { $ReportPath = [IO.Path]::ChangeExtension($msi, '.smoke.json') }
$report = [ordered]@{
    schema_version = 1
    msi = $msi
    msi_sha256 = (Get-FileHash -LiteralPath $msi -Algorithm SHA256).Hash
    started_at = (Get-Date).ToUniversalTime().ToString('o')
    install_root = $installRootFull
    data_root = $dataRootFull
    ports = @{control=$ControlPort}
    checks = [ordered]@{}
}

Assert-True (-not (Test-Path -LiteralPath $installRootFull)) "Smoke-test install root already exists: $installRootFull"
Assert-True (-not (Test-Path -LiteralPath $dataRootFull)) "Smoke-test data root already exists: $dataRootFull"
foreach ($name in @('l4d2panel-control')) {
    Assert-True (-not (Get-Service -Name $name -ErrorAction SilentlyContinue)) "Smoke-test service already exists: $name"
}
foreach ($port in @($ControlPort, 55432)) {
    Assert-True (-not (Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue)) "Smoke-test TCP port is already in use: $port"
}

$installLog = [IO.Path]::ChangeExtension($ReportPath, '.install.log')
$repairLog = [IO.Path]::ChangeExtension($ReportPath, '.repair.log')
$uninstallLog = [IO.Path]::ChangeExtension($ReportPath, '.uninstall.log')
$installed = $false
try {
    $installArguments = @(
        '/i', $msi, '/qn', '/norestart', '/l*v', $installLog,
        "INSTALLFOLDER=$installRootFull"
    )
    $report.install_exit_code = Invoke-MsiExec -Arguments $installArguments -Operation 'MSI smoke install'
    $installed = $true

    $services = @{}
    foreach ($name in @('l4d2panel-control')) {
        $service = Get-Service -Name $name
        $service.WaitForStatus('Running', [TimeSpan]::FromSeconds(45))
        $cim = Get-CimInstance Win32_Service -Filter "Name='$name'"
        Assert-True ($service.Status -eq 'Running') "$name is not running."
        Assert-True ($cim.StartName -eq "NT SERVICE\$name") "$name uses unexpected account $($cim.StartName)."
        Assert-True ($cim.PathName -notmatch 'postgres(?:ql)?://|master_key|MASTER_KEY') "$name command line contains a secret."
        $services[$name] = @{status=[string]$service.Status; start_name=$cim.StartName; start_mode=$cim.StartMode}
    }
    $report.checks.services = $services

    $configPath = Join-Path $dataRootFull 'config\control.json'
    $config = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json
    Assert-True ($config.schema_version -eq 2 -and $config.listen_address -eq "0.0.0.0:$ControlPort") 'Control configuration has an unexpected schema or listen address.'
    Assert-True ([string]$config.master_key -match '^[A-Za-z0-9+/]{43}=$') 'Control master key was not generated correctly.'
    Assert-True ([string]$config.database_path -eq (Join-Path $dataRootFull 'data\l4d2panel.sqlite3')) 'Control database_path is incorrect.'
    $report.checks.configuration = @{listen_address=$config.listen_address; schema_version=$config.schema_version; database_path=$config.database_path}

    $configIdentities = @((Get-Acl -LiteralPath $configPath).Access | ForEach-Object { $_.IdentityReference.Value })
    Assert-True ($configIdentities -contains 'NT SERVICE\l4d2panel-control') 'Control has no read ACL on its secret configuration.'
    foreach ($sqliteFile in @('l4d2panel.sqlite3', 'l4d2panel.sqlite3-wal', 'l4d2panel.sqlite3-shm')) {
        $path = Join-Path $dataRootFull "data\$sqliteFile"
        Assert-True (Test-Path -LiteralPath $path) "$sqliteFile was not created."
        $identities = @((Get-Acl -LiteralPath $path).Access | ForEach-Object { $_.IdentityReference.Value })
        Assert-True ($identities -contains 'NT SERVICE\l4d2panel-control') "$sqliteFile is not writable by Control."
    }
    $report.checks.secret_acl_isolation = $true

    $controlListener = Get-NetTCPConnection -State Listen -LocalPort $ControlPort
    Assert-True (@($controlListener | Where-Object LocalAddress -eq '0.0.0.0').Count -ge 1) 'Control is not listening on all IPv4 interfaces.'
    Assert-True (-not (Get-NetTCPConnection -State Listen -LocalPort 55432 -ErrorAction SilentlyContinue)) 'Legacy PostgreSQL port 55432 is listening.'
    $report.checks.listeners = @{control='0.0.0.0'; legacy_database_port=$false}

    $ready = Wait-HttpStatus -Uri "http://127.0.0.1:$ControlPort/readyz"
    $setup = Wait-HttpStatus -Uri "http://127.0.0.1:$ControlPort/setup"
    $report.checks.http = @{ready=$ready.StatusCode; setup=$setup.StatusCode}

    $setupKey = (& (Join-Path $installRootFull 'l4d2panel-control.exe') setup-key issue --config $configPath | Out-String).Trim()
    Assert-True ($LASTEXITCODE -eq 0 -and $setupKey -match '^l4d2p_setup_[A-Za-z0-9_-]{43}$') 'Initialization key was not issued.'
    $bootstrapResponse = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$ControlPort/api/v1/auth/bootstrap"
    $bootstrap = $bootstrapResponse.Content | ConvertFrom-Json
    Assert-True ($bootstrap.required -and $bootstrap.key_configured -and $bootstrap.csrf_token) 'First-owner bootstrap is not available.'
    $bootstrapCookie = ([string]$bootstrapResponse.Headers['Set-Cookie'] -split ';')[0]
    $ownerBody = @{setup_key=$setupKey} | ConvertTo-Json
    $owner = Invoke-RestMethod -UseBasicParsing -Method Post -ContentType 'application/json' `
        -Headers @{'X-Bootstrap-CSRF-Token'=$bootstrap.csrf_token; 'Idempotency-Key'=[guid]::NewGuid().ToString(); Cookie=$bootstrapCookie} -Body $ownerBody `
        -Uri "http://127.0.0.1:$ControlPort/api/v1/auth/bootstrap"
    Assert-True ($owner.user.role -eq 'owner' -and $owner.user.username -eq 'admin') 'First owner was not created.'
    $setupKey = $null
    $report.checks.first_owner_created = $true

    $firewallRules = @(Get-NetFirewallRule -Group 'L4D2 Panel Center')
    Assert-True ($firewallRules.Count -eq 1 -and $firewallRules[0].Enabled -eq 'True' -and $firewallRules[0].Direction -eq 'Inbound' -and $firewallRules[0].Action -eq 'Allow' -and $firewallRules[0].Profile -eq 'Any') 'Firewall rule is not an enabled inbound allow rule for every profile.'
    $addressFilters = @($firewallRules | Get-NetFirewallAddressFilter)
    Assert-True ($addressFilters.Count -eq 1 -and [string]$addressFilters[0].RemoteAddress -eq 'Any') 'Firewall rule does not allow the documented any-source scope.'
    $portFilters = @($firewallRules | Get-NetFirewallPortFilter)
    Assert-True ($portFilters.Count -eq 1 -and $portFilters[0].Protocol -eq 'TCP') 'Firewall rule is not limited to TCP.'
    $firewallPorts = @($portFilters | ForEach-Object { [int]$_.LocalPort } | Sort-Object -Unique)
    Assert-True ($firewallPorts.Count -eq 1 -and $firewallPorts -contains $ControlPort) 'Firewall rules do not expose only TCP 19090.'
    $report.checks.firewall_ports = $firewallPorts

    foreach ($serviceName in @('l4d2panel-control')) {
        $acl = Get-Acl -LiteralPath $installRootFull
        Assert-True (@($acl.Access | Where-Object { $_.IdentityReference.Value -eq "NT SERVICE\$serviceName" -and $_.AccessControlType -eq 'Allow' }).Count -ge 1) "$serviceName has no read/execute ACL on the install root."
    }
    $report.checks.install_root_service_acl = $true

    foreach ($pattern in @('postgres://', 'database_url', 'master_key', 'PGPASSWORD', 'DATABASE_MODE', 'l4d2p_setup_')) {
        Assert-True (@(Select-String -LiteralPath $installLog -SimpleMatch -Pattern $pattern -ErrorAction SilentlyContinue).Count -eq 0) "MSI log contains sensitive pattern $pattern."
    }
    $report.checks.install_log_secret_redaction = $true

    $repairArguments = @(
        '/i', $msi, '/qn', '/norestart', '/l*v', $repairLog, 'REINSTALL=ALL', 'REINSTALLMODE=vomus',
        "INSTALLFOLDER=$installRootFull"
    )
    $report.repair_exit_code = Invoke-MsiExec -Arguments $repairArguments -Operation 'MSI repair/upgrade simulation'
    Wait-HttpStatus -Uri "http://127.0.0.1:$ControlPort/readyz" | Out-Null
    $afterRepair = Invoke-RestMethod -UseBasicParsing -Uri "http://127.0.0.1:$ControlPort/api/v1/auth/bootstrap"
    Assert-True (-not $afterRepair.required) 'Owner data was lost during repair/upgrade.'
    $checkpoints = @(Get-ChildItem -LiteralPath (Join-Path $dataRootFull 'backups') -Filter 'upgrade-*.sqlite3')
    Assert-True ($checkpoints.Count -ge 1) 'Repair/upgrade did not create an SQLite checkpoint.'
    $report.checks.repair_preserved_data = $true

    $marker = Join-Path $dataRootFull 'smoke-preserve.marker'
    [IO.File]::WriteAllText($marker, 'preserve')
    $uninstallArguments = @('/x', $msi, '/qn', '/norestart', '/l*v', $uninstallLog)
    $report.uninstall_exit_code = Invoke-MsiExec -Arguments $uninstallArguments -Operation 'MSI smoke uninstall'
    $installed = $false
    foreach ($name in @('l4d2panel-control')) {
        Assert-True (-not (Get-Service -Name $name -ErrorAction SilentlyContinue)) "$name remained after uninstall."
    }
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $installRootFull 'l4d2panel-control.exe'))) 'Program binaries remained after ordinary uninstall.'
    Assert-True (Test-Path -LiteralPath $marker) 'Ordinary uninstall did not preserve ProgramData.'
    Assert-True (-not (Get-NetFirewallRule -Group 'L4D2 Panel Center' -ErrorAction SilentlyContinue)) 'Firewall rules remained after uninstall.'
    $report.checks.uninstall_preserved_data = $true

    if ($CleanupTestData) {
        & "$PSScriptRoot\Remove-L4D2PanelCenterData.ps1" -Confirmation 'DELETE L4D2PANEL CENTER DATA' -Confirm:$false
        Assert-True (-not (Test-Path -LiteralPath $dataRootFull)) 'Test ProgramData remained after explicit purge.'
        $report.checks.explicit_test_data_cleanup = $true
    }
    $report.ok = $true
} catch {
    $report.ok = $false
    $report.error = $_.Exception.Message
    throw
} finally {
    if ($installed) {
        try {
            Invoke-MsiExec -Arguments @('/x', $msi, '/qn', '/norestart', '/l*v', $uninstallLog) -Operation 'MSI smoke cleanup uninstall' | Out-Null
        } catch {
            $report.cleanup_error = $_.Exception.Message
        }
    }
    $report.finished_at = (Get-Date).ToUniversalTime().ToString('o')
    [IO.File]::WriteAllText([IO.Path]::GetFullPath($ReportPath), ($report | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
}

Write-Output "MSI elevated install smoke test passed: $ReportPath"
