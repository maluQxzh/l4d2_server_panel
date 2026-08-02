# Windows 中心端安装

Windows 10/11 x64 与 Windows Server 2019+ 可使用 WiX 7 MSI 安装中心端，不依赖 Docker、WSL2、外部数据库或内置 HTTPS 服务。

## 目录与服务

- 程序：`%ProgramFiles%\L4D2Panel\Control`
- 配置：`%ProgramData%\L4D2Panel\config\control.json`
- SQLite：`%ProgramData%\L4D2Panel\data\l4d2panel.sqlite3`
- 日志：`%ProgramData%\L4D2Panel\logs`
- 升级检查点：`%ProgramData%\L4D2Panel\backups`

固定服务名只有 `l4d2panel-control`，使用 `NT SERVICE\l4d2panel-control` 虚拟账号。SQLite 数据目录禁用继承，只允许 Administrators、SYSTEM 与 Control 服务 SID。

## 安装和初始化

1. 运行 MSI 并选择安装目录。
2. 安装器写入严格的 schema v2 配置，执行 `doctor`、SQLite 迁移并启动服务。
3. Control 固定监听 `0.0.0.0:19090`；安装器为所有网络配置文件、任意来源创建 TCP 19090 入站规则。
4. 完成页选择“显示初始化密钥”，或以管理员身份运行：

```powershell
& "$env:ProgramFiles\L4D2Panel\Control\New-L4D2PanelSetupKey.ps1"
```

脚本只在当前窗口显示一次 `l4d2p_setup_...` 密钥。初始化前再次运行会轮换密钥并立即使旧值失效；已有用户后拒绝签发。

静默安装不显示完成页：

```powershell
msiexec /i .\l4d2panel-center-0.1.0-windows-x64.msi /qn /norestart
& "$env:ProgramFiles\L4D2Panel\Control\New-L4D2PanelSetupKey.ps1"
```

数据库路径不可通过 MSI 属性改为共享盘。原生安装固定使用 `%ProgramData%` 本机路径；不要用目录联接把数据库、WAL 或 SHM 移到 SMB/NFS。

## 配置 HTTPS

Control 不直接处理证书或 TLS。管理员必须在访问 `/setup` 前配置 HTTPS 反向代理，将 HTTP 和 WebSocket 转发到 `http://127.0.0.1:19090` 或中心端受保护的内网地址，并保留 `Host`、`X-Forwarded-Proto`、`X-Forwarded-For`、`Upgrade` 和 `Connection`。

代理位于其他主机时，应把它的精确 IP/CIDR 写入 `control.json` 的 `trusted_proxies`，重启 Control，并把 Windows 防火墙规则的远程地址范围收紧到该代理。不要把 19090 明文管理入口直接暴露到公网。HTTPS 代理还负责受信任证书、HTTP 跳转和 HSTS。

配置完成后访问 `https://你的域名/setup`，输入初始化密钥。系统会创建 `admin`／`系统管理员` 根所有者并登录；该密钥同时是初始密码，应在设置页立即修改。

## 诊断和备份

```powershell
& "$env:ProgramFiles\L4D2Panel\Control\l4d2panel-control.exe" doctor `
  --config "$env:ProgramData\L4D2Panel\config\control.json"

Stop-Service l4d2panel-control
& "$env:ProgramFiles\L4D2Panel\Control\l4d2panel-control.exe" backup-database `
  --config "$env:ProgramData\L4D2Panel\config\control.json" `
  --output "$env:ProgramData\L4D2Panel\backups\manual.sqlite3"
```

`doctor` 输出数据库引擎、SQLite 版本、本地文件系统、目录可写性、`foreign_keys`、`journal_mode`、`quick_check`、schema 状态和 Control 监听地址，不输出主密钥。备份要求 Control 已停止并能取得独占锁；输出不能已存在。

## 升级、卸载和恢复

`Update-L4D2PanelCenter.ps1` 会停止 Control，调用 `backup-database` 创建一致性检查点，再启动 MSI。新 MSI 执行迁移和健康检查；失败时保留原数据库、检查点和 `UPGRADE-RECOVERY.txt`。恢复前必须停止 Control，并先验证检查点可独立打开和通过 `quick_check`。

安装、升级和卸载脚本只管理当前 Control 服务、SQLite、ACL 与 TCP 19090 防火墙规则，不会自动迁移或清理旧版捆绑代理的服务或状态目录；确认不再使用后由管理员自行处理。

普通卸载删除程序、服务和产品防火墙规则，但保留整个 `%ProgramData%\L4D2Panel`。彻底清理必须在服务已删除后显式运行：

```powershell
.\Remove-L4D2PanelCenterData.ps1 `
  -Confirmation 'DELETE L4D2PANEL CENTER DATA'
```

该操作会永久删除 SQLite 数据库、WAL、SHM、备份、主密钥和日志，无法恢复。
