#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [string]$InstallRoot = "$env:ProgramFiles\L4D2Panel\Control",
    [string]$DataRoot = "$env:ProgramData\L4D2Panel"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$control = Join-Path $InstallRoot 'l4d2panel-control.exe'
$config = Join-Path $DataRoot 'config\control.json'
if (-not (Test-Path -LiteralPath $control) -or -not (Test-Path -LiteralPath $config)) {
    throw 'L4D2 Panel Control is not installed or its configuration is missing.'
}
$key = (& $control setup-key issue --config $config | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $key -notmatch '^l4d2p_setup_[A-Za-z0-9_-]{43}$') {
    throw 'The initialization key could not be generated. The center may already be initialized.'
}
Write-Host ''
Write-Host 'L4D2 Panel 初始化密钥（同时是 admin 的初始密码）：' -ForegroundColor Yellow
Write-Host $key -ForegroundColor Green
Write-Host ''
Write-Host '该密钥只显示在当前窗口。请配置 HTTPS 后访问 /setup，并在进入设置后立即修改密码。'
