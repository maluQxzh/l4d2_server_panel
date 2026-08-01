#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding(SupportsShouldProcess, ConfirmImpact='High')]
param(
    [Parameter(Mandatory)][string]$Confirmation,
    [string]$DataRoot = "$env:ProgramData\L4D2Panel"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$required = 'DELETE L4D2PANEL CENTER DATA'
if ($Confirmation -cne $required) {
    throw "Refusing cleanup. Pass the exact confirmation phrase: $required"
}
$resolved = [IO.Path]::GetFullPath($DataRoot).TrimEnd('\')
$expected = [IO.Path]::GetFullPath("$env:ProgramData\L4D2Panel").TrimEnd('\')
if ($resolved -ne $expected) { throw "Cleanup target must be exactly $expected" }
foreach ($serviceName in @('l4d2panel-caddy', 'l4d2panel-control')) {
    if (Get-Service -Name $serviceName -ErrorAction SilentlyContinue) {
        throw "Service $serviceName still exists. Uninstall L4D2 Panel before purging data."
    }
}
if ($PSCmdlet.ShouldProcess($resolved, 'Permanently delete databases, keys, certificates, logs, and backups')) {
    Remove-Item -LiteralPath $resolved -Recurse -Force
    Write-Output "Permanently removed $resolved. This cannot be recovered by the panel."
}
