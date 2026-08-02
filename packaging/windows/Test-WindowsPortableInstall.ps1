#Requires -Version 7.4
#Requires -RunAsAdministrator
[CmdletBinding()]
param([Parameter(Mandatory)][string]$PackagePath)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$package = (Resolve-Path -LiteralPath $PackagePath).Path
$smokeRoot = "C:\l4d2panel-center-smoke-$PID"
if (Test-Path -LiteralPath $smokeRoot) { throw "Smoke root already exists: $smokeRoot" }

try {
    Expand-Archive -LiteralPath $package -DestinationPath $smokeRoot
    $control = Join-Path $smokeRoot 'l4d2panel-control.exe'
    $installOutput = (& $control portable install 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) { throw "Portable install failed: $installOutput" }
    $setupKey = [regex]::Match($installOutput, 'l4d2p_setup_[A-Za-z0-9_-]{43}').Value
    $installOutput = $null
    if (-not $setupKey) { throw 'Portable install did not display a setup key.' }
    if ((Get-Service l4d2panel-control).Status -ne 'Running') { throw 'Control service is not running.' }
    if ((Invoke-WebRequest -UseBasicParsing http://127.0.0.1:19090/readyz).StatusCode -ne 200) { throw 'Control is not ready.' }
    $firewall = @(Get-NetFirewallRule -DisplayName 'L4D2 Panel Center TCP 19090')
    if ($firewall.Count -ne 1 -or $firewall[0].Profile -ne 'Any' -or $firewall[0].Action -ne 'Allow') { throw 'Portable firewall rule is invalid.' }
    $portFilter = @($firewall | Get-NetFirewallPortFilter)
    if ($portFilter.Count -ne 1 -or $portFilter[0].Protocol -ne 'TCP' -or [int]$portFilter[0].LocalPort -ne 19090) { throw 'Portable firewall port is invalid.' }

    $bootstrapResponse = Invoke-WebRequest -UseBasicParsing -Uri http://127.0.0.1:19090/api/v1/auth/bootstrap
    $bootstrap = $bootstrapResponse.Content | ConvertFrom-Json
    $cookie = ([string]$bootstrapResponse.Headers['Set-Cookie'] -split ';')[0]
    $owner = Invoke-RestMethod -UseBasicParsing -Method Post -ContentType 'application/json' -Headers @{
        'X-Bootstrap-CSRF-Token'=$bootstrap.csrf_token; 'Idempotency-Key'=[guid]::NewGuid().ToString(); Cookie=$cookie
    } -Body (@{setup_key=$setupKey} | ConvertTo-Json) -Uri http://127.0.0.1:19090/api/v1/auth/bootstrap
    $setupKey = $null
    if ($owner.user.role -ne 'owner') { throw 'Portable first-owner creation failed.' }

    $upgradeOutput = (& $control portable upgrade $package 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) { throw "Portable upgrade did not start: $upgradeOutput" }
    $jobID = [regex]::Match($upgradeOutput, '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}', 'IgnoreCase').Value
    if (-not $jobID) { throw 'Portable upgrade did not return a job ID.' }
    $statePath = Join-Path $smokeRoot "data\update\jobs\$jobID\state.json"
    $deadline = [DateTime]::UtcNow.AddMinutes(4)
    $state = $null
    do {
        Start-Sleep -Milliseconds 500
        if (Test-Path -LiteralPath $statePath) { $state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json }
    } while ((!$state -or $state.status -in @('queued', 'running')) -and [DateTime]::UtcNow -lt $deadline)
    if (-not $state -or $state.status -ne 'succeeded') { throw "Portable same-version upgrade failed: $($state | ConvertTo-Json -Compress)" }
    if ((Invoke-WebRequest -UseBasicParsing http://127.0.0.1:19090/readyz).StatusCode -ne 200) { throw 'Control did not recover after upgrade.' }
    if ((Invoke-RestMethod -UseBasicParsing http://127.0.0.1:19090/api/v1/auth/bootstrap).required) { throw 'Owner data was lost during portable upgrade.' }

    & $control portable unregister | Out-Null
    if ($LASTEXITCODE -ne 0 -or (Get-Service l4d2panel-control -ErrorAction SilentlyContinue)) { throw 'Portable unregister failed.' }
    if (-not (Test-Path -LiteralPath (Join-Path $smokeRoot 'data\config\control.json'))) { throw 'Portable unregister removed retained data.' }
    & $control portable purge --confirmation 'DELETE L4D2PANEL PORTABLE DATA' | Out-Null
    if ($LASTEXITCODE -ne 0 -or (Test-Path -LiteralPath (Join-Path $smokeRoot 'data'))) { throw 'Portable purge failed.' }
} finally {
    $control = Join-Path $smokeRoot 'l4d2panel-control.exe'
    if (Test-Path -LiteralPath $control) { & $control portable unregister 2>$null | Out-Null }
    netsh.exe advfirewall firewall delete rule name='L4D2 Panel Center TCP 19090' | Out-Null
    if (Test-Path -LiteralPath $smokeRoot) { Remove-Item -LiteralPath $smokeRoot -Recurse -Force }
}
Write-Output 'Windows portable install and same-version upgrade smoke test passed.'
