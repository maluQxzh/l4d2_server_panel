# L4D2 Panel

面向《求生之路 2》专用服务器的自托管多节点管理面板。中心控制台可通过 Linux/macOS Docker 或 Windows x64 便携 ZIP 部署，并通过节点主动建立的 WSS 连接管理 Windows 与 Linux Agent；节点不需要开放面板管理端口，游戏端口仍由管理员按实例开放。

## 当前实现

- 节点与任务：带预设显示名称的一次性注册令牌、可轮换的节点凭据、删除节点时的中心端关联数据清理、断线离线队列、任务幂等、任务取消，以及 `queued/dispatched/running/succeeded/failed/canceled` 状态机。
- 运行环境：每个节点维护一套活动 SteamCMD 和多个独占游戏安装。中心端可在线校验自定义 SteamCMD 绝对路径，或让 Agent 在自身数据目录安装托管版本；游戏安装、绝对路径导入、校验修复、更新和删除统一从“运行环境”手动触发，不扫描磁盘。未绑定安装可只删除登记并保留文件，或经明确确认永久删除整个登记目录。
- 地图管理：从 QQ 闪传以默认 8 路安全分段下载 ZIP/7Z/RAR/VPK，节点用纯 Go 合并、清理并校验为共享 VPK 对象；同卷实例通过受注册表证明的硬链接复用，不为每个实例复制一份。
- 实例：从所选运行节点绑定一套已就绪且未被占用的绝对路径游戏安装；创建时不重复校验安装结构，随后负责端口、配置、既有进程受控接管、启停/重启、RCON 优雅退出和崩溃自动恢复。删除实例默认解除绑定并保留游戏文件。
- 游戏管理：A2S 状态、RCON 玩家/踢出/封禁/地图/广播、结构化配置、受限文件读写与本机上传、模组安装入口的节点直连 QQ闪传批量导入（固定写入 `left4dead2/addons` 并允许 VPK）、一键扫描并纳管该目录中已有的直属 VPK、按名称搜索及按添加时间/名称排序、上传或 HTTPS/Workshop 安装 VPK/SourceMod/Metamod、非 Workshop 单文件 VPK/SMX 最近 3 个历史版本回退、整套 SourceMod/Metamod 安全卸载、备份与计划任务。SourceMod 安装前必须已有成功纳管且磁盘加载文件完整的 Metamod:Source；安装和更新永不覆盖已有的 `left4dead2/addons/sourcemod/configs/admins_simple.ini`，卸载也保留该文件。实例未停止时不能停用已启用的 VPK/SMX，也不能卸载整套服务端组件。普通文件 API 不允许改写可执行或加载关键文件，这些内容必须经包管理流程安装或扫描纳管。
- 管理与安全：所有者/管理员/操作员/观察者作为权限预设，每条全局、节点或实例绑定都可逐项勾选具体权限；团队账号与绑定只允许全局所有者的交互式会话管理，新账号使用仅显示一次且限时有效的系统生成密码完成首次激活和强制改密；HttpOnly 会话、CSRF、Argon2id、TOTP 与一次性恢复码、有限期且可限定资源的 API Token、不可编辑审计记录。
- 可观测性：10 秒心跳、15 秒指标、A2S 探测、站内通知、SMTP、带 HMAC-SHA256 签名和出站 SSRF 防护的 Webhook、公开状态页，以及节点本地日志的有限检索/分块下载。
- 游戏版本检测和维护全部由管理员在运行环境中手动触发；Control 不再周期探测更新，也不创建自动游戏维护批次。

这不是操作系统远程终端。所有 Agent 操作都是固定任务类型；启动/重启不能覆盖已登记的可执行文件、参数或环境，游戏进程只继承受限环境变量。RCON 是受权限和审计约束的游戏命令入口。

## 架构

```text
浏览器 ──HTTPS/WSS──> 用户管理的反向代理 ──HTTP──> Control API ──> 本机 SQLite（WAL）
                                                    ▲
                                                    │ WSS（Agent 主动连接）
                                         Windows/Linux Agent ──> srcds / SteamCMD
```

