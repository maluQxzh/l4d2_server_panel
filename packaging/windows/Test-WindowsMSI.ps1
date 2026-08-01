#Requires -Version 5.1
[CmdletBinding()]
param([Parameter(Mandatory)][string]$MsiPath)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-True {
    param([bool]$Condition, [Parameter(Mandatory)][string]$Message)
    if (-not $Condition) { throw $Message }
}

function Invoke-MsiQuery {
    param(
        [Parameter(Mandatory)]$Database,
        [Parameter(Mandatory)][string]$Sql,
        [Parameter(Mandatory)][ValidateRange(1, 32)][int]$FieldCount
    )
    $view = $Database.GetType().InvokeMember('OpenView', 'InvokeMethod', $null, $Database, @($Sql))
    try {
        $view.GetType().InvokeMember('Execute', 'InvokeMethod', $null, $view, $null) | Out-Null
        while ($true) {
            $record = $view.GetType().InvokeMember('Fetch', 'InvokeMethod', $null, $view, $null)
            if ($null -eq $record) { break }
            $fields = for ($index = 1; $index -le $FieldCount; $index++) {
                $record.GetType().InvokeMember('StringData', 'GetProperty', $null, $record, @($index))
            }
            [pscustomobject]@{ Fields = [string[]]$fields }
        }
    } finally {
        $view.GetType().InvokeMember('Close', 'InvokeMethod', $null, $view, $null) | Out-Null
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
    }
}

