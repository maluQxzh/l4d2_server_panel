# Windows MSI 构建

此目录使用 WiX 7 生成 x64 MSI。包内只包含 Control、SQLite 迁移、安装/升级/清理与初始化密钥脚本、SBOM 和第三方声明，不包含外部数据库或 HTTPS 服务。

构建需要 Go 1.25.12、Node.js 24、PowerShell 7.4 与 .NET/WiX 7。脚本会先构建 React 生产资源并嵌入 Control，再恢复仓库中的静态回退页。

```powershell
.\packaging\windows\Build-WindowsMSI.ps1 -Version 0.1.0
```

本地迭代可加 `-Fast`；正式发布不要使用该开关。每次构建都会重建 payload 和 cabinet cache，防止旧文件进入新 MSI。

设置 `WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT` 与 `WINDOWS_SIGNING_TIMESTAMP_URL` 后，脚本会签署并验证 EXE/MSI；否则产物名带 `-unsigned`。

构建结束自动运行 `Test-WindowsMSI.ps1`，检查 x64 条件、自定义动作顺序、属性与注册表映射、嵌入 cabinet、SQLite 迁移、禁止的旧数据库文件以及体积上限。

管理员可运行真实安装冒烟：

```powershell
.\packaging\windows\Test-WindowsMSIInstall.ps1 `
  -MsiPath .\dist\windows-center\l4d2panel-center-0.1.0-windows-x64-unsigned.msi `
  -CleanupTestData
```

该测试覆盖单一 Control 服务、`0.0.0.0:19090`、Windows 防火墙、初始化密钥、SQLite/WAL/SHM ACL、无 55432 监听、`/readyz`、`/setup`、普通卸载保留数据和显式 purge。
