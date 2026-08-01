# Windows 原生中心端安装

## 支持范围与目录

原生 MSI 支持 Windows 10/11 x64，以及 Windows Server 2019、2022、2025 x64；不支持 ARM64。它不依赖 Docker、WSL2 或 Windows 容器。安装必须从管理员会话运行。

- 程序：`%ProgramFiles%\L4D2Panel\Control`
- 严格 JSON 配置：`%ProgramData%\L4D2Panel\config\control.json`
- PostgreSQL：`%ProgramData%\L4D2Panel\postgres`
- Caddy 数据和证书：`%ProgramData%\L4D2Panel\caddy`
- 日志和升级检查点：`%ProgramData%\L4D2Panel\logs`、`%ProgramData%\L4D2Panel\backups`

三个固定服务名为 `l4d2panel-postgres`、`l4d2panel-control`、`l4d2panel-caddy`。每个服务使用独立的 `NT SERVICE\<服务名>` 虚拟账号。配置目录只允许 Administrators、SYSTEM 和 Control 服务 SID 读取；PostgreSQL、Caddy、日志目录分别只授予需要写入的服务 SID。

## 交互安装

1. 校验发布页提供的 MSI SHA-256；正式签名产物还应在文件属性中验证 Authenticode 签名和时间戳。文件名带 `-unsigned` 的包只用于开发测试。
2. 运行 MSI，选择数据库模式。推荐“内置 PostgreSQL 17”；外部数据库必须是 PostgreSQL 17，远程地址必须启用 `sslmode=require`、`verify-ca` 或 `verify-full`。
3. 选择内置 Caddy 或同机外部反向代理，填写域名/`localhost` 和入口端口。端口可以不是 80/443；公网 ACME 通常仍要求 CA 能访问标准验证端口，无法满足时应由现有网关终止 TLS。
4. 安装器检查系统架构、端口、数据库权限与版本，执行迁移，再检查 Control `/readyz` 和 Caddy 配置。ACME 因 DNS 或公网入口未就绪时不会删除已初始化的数据库，诊断信息保留在安装日志和 Caddy 日志中。
5. 打开安装结果显示的 `https://域名[:端口]/setup`，在可信网络中创建第一个所有者。只有第一个成功的初始化请求能创建所有者。

受管 PostgreSQL 固定监听 `127.0.0.1:55432` 并使用 SCRAM-SHA-256；Control 固定监听 `127.0.0.1:8080`。不要为这两个端口创建入站防火墙规则。使用外部代理时，应代理 HTTP 和 WebSocket 到 `127.0.0.1:8080`，保留 `Host`、`X-Forwarded-Proto`，并只从回环地址连接。

## 静默安装

受管数据库和内置 Caddy：

```powershell
msiexec /i .\l4d2panel-center-0.1.0-windows-x64.msi /qn /norestart `
  DATABASE_MODE=managed PROXY_MODE=caddy `
  PANEL_DOMAIN=panel.example.com HTTP_PORT=9080 HTTPS_PORT=9443
```

外部数据库只能通过响应文件提供完整 URL：

```powershell
Copy-Item .\packaging\windows\external-database.response.example.json C:\Secure\database.json
icacls C:\Secure\database.json /inheritance:r
icacls C:\Secure\database.json /grant:r "*S-1-5-32-544:F" "*S-1-5-18:F"
msiexec /i .\l4d2panel-center-0.1.0-windows-x64.msi /qn /norestart `
  DATABASE_MODE=external PROXY_MODE=external PANEL_DOMAIN=panel.example.com `
  HTTP_PORT=9080 HTTPS_PORT=9443 RESPONSE_FILE=C:\Secure\database.json
```

响应文件格式固定为 `schema_version: 1` 和 `database_url` 两个字段；未知字段、继承 ACL 或额外可读身份都会被拒绝。数据库密码、主密钥不得写入 MSI 属性、服务命令行或安装日志。安装器自动生成的数据库密码和 32 字节主密钥只写入受限 Control 配置。

## Control 诊断和服务命令

```powershell
$control = "$env:ProgramFiles\L4D2Panel\Control\l4d2panel-control.exe"
$config = "$env:ProgramData\L4D2Panel\config\control.json"
& $control doctor --config $config
& $control migrate --config $config
& $control service status --config $config
& $control service restart --config $config
```

`doctor` 只输出不含连接凭据和主密钥的 JSON，检查 PostgreSQL 17 和迁移建表权限。`serve`、`migrate`、`doctor`、`bootstrap-owner` 均接受 `--config`；加载顺序固定为内置默认值、JSON、环境变量覆盖。服务日志按 20 MiB、5 个备份有界轮转到 ProgramData，致命服务错误同时进入 Windows Event Log。

## 升级与恢复

```powershell
& "$env:ProgramFiles\L4D2Panel\Control\Update-L4D2PanelCenter.ps1" `
  -MsiPath C:\Updates\l4d2panel-center-new.msi `
  -Sha256 '<发布页SHA-256>' `
  -SignerThumbprint '<可选的发布证书指纹>'
```

升级器检查磁盘、服务和 PostgreSQL 主版本，使用 `pg_dump` 创建自包含数据库备份，复制配置、Caddy 数据和二进制哈希，然后停止 Caddy/Control、安装 MSI、迁移并完成健康检查。只自动接受 PostgreSQL 17.x 小版本；跨主版本会拒绝。迁移一旦提交，失败流程不会猜测性启动旧 Control，服务保持停止并显示检查点与显式 `pg_restore` 恢复命令。

普通“应用和功能”卸载会删除程序、防火墙规则和三个服务，但保留整个 `%ProgramData%\L4D2Panel`。确实需要不可恢复地清除数据时，先完成普通卸载，再以管理员身份运行：

```powershell
& .\Remove-L4D2PanelCenterData.ps1 -Confirmation 'DELETE L4D2PANEL CENTER DATA'
```

清理脚本只接受默认 ProgramData 精确路径，并在任何服务仍存在时拒绝执行。
