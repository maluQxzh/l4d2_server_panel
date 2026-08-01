# L4D2 Panel

面向《求生之路 2》专用服务器的自托管多节点管理面板。中心控制台可通过 Linux/macOS Docker 或 Windows x64 原生 MSI 部署，并通过节点主动建立的 WSS 连接管理 Windows 与 Linux Agent；节点不需要开放面板管理端口，游戏端口仍由管理员按实例开放。

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
浏览器 ──HTTPS/WSS──> Caddy ──> Control API ──> PostgreSQL
                                      ▲
                                      │ WSS（Agent 主动连接）
                           Windows/Linux Agent ──> srcds / SteamCMD
```

- 中心端：Go、React、PostgreSQL 17、Caddy；首版为单控制平面实例。
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
openssl rand -hex 32
```

将 `openssl rand -base64 32` 的输出写入 `MASTER_KEY`；可将十六进制输出用作仅含 URL 安全字符的 `POSTGRES_PASSWORD`。令 `PANEL_DOMAIN` 和 `PUBLIC_URL` 指向当前主机。Caddy 为可公开解析的域名申请并续期 HTTPS 证书；`localhost` 使用 Caddy 的本地证书行为。

中心端对外端口不限定为 80/443。下面的配置把宿主机 HTTP/HTTPS 端口改为 8080/8443；使用非标准 HTTPS 端口时，`PUBLIC_URL` 必须包含端口：

```dotenv
PANEL_DOMAIN=panel.example.com
PUBLIC_URL=https://panel.example.com:8443
HTTP_PORT=8080
HTTPS_PORT=8443
```

Caddy 容器内部仍监听 80/443，并把外部 HTTP 端口跳转到配置的外部 HTTPS 端口。公开 CA 的自动证书签发通常仍要求公网 80/443 能到达 Caddy；若网络只开放自定义端口，请通过上游反向代理终止 TLS，或自行提供证书。

macOS 本机试用时可使用下面的配置，避开可能已被其他应用占用的 80/443：

```dotenv
PANEL_DOMAIN=localhost
PUBLIC_URL=https://localhost:8443
HTTP_PORT=8080
HTTPS_PORT=8443
```

`localhost` 使用 Caddy 容器内的本地 CA，macOS 和浏览器默认不会自动信任该 CA，首次访问可能显示证书警告。正式部署应使用可公开解析的域名和受信任证书。Docker Desktop 会按 Mac 架构自动构建 `linux/arm64` 或 `linux/amd64` 中心镜像，不需要 Rosetta。

编辑完 `.env` 后构建并启动中心端：

```bash
docker compose build app
docker compose up -d
```

打开 `PUBLIC_URL`。数据库中没有用户时，网站会自动进入 `/setup`，在页面中设置首个所有者的用户名、显示名称和密码，成功后直接进入控制台。初始化链接不使用预置密码；应在可信网络中完成首次初始化，不要把一个尚未初始化的中心端长时间暴露到公网。初始化接口使用 15 分钟有效的 HttpOnly/SameSite 引导 Cookie 和独立 CSRF 令牌，并在数据库中串行化创建操作；仅第一个成功请求可以建立所有者。

无界面的自动化部署仍可使用 `docker compose run --rm -T app bootstrap-owner ...` 命令。旧版 `BOOTSTRAP_USERNAME`、`BOOTSTRAP_PASSWORD` 和 `BOOTSTRAP_DISPLAY_NAME` 启动环境变量也继续兼容，但不再写入示例配置或作为推荐流程。数据库已有任意用户后，网页初始化和命令行引导都会拒绝再次创建，后续账号应从已认证的团队管理页面维护。

数据库迁移在 Control 启动时按文件名顺序执行。迁移失败或已应用迁移的校验和变化会中止启动，不会自动降级。涉及实例写锁的升级还会先检查旧数据；如果同一实例已有多个可能仍在执行的写任务，会给出处理提示并安全中止，而不会猜测性覆盖任务状态。升级前应备份 PostgreSQL 命名卷。

PostgreSQL 默认不向宿主机发布端口。

生产环境保持 `COOKIE_SECURE=true` 并使用 HTTPS。仅本机开发、直接运行 Control 时才可临时设为 `false`。

## Windows 原生部署中心端