- 中心端：Go、React、本机 SQLite；硬性限定单控制平面实例，TLS 由部署者管理。
- 节点端：单个旁置部署的 Go Agent，使用二进制同目录的配置和数据；既可直接前台运行，也可通过同一二进制的 `install` 命令注册为 systemd 或 Windows Service 后台常驻并开机自启。
- 游戏实例：每个实例使用独立目录；游戏安装使用节点绝对路径，该目录本身成为实例文件边界。Agent 的实例文件 API 只能访问已登记实例目录内部及自己的数据目录。升级迁移会删除旧的相对路径游戏安装及其绑定实例，需要管理员用绝对路径重新登记。当前同一节点的 Agent 与所有游戏进程共用一个低权限服务账号，目录分离不是每实例 OS 沙箱；在部署相互不信任的原生插件或租户前必须另做主机/账号隔离。
- 本地数据：实例备份和完整进程日志保存在节点。中心只保存备份元数据、任务/审计、通知和聚合指标；网页下载使用一次性、最大 1 MiB 的校验分块。

更完整的数据流和存储边界见 [架构说明](docs/architecture.md)。

## 快速部署中心端

支持以下中心端 Docker 主机：

- Linux：Docker Engine 24+ 与 Docker Compose v2。
- macOS：带 Docker Compose v2 的 Docker Desktop，支持 Apple Silicon 和 Intel Mac。中心端仍运行在 Linux 容器中；macOS 只作为中心端宿主机，不属于游戏节点 Agent 的支持平台。

如果使用已经构建好的 `linux/amd64` 或 `linux/arm64` 离线镜像，不需要源码和本地镜像构建工具链。把镜像包附带的 `docker-compose.yml` 放入永久部署目录后，`docker load` 导入镜像，`docker compose up -d` 即会自动生成并持久保存部署主密钥、创建 SQLite、执行迁移和启动检查并启动 Control；最后单独运行本地 `setup-key issue` 命令安全领取初始化密钥。固定 Compose 会把宿主机 `/var/run/docker.sock` 挂载给 Control，使全局所有者可以在设置页上传同平台 `.tar.gz` 镜像并由独立 Worker 自动重建 Control 和失败回滚。Docker Socket 权限实际等同宿主机 root，部署前必须接受这一信任边界。架构选择、校验、首次部署、在线升级与回滚见 [Linux/macOS 容器中心端安装说明](docs/container-center-installation.md)。下面继续说明从当前源码构建镜像的部署流程。

```bash
docker load --input l4d2panel-control-<version>-linux-<arch>.tar.gz
docker compose up -d
docker compose exec -T app /usr/local/bin/l4d2panel setup-key issue
```

默认配置可直接启动，Control 明文端口只发布到宿主机 `127.0.0.1:19090`，部署主密钥首次启动时在 `app_data` 卷内生成为权限 `0600` 的 `control-master.key`。生产部署应在 `.env` 中配置最终 HTTPS 地址和精确可信代理：

```bash
cp .env.example .env
```

```dotenv
PUBLIC_URL=https://panel.example.com
CONTROL_BIND_ADDRESS=127.0.0.1
CONTROL_PORT=19090
TRUSTED_PROXIES=127.0.0.1/32,::1/128
```

远程代理应把自己的精确 IP/CIDR 写入 `TRUSTED_PROXIES`；不要信任整个容器或局域网网段。同机 Nginx 经 Docker 发布端口连接时，Control 看到的来源在部分 Linux 配置中会是网桥网关，此时也应只加入实际观察到的单个 `/32` 地址，而不是整个私网。Docker Desktop 会按 Mac 架构自动构建 `linux/arm64` 或 `linux/amd64` 中心镜像，不需要 Rosetta。

编辑完 `.env` 后构建并启动中心端：

```bash
docker compose build app
docker compose up -d
docker compose exec -T app /usr/local/bin/l4d2panel setup-key issue
```

最后一条命令只在当前终端输出一次 `l4d2p_setup_...` 初始化密钥。配置 HTTPS 后打开 `PUBLIC_URL/setup` 并输入密钥；系统会原子创建 `admin`／`系统管理员` 根所有者并登录，密钥同时是初始密码。进入设置后应立即修改密码。初始化接口继续使用 15 分钟有效的 HttpOnly/SameSite 引导 Cookie、独立 CSRF 令牌、速率限制和数据库立即事务；丢失密钥时可在尚无用户前重新运行命令，新密钥会使旧密钥失效。

下面是 [`deploy/nginx.conf.example`](deploy/nginx.conf.example) 中的同机 Nginx 最小生产示例；证书路径、域名和防火墙应按实际环境调整：

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    '' close;
}

