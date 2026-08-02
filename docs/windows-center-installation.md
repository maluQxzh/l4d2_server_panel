# Windows 便携中心端

Windows 10/11 x64 与 Windows Server 2019+ 使用发布页中的 `l4d2panel-center-<version>-windows-x64.zip`，不依赖 Docker、WSL2、外部数据库或内置 HTTPS 服务。

## 解压、安装与初始化

把整个 ZIP 解压到本机固定磁盘上的永久目录，例如 `D:\L4D2PanelCenter`。不要从网络共享、可移动磁盘或临时目录运行，也不要在注册服务后移动目录。

双击 `l4d2panel-control.exe` 并允许 UAC。程序会创建旁置目录：

- `data/config/control.json`
- `data/database/l4d2panel.sqlite3`
- `data/logs`
- `data/backups/upgrades`
- `data/update`

程序自动执行 doctor 和 SQLite 迁移，注册 `l4d2panel-control` 虚拟账号服务，为所有网络配置文件开放 TCP 19090，并显示一次初始化密钥。密钥同时是 `admin` 的初始密码，不会写入配置或日志。

Control 只提供 HTTP。管理员必须先配置自己的 HTTPS 反向代理，再访问 `https://你的域名/setup`；代理应保留 Host、`X-Forwarded-*`、Upgrade 和 Connection，并把精确代理 IP/CIDR 写入 `trusted_proxies`。请把 Windows 防火墙规则的来源范围收紧到可信代理，不能把明文 19090 暴露到公网。

## 内置管理命令

在便携目录的管理员 PowerShell 中运行：

```powershell
.\l4d2panel-control.exe portable status
.\l4d2panel-control.exe portable start
.\l4d2panel-control.exe portable stop
.\l4d2panel-control.exe portable restart
.\l4d2panel-control.exe doctor --config .\data\config\control.json
```

删除服务和 TCP 19090 防火墙规则但保留数据：

```powershell
.\l4d2panel-control.exe portable unregister
```

永久删除旁置 `data` 前必须先离线备份，并使用精确确认短语：

```powershell
.\l4d2panel-control.exe portable purge --confirmation "DELETE L4D2PANEL PORTABLE DATA"
```

## 一键升级和恢复

从可信发布渠道下载新版 ZIP，以全局所有者打开“设置 → 中心端升级”，选择 ZIP 并确认。浏览器会显示上传进度；中心端随后短暂离线并自动完成结构/哈希校验、停服、快照、文件替换、迁移、启动和 `/readyz` 健康检查。允许升级和同版本重装，禁止降级。

升级清单不带发布者签名，只能检查包布局、大小和文件 SHA-256，不能证明发布者身份。不要上传第三方修改的包。

失败时独立升级工作进程自动恢复旧程序、migrations、配置与 SQLite，并启动旧服务。`data/backups/upgrades` 保留最近 3 份完整快照；状态与脱敏错误保存在 `data/update`。若自动回滚也失败，服务保持停止，不要删除对应快照或上传包；在旁置备份恢复程序和数据库后运行：

```powershell
.\l4d2panel-control.exe doctor --config .\data\config\control.json
.\l4d2panel-control.exe portable start
```

Web 不可用时可从旧版便携目录执行同一升级引擎：

```powershell
.\l4d2panel-control.exe portable upgrade D:\Downloads\l4d2panel-center-<version>-windows-x64.zip
```

## 旧 MSI 边界

便携版是全新部署，不读取 `%ProgramData%\L4D2Panel`、不导入旧数据库，也不卸载旧产品。若检测到同名旧服务，初始化会拒绝覆盖并要求管理员先通过原 MSI 卸载。确认旧部署已备份并完全停止后再使用新的便携目录。
