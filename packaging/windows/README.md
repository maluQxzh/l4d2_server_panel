# Windows MSI 构建

此目录使用 WiX 7 生成 x64 MSI。包内只包含 Control、Caddy、SQLite 迁移、安装/升级/清理脚本、SBOM 与第三方声明，不包含 PostgreSQL 或其他数据库服务。

```powershell
.\packaging\windows\Build-WindowsMSI.ps1 -Version 0.1.0
```

本地迭代可加 `-Fast`；正式发布不要使用该开关。每次构建都会删除 staging 中残留的旧数据库 payload，并重建当前 cabinet cache，防止旧文件进入新 MSI。Caddy 下载会校验固定 checksum 清单和载荷哈希。

设置 `WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT` 与 `WINDOWS_SIGNING_TIMESTAMP_URL` 后，脚本会签署并验证 EXE/MSI；否则产物名带 `-unsigned`。

构建结束自动运行 `Test-WindowsMSI.ps1`，检查 x64 条件、自定义动作顺序、属性与注册表映射、嵌入 cabinet、SQLite 迁移、禁止的 PostgreSQL 文件以及 35 MiB 体积上限。

管理员可运行真实安装冒烟：

```powershell
.\packaging\windows\Test-WindowsMSIInstall.ps1 `
  -MsiPath .\dist\windows-center\l4d2panel-center-0.1.0-windows-x64-unsigned.msi `
  -CleanupTestData
```

该测试覆盖 Control/Caddy 两个服务、SQLite/WAL/SHM ACL、无 55432 监听、`/readyz`、`/setup`、非默认端口、普通卸载保留数据和显式 purge。
