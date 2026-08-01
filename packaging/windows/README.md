# Windows 原生中心端打包

此目录使用 WiX 7 生成 x64 MSI。MSI 包含 Control、Caddy 与 PostgreSQL 17 Windows ZIP 载荷；不从安装目标主机下载可执行文件。

构建时会从已校验的 PostgreSQL ZIP 中保留服务端、命令行工具、运行库和 `share` 数据，并排除面板不使用的 pgAdmin、StackBuilder、开发头文件和文档。中心端 MSI 不提供 PostgreSQL 桌面管理工具。

构建前必须阅读并满足 [WiX OSMF 条款](https://docs.firegiant.com/wix/osmf/)。工程通过 `<AcceptEula>wix7</AcceptEula>` 显式接受 WiX 7 EULA；达到适用收入阈值时，发布组织必须先完成赞助。

```powershell
pwsh ./packaging/windows/Build-WindowsMSI.ps1 `
  -PostgreSqlArchive C:\payloads\postgresql-17.x-windows-x64-binaries.zip `
  -PostgreSqlSha256 <64位十六进制SHA256> `
  -Version 0.1.0
```

频繁修改 Control、安装脚本或 WiX 后，可使用快速开发构建：

```powershell
pwsh ./packaging/windows/Build-WindowsMSI.ps1 `
  -PostgreSqlArchive C:\payloads\postgresql-17.x-windows-x64-binaries.zip `
  -PostgreSqlSha256 <64位十六进制SHA256> `
  -Version 0.1.0 `
  -Fast
```

`-Fast` 复用已校验并展开的 PostgreSQL/Caddy 载荷和 WiX cabinet 缓存，使用低压缩并跳过重复 ICE 校验；产物名带 `-dev`，只用于本地迭代。默认构建继续使用高压缩和完整 MSI 验证。依赖或缓存异常时使用 `-Clean` 重建当前签名/压缩配置的缓存。

每次构建都会运行 `Test-WindowsMSI.ps1`，直接检查最终 MSI 数据库中的安全属性、注册表参数映射、标准动作顺序、回滚动作、x64 条件、向导 Control 端口、嵌入式 cabinet 和必要载荷；快速构建也不会跳过这些结构检查。

在 Windows 发布候选机上，以管理员身份运行 `Test-WindowsMSIInstall.ps1` 可执行受管 PostgreSQL、Control、Caddy、ACL、回环监听、HTTP/HTTPS、普通卸载保留数据和显式测试数据清理的完整原生安装冒烟测试。该脚本要求安装目录和 `%ProgramData%\L4D2Panel` 在开始前均不存在，防止覆盖真实部署。

PostgreSQL ZIP 必须来自受信任的 PostgreSQL Windows 二进制发布渠道，并在发布配置中固定 SHA-256。Caddy 下载首先校验固定的官方 checksum 清单哈希，再校验载荷哈希。若设置 `WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT` 和 `WINDOWS_SIGNING_TIMESTAMP_URL`，脚本会签署并验证 EXE/DLL/MSI；否则产物名明确带 `-unsigned`。

测试环境可以使用 `/qn`，但不得将数据库密码写入 MSI 属性：

```powershell
msiexec /i .\l4d2panel-center.msi /qn `
  DATABASE_MODE=external PROXY_MODE=external `
  PANEL_DOMAIN=panel.example.com CONTROL_PORT=18080 `
  HTTP_PORT=9080 HTTPS_PORT=9443 `
  RESPONSE_FILE=C:\Secure\database.json
```

响应文件必须禁用 ACL 继承，并且只允许当前管理员、Administrators 与 SYSTEM 读取。普通卸载会保留 `%ProgramData%\L4D2Panel`；彻底删除必须单独运行 `Remove-L4D2PanelCenterData.ps1` 并提供确认短语。
