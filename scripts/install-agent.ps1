[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidatePattern('^https?://')][string]$CentralUrl,
    [ValidatePattern('^[A-Za-z0-9._-]*$')][string]$EnrollmentToken = '',
    [ValidatePattern('^[A-Za-z0-9._-]+$')][string]$NodeName = $env:COMPUTERNAME,
    [string]$AgentBinary,
    [ValidatePattern('^https://')][string]$AgentDownloadUrl = 'https://github.com/l4d2-panel/l4d2panel/releases/latest/download/l4d2panel-agent-windows-amd64.exe',
    [ValidatePattern('^[A-Fa-f0-9]{64}$')][string]$AgentSha256,
    [ValidateRange(10, 1024)][int]$MinimumFreeSpaceGB = 20,
    [switch]$AllowUnverifiedDownload
)

$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$serviceName = 'l4d2panel-agent'
$serviceAccount = "NT SERVICE\$serviceName"

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw '请以管理员身份运行 PowerShell。'
    }
}

function Set-RestrictedAcl {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][Security.AccessControl.FileSystemRights]$ServiceRights,
        [Parameter(Mandatory = $true)][bool]$Directory
    )
    $serviceSid = ([Security.Principal.NTAccount]::new($serviceAccount)).Translate([Security.Principal.SecurityIdentifier])
    $systemSid = [Security.Principal.SecurityIdentifier]::new('S-1-5-18')
    $adminSid = [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544')
    $inheritance = if ($Directory) {
        [Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
    } else {
        [Security.AccessControl.InheritanceFlags]::None
    }
    $propagation = [Security.AccessControl.PropagationFlags]::None
    $acl = if ($Directory) {
        [Security.AccessControl.DirectorySecurity]::new()
    } else {
        [Security.AccessControl.FileSecurity]::new()
    }
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($entry in @(
        @($systemSid, [Security.AccessControl.FileSystemRights]::FullControl),
        @($adminSid, [Security.AccessControl.FileSystemRights]::FullControl),
        @($serviceSid, $ServiceRights)
    )) {
        $rule = [Security.AccessControl.FileSystemAccessRule]::new(
            $entry[0], $entry[1], $inheritance, $propagation,
            [Security.AccessControl.AccessControlType]::Allow)
        $acl.AddAccessRule($rule) | Out-Null
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function Write-Utf8NoBom {
    param([string]$Path, [string]$Content)
    [IO.File]::WriteAllText($Path, $Content, [Text.UTF8Encoding]::new($false))
}

function Assert-FreeSpace {
    param([string]$Path, [int]$MinimumGB)
    $root = [IO.Path]::GetPathRoot([IO.Path]::GetFullPath($Path)).TrimEnd('\')
    $drive = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$root'"
    if (-not $drive -or $drive.FreeSpace -lt ($MinimumGB * 1GB)) {
        $available = if ($drive) { [Math]::Round($drive.FreeSpace / 1GB, 1) } else { 0 }
        throw "实例磁盘可用空间仅 ${available} GB，至少需要 ${MinimumGB} GB。"
    }
}

function Install-X86RuntimeIfMissing {
    $runtime = Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\WOW6432Node\Microsoft\VisualStudio\14.0\VC\Runtimes\x86' -ErrorAction SilentlyContinue
    if ($runtime -and $runtime.Installed -eq 1) { return }
    $installer = Join-Path ([IO.Path]::GetTempPath()) ("vc-redist-x86-{0}.exe" -f [Guid]::NewGuid())
    try {
        Invoke-WebRequest -Uri 'https://aka.ms/vs/17/release/vc_redist.x86.exe' -OutFile $installer -UseBasicParsing
        $signature = Get-AuthenticodeSignature -FilePath $installer
        if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'Microsoft') {
            throw 'Microsoft x86 VC++ 运行库签名验证失败。'
        }
        $process = Start-Process -FilePath $installer -ArgumentList '/install', '/quiet', '/norestart' -Wait -PassThru
        if ($process.ExitCode -notin @(0, 1638, 3010)) {
            throw "x86 VC++ 运行库安装失败，退出码 $($process.ExitCode)。"
        }
    } finally {
        Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue
    }
}

function Expand-TrustedZip {
    param([string]$Archive, [string]$Destination)
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $destinationFull = [IO.Path]::GetFullPath($Destination).TrimEnd('\') + '\'
    $zip = [IO.Compression.ZipFile]::OpenRead($Archive)
    try {
        foreach ($entry in $zip.Entries) {
            $entryPath = [IO.Path]::GetFullPath((Join-Path $Destination $entry.FullName))
            if (-not $entryPath.StartsWith($destinationFull, [StringComparison]::OrdinalIgnoreCase)) {
                throw "SteamCMD 压缩包包含越界路径：$($entry.FullName)"
            }
        }
    } finally {
        $zip.Dispose()
    }
    Expand-Archive -LiteralPath $Archive -DestinationPath $Destination -Force
}

Assert-Administrator
if (-not [Environment]::Is64BitOperatingSystem) { throw '首版仅支持 amd64 Windows。' }
$os = Get-CimInstance Win32_OperatingSystem
if ([int]$os.BuildNumber -lt 17763) { throw '需要 Windows 10/11 或 Windows Server 2019+（Build 17763+）。' }
if ($CentralUrl.StartsWith('http://', [StringComparison]::OrdinalIgnoreCase)) {
    Write-Warning '局域网 HTTP 不加密注册令牌、节点凭据、任务和结果；仅应在可信隔离网络中使用。Agent 会拒绝公网 HTTP 地址。'
}

$programRoot = Join-Path $env:ProgramFiles 'L4D2Panel'
$dataRoot = Join-Path $env:ProgramData 'L4D2Panel'
$instanceRoot = Join-Path $dataRoot 'instances'
$steamRoot = Join-Path $dataRoot 'steamcmd'
$targetBinary = Join-Path $programRoot 'l4d2panel-agent.exe'
$configPath = Join-Path $dataRoot 'agent.json'
$existingService = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
if (-not (Test-Path -LiteralPath $configPath) -and [string]::IsNullOrWhiteSpace($EnrollmentToken)) {
    throw '首次安装必须提供 -EnrollmentToken。'
}
Assert-FreeSpace -Path $dataRoot -MinimumGB $MinimumFreeSpaceGB
Install-X86RuntimeIfMissing
New-Item -ItemType Directory -Force -Path $programRoot, $dataRoot, $instanceRoot, $steamRoot | Out-Null

$steamBinary = Join-Path $steamRoot 'steamcmd.exe'
if (-not (Test-Path -LiteralPath $steamBinary)) {
    $steamArchive = Join-Path ([IO.Path]::GetTempPath()) ("steamcmd-{0}.zip" -f [Guid]::NewGuid())
    $steamStaging = Join-Path ([IO.Path]::GetTempPath()) ("steamcmd-{0}" -f [Guid]::NewGuid())
    try {
        New-Item -ItemType Directory -Path $steamStaging | Out-Null
        Invoke-WebRequest -Uri 'https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip' -OutFile $steamArchive -UseBasicParsing
        Expand-TrustedZip -Archive $steamArchive -Destination $steamStaging
        Copy-Item -Path (Join-Path $steamStaging '*') -Destination $steamRoot -Recurse -Force
        & $steamBinary +quit
        if ($LASTEXITCODE -ne 0) { throw "SteamCMD 初始化失败，退出码 $LASTEXITCODE。" }
    } finally {
        Remove-Item -LiteralPath $steamArchive -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $steamStaging -Recurse -Force -ErrorAction SilentlyContinue
    }
}

$downloadedBinary = Join-Path ([IO.Path]::GetTempPath()) ("l4d2panel-agent-{0}.exe" -f [Guid]::NewGuid())
try {
    if ($AgentBinary) {
        Copy-Item -LiteralPath (Resolve-Path -LiteralPath $AgentBinary) -Destination $downloadedBinary -Force
    } else {
        Invoke-WebRequest -Uri $AgentDownloadUrl -OutFile $downloadedBinary -UseBasicParsing
        if (-not $AgentSha256) {
            try {
                $checksumText = (Invoke-WebRequest -Uri ($AgentDownloadUrl + '.sha256') -UseBasicParsing).Content
                if ($checksumText -match '(?i)\b([a-f0-9]{64})\b') { $AgentSha256 = $Matches[1] }
            } catch {
                if (-not $AllowUnverifiedDownload) { throw '无法下载 Agent SHA-256 校验文件；如确认来源可信，可显式使用 -AllowUnverifiedDownload。' }
            }
        }
    }
    if ($AgentSha256) {
        $actual = (Get-FileHash -LiteralPath $downloadedBinary -Algorithm SHA256).Hash
        if ($actual -ne $AgentSha256) { throw "Agent SHA-256 不匹配（实际 $actual）。" }
    } elseif (-not $AgentBinary -and -not $AllowUnverifiedDownload) {
        throw '下载的 Agent 未通过 SHA-256 校验。'
    }

    if ($existingService -and $existingService.Status -ne 'Stopped') {
        Stop-Service -Name $serviceName -Force
        (Get-Service -Name $serviceName).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
    Copy-Item -LiteralPath $downloadedBinary -Destination $targetBinary -Force
} finally {
    Remove-Item -LiteralPath $downloadedBinary -Force -ErrorAction SilentlyContinue
}

$keepExistingEnrollment = $false
if (Test-Path -LiteralPath $configPath) {
    try {
        $saved = Get-Content -Raw -LiteralPath $configPath -Encoding UTF8 | ConvertFrom-Json
        $keepExistingEnrollment = -not [string]::IsNullOrWhiteSpace($saved.node_id) -and -not [string]::IsNullOrWhiteSpace($saved.credential)
        if ($keepExistingEnrollment -and $saved.central_url.TrimEnd('/') -ne $CentralUrl.TrimEnd('/')) {
            throw '现有节点凭据属于另一个 CentralUrl；请先卸载并移走旧配置后重新注册。'
        }
    } catch {
        throw "无法安全复用现有配置：$($_.Exception.Message)"
    }
}
if (-not $keepExistingEnrollment) {
    $config = [ordered]@{
        central_url = $CentralUrl.TrimEnd('/')
        enrollment_token = $EnrollmentToken
        node_name = $NodeName
        instance_roots = @($instanceRoot)
        steamcmd_path = $steamBinary
        data_dir = $dataRoot
        heartbeat_interval = '10s'
        metrics_interval = '15s'
        steamcmd_concurrency = 1
        insecure_skip_verify = $false
    }
    Write-Utf8NoBom -Path $configPath -Content ($config | ConvertTo-Json -Depth 4)
    & $targetBinary validate --config $configPath
    if ($LASTEXITCODE -ne 0) { throw 'Agent 配置校验失败。' }
    & $targetBinary enroll --config $configPath
    if ($LASTEXITCODE -ne 0) { throw '节点注册失败。' }
} else {
    & $targetBinary validate --config $configPath
    if ($LASTEXITCODE -ne 0) { throw '现有 Agent 配置校验失败。' }
}

if (-not $existingService) {
    & $targetBinary install --config $configPath
    if ($LASTEXITCODE -ne 0) { throw 'Windows 服务安装失败。' }
} else {
    $binaryPath = ('"{0}" run --config "{1}"' -f $targetBinary, $configPath)
    & sc.exe config $serviceName binPath= $binaryPath obj= $serviceAccount password= '' start= delayed-auto | Out-Null
    if ($LASTEXITCODE -ne 0) { throw '更新 Windows 服务配置失败。' }
    & sc.exe sidtype $serviceName unrestricted | Out-Null
    & sc.exe failure $serviceName reset= 86400 actions= restart/10000/restart/30000/restart/60000 | Out-Null
}

Set-RestrictedAcl -Path $programRoot -ServiceRights ([Security.AccessControl.FileSystemRights]::ReadAndExecute) -Directory $true
Set-RestrictedAcl -Path $dataRoot -ServiceRights ([Security.AccessControl.FileSystemRights]::Modify) -Directory $true
Set-RestrictedAcl -Path $configPath -ServiceRights ([Security.AccessControl.FileSystemRights]::Modify) -Directory $false
if ((Get-Service -Name $serviceName).Status -ne [ServiceProcess.ServiceControllerStatus]::Running) {
    Start-Service -Name $serviceName
}

$enabledProfiles = Get-NetFirewallProfile | Where-Object Enabled
if ($enabledProfiles) {
    Write-Warning 'Windows 防火墙已启用。节点只需出站连接 CentralUrl 对应端口；创建实例后请仅开放该实例实际使用的游戏/A2S/RCON 端口，并限制 RCON 来源。'
}
Write-Host "节点 Agent 已以低权限虚拟账号 $serviceAccount 安装并启动。"