server {
    listen 80;
    server_name panel.example.com;
    return 308 https://$host$request_uri;
}

server {
    listen 443 ssl;
    http2 on;
    server_name panel.example.com;
    ssl_certificate /etc/letsencrypt/live/panel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/panel.example.com/privkey.pem;
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

    location / {
        proxy_pass http://127.0.0.1:19090;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        proxy_buffering off;
    }
}
```

无界面的自动化部署仍可使用 `docker compose run --rm -T app bootstrap-owner ...` 命令。旧版 `BOOTSTRAP_USERNAME`、`BOOTSTRAP_PASSWORD` 和 `BOOTSTRAP_DISPLAY_NAME` 启动环境变量也继续兼容，但不再写入示例配置或作为推荐流程。数据库已有任意用户后，网页初始化和命令行引导都会拒绝再次创建，后续账号应从已认证的团队管理页面维护。

SQLite 数据库位于 `app_data` 卷的 `/app/data/l4d2panel.sqlite3`，自动生成的主密钥位于同一卷的 `/app/data/control-master.key`。迁移在 Control 启动时按文件名顺序、以立即写事务执行；迁移失败、未知迁移或已应用迁移校验和变化都会中止启动。SQLite 文件、WAL 与 SHM 必须位于容器本机持久卷，禁止放到 NFS、CIFS 或 SMB bind mount。中心端不支持多个 Control 共享数据库。备份和恢复必须同时保留数据库与原主密钥，换用其他主密钥会导致已加密秘密无法恢复。

升级不会自动迁移或清理旧版捆绑代理的服务、状态目录或 Docker 孤立卷；确认不再使用后，由管理员按原部署方式自行处理。

停机备份可运行 `docker compose run --rm -T app backup-database --output /app/data/control-backup.sqlite3`；输出文件必须不存在，命令会先执行 `quick_check`，再生成一致性副本。

生产环境保持 `COOKIE_SECURE=true` 并使用 HTTPS。仅本机开发、直接运行 Control 时才可临时设为 `false`。

## Windows 原生部署中心端

Windows 10/11 x64 和 Windows Server 2019+ 使用发布页中的 `l4d2panel-center-*-windows-x64.zip`。解压到本机固定磁盘的永久目录后双击 `l4d2panel-control.exe`：程序自动请求 UAC、创建旁置 `data`、注册唯一的 Control 服务、开放 TCP 19090，并一次性显示初始化密钥。

Control 固定监听 `0.0.0.0:19090`，SQLite 不监听网络端口。管理员应先收紧防火墙并配置自己管理的 HTTPS，再访问 `/setup`。全局所有者可以在设置页选择新的官方 ZIP 一键升级；程序自动创建程序、配置与 SQLite 快照，失败时完整回滚，最近保留 3 份。

便携版不读取或迁移旧 MSI 数据；同名旧服务存在时会拒绝初始化。完整的安装、内置管理命令、Web/CLI 升级和恢复说明见 [Windows 中心端说明](docs/windows-center-installation.md)。

## 注册节点

Agent 保持二进制、`agent.yaml` 和 `data` 旁置部署，不需要额外安装脚本；同一二进制同时提供便携前台模式和 systemd/Windows Service 常驻模式。先从发布页下载对应平台二进制、`.sha256` 和 `agent.yaml.example`，验证 SHA-256，再把示例复制到二进制同目录并命名为 `agent.yaml`。在中心端“节点”页面创建短期一次性注册令牌，首次启动前只需修改：

```yaml
central_url: "http://localhost:19090"
enrollment_token: "一次性令牌"
```

Linux 限制配置权限后，可以直接以前台便携模式启动：

```bash
chmod 600 agent.yaml
chmod +x l4d2panel-agent-linux-amd64
./l4d2panel-agent-linux-amd64
```

也可以一条命令注册为 systemd 服务；非 root 终端会调用 `sudo`，创建专用低权限 `l4d2panel` 账号，安装、启动并启用开机自启：

```bash
./l4d2panel-agent-linux-amd64 install --config ./agent.yaml
```

Windows 在仅受节点管理员控制的永久目录中直接运行时同样是前台模式：

```powershell
Copy-Item .\agent.yaml.example .\agent.yaml
.\l4d2panel-agent-windows-amd64.exe
```

若需要后台常驻和开机自启，运行下面的命令；程序会自动请求 UAC，并注册使用 `NT SERVICE\l4d2panel-agent` 虚拟低权限账号的 Windows Service：

```powershell
.\l4d2panel-agent-windows-amd64.exe install --config .\agent.yaml
```

安装器会让 SYSTEM、Administrators 和固定服务账号的权限继承到服务随后创建的日志、journal、地图及其他运行目录。升级曾由旧版安装的节点后，可幂等地重新执行同一条 `install --config` 命令以修复现有目录权限，不需要重新注册节点。

前台启动和 `install` 都会在尚未注册时自动注册，把一次性令牌从 `agent.yaml` 中移除并原子写入节点 ID 和长期凭据。未显式配置的数据、实例及地图目录默认为同目录的 `data`、`data/instances` 与 `data/maps`。实例若位于其他文件系统卷，需在 `map_roots` 中为该卷增加一个绝对地图根；同一卷只允许一个根，不支持符号链接或 Junction。服务模式可用 `start/stop/restart/status/uninstall --config ./agent.yaml` 显式管理；这些命令在需要时同样请求提权。首次前台启动也可以不预建文件，使用参数创建同目录的 `agent.yaml`：

```bash
./l4d2panel-agent-linux-amd64 \
  --central-url http://localhost:19090 \
  --enrollment-token 一次性令牌
