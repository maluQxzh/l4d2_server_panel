#Requires -Version 7.4
[CmdletBinding()]
param([Parameter(Mandatory)][string]$PackagePath)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem
$resolved = (Resolve-Path -LiteralPath $PackagePath).Path
$archive = [IO.Compression.ZipFile]::OpenRead($resolved)
try {
    $entries = @($archive.Entries | Where-Object { $_.Name })
    $names = @($entries | ForEach-Object FullName)
    foreach ($required in @('l4d2panel-control.exe', 'portable-manifest.json', 'PORTABLE-README.txt', 'sbom.cdx.json', 'THIRD-PARTY-NOTICES.txt')) {
        if ($required -notin $names) { throw "Portable package is missing $required." }
    }
    if (-not ($names | Where-Object { $_ -like 'migrations/sqlite/*.sql' })) { throw 'SQLite migrations are missing.' }
    if ($names | Where-Object { $_ -match '(?i)(caddy|\.msi$|\.wix|\.ps1$|\.cmd$|\.bat$)' }) { throw 'Portable package contains a forbidden installer or script payload.' }
    $manifestEntry = $archive.GetEntry('portable-manifest.json')
    $reader = [IO.StreamReader]::new($manifestEntry.Open(), [Text.Encoding]::UTF8)
    try { $manifest = $reader.ReadToEnd() | ConvertFrom-Json } finally { $reader.Dispose() }
    if ($manifest.format_version -ne 1 -or $manifest.product -ne 'l4d2panel-center' -or $manifest.platform -ne 'windows' -or $manifest.architecture -ne 'amd64') { throw 'Portable manifest identity is invalid.' }
    if ([string]$manifest.version -notmatch '^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$') { throw 'Portable manifest version is invalid.' }
    $listed = @{}
    foreach ($file in $manifest.files) {
        if ($listed.ContainsKey([string]$file.path)) { throw "Duplicate manifest path: $($file.path)" }
        $listed[[string]$file.path] = $true
        $entry = $archive.GetEntry([string]$file.path)
        if ($null -eq $entry -or $entry.Length -ne [long]$file.size) { throw "Manifest size mismatch: $($file.path)" }
        $stream = $entry.Open()
        try {
            $hasher = [Security.Cryptography.SHA256]::Create()
            try { $actual = [Convert]::ToHexString($hasher.ComputeHash($stream)).ToLowerInvariant() } finally { $hasher.Dispose() }
        } finally { $stream.Dispose() }
        if ($actual -ne [string]$file.sha256) { throw "Manifest hash mismatch: $($file.path)" }
    }
    foreach ($entry in $entries) {
        if ($entry.FullName -ne 'portable-manifest.json' -and -not $listed.ContainsKey($entry.FullName)) { throw "Unlisted package file: $($entry.FullName)" }
    }
} finally { $archive.Dispose() }
Write-Output "Portable package validation passed: $resolved"
