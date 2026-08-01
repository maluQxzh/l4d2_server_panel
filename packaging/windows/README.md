# Windows 原生中心端打包

此目录使用 WiX 7 生成 x64 MSI。MSI 包含 Control、Caddy 与 PostgreSQL 17 Windows ZIP 载荷；不从安装目标主机下载可执行文件。

构建前必须阅读并满足 [WiX OSMF 条款](https://docs.firegiant.com/wix/osmf/)。工程通过 `<AcceptEula>wix7</AcceptEula>` 显式接受 WiX 7 EULA；达到适用收入阈值时，发布组织必须先完成赞助。

```powershell
pwsh ./packaging/windows/Build-WindowsMSI.ps1 `
  -PostgreSqlArchive C:\payloads\postgresql-17.x-windows-x64-binaries.zip `
  -PostgreSqlSha256 <64位十六进制SHA256> `
  -Version 0.1.0
```

PostgreSQL ZIP 必须来自受信任的 PostgreSQL Windows 二进制发布渠道，并在发布配置中固定 SHA-256。Caddy 下载首先校验固定的官方 checksum 清单哈希，再校验载荷哈希。若设置 `WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT` 和 `WINDOWS_SIGNING_TIMESTAMP_URL`，脚本会签署并验证 EXE/DLL/MSI；否则产物名明确带 `-unsigned`。

测试环境可以使用 `/qn`，但不得将数据库密码写入 MSI 属性：

```powershell
msiexec /i .\l4d2panel-center.msi /qn `
  DATABASE_MODE=external PROXY_MODE=external `
  PANEL_DOMAIN=panel.example.com HTTP_PORT=9080 HTTPS_PORT=9443 `
  RESPONSE_FILE=C:\Secure\database.json
```

响应文件必须禁用 ACL 继承，并且只允许当前管理员、Administrators 与 SYSTEM 读取。普通卸载会保留 `%ProgramData%\L4D2Panel`；彻底删除必须单独运行 `Remove-L4D2PanelCenterData.ps1` 并提供确认短语。