```

也兼容 `CENTRAL_URL`、`ENROLLMENT_TOKEN` 和 `NODE_NAME` 环境变量。令牌属于秘密；共享主机上优先使用受限配置文件，避免把令牌写入 shell 历史或暴露在进程参数中。便携模式使用当前账号且关闭终端即停止；内置服务模式使用平台专用低权限账号、自动重启和开机自启。部署者仍可用显式 `run --config` 交给其他进程管理器。

服务模式导入由 `root`／Administrator 准备的现有游戏目录时，Agent 会在登记路径后立即验证服务账号能否安全写入目录；不可写时任务会明确要求节点管理员在本机重新执行原来的 `install --config` 命令。幂等安装会重新验证所有已登记路径必须是无链接、包含平台固定 `srcds` 的精确游戏根，并批量把这些目录交给固定低权限服务账号，随后在网页重试“检测 srcds”即可；远程任务本身不会获得改属主或修改 ACL 的系统权限。由 Agent 通过托管流程新建的目录天然属于服务账号，不需要这一步。

默认应使用受信任证书的 HTTPS 中心地址。确实位于同一主机或可信隔离局域网时，也可以把 `central_url`/`--central-url` 设置为 `http://localhost:19090`、回环 IP 或明确的私网/链路本地 IP；Agent 会相应使用普通 WebSocket。为避免 DNS 重绑定后把凭据发送到公网，明文模式不解析或接受 `panel.lan` 一类主机名，必须填写 IP（`localhost` 除外），且公网 HTTP 仍会被拒绝。HTTP 不加密一次性注册令牌、长期节点凭据、任务和结果，能够监听或篡改局域网流量的设备可以接管节点；只应在受控网络中使用。

升级不需要新的注册令牌：先等待节点任务结束并从中心端正常停止运行中的实例，再停止 Agent；验证新二进制的 SHA-256 和版本，保留 `agent.yaml` 与 `data/`，原子替换二进制后重新启动即可。失败时用保留的前一版本二进制回滚。完整的 Windows/Linux 首次部署、升级和回滚命令见 [便携 Agent 部署说明](docs/agent-portable.md)。

- 正式支持 amd64 Windows 10/11、Windows Server 2019+、Ubuntu 22.04/24.04、Debian 12。
- Agent 不扫描或安装 SteamCMD，也不下载游戏本体。节点上线后在中心端“运行环境”中登记现有路径或安装托管 SteamCMD；运行 L4D2 仍需节点具备对应的 32 位运行库。

## 数据保留与恢复

