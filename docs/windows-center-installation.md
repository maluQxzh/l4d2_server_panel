# Windows 中心端安装

Windows 10/11 x64 与 Windows Server 2019+ 可使用 WiX 7 MSI 安装中心端，不依赖 Docker、WSL2 或外部数据库。

## 目录与服务

- 程序：`%ProgramFiles%\L4D2Panel\Control`
- 配置：`%ProgramData%\L4D2Panel\config\control.json`
- SQLite：`%ProgramData%\L4D2Panel\data\l4d2panel.sqlite3`
- 日志：`%ProgramData%\L4D2Panel\logs`
- 升级检查点：`%ProgramData%\L4D2Panel\backups`
- Caddy：`%ProgramData%\L4D2Panel\caddy`

固定服务名只有 `l4d2panel-control` 与 `l4d2panel-caddy`。两者使用独立的 `NT SERVICE\<服务名>` 虚拟账号。SQLite 数据目录禁用继承，只允许 Administrators、SYSTEM 与 Control 服务 SID；Caddy 无权读取数据库或 Control 主密钥配置。

## 交互安装

1. 运行 MSI，选择安装目录。
2. 选择内置 Caddy 或同机外部反向代理，并设置域名、Control 内部端口、HTTP/HTTPS 入口端口。
3. 安装器写入严格的 schema v2 配置，先执行 `doctor`，再执行迁移并启动服务。
4. 访问 `/setup` 创建首个所有者。

Control 默认监听 `127.0.0.1:18080`。SQLite 不监听网络端口；Windows 防火墙只开放选中的 Caddy HTTP/HTTPS 入口。使用外部代理时，应从回环地址代理 HTTP 和 WebSocket，并保留 `Host` 与 `X-Forwarded-Proto`。

静默安装示例：

```powershell
msiexec /i .\l4d2panel-center-0.1.0-windows-x64.msi /qn /norestart `
  PROXY_MODE=caddy PANEL_DOMAIN=panel.example.com `
  CONTROL_PORT=18080 HTTP_PORT=8088 HTTPS_PORT=8443
```

数据库路径不可通过 MSI 属性改为共享盘。原生安装固定使用 `%ProgramData%` 本机路径；不要用目录联接或其他方式把数据库、WAL、SHM 移到 SMB/NFS。

## 诊断和备份

```powershell
& "$env:ProgramFiles\L4D2Panel\Control\l4d2panel-control.exe" doctor `
  --config "$env:ProgramData\L4D2Panel\config\control.json"

Stop-Service l4d2panel-caddy,l4d2panel-control
& "$env:ProgramFiles\L4D2Panel\Control\l4d2panel-control.exe" backup-database `
  --config "$env:ProgramData\L4D2Panel\config\control.json" `
  --output "$env:ProgramData\L4D2Panel\backups\manual.sqlite3"
```

`doctor` 输出数据库引擎、SQLite 版本、本地文件系统、目录可写性、`foreign_keys`、`journal_mode`、`quick_check`、schema 状态和 Control 监听地址，不输出主密钥。备份要求 Control 已停止并能取得独占锁；输出不能已存在，成功文件会同步后原子改名。

## 升级、卸载和恢复

`Update-L4D2PanelCenter.ps1` 会停止 Caddy/Control，调用 `backup-database` 创建一致性检查点，再启动 MSI。新 MSI 执行迁移和健康检查；失败时保留原数据库、检查点和 `UPGRADE-RECOVERY.txt`。恢复前必须停止 Control，并先验证检查点可独立打开和通过 `quick_check`。

普通卸载删除程序和两个服务，但保留整个 `%ProgramData%\L4D2Panel`。彻底清理必须在服务已删除后显式运行：

```powershell
.\Remove-L4D2PanelCenterData.ps1 `
  -Confirmation 'DELETE L4D2PANEL CENTER DATA'
```

该操作会永久删除 SQLite 数据库、WAL、SHM、备份、主密钥、日志和 Caddy 状态，无法恢复。
