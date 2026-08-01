#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$PostgreSqlArchive,
    [Parameter(Mandatory)][ValidatePattern('^[A-Fa-f0-9]{64}$')][string]$PostgreSqlSha256,
    [string]$Version = '0.1.0',
    [string]$Configuration = 'Release',
    [string]$OutputDirectory = '',
    [string]$SigningCertificateThumbprint = $env:WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT,
    [string]$TimestampUrl = $env:WINDOWS_SIGNING_TIMESTAMP_URL
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repoRoot = (Resolve-Path "$PSScriptRoot\..\..").Path
if (-not $OutputDirectory) { $OutputDirectory = "$repoRoot\dist\windows-center" }
$output = [IO.Path]::GetFullPath($OutputDirectory)
$staging = "$output\staging"
$payload = "$staging\payload"
if (Test-Path -LiteralPath $staging) { Remove-Item -LiteralPath $staging -Recurse -Force }
New-Item -ItemType Directory -Force -Path $payload, "$payload\caddy", "$payload\postgresql" | Out-Null

function Assert-Hash {
    param([string]$Path, [string]$Expected)
    $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
    if ($actual -ne $Expected) { throw "SHA-256 mismatch for $Path. Expected $Expected, got $actual." }
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

$postgresArchivePath = (Resolve-Path $PostgreSqlArchive).Path
Assert-Hash -Path $postgresArchivePath -Expected $PostgreSqlSha256
$archiveName = [IO.Path]::GetFileName($postgresArchivePath)
if ($archiveName -notmatch '^postgresql-17\.[0-9]+.*windows-x64.*\.zip$') {
    throw 'PostgreSQL payload must be a pinned PostgreSQL 17.x Windows x64 binary ZIP.'
}
Expand-Archive -LiteralPath $postgresArchivePath -DestinationPath "$staging\postgres-extract"
$pgRoot = Get-ChildItem -LiteralPath "$staging\postgres-extract" -Directory | Select-Object -First 1
if (-not $pgRoot -or -not (Test-Path -LiteralPath "$($pgRoot.FullName)\bin\postgres.exe")) {
    throw 'PostgreSQL archive layout is not recognized.'
}
Copy-Item -Path "$($pgRoot.FullName)\*" -Destination "$payload\postgresql" -Recurse -Force

$caddyVersion = '2.11.3'
$caddyAsset = "caddy_${caddyVersion}_windows_amd64.zip"
$caddyArchive = "$staging\$caddyAsset"
$caddyChecksums = "$staging\caddy_${caddyVersion}_checksums.txt"
Invoke-WebRequest -UseBasicParsing -Uri "https://github.com/caddyserver/caddy/releases/download/v$caddyVersion/$caddyAsset" -OutFile $caddyArchive
Invoke-WebRequest -UseBasicParsing -Uri "https://github.com/caddyserver/caddy/releases/download/v$caddyVersion/caddy_${caddyVersion}_checksums.txt" -OutFile $caddyChecksums
Assert-Hash -Path $caddyChecksums -Expected '977489c16a19c0187bade0e2d25f9d853e30dfdc63088455c727740791195dd7'
$checksumLine = Get-Content -LiteralPath $caddyChecksums | Where-Object { $_ -match "\s$caddyAsset$" }
if (-not $checksumLine) { throw "Caddy checksum manifest does not contain $caddyAsset." }
Assert-Hash -Path $caddyArchive -Expected (($checksumLine -split '\s+')[0])
Expand-Archive -LiteralPath $caddyArchive -DestinationPath "$payload\caddy"

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
Copy-Item -LiteralPath "$repoRoot\migrations" -Destination "$payload\migrations" -Recurse

Invoke-Sign -Path "$payload\l4d2panel-control.exe"
Invoke-Sign -Path "$payload\caddy\caddy.exe"
Get-ChildItem -LiteralPath "$payload\postgresql" -Recurse -Filter *.exe | ForEach-Object { Invoke-Sign -Path $_.FullName }

$components = @(
    [ordered]@{ type='application'; name='l4d2panel-control'; version=$Version; licenses=@(@{license=@{id='AGPL-3.0-or-later'}}) },
    [ordered]@{ type='application'; name='caddy'; version=$caddyVersion; licenses=@(@{license=@{id='Apache-2.0'}}) },
    [ordered]@{ type='application'; name='postgresql'; version=([regex]::Match($archiveName, '17\.[0-9]+').Value); licenses=@(@{license=@{id='PostgreSQL'}}) }
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
PostgreSQL $([regex]::Match($archiveName, '17\.[0-9]+').Value) - PostgreSQL License - https://www.postgresql.org/

The release pipeline retains upstream LICENSE files inside each payload directory.
"@ | Set-Content -LiteralPath "$payload\THIRD-PARTY-NOTICES.txt" -Encoding utf8NoBOM

Copy-Item -LiteralPath "$PSScriptRoot\Install-L4D2PanelCenter.ps1", "$PSScriptRoot\Update-L4D2PanelCenter.ps1", "$PSScriptRoot\Remove-L4D2PanelCenterData.ps1", "$PSScriptRoot\Package.wxs", "$PSScriptRoot\L4D2Panel.Center.wixproj" -Destination $staging
& dotnet build "$staging\L4D2Panel.Center.wixproj" -c $Configuration -p:PanelVersion=$Version -p:PayloadDir=$payload
if ($LASTEXITCODE -ne 0) { throw 'WiX MSI build failed.' }
$msi = Get-ChildItem -LiteralPath "$staging\bin" -Recurse -Filter *.msi | Select-Object -First 1
if (-not $msi) { throw 'WiX completed without producing an MSI.' }
$suffix = if ($SigningCertificateThumbprint) { '' } else { '-unsigned' }
$finalMsi = "$output\l4d2panel-center-$Version-windows-x64$suffix.msi"
Copy-Item -LiteralPath $msi.FullName -Destination $finalMsi -Force
Invoke-Sign -Path $finalMsi
(Get-FileHash -LiteralPath $finalMsi -Algorithm SHA256).Hash.ToLowerInvariant() + "  " + [IO.Path]::GetFileName($finalMsi) |
    Set-Content -LiteralPath "$finalMsi.sha256" -Encoding ascii
Copy-Item -LiteralPath "$payload\sbom.cdx.json", "$payload\THIRD-PARTY-NOTICES.txt" -Destination $output -Force
Write-Output $finalMsi
