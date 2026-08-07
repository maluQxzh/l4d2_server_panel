#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Version,
    [string]$WslDistribution = 'l4d2',
    [string]$GoProxy = $env:GOPROXY,
    [string]$AlpineMirror = $env:ALPINE_MIRROR
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$') {
    throw "Version must be a semantic release version: $Version"
}

$repoRoot = (Resolve-Path "$PSScriptRoot\..").Path
$nodeMajor = [int](& node -p "process.versions.node.split('.')[0]")
if ($LASTEXITCODE -ne 0 -or $nodeMajor -lt 24) { throw 'Node.js 24 or later is required.' }
$goVersion = (& go version).Trim()
$goVersionMatch = [regex]::Match($goVersion, '^go version go1\.25\.(\d+) ')
if ($LASTEXITCODE -ne 0 -or -not $goVersionMatch.Success -or [int]$goVersionMatch.Groups[1].Value -lt 12) {
    throw 'Go 1.25.12 or a later 1.25 security patch is required.'
}
$commit = (& git -C $repoRoot rev-parse --short HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commit) { throw 'Unable to resolve the Git commit.' }
$buildDate = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

Push-Location "$repoRoot\web"
try {
    & npm ci --prefer-offline --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed.' }
    & npm run build
    if ($LASTEXITCODE -ne 0) { throw 'Web production build failed.' }
} finally {
    Pop-Location
}

& "$PSScriptRoot\windows\Build-WindowsPortable.ps1" -Version $Version -SkipWebBuild
if ($LASTEXITCODE -ne 0) { throw 'Windows center build failed.' }

$agentOutput = "$repoRoot\dist\agents"
New-Item -ItemType Directory -Force -Path $agentOutput | Out-Null
$ldflags = "-s -w -X github.com/l4d2-panel/l4d2panel/internal/version.Version=$Version -X github.com/l4d2-panel/l4d2panel/internal/version.Commit=$commit -X github.com/l4d2-panel/l4d2panel/internal/version.Date=$buildDate"
$savedCgo = $env:CGO_ENABLED
$savedGoos = $env:GOOS
$savedGoarch = $env:GOARCH
try {
    $env:CGO_ENABLED = '0'
    $env:GOARCH = 'amd64'
    $env:GOOS = 'linux'
    & go -C $repoRoot build -trimpath -ldflags $ldflags -o "$agentOutput\l4d2panel-agent-linux-amd64" ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw 'Linux Agent build failed.' }
    $env:GOOS = 'windows'
    & go -C $repoRoot build -trimpath -ldflags $ldflags -o "$agentOutput\l4d2panel-agent-windows-amd64.exe" ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw 'Windows Agent build failed.' }
} finally {
    $env:CGO_ENABLED = $savedCgo
    $env:GOOS = $savedGoos
    $env:GOARCH = $savedGoarch
}

Copy-Item -LiteralPath "$repoRoot\configs\agent.portable.example.yaml" -Destination "$agentOutput\agent.yaml.example" -Force
$utf8NoBom = [Text.UTF8Encoding]::new($false)
foreach ($name in @('l4d2panel-agent-linux-amd64', 'l4d2panel-agent-windows-amd64.exe')) {
    $hash = (Get-FileHash -LiteralPath "$agentOutput\$name" -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$agentOutput\$name.sha256", "$hash  $name`n", $utf8NoBom)
}

$windowsRepoArgument = $repoRoot.Replace('\', '/')
$wslRepoRoot = (& wsl.exe -d $WslDistribution -- wslpath -a $windowsRepoArgument).Trim()
if ($LASTEXITCODE -ne 0 -or -not $wslRepoRoot) { throw "Unable to resolve the repository path in WSL distribution $WslDistribution." }
$containerScript = "$wslRepoRoot/packaging/container/build-container-offline.sh"
$environmentArguments = @(
    "VERSION=$Version",
    "COMMIT=$commit",
    "BUILD_DATE=$buildDate",
    "OUTPUT_DIR=$wslRepoRoot/dist/container",
    "GOPROXY=$GoProxy",
    "ALPINE_MIRROR=$AlpineMirror"
)
& wsl.exe -d $WslDistribution -- env @environmentArguments bash $containerScript
if ($LASTEXITCODE -ne 0) { throw 'Container center build failed.' }

Write-Output "Release artifacts $Version ($commit, $buildDate) were written beneath $repoRoot\dist."
