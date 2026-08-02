# L4D2 Panel

面向《求生之路 2》专用服务器的自托管多节点管理面板。中心控制台可通过 Linux/macOS Docker 或 Windows x64 便携 ZIP 部署，并通过节点主动建立的 WSS 连接管理 Windows 与 Linux Agent；节点不需要开放面板管理端口，游戏端口仍由管理员按实例开放。

## 当前实现

- 节点与任务：一次性注册令牌、可轮换/吊销的节点凭据、断线离线队列、任务幂等、任务取消，以及 `queued/dispatched/running/succeeded/failed/canceled` 状态机。
- 实例：对白名单根执行有界只读安装扫描、App 222860 安装或导入、端口分配与冲突检查、既有进程受控接管、启停/重启、RCON 优雅退出、崩溃自动恢复、校验、更新、重装、解除管理和文件隔离。
- 游戏管理：A2S 状态、RCON 玩家/踢出/封禁/地图/广播、结构化配置与版本回滚、受限文件读写、上传或 HTTPS/Workshop 安装 VPK/SourceMod/Metamod、非 Workshop 单文件 VPK/SMX 最近 3 个历史版本回退、备份与计划任务。文件 API 不允许改写可执行或加载关键文件，这些内容必须经包管理流程安装。
- 管理与安全：所有者/管理员/操作员/观察者，支持全局、节点或实例范围绑定；团队账号与绑定只允许全局所有者的交互式会话管理；HttpOnly 会话、CSRF、Argon2id、TOTP 与一次性恢复码、有限期且可限定资源的 API Token、不可编辑审计记录。
- 可观测性：10 秒心跳、15 秒指标、A2S 探测、每 30 分钟的能力协商式更新检测、站内通知、SMTP、带 HMAC-SHA256 签名和出站 SSRF 防护的 Webhook、公开状态页，以及节点本地日志的有限检索/分块下载。
- 自动维护更新：实例设置标准五段 `maintenance_cron` 后，仅在探测确认 `update_available=true`、节点在线且 Agent 已连接时，按 Control 全局时区在窗口内创建带 10/5/1 分钟广播的持久化更新批次。同一节点逐实例派发；普通手动/计划任务使用独立派发额度。Control 短时重启可补偿最近 5 分钟窗口，批次入队会写审计与站内/外部通知。

这不是操作系统远程终端。所有 Agent 操作都是固定任务类型；启动/重启不能覆盖已登记的可执行文件、参数或环境，游戏进程只继承受限环境变量。RCON 是受权限和审计约束的游戏命令入口。

## 架构

```text
浏览器 ──HTTPS/WSS──> 用户管理的反向代理 ──HTTP──> Control API ──> 本机 SQLite（WAL）
                                                    ▲
                                                    │ WSS（Agent 主动连接）
                                         Windows/Linux Agent ──> srcds / SteamCMD
```

- 中心端：Go、React、本机 SQLite；硬性限定单控制平面实例，TLS 由部署者管理。
- 节点端：单个 Go Agent，以 Windows Service 或 systemd service 运行。
- 游戏实例：每个实例使用独立目录；Agent API 仅允许访问配置的实例根目录和自己的数据目录。当前同一节点的 Agent 与所有游戏进程共用一个低权限服务账号，目录分离不是每实例 OS 沙箱；在部署相互不信任的原生插件或租户前必须另做主机/账号隔离。
- 本地数据：实例备份和完整进程日志保存在节点。中心只保存备份元数据、任务/审计、通知和聚合指标；网页下载使用一次性、最大 1 MiB 的校验分块。

更完整的数据流和存储边界见 [架构说明](docs/architecture.md)。

## 快速部署中心端

支持以下中心端 Docker 主机：

- Linux：Docker Engine 24+ 与 Docker Compose v2。
- macOS：带 Docker Compose v2 的 Docker Desktop，支持 Apple Silicon 和 Intel Mac。中心端仍运行在 Linux 容器中；macOS 只作为中心端宿主机，不属于游戏节点 Agent 的支持平台。

在 Linux Shell 或 macOS“终端”中复制环境变量示例，并替换所有占位值：

```bash
cp .env.example .env
openssl rand -base64 32
```

将输出写入 `MASTER_KEY`，并令 `PUBLIC_URL` 指向用户配置的 HTTPS 地址。Control 默认在容器和宿主机发布明文 TCP `19090`；该端口只应允许 HTTPS 代理访问：

```dotenv
PUBLIC_URL=https://panel.example.com
CONTROL_PORT=19090
TRUSTED_PROXIES=127.0.0.1/32,::1/128
```

