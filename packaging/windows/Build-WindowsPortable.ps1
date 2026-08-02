#Requires -Version 7.4
[CmdletBinding()]
param(
    [string]$Version = '0.1.0',
    [string]$OutputDirectory = '',
    [string]$SigningCertificateThumbprint = $env:WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT,
    [string]$TimestampUrl = $env:WINDOWS_SIGNING_TIMESTAMP_URL,
    [switch]$SkipWebBuild
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$') { throw "Version must be a semantic release version: $Version" }

$repoRoot = (Resolve-Path "$PSScriptRoot\..\..").Path
if (-not $OutputDirectory) { $OutputDirectory = "$repoRoot\dist\windows-center" }
$output = [IO.Path]::GetFullPath($OutputDirectory)
$staging = Join-Path $output 'staging-portable'
$payload = Join-Path $staging 'payload'
if (Test-Path -LiteralPath $staging) { Remove-Item -LiteralPath $staging -Recurse -Force }
New-Item -ItemType Directory -Force -Path $payload, $output | Out-Null

function Invoke-Sign {
    param([Parameter(Mandatory)][string]$Path)
    if (-not $SigningCertificateThumbprint) { return }
    if (-not $TimestampUrl) { throw 'WINDOWS_SIGNING_TIMESTAMP_URL is required when signing is enabled.' }
    $signtool = (Get-Command signtool.exe -ErrorAction Stop).Source
    & $signtool sign /sha1 ($SigningCertificateThumbprint -replace ' ', '') /fd SHA256 /tr $TimestampUrl /td SHA256 $Path
    if ($LASTEXITCODE -ne 0) { throw "Authenticode signing failed: $Path" }
    & $signtool verify /pa /all $Path
    if ($LASTEXITCODE -ne 0) { throw "Authenticode verification failed: $Path" }
}

if (-not $SkipWebBuild) {
    $nodeMajor = [int](& node -p "process.versions.node.split('.')[0]")
    if ($LASTEXITCODE -ne 0 -or $nodeMajor -lt 24) { throw 'Node.js 24 or later is required.' }
    Push-Location "$repoRoot\web"
    try {
        & npm ci
        if ($LASTEXITCODE -ne 0) { throw 'npm ci failed.' }
        & npm run build
        if ($LASTEXITCODE -ne 0) { throw 'Web production build failed.' }
    } finally { Pop-Location }
}

$sourceStatic = "$repoRoot\internal\control\static"
$staticBackup = "$staging\static-source-backup"
Copy-Item -LiteralPath $sourceStatic -Destination $staticBackup -Recurse
try {
    Remove-Item -LiteralPath $sourceStatic -Recurse -Force
    New-Item -ItemType Directory -Force -Path $sourceStatic | Out-Null
    Copy-Item -Path "$repoRoot\web\dist\*" -Destination $sourceStatic -Recurse -Force
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    Push-Location $repoRoot
    try {
        & go build -trimpath -ldflags "-s -w -X github.com/l4d2-panel/l4d2panel/internal/version.Version=$Version" -o "$payload\l4d2panel-control.exe" ./cmd/control
        if ($LASTEXITCODE -ne 0) { throw 'Control x64 build failed.' }
    } finally { Pop-Location }
} finally {
    if (Test-Path -LiteralPath $sourceStatic) { Remove-Item -LiteralPath $sourceStatic -Recurse -Force }
    Move-Item -LiteralPath $staticBackup -Destination $sourceStatic
}
Invoke-Sign "$payload\l4d2panel-control.exe"

New-Item -ItemType Directory -Force -Path "$payload\migrations\sqlite", "$payload\licenses" | Out-Null
Copy-Item -Path "$repoRoot\migrations\sqlite\*.sql" -Destination "$payload\migrations\sqlite" -Force
$goModuleCache = (& go env GOMODCACHE).Trim()
if (-not $goModuleCache) { throw 'Unable to locate the Go module cache.' }
Copy-Item -LiteralPath "$goModuleCache\modernc.org\sqlite@v1.55.0\LICENSE" -Destination "$payload\licenses\modernc-sqlite-LICENSE.txt"
Copy-Item -LiteralPath "$goModuleCache\modernc.org\libc@v1.74.1\LICENSE" -Destination "$payload\licenses\modernc-libc-LICENSE.txt"
Copy-Item -LiteralPath "$goModuleCache\modernc.org\libc@v1.74.1\LICENSE-3RD-PARTY.md" -Destination "$payload\licenses\modernc-libc-LICENSE-3RD-PARTY.md"

$components = @(
    [ordered]@{ type='application'; name='l4d2panel-control'; version=$Version; licenses=@(@{license=@{id='AGPL-3.0-or-later'}}) },
    [ordered]@{ type='library'; name='modernc.org/sqlite'; version='1.55.0'; licenses=@(@{license=@{id='BSD-3-Clause'}}) },
    [ordered]@{ type='library'; name='modernc.org/libc'; version='1.74.1'; licenses=@(@{license=@{id='BSD-3-Clause'}}) }
)
[ordered]@{
    bomFormat='CycloneDX'; specVersion='1.5'; serialNumber="urn:uuid:$([guid]::NewGuid())"; version=1
    metadata=@{timestamp=(Get-Date).ToUniversalTime().ToString('o'); component=@{type='application'; name='l4d2panel-center'; version=$Version}}
    components=$components
} | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath "$payload\sbom.cdx.json" -Encoding utf8NoBOM

@'
L4D2 Panel Center third-party payload

modernc.org/sqlite v1.55.0 - BSD-3-Clause - https://modernc.org/sqlite
modernc.org/libc v1.74.1 - BSD-3-Clause - https://modernc.org/libc

Upstream license files are included in the licenses directory.
'@ | Set-Content -LiteralPath "$payload\THIRD-PARTY-NOTICES.txt" -Encoding utf8NoBOM

@'
L4D2 Panel Windows x64 portable center

1. Extract the entire ZIP to a permanent directory on a local fixed drive.
2. Double-click l4d2panel-control.exe and approve the UAC prompt.
3. Record the one-time setup key, configure your own HTTPS reverse proxy, then open /setup.

Do not move the directory after the Windows service is registered. Run
"l4d2panel-control.exe portable unregister" before moving or removing it.
Use the Settings page to upload a newer official portable ZIP for one-click upgrade.
'@ | Set-Content -LiteralPath "$payload\PORTABLE-README.txt" -Encoding utf8NoBOM

$manifestFiles = @()
Get-ChildItem -LiteralPath $payload -Recurse -File | Sort-Object FullName | ForEach-Object {
    $relative = [IO.Path]::GetRelativePath($payload, $_.FullName).Replace('\', '/')
    $manifestFiles += [ordered]@{ path=$relative; size=$_.Length; sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant() }
}
[ordered]@{ format_version=1; product='l4d2panel-center'; platform='windows'; architecture='amd64'; version=$Version; files=$manifestFiles } |
    ConvertTo-Json -Depth 5 | Set-Content -LiteralPath "$payload\portable-manifest.json" -Encoding utf8NoBOM

$zipPath = Join-Path $output "l4d2panel-center-$Version-windows-x64.zip"
if (Test-Path -LiteralPath $zipPath) { Remove-Item -LiteralPath $zipPath -Force }
Add-Type -AssemblyName System.IO.Compression.FileSystem
[IO.Compression.ZipFile]::CreateFromDirectory($payload, $zipPath, [IO.Compression.CompressionLevel]::Optimal, $false)
& "$PSScriptRoot\Test-WindowsPortablePackage.ps1" -PackagePath $zipPath
if ($LASTEXITCODE -ne 0) { throw 'Portable package structural validation failed.' }
(Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant() + "  " + [IO.Path]::GetFileName($zipPath) |
    Set-Content -LiteralPath "$zipPath.sha256" -Encoding ascii
Write-Output $zipPath