- 新实例默认创建每日 04:00 备份计划，保留 7 个日备份和 4 个周备份；每日备份会为每个 ISO 周创建一次周副本，日/周条目都可在面板中列出和下载。恢复前自动创建检查点；备份只包含 `cfg`、`addons` 及其 `left4dead2` 对应目录（包括 `left4dead2/addons/workshop` 中已分配的地图 VPK），不包含游戏本体、日志或旧版本可能遗留且含秘密的 `left4dead2/cfg/l4d2panel.cfg`。当前 v3 清单另带经过严格校验的无秘密启动资料（模式、地图、人数、高级参数、自动重启及安全 CVar 等）和地图 ID/聚合哈希/BSP 引用，不具备表示 RCON 密码的字段；恢复会优先复用目标节点校验一致的地图对象，节点缺少对象时仍保留备份中的 Workshop VPK，游戏配置继续由 `server.cfg` 提供。旧 v1/v2 清单继续可读。
- 网站可把下载到节点外的备份再上传到任一目标实例：Control 创建 24 小时可续期会话，客户端按最多 1 MiB、带 offset 和 SHA-256 的顺序分块发送，正文只以主密钥密文短暂进入任务并暂存在目标 Agent；末块验证整包大小/哈希后才进入 ready。下载端在支持 File System Access API 的 Chromium 浏览器中逐块写入用户选择的文件并校验完整 SHA-256，不把整包聚合到内存；其他浏览器仅对不超过 512 MiB 的备份使用内存回退。每个 Agent 默认最多保留 32 个上传会话；首次可信分块会以严格 JSON、权限 `0600` 的原子 sidecar 持久化整包大小和 SHA-256，64 GiB 上限按所有会话预约总量以及元数据/未知文件共同核算，旧版无 sidecar 文件在升级前保守预约 20 GiB。写入前还会确认磁盘能够兑现全部未完成预约并保留至少 5 GiB 可用空间，因而多个大上传不能先占部分空间后互相卡死。无活动的未完成 `.part` 会在约 25 小时后清理，完整 `.zip` 保留 48 小时；完成、续租、删除和清理均按 UUID 将数据、sidecar 与原子临时文件成组处理。完整校验及每次恢复尝试都会续租完整会话，以覆盖 Control 在失败后提供的 24 小时无重传重试窗口。完成会话会排队标准恢复流程；同一幂等键重放原任务，恢复失败或取消后上传会话续期 24 小时，使用新键即可无重传重试。跨系统恢复先按依赖顺序安装并验证目标平台 Metamod/SourceMod，再把来源可移植数据作为最终覆盖层写入，避免官方包的默认配置覆盖用户配置；任一步失败都会用恢复前检查点以及原 registry/旧版遗留配置回滚。
- 浏览器持久化上传文件身份、未确认任务和幂等键，刷新或关闭后重选同一文件即可续传。续传提示提供“放弃会话”：已有服务端 upload ID 时，Control 原子标记会话过期并排队只按 UUID 清理 Agent 暂存；尚未创建服务端会话时只清除浏览器本地记录。
- 实例进程日志在 Agent 数据目录保留当前文件加 19 个历史段，每段上限 100 MiB（约 2 GiB 上限）。Agent 启动时及此后每小时清理超过 7 天的已关闭段和停止实例日志；运行中当前段由轮转器管理。它们不会长期复制到中心；日志搜索结果和下载分块被读取后从任务密文中消费。
- “隔离文件”会把实例目录移到所选实例根的 `.quarantine` 并强制保留至少 7 天。到期不会后台自动删除：管理员必须在浏览器会话中先领取仅返回一次、5 分钟有效且绑定用户/节点/条目的确认令牌，再提交永久清理任务。Agent 只接受 Control 记录的根索引和严格 basename，并再次拒绝未满 7 天、路径逃逸、符号链接/重解析点或未知条目；确认请求和任务均写入审计/任务历史。
- 节点磁盘损坏会同时丢失实例与本地备份。关键服应经网页分块下载备份并保存到节点外。

## 本地开发

需要 Go 1.25.12 或更高补丁版本、Node.js 24；SQLite 通过无 CGO 的 Go 驱动嵌入：

```bash
go mod download
go run ./cmd/control
```

另一个终端运行：

```bash
cd web
npm ci
npm run dev
```

前端开发服务器代理 `/api`、`/agent` 和 WebSocket 请求。Agent 可以前台方式运行：

```bash
go run ./cmd/agent run --config ./agent.dev.json
```

配置格式参考 [`configs/agent.example.json`](configs/agent.example.json)。

## 测试与发布验证

便携节点端以后统一使用下面的构建目标。一次同时构建无 CGO 的 `linux/amd64` 和 `windows/amd64`，产物目录固定包含两个 Agent 二进制、各自的 SHA-256 校验文件，以及需要复制为 `agent.yaml` 的首次绑定示例：

```bash
make build-agent-portable \
  VERSION=v0.1.0 \
  COMMIT=$(git rev-parse --short HEAD)
```