$resolvedMsi = (Get-Item -LiteralPath $MsiPath).FullName
$installer = New-Object -ComObject WindowsInstaller.Installer
$database = $null
try {
    $database = $installer.GetType().InvokeMember('OpenDatabase', 'InvokeMethod', $null, $installer, @($resolvedMsi, 0))

    $properties = @{}
    foreach ($row in Invoke-MsiQuery -Database $database -Sql 'SELECT `Property`, `Value` FROM `Property`' -FieldCount 2) {
        $properties[$row.Fields[0]] = $row.Fields[1]
    }
    $expectedProperties = [ordered]@{
        DATABASE_MODE = 'managed'
        PROXY_MODE = 'caddy'
        PANEL_DOMAIN = 'localhost'
        CONTROL_PORT = '18080'
        HTTP_PORT = '8088'
        HTTPS_PORT = '8443'
        SKIP_START = 'false'
        SKIP_MIGRATE = 'false'
    }
    foreach ($entry in $expectedProperties.GetEnumerator()) {
        Assert-True ($properties.ContainsKey($entry.Key)) "MSI property $($entry.Key) is missing."
        Assert-True ($properties[$entry.Key] -ceq $entry.Value) "MSI property $($entry.Key) must be '$($entry.Value)', got '$($properties[$entry.Key])'."
    }
    foreach ($forbidden in @('DATABASE_URL', 'MASTER_KEY', 'RCON_PASSWORD', 'SMTP_PASSWORD')) {
        Assert-True (-not $properties.ContainsKey($forbidden)) "Sensitive property $forbidden must not be stored in the MSI."
    }
    $secure = @([string]$properties.SecureCustomProperties -split ';')
    foreach ($name in $expectedProperties.Keys) {
        Assert-True ($secure -contains $name) "MSI property $name must be marked Secure for the elevated execute sequence."
    }

    $registry = @{}
    foreach ($row in Invoke-MsiQuery -Database $database -Sql 'SELECT `Root`, `Key`, `Name`, `Value` FROM `Registry`' -FieldCount 4) {
        $registry[$row.Fields[2]] = $row.Fields
    }
    $expectedRegistry = [ordered]@{
        DatabaseMode = '[DATABASE_MODE]'
        ProxyMode = '[PROXY_MODE]'
        PanelDomain = '[PANEL_DOMAIN]'
        ControlPort = '#[CONTROL_PORT]'
        HttpPort = '#[HTTP_PORT]'
        HttpsPort = '#[HTTPS_PORT]'
        ResponseFile = '[RESPONSE_FILE]'
        SkipStart = '[SKIP_START]'
        SkipMigrate = '[SKIP_MIGRATE]'
    }
    foreach ($entry in $expectedRegistry.GetEnumerator()) {
        Assert-True ($registry.ContainsKey($entry.Key)) "Installer registry value $($entry.Key) is missing."
        $fields = $registry[$entry.Key]
        Assert-True ($fields[0] -eq '2' -and $fields[1] -eq 'SOFTWARE\L4D2Panel\Installer') "Installer registry value $($entry.Key) must be under 64-bit HKLM."
        Assert-True ($fields[3] -ceq $entry.Value) "Installer registry value $($entry.Key) has unexpected data '$($fields[3])'."
    }

    $sequence = @{}
    foreach ($row in Invoke-MsiQuery -Database $database -Sql 'SELECT `Action`, `Condition`, `Sequence` FROM `InstallExecuteSequence`' -FieldCount 3) {
        $sequence[$row.Fields[0]] = [int]$row.Fields[2]
    }
    foreach ($action in @('InstallFiles', 'WriteRegistryValues', 'RollbackCenterServices', 'ConfigureCenter', 'RemoveCenterServices', 'RemoveFiles', 'InstallFinalize')) {
        Assert-True ($sequence.ContainsKey($action)) "MSI execute action $action is missing."
    }
    Assert-True ($sequence.InstallFiles -lt $sequence.WriteRegistryValues) 'InstallFiles must run before WriteRegistryValues.'
    Assert-True ($sequence.WriteRegistryValues -lt $sequence.RollbackCenterServices) 'Rollback cleanup must be armed after installer options are written.'
    Assert-True ($sequence.RollbackCenterServices -lt $sequence.ConfigureCenter) 'Rollback cleanup must be armed before center configuration starts.'
    Assert-True ($sequence.ConfigureCenter -lt $sequence.InstallFinalize) 'Center configuration must complete before InstallFinalize.'
    Assert-True ($sequence.RemoveCenterServices -lt $sequence.RemoveFiles) 'Services must be removed before their scripts and binaries.'

    $launchConditions = @(Invoke-MsiQuery -Database $database -Sql 'SELECT `Condition` FROM `LaunchCondition`' -FieldCount 1 | ForEach-Object { $_.Fields[0] })
    Assert-True ($launchConditions -contains 'WIX_NATIVE_MACHINE = 34404') 'The native amd64 launch condition is missing.'
    Assert-True ($launchConditions -contains 'VersionNT64 >= v10.0') 'The supported Windows version launch condition is missing.'

    $controlProperties = @(Invoke-MsiQuery -Database $database -Sql 'SELECT `Property` FROM `Control` WHERE `Dialog_` = ''CenterOptionsDlg''' -FieldCount 1 | ForEach-Object { $_.Fields[0] })
    Assert-True ($controlProperties -contains 'CONTROL_PORT') 'The MSI UI does not expose CONTROL_PORT.'

    $fileNames = @(Invoke-MsiQuery -Database $database -Sql 'SELECT `FileName` FROM `File`' -FieldCount 1 | ForEach-Object { ($_.Fields[0] -split '\|')[-1] })
    Assert-True ($fileNames.Count -gt 500) "MSI payload is unexpectedly small ($($fileNames.Count) files)."
    foreach ($requiredFile in @('Install-L4D2PanelCenter.ps1', 'Update-L4D2PanelCenter.ps1', 'Remove-L4D2PanelCenterData.ps1', 'l4d2panel-control.exe', 'caddy.exe', 'postgres.exe', 'initdb.exe', 'pg_ctl.exe', 'psql.exe', 'createdb.exe', 'pg_dump.exe')) {
        Assert-True ($fileNames -contains $requiredFile) "Required MSI payload file $requiredFile is missing."
    }

    $mediaCabinets = @(Invoke-MsiQuery -Database $database -Sql 'SELECT `Cabinet` FROM `Media`' -FieldCount 1 | ForEach-Object { $_.Fields[0] })
    Assert-True ($mediaCabinets.Count -ge 1 -and @($mediaCabinets | Where-Object { $_ -like '#*' }).Count -eq $mediaCabinets.Count) 'All MSI cabinets must be embedded.'

    $summary = $installer.GetType().InvokeMember('SummaryInformation', 'GetProperty', $null, $installer, @($resolvedMsi, 0))
    try {
        $template = $summary.GetType().InvokeMember('Property', 'GetProperty', $null, $summary, @(7))
        Assert-True ([string]$template -match 'x64') "MSI summary template must target x64, got '$template'."
    } finally {
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($summary)
    }

    Write-Output "MSI structural validation passed: $resolvedMsi"
} finally {
    if ($null -ne $database) { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($database) }
    [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer)
}