远程代理应把自己的精确 IP/CIDR 写入 `TRUSTED_PROXIES`；不要信任整个容器或局域网网段。同机 Nginx 经 Docker 发布端口连接时，Control 看到的来源在部分 Linux 配置中会是网桥网关，此时也应只加入实际观察到的单个 `/32` 地址，而不是整个私网。Docker Desktop 会按 Mac 架构自动构建 `linux/arm64` 或 `linux/amd64` 中心镜像，不需要 Rosetta。

编辑完 `.env` 后构建并启动中心端：

```bash
docker compose build app
docker compose up -d
docker compose exec -T app setup-key issue
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

SQLite 数据库位于 `app_data` 卷的 `/app/data/l4d2panel.sqlite3`。迁移在 Control 启动时按文件名顺序、以立即写事务执行；迁移失败、未知迁移或已应用迁移校验和变化都会中止启动。SQLite 文件、WAL 与 SHM 必须位于容器本机持久卷，禁止放到 NFS、CIFS 或 SMB bind mount。中心端不支持多个 Control 共享数据库。

升级不会自动迁移或清理旧版捆绑代理的服务、状态目录或 Docker 孤立卷；确认不再使用后，由管理员按原部署方式自行处理。

停机备份可运行 `docker compose run --rm -T app backup-database --output /app/data/control-backup.sqlite3`；输出文件必须不存在，命令会先执行 `quick_check`，再生成一致性副本。

生产环境保持 `COOKIE_SECURE=true` 并使用 HTTPS。仅本机开发、直接运行 Control 时才可临时设为 `false`。

## Windows 原生部署中心端

Windows 10/11 x64 和 Windows Server 2019+ 使用发布页中的 `l4d2panel-center-*-windows-x64.zip`。解压到本机固定磁盘的永久目录后双击 `l4d2panel-control.exe`：程序自动请求 UAC、创建旁置 `data`、注册唯一的 Control 服务、开放 TCP 19090，并一次性显示初始化密钥。

Control 固定监听 `0.0.0.0:19090`，SQLite 不监听网络端口。管理员应先收紧防火墙并配置自己管理的 HTTPS，再访问 `/setup`。全局所有者可以在设置页选择新的官方 ZIP 一键升级；程序自动创建程序、配置与 SQLite 快照，失败时完整回滚，最近保留 3 份。

便携版不读取或迁移旧 MSI 数据；同名旧服务存在时会拒绝初始化。完整的安装、内置管理命令、Web/CLI 升级和恢复说明见 [Windows 中心端说明](docs/windows-center-installation.md)。

## 注册节点

先在“节点”页面创建短期一次性注册令牌。Linux 节点：

```bash
sudo CENTRAL_URL=https://panel.example.com \
  ENROLLMENT_TOKEN=一次性令牌 \
  NODE_NAME=game-linux-01 \
  ./scripts/install-agent.sh
```

Windows 节点以管理员 PowerShell 运行：

```powershell
.\scripts\install-agent.ps1 `
  -CentralUrl https://panel.example.com `
  -EnrollmentToken 一次性令牌 `
  -NodeName game-windows-01
```

默认应使用受信任证书的 HTTPS 中心地址。确实位于同一主机或可信隔离局域网时，也可以把 `CENTRAL_URL`/`-CentralUrl` 设置为 `http://localhost:19090`、回环 IP 或明确的私网/链路本地 IP；Agent 会相应使用普通 WebSocket。为避免 DNS 重绑定后把凭据发送到公网，明文模式不解析或接受 `panel.lan` 一类主机名，必须填写 IP（`localhost` 除外），且公网 HTTP 仍会被拒绝。HTTP 不加密一次性注册令牌、长期节点凭据、任务和结果，能够监听或篡改局域网流量的设备可以接管节点；只应在受控网络中使用。

正式发布包附带 Agent 与 SHA-256 文件。从源码测试时通过 `AGENT_BINARY` 或 `-AgentBinary` 指向本地二进制。重复运行安装器会保留现有节点凭据并升级二进制，因此升级时可省略注册令牌。

- 正式支持 amd64 Windows 10/11、Windows Server 2019+、Ubuntu 22.04/24.04、Debian 12。
- Windows 使用 `NT SERVICE\l4d2panel-agent` 虚拟服务账号和受限 ACL；Linux 使用无登录的 `l4d2panel` 账号和 systemd 沙箱。
- 安装器检查系统/架构、磁盘、SteamCMD 和 32 位运行库，并提示主机防火墙状态。其他 systemd 发行版为尽力兼容。

## 数据保留与恢复