Windows 10/11 x64 和 Windows Server 2019+ 可以直接安装发布页中的 `l4d2panel-center-*-windows-x64.msi`，不需要 Docker、WSL2 或 Windows 容器。向导默认安装受管 PostgreSQL 17、Control 和 Caddy；程序位于 `%ProgramFiles%\L4D2Panel\Control`，数据库、配置、证书、日志与升级检查点位于 `%ProgramData%\L4D2Panel`。

向导允许选择域名或 `localhost`、Control 回环监听端口以及任意未占用的 HTTP/HTTPS 入口端口，不限定为 80/443。Control 默认监听 `127.0.0.1:18080`，可避开本机已有服务；安装器会同步 Control 配置、Caddy 上游与健康检查地址。安装完成后访问显示的 `/setup` 地址创建首个所有者。受管数据库只监听 `127.0.0.1:55432`；只有内置 Caddy 选中的入口端口会加入 Windows 防火墙。

外部 PostgreSQL 密码不能作为 MSI 命令行属性传递，必须放在禁用 ACL 继承、仅管理员与 SYSTEM 可读的响应文件中。普通卸载保留 ProgramData；升级使用安装目录中的 `Update-L4D2PanelCenter.ps1`，它会先校验 MSI、执行 `pg_dump` 并保存配置/证书检查点。完整的交互安装、`/qn`、外部数据库/代理、升级与彻底清理说明见 [Windows 中心端安装说明](docs/windows-center-installation.md)。

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

需要 Go 1.25.12 或更高补丁版本、Node.js 24、PostgreSQL 17：

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

有 PostgreSQL 17 时可运行 20 节点/100 实例的中心端模拟验收。它会在独立临时 schema 中通过真实 HTTP 路由注册节点、并发创建实例、重放幂等请求，并通过 WebSocket 游标验证持久事件；结束后自动删除 schema，不连接 Agent、SteamCMD 或游戏服：

```bash
TEST_DATABASE_URL='postgres://user:password@127.0.0.1:5432/l4d2panel_test?sslmode=disable' \
RUN_FLEET_ACCEPTANCE=1 \
go test ./internal/control -run TestFleetScaleAcceptance -count=1 -timeout=2m
```

CI 还执行上述 20/100 模拟验收、Go race 测试、Windows/Linux Agent 交叉构建、安装脚本静态检查、Playwright、`docker compose config`，并同时构建 `linux/amd64` 与 `linux/arm64` 中心镜像。Windows 发布作业组装固定版本 PostgreSQL/Caddy 载荷，生成 MSI、SHA-256、CycloneDX SBOM 和第三方许可证清单，并执行静默安装、三个服务、回环监听、`/readyz`、`/setup` 与保留数据卸载冒烟测试；配置签名证书时还会签署并验证 EXE/MSI，否则产物明确标记 `unsigned`。上述自动化不等同于真实游戏验证：发布前仍需分别在受支持的 Windows、Ubuntu 和 Debian 主机上运行安装器，并用真实 SteamCMD 安装/启动 App 222860，验证 A2S、RCON、更新、备份恢复和重启。当前开发工作站未对真实 Docker Compose 集群或 Steam 服务完成端到端运行验证。

## API 与参考资料

- REST 基路径为 `/api/v1`；长操作通常返回 `202` 和 `job_id`。浏览器会话的写请求需要 `X-CSRF-Token`，Bearer API Token 不使用 CSRF。
- 完整契约见 [OpenAPI](docs/openapi.yaml)；部署后也可读取 `/api/openapi.yaml` 或 `/docs/openapi.yaml`。架构与安全边界见 [architecture.md](docs/architecture.md) 和 [security.md](docs/security.md)。
- L4D2 专用服 App/Depot：[SteamDB App 222860 配置](https://steamdb.info/app/222860/config/)
- Source Dedicated Server 参数：[Valve Developer Community](<https://developer.valvesoftware.com/wiki/Command_line_options_(Source_Dedicated_Server)>)
- VPK 格式：[Valve Developer Community](https://developer.valvesoftware.com/wiki/VPK)
- SourceMod 安装：[AlliedModders Wiki](https://wiki.alliedmods.net/Installing_SourceMod)

原版 L4D2 不把 `-tickrate` 作为面板可调设置；高级参数校验会提示该参数无效。
