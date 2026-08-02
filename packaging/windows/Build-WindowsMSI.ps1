#Requires -Version 7.4
[CmdletBinding()]
param(
    [string]$Version = '0.1.0',
    [string]$Configuration = 'Release',
    [ValidateSet('none', 'low', 'medium', 'high', 'mszip')][string]$CompressionLevel = 'high',
    [string]$OutputDirectory = '',
    [string]$SigningCertificateThumbprint = $env:WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT,
    [string]$TimestampUrl = $env:WINDOWS_SIGNING_TIMESTAMP_URL,
    [switch]$Fast,
    [switch]$Clean
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

if ($Fast -and -not $PSBoundParameters.ContainsKey('CompressionLevel')) {
    $CompressionLevel = 'low'
}

$repoRoot = (Resolve-Path "$PSScriptRoot\..\..").Path
if (-not $OutputDirectory) { $OutputDirectory = "$repoRoot\dist\windows-center" }
$output = [IO.Path]::GetFullPath($OutputDirectory)
$buildFlavor = if ($SigningCertificateThumbprint) {
    'signed-' + (($SigningCertificateThumbprint -replace '[^A-Fa-f0-9]', '').ToLowerInvariant())
} else {
    'unsigned'
}
$staging = if ($buildFlavor -eq 'unsigned') { "$output\staging" } else { "$output\staging-$buildFlavor" }
$payload = "$staging\payload"
$cabinetCache = "$output\cabinet-cache\$buildFlavor\$CompressionLevel"
if ($Clean) {
    if (Test-Path -LiteralPath $staging) { Remove-Item -LiteralPath $staging -Recurse -Force }
    if (Test-Path -LiteralPath $cabinetCache) { Remove-Item -LiteralPath $cabinetCache -Recurse -Force }
}
if (Test-Path -LiteralPath $cabinetCache) { Remove-Item -LiteralPath $cabinetCache -Recurse -Force }
if (Test-Path -LiteralPath $payload) { Remove-Item -LiteralPath $payload -Recurse -Force }
New-Item -ItemType Directory -Force -Path $payload, $cabinetCache | Out-Null

function Invoke-Sign {
    param([string]$Path)
    if (-not $SigningCertificateThumbprint) { return }
    if (-not $TimestampUrl) { throw 'WINDOWS_SIGNING_TIMESTAMP_URL is required when signing is enabled.' }
    $signtool = (Get-Command signtool.exe -ErrorAction Stop).Source
    & $signtool sign /sha1 ($SigningCertificateThumbprint -replace ' ', '') /fd SHA256 /tr $TimestampUrl /td SHA256 $Path
    if ($LASTEXITCODE -ne 0) { throw "Authenticode signing failed: $Path" }
    & $signtool verify /pa /all $Path
    if ($LASTEXITCODE -ne 0) { throw "Authenticode verification failed: $Path" }
}

$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$nodeMajor = [int](& node -p "process.versions.node.split('.')[0]")
if ($LASTEXITCODE -ne 0 -or $nodeMajor -lt 24) { throw 'Node.js 24 or later is required to build the embedded web application.' }
Push-Location "$repoRoot\web"
try {
    & npm ci
    if ($LASTEXITCODE -ne 0) { throw 'Web dependency installation failed.' }
    & npm run build
    if ($LASTEXITCODE -ne 0) { throw 'Web production build failed.' }
} finally {
    Pop-Location
}

$sourceStatic = "$repoRoot\internal\control\static"
$staticBackup = "$staging\static-source-backup"
if (Test-Path -LiteralPath $staticBackup) { Remove-Item -LiteralPath $staticBackup -Recurse -Force }
Copy-Item -LiteralPath $sourceStatic -Destination $staticBackup -Recurse
try {
    Remove-Item -LiteralPath $sourceStatic -Recurse -Force
    New-Item -ItemType Directory -Force -Path $sourceStatic | Out-Null
    Copy-Item -Path "$repoRoot\web\dist\*" -Destination $sourceStatic -Recurse -Force
    Push-Location $repoRoot
    try {
        & go build -trimpath -ldflags "-s -w -X github.com/l4d2-panel/l4d2panel/internal/version.Version=$Version" -o "$payload\l4d2panel-control.exe" ./cmd/control
        if ($LASTEXITCODE -ne 0) { throw 'Control x64 build failed.' }
    } finally {
        Pop-Location
    }
} finally {
    if (Test-Path -LiteralPath $sourceStatic) { Remove-Item -LiteralPath $sourceStatic -Recurse -Force }
    Move-Item -LiteralPath $staticBackup -Destination $sourceStatic
}
$migrationsPayload = "$payload\migrations"
if (Test-Path -LiteralPath $migrationsPayload) { Remove-Item -LiteralPath $migrationsPayload -Recurse -Force }
New-Item -ItemType Directory -Force -Path "$migrationsPayload\sqlite" | Out-Null
Copy-Item -Path "$repoRoot\migrations\sqlite\*.sql" -Destination "$migrationsPayload\sqlite" -Force

Invoke-Sign -Path "$payload\l4d2panel-control.exe"

$goModuleCache = (& go env GOMODCACHE).Trim()
if (-not $goModuleCache) { throw 'Unable to locate the Go module cache for third-party licenses.' }
$licensePayload = "$payload\licenses"
if (Test-Path -LiteralPath $licensePayload) { Remove-Item -LiteralPath $licensePayload -Recurse -Force }
New-Item -ItemType Directory -Force -Path $licensePayload | Out-Null
Copy-Item -LiteralPath "$goModuleCache\modernc.org\sqlite@v1.55.0\LICENSE" -Destination "$licensePayload\modernc-sqlite-LICENSE.txt"
Copy-Item -LiteralPath "$goModuleCache\modernc.org\libc@v1.74.1\LICENSE" -Destination "$licensePayload\modernc-libc-LICENSE.txt"
Copy-Item -LiteralPath "$goModuleCache\modernc.org\libc@v1.74.1\LICENSE-3RD-PARTY.md" -Destination "$licensePayload\modernc-libc-LICENSE-3RD-PARTY.md"

$components = @(
    [ordered]@{ type='application'; name='l4d2panel-control'; version=$Version; licenses=@(@{license=@{id='AGPL-3.0-or-later'}}) },
    [ordered]@{ type='library'; name='modernc.org/sqlite'; version='1.55.0'; properties=@(@{name='embedded.sqlite.version';value='3.53.3'}); licenses=@(@{license=@{id='BSD-3-Clause'}}) },
    [ordered]@{ type='library'; name='modernc.org/libc'; version='1.74.1'; licenses=@(@{license=@{id='BSD-3-Clause'}}) }
)
$sbom = [ordered]@{
    bomFormat='CycloneDX'; specVersion='1.5'; serialNumber="urn:uuid:$([guid]::NewGuid())"; version=1
    metadata=@{timestamp=(Get-Date).ToUniversalTime().ToString('o'); component=@{type='application'; name='l4d2panel-center'; version=$Version}}
    components=$components
}
$sbom | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath "$payload\sbom.cdx.json" -Encoding utf8NoBOM
@"
L4D2 Panel Center third-party payload

modernc.org/sqlite v1.55.0 (SQLite 3.53.3) - BSD-3-Clause - https://modernc.org/sqlite
modernc.org/libc v1.74.1 - BSD-3-Clause - https://modernc.org/libc

The release pipeline retains upstream LICENSE files inside each payload directory.
"@ | Set-Content -LiteralPath "$payload\THIRD-PARTY-NOTICES.txt" -Encoding utf8NoBOM

Copy-Item -LiteralPath "$PSScriptRoot\Install-L4D2PanelCenter.ps1", "$PSScriptRoot\Update-L4D2PanelCenter.ps1", "$PSScriptRoot\Remove-L4D2PanelCenterData.ps1", "$PSScriptRoot\New-L4D2PanelSetupKey.ps1", "$PSScriptRoot\New-L4D2PanelSetupKey.cmd", "$PSScriptRoot\Package.wxs", "$PSScriptRoot\L4D2Panel.Center.wixproj" -Destination $staging
$dotnetArguments = @(
    'build', "$staging\L4D2Panel.Center.wixproj", '-c', $Configuration,
    "-p:PanelVersion=$Version", "-p:PayloadDir=$payload", "-p:CompressionLevel=$CompressionLevel",
    '-p:ReuseCabinetCache=true', "-p:CabinetCachePath=$cabinetCache"
)
if ($Fast) {
    $dotnetArguments += '-p:SuppressValidation=true'
    if (Test-Path -LiteralPath "$staging\obj\project.assets.json") { $dotnetArguments += '--no-restore' }
}
& dotnet @dotnetArguments
if ($LASTEXITCODE -ne 0) { throw 'WiX MSI build failed.' }
$msi = Get-ChildItem -LiteralPath "$staging\bin" -Recurse -Filter *.msi | Select-Object -First 1
if (-not $msi) { throw 'WiX completed without producing an MSI.' }
$suffix = if ($SigningCertificateThumbprint) { '' } else { '-unsigned' }
$profileSuffix = if ($Fast) { '-dev' } else { '' }
$finalMsi = "$output\l4d2panel-center-$Version-windows-x64$profileSuffix$suffix.msi"
Copy-Item -LiteralPath $msi.FullName -Destination $finalMsi -Force
Invoke-Sign -Path $finalMsi
& "$PSScriptRoot\Test-WindowsMSI.ps1" -MsiPath $finalMsi
if ($LASTEXITCODE -ne 0) { throw 'MSI structural validation failed.' }
(Get-FileHash -LiteralPath $finalMsi -Algorithm SHA256).Hash.ToLowerInvariant() + "  " + [IO.Path]::GetFileName($finalMsi) |
    Set-Content -LiteralPath "$finalMsi.sha256" -Encoding ascii
Copy-Item -LiteralPath "$payload\sbom.cdx.json", "$payload\THIRD-PARTY-NOTICES.txt" -Destination $output -Force
Write-Output $finalMsi