- 新实例默认创建每日 04:00 备份计划，保留 7 个日备份和 4 个周备份；每日备份会为每个 ISO 周创建一次周副本，日/周条目都可在面板中列出和下载。恢复前自动创建检查点；备份只包含 `cfg`、`addons` 及其 `left4dead2` 对应目录，不包含游戏本体、日志或含秘密的生成文件 `left4dead2/cfg/l4d2panel.cfg`。v2 清单另带经过严格校验的无秘密启动资料（模式、地图、人数、高级参数、自动重启及安全 CVar 等），不具备表示 RCON/服务器密码的字段；恢复会使用来源启动资料和目标实例当前的加密秘密重新生成该文件。旧 v1 清单继续可读，因其没有启动快照，恢复时保留目标实例原启动资料。
- 网站可把下载到节点外的备份再上传到任一目标实例：Control 创建 24 小时可续期会话，客户端按最多 1 MiB、带 offset 和 SHA-256 的顺序分块发送，正文只以主密钥密文短暂进入任务并暂存在目标 Agent；末块验证整包大小/哈希后才进入 ready。下载端在支持 File System Access API 的 Chromium 浏览器中逐块写入用户选择的文件并校验完整 SHA-256，不把整包聚合到内存；其他浏览器仅对不超过 512 MiB 的备份使用内存回退。每个 Agent 默认最多保留 32 个上传会话；首次可信分块会以严格 JSON、权限 `0600` 的原子 sidecar 持久化整包大小和 SHA-256，64 GiB 上限按所有会话预约总量以及元数据/未知文件共同核算，旧版无 sidecar 文件在升级前保守预约 20 GiB。写入前还会确认磁盘能够兑现全部未完成预约并保留至少 5 GiB 可用空间，因而多个大上传不能先占部分空间后互相卡死。无活动的未完成 `.part` 会在约 25 小时后清理，完整 `.zip` 保留 48 小时；完成、续租、删除和清理均按 UUID 将数据、sidecar 与原子临时文件成组处理。完整校验及每次恢复尝试都会续租完整会话，以覆盖 Control 在失败后提供的 24 小时无重传重试窗口。完成会话会排队标准恢复流程；同一幂等键重放原任务，恢复失败或取消后上传会话续期 24 小时，使用新键即可无重传重试。跨系统恢复先按依赖顺序安装并验证目标平台 Metamod/SourceMod，再把来源可移植数据作为最终覆盖层写入，避免官方包的默认配置覆盖用户配置；任一步失败都会用恢复前检查点以及原 registry/生成配置回滚。
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

CI 还执行上述 20/100 模拟验收、Go race 测试、Windows/Linux Control 与 Agent 无 CGO 交叉构建、安装脚本静态检查、Playwright、`docker compose config`，并同时构建 `linux/amd64` 与 `linux/arm64` 中心镜像；Compose 作业会实际启动、停止并重启 Control，确认 SQLite 卷可独立通过 `doctor`。Windows 发布作业生成便携 ZIP、未签名文件清单、SHA-256、CycloneDX SBOM 和第三方许可证，并验证包中没有安装器或运行期脚本载荷。正式发布仍需在 Windows 10/11 与 Server 2019/2022/2025 VM 上验证提升权限安装、初始化、一键升级、故障回滚、注销与清理。上述自动化不等同于真实游戏验证：发布前仍需分别在受支持的 Windows、Ubuntu 和 Debian 主机上用真实 SteamCMD 安装/启动 App 222860，验证 A2S、RCON、更新、备份恢复和重启。

## API 与参考资料

- REST 基路径为 `/api/v1`；长操作通常返回 `202` 和 `job_id`。浏览器会话的写请求需要 `X-CSRF-Token`，Bearer API Token 不使用 CSRF。
- 完整契约见 [OpenAPI](docs/openapi.yaml)；部署后也可读取 `/api/openapi.yaml` 或 `/docs/openapi.yaml`。架构与安全边界见 [architecture.md](docs/architecture.md) 和 [security.md](docs/security.md)。
- L4D2 专用服 App/Depot：[SteamDB App 222860 配置](https://steamdb.info/app/222860/config/)
- Source Dedicated Server 参数：[Valve Developer Community](<https://developer.valvesoftware.com/wiki/Command_line_options_(Source_Dedicated_Server)>)
- VPK 格式：[Valve Developer Community](https://developer.valvesoftware.com/wiki/VPK)
- SourceMod 安装：[AlliedModders Wiki](https://wiki.alliedmods.net/Installing_SourceMod)

原版 L4D2 不把 `-tickrate` 作为面板可调设置；高级参数校验会提示该参数无效。