输出目录固定为 `dist/agents/`。正式 GitHub Release 使用相同的无 CGO 构建参数、文件名和配置示例；`dist` 是本地构建输出，不进入源码提交。

Windows 开发机可用一个命令构建 Windows 中心端、两个容器中心端和两个便携 Agent。脚本只执行一次宿主前端构建，容器架构按顺序导出，避免同一 Docker builder 上的并发 exporter 争用：

```powershell
.\packaging\Build-ReleaseArtifacts.ps1 `
  -Version 0.1.0 `
  -WslDistribution l4d2
```

Linux/macOS 只构建两个离线容器包时使用：

```bash
VERSION=0.1.0 make build-container-offline
```

Dockerfile 使用持久的 npm、Go 模块和 Go 编译 BuildKit 缓存；`dist` 不进入构建上下文，版本或构建日期变化也不会重新安装最终 Alpine 依赖。首次构建仍需下载基础镜像和依赖，后续构建会复用缓存。如果部署者所在网络无法稳定访问默认官方源，可在确认并信任镜像运营方后显式覆盖构建源；Go 模块仍受 `go.sum` 校验，Alpine 包仍受仓库签名校验：

```powershell
.\packaging\Build-ReleaseArtifacts.ps1 `
  -Version 0.1.0 `
  -WslDistribution l4d2 `
  -GoProxy 'https://goproxy.cn,direct' `
  -AlpineMirror 'https://mirrors.aliyun.com/alpine'
```

也可以为 `build-container-offline.sh` 设置 `GOPROXY`、`ALPINE_MIRROR`、`PLATFORMS`、`OUTPUT_DIR` 和 `GZIP_LEVEL`。生产发布应优先使用受控缓存代理；切换下载源只解决可用性，不提供发布者签名。

```bash
go test ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
cd web
npm test -- --run
npm audit --audit-level=high
npm run build
```

20 节点/100 实例的中心端模拟验收使用独立临时 SQLite 文件，通过真实 HTTP 路由注册节点、并发创建实例、重放幂等请求，并通过 WebSocket 游标验证持久事件；它不连接 Agent、SteamCMD 或游戏服：

```bash
RUN_FLEET_ACCEPTANCE=1 \
go test ./internal/control -run TestFleetScaleAcceptance -count=1 -timeout=2m
```

CI 还执行上述 20/100 模拟验收、Go race 测试、Windows/Linux Control 与 Agent 无 CGO 交叉构建、Playwright、`docker compose config`，并同时构建 `linux/amd64` 与 `linux/arm64` 中心镜像；Compose 作业会实际启动、停止并重启 Control，确认 SQLite 卷可独立通过 `doctor`。Windows 发布作业生成便携 ZIP、未签名文件清单、SHA-256、CycloneDX SBOM 和第三方许可证，并验证包中没有安装器或运行期脚本载荷。正式发布仍需在 Windows 10/11 与 Server 2019/2022/2025 VM 上验证提升权限安装、初始化、一键升级、故障回滚、注销与清理。上述自动化不等同于真实游戏验证：发布前仍需分别在受支持的 Windows、Ubuntu 和 Debian 主机上用真实 SteamCMD 安装/启动 App 222860，验证 A2S、RCON、更新、备份恢复和重启。

## API 与参考资料

- REST 基路径为 `/api/v1`；长操作通常返回 `202` 和 `job_id`。浏览器会话的写请求需要 `X-CSRF-Token`，Bearer API Token 不使用 CSRF。
- 完整契约见 [OpenAPI](docs/openapi.yaml)；部署后也可读取 `/api/openapi.yaml` 或 `/docs/openapi.yaml`。架构与安全边界见 [architecture.md](docs/architecture.md) 和 [security.md](docs/security.md)。
- L4D2 专用服 App/Depot：[SteamDB App 222860 配置](https://steamdb.info/app/222860/config/)
- Source Dedicated Server 参数：[Valve Developer Community](<https://developer.valvesoftware.com/wiki/Command_line_options_(Source_Dedicated_Server)>)
- VPK 格式：[Valve Developer Community](https://developer.valvesoftware.com/wiki/VPK)
- SourceMod 安装：[AlliedModders Wiki](https://wiki.alliedmods.net/Installing_SourceMod)

实例可选用 `-tickrate <数值>` 启动参数；Control 与 Agent 都只接受 1–1000 的单个整数值，并继续使用参数数组直接启动游戏进程。
