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
$cacheState = "$staging\.cache"
$downloadCache = "$output\download-cache"
$cabinetCache = "$output\cabinet-cache\$buildFlavor\$CompressionLevel"
if ($Clean) {
    if (Test-Path -LiteralPath $staging) { Remove-Item -LiteralPath $staging -Recurse -Force }
    if (Test-Path -LiteralPath $cabinetCache) { Remove-Item -LiteralPath $cabinetCache -Recurse -Force }
    if (Test-Path -LiteralPath $downloadCache) { Remove-Item -LiteralPath $downloadCache -Recurse -Force }
}
if (Test-Path -LiteralPath $cabinetCache) { Remove-Item -LiteralPath $cabinetCache -Recurse -Force }
New-Item -ItemType Directory -Force -Path $payload, $cacheState, $downloadCache, $cabinetCache | Out-Null
foreach ($legacyPath in @("$payload\postgresql", "$staging\postgres-extract", "$cacheState\postgresql.sha256")) {
    if (Test-Path -LiteralPath $legacyPath) { Remove-Item -LiteralPath $legacyPath -Recurse -Force }
}

function Assert-Hash {
    param([string]$Path, [string]$Expected)
    $algorithm = switch ($Expected.Length) {
        64 { 'SHA256' }
        128 { 'SHA512' }
        default { throw "Unsupported checksum length for $Path. Expected a SHA-256 or SHA-512 digest." }
    }
    $actual = (Get-FileHash -LiteralPath $Path -Algorithm $algorithm).Hash
    if ($actual -ne $Expected) { throw "$algorithm mismatch for $Path. Expected $Expected, got $actual." }
}

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

$caddyVersion = '2.11.3'
$caddyAsset = "caddy_${caddyVersion}_windows_amd64.zip"
$caddyArchive = "$downloadCache\$caddyAsset"
$caddyChecksums = "$downloadCache\caddy_${caddyVersion}_checksums.txt"
if (-not (Test-Path -LiteralPath $caddyChecksums)) {
    Invoke-WebRequest -UseBasicParsing -Uri "https://github.com/caddyserver/caddy/releases/download/v$caddyVersion/caddy_${caddyVersion}_checksums.txt" -OutFile $caddyChecksums
}
Assert-Hash -Path $caddyChecksums -Expected '977489c16a19c0187bade0e2d25f9d853e30dfdc63088455c727740791195dd7'
$checksumLine = Get-Content -LiteralPath $caddyChecksums | Where-Object { $_ -match "\s$caddyAsset$" }
if (-not $checksumLine) { throw "Caddy checksum manifest does not contain $caddyAsset." }
$caddyExpected = ($checksumLine -split '\s+')[0].ToLowerInvariant()
if (-not (Test-Path -LiteralPath $caddyArchive)) {
    Invoke-WebRequest -UseBasicParsing -Uri "https://github.com/caddyserver/caddy/releases/download/v$caddyVersion/$caddyAsset" -OutFile $caddyArchive
}
Assert-Hash -Path $caddyArchive -Expected $caddyExpected
$caddyPayload = "$payload\caddy"
$caddyMarker = "$cacheState\caddy.sha512"
$caddyReady = (Test-Path -LiteralPath "$caddyPayload\caddy.exe") -and
    (Test-Path -LiteralPath $caddyMarker) -and
    ((Get-Content -LiteralPath $caddyMarker -Raw).Trim() -eq $caddyExpected)
if (-not $caddyReady) {
    if (Test-Path -LiteralPath $caddyPayload) { Remove-Item -LiteralPath $caddyPayload -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $caddyPayload | Out-Null
    Expand-Archive -LiteralPath $caddyArchive -DestinationPath $caddyPayload
    if (-not (Test-Path -LiteralPath "$caddyPayload\caddy.exe")) { throw 'Caddy archive layout is not recognized.' }
    Set-Content -LiteralPath $caddyMarker -Value $caddyExpected -Encoding ascii
}

$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
Push-Location $repoRoot
try {
    & go build -trimpath -ldflags "-s -w -X github.com/l4d2-panel/l4d2panel/internal/version.Version=$Version" -o "$payload\l4d2panel-control.exe" ./cmd/control
    if ($LASTEXITCODE -ne 0) { throw 'Control x64 build failed.' }
} finally {
    Pop-Location
}
$migrationsPayload = "$payload\migrations"
if (Test-Path -LiteralPath $migrationsPayload) { Remove-Item -LiteralPath $migrationsPayload -Recurse -Force }
New-Item -ItemType Directory -Force -Path "$migrationsPayload\sqlite" | Out-Null
Copy-Item -Path "$repoRoot\migrations\sqlite\*.sql" -Destination "$migrationsPayload\sqlite" -Force

Invoke-Sign -Path "$payload\l4d2panel-control.exe"
Invoke-Sign -Path "$payload\caddy\caddy.exe"

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
    [ordered]@{ type='application'; name='caddy'; version=$caddyVersion; licenses=@(@{license=@{id='Apache-2.0'}}) },
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

Caddy $caddyVersion - Apache License 2.0 - https://github.com/caddyserver/caddy
modernc.org/sqlite v1.55.0 (SQLite 3.53.3) - BSD-3-Clause - https://modernc.org/sqlite
modernc.org/libc v1.74.1 - BSD-3-Clause - https://modernc.org/libc

The release pipeline retains upstream LICENSE files inside each payload directory.
"@ | Set-Content -LiteralPath "$payload\THIRD-PARTY-NOTICES.txt" -Encoding utf8NoBOM

Copy-Item -LiteralPath "$PSScriptRoot\Install-L4D2PanelCenter.ps1", "$PSScriptRoot\Update-L4D2PanelCenter.ps1", "$PSScriptRoot\Remove-L4D2PanelCenterData.ps1", "$PSScriptRoot\Package.wxs", "$PSScriptRoot\L4D2Panel.Center.wixproj" -Destination $staging
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
