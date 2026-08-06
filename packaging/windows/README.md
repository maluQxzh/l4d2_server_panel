# Windows 便携中心端打包

Windows 中心端只发布 x64 ZIP，不再构建或分发 MSI/WiX 安装包。构建需要 Go 1.25.12、Node.js 24 和 PowerShell 7.4：

```powershell
.\packaging\windows\Build-WindowsPortable.ps1 -Version 0.1.0
```

开发构建可使用 `-Version dev` 或安全的开发标签（例如 `dev-4535c98`）；版本标签只允许 ASCII 字母、数字、点、下划线和连字符。

产物位于 `dist/windows-center/l4d2panel-center-<version>-windows-x64.zip`。ZIP 包含 Control、SQLite migrations、未签名文件哈希清单、SBOM、许可证和便携部署说明。构建会自动运行结构与哈希验证。

在可丢弃的提升权限 Windows VM 上运行真实服务、初始化、同版本升级、数据保留注销与显式清理冒烟：

```powershell
.\packaging\windows\Test-WindowsPortableInstall.ps1 -PackagePath .\dist\windows-center\l4d2panel-center-0.1.0-windows-x64.zip
```

可选的 Authenticode 签名仅作用于包内 EXE；设置 `WINDOWS_SIGNING_CERTIFICATE_THUMBPRINT` 与 `WINDOWS_SIGNING_TIMESTAMP_URL` 后启用。ZIP 的便携清单本身不提供发布者身份验证。
