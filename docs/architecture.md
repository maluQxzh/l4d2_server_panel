# 架构说明

## 控制平面

Control 是唯一可信协调者，负责身份、资源权限、期望状态、任务队列、审计、事件和告警。本机 SQLite（WAL）保存持久状态；Agent 保存实际进程状态、本地注册表和任务幂等日志。中心端硬性限定一个 Control 实例：数据库旁的 OS 独占锁阻止第二个 Control 打开数据库，不能把多个 Control 副本放在负载均衡器后。唯一例外是本机 `setup-key issue` 的单个立即事务，它不返回通用 Store、不执行迁移或关闭检查点，也不能启动第二个 Control。

React 前端被编译进 Control 二进制并同域提供。浏览器 REST 接口位于 `/api/v1`，实时事件位于 `/api/v1/events`，Agent 注册使用 `/api/v1/agent/enroll`，长期连接使用 `/agent/v1/connect`。Control 提供明文 HTTP，部署者负责通过外部反向代理终止 TLS 并转发 HTTP/WebSocket。

中心 Compose 栈始终运行 Linux 容器，可部署在 Linux Docker Engine 或 macOS Docker Desktop。Control 镜像提供 `linux/amd64` 与 `linux/arm64` 构建路径，Docker Desktop 在 Intel Mac 和 Apple Silicon 上自动选择对应架构；这不会把 macOS 加入 Agent 或游戏服节点的支持平台。离线镜像由 `docker load` 导入，固定 Compose 模板通过 `docker compose up -d` 启动。Docker Compose 显式配置持久主密钥文件时，Control 首次启动在 `app_data` 卷中生成该文件，后续启动复用；同一启动路径自动运行数据库迁移并在失败时拒绝监听。预构建镜像模板默认只向 `127.0.0.1` 发布 Control 端口，外部反向代理必须由管理员显式扩大监听并收紧宿主防火墙。

固定 Compose 把宿主 Docker Socket 只读根文件系统之外挂载为 `/var/run/docker.sock`，并用独立 `app_updates` 卷保存升级 journal、镜像元数据和数据库快照。全局所有者通过交互式会话上传固定标签的单平台 Docker `save` gzip 包；Control 校验归档路径、全部 OCI blob 摘要、产品/版本/平台元数据和正式版本禁止降级规则，确认 daemon 仍接受固定支持的 Docker API 版本，再加载镜像并创建使用当前镜像运行的短期 Worker。没有有序版本号的开发构建允许替换为其他开发构建或转入正式版本，但正式版本不能切回开发标签。Worker 停止当前 Control 后快照 SQLite、WAL/SHM 和主密钥，保留旧容器并用复核后的不可变镜像 ID 重建同名容器。重建请求只包含固定 Compose 允许的环境变量、发布端口、三个命名卷、Socket、单一网络，以及由 Control 固定生成的重启、只读根、安全、日志和健康检查参数；不会回放 Inspect 中 daemon 生成的 HostConfig、IPAM、旧镜像入口点或命令。新容器通过 Docker exec 访问 `/readyz` 后才删除旧容器；创建、迁移或就绪失败会先收集退出码、OOM 状态和有界脱敏的尾部日志，再删除新容器、恢复同一快照和离线标签的旧镜像指向并重新启动旧容器。终态及诊断由新旧 Control 从 `app_updates` 卷协调回 SQLite、审计和持久事件。该设计仍是单 Control，不引入第二个并发访问 SQLite 的服务实例。

Windows 10/11 x64 与 Windows Server 2019+ 使用便携 ZIP，不依赖 Docker/WSL2。EXE 在解压目录旁创建 `data`，只注册 `l4d2panel-control` 虚拟账号服务。Control 固定在 `0.0.0.0:19090` 监听，SQLite 不监听端口；配置、数据库、日志、升级状态和快照均位于受 ACL 限制的旁置目录。Web 升级由服务复制出的独立工作进程执行，因此 Control 停止和程序替换期间状态仍然持久；失败时旧程序、配置与 SQLite 快照作为一个部署版本恢复。无序开发构建可以替换为其他开发构建或转入正式版本；有序语义版本只能同版重装或向上升级，不能切回开发标签。ACL 保护的文件 journal 是停服期间的状态源；新旧服务恢复后把任务终态镜像到 SQLite，再依次写入审计和持久事件。

## 节点协议与任务流

1. 有全局管理权限的交互式管理员填写节点显示名称并创建短期一次性注册令牌；Control 在消费令牌时以该名称建立节点，不使用 Agent 主机名覆盖它。旧 API 创建的无标签令牌仍回退到 Agent 报告的名称。
2. Windows 和 Linux 新节点统一使用旁置 Agent 文件布局：无子命令入口读取二进制同目录的 `agent.yaml` 并以前台便携模式运行；`install --config` 复用同一注册逻辑后注册为 systemd 或 Windows Service，使用平台专用低权限账号、故障重启和开机自启。缺少长期凭据时通过 HTTPS 交换节点 ID 和长期凭据，将凭据原子写回受限配置，并移除一次性令牌。同机回环或可信隔离局域网可以显式配置指向 `localhost`、回环、私网或链路本地 IP 的 HTTP；公网 HTTP 和明文 DNS 主机名会被拒绝。显式 `run --config` 仍可交给部署者选择的外部进程管理器；两种模式都不需要额外安装脚本层。
3. Agent 主动建立带 Bearer 节点凭据的 WSS；上述明文局域网模式相应使用 WS。连接建立后发送协议版本、平台、能力、实例根、心跳和指标。
4. 用户写操作先在 SQLite 立即事务中创建任务，再通过连接派发；节点离线时任务保持排队。
5. Agent 在执行前原子持久化任务 ID 以及实例、类型和 payload 的指纹。同一 ID 的重复任务返回已有状态/结果，不重复副作用；同一 ID 携带不同工作会被拒绝。Agent 重启会把未完成任务记为失败，而不是猜测性重跑。
6. Agent 只有在任务领取和对应状态已成功写入本地 journal 后才执行或上报；终态写入失败会保留原有持久状态并按退避重试，未持久化的终态绝不发送。Control 持久化结果、成功应用资源/失败/取消投影并发布事件后发送 `job_ack`。只有持久化、投影、持久事件发布或 ACK 写入等明确可重试失败会中止确认并关闭会话，以便重连重放；未知旧任务、授权或协议校验等永久拒绝不得关闭节点会话，避免单条陈旧 journal 记录阻断心跳及其他任务。已经落库的不可变终态与 Agent 重放不一致同样属于永久语义冲突，Control 不确认、不覆盖该任务，只写入有界去重的审计和通知并保持会话在线。Agent 只在收到确认后删除终态日志，确认丢失时同样重放到中心成功确认。
7. Control 发布过滤并脱敏的浏览器事件；浏览器以每页 500 条、单连接最多 5000 条按事件游标补齐遗漏。单 Control 进程把持久事件的提交与进程内广播串行化；`event_log.id` 使用不会因清理复用的 `AUTOINCREMENT` 游标。持久事件 ID 出现缺口时立即从 SQLite 补齐，另每 2 秒检查丢失尾部；瞬时日志和指标允许对慢客户端丢弃。

管理员删除节点时，Control 在一个立即事务内删除节点及其中心端实例、运行环境、游戏安装、地图、任务、指标、通知和资源权限绑定，然后断开 Agent 并使在线浏览器授权快照失效。删除不向 Agent 派发文件或进程操作，因此不会删除节点主机上的游戏文件，也不能替代管理员先停止游戏进程；不可编辑审计和按期保留的事件历史继续保留。

任务状态固定为 `queued → dispatched → running → succeeded|failed|canceled`。Control 数据库和 Agent 都限制每个实例同时只有一个写操作，并为每个物理游戏安装增加独立写锁；A2S/玩家、目录列表、文件读取以及日志/备份分块不占实例写锁。SteamCMD 并发由每个 Agent 的 `steamcmd_concurrency`（1–4，默认 1）控制，切换活动 SteamCMD 时还会拒绝与安装维护任务并发。系统发起的 A2S、目录发现和更新检查，以及玩家页读取时自动触发的 RCON 玩家刷新，属于后台只读任务；即使旧记录曾保存浏览器用户为发起人，`rcon.players` 也始终按后台探测分类。玩家列表接口用 `loading/refreshing/ready/failed` 明确区分异步探测状态，玩家页直接轮询该资源接口，因此拥有 `players.read` 即可及时取得结果，不依赖 `jobs.read`。任务 API 可在 SQL 应用数量上限前包含、排除或仅查询这些记录，任务中心据此默认展示重要操作并提供独立后台探测视图，实例“最近操作”同样排除它们，不改变任务的持久终态、资源投影或 Agent 确认流程。受限账号的任务资源过滤同样在 SQL 数量上限前完成，避免其他资源的高频任务挤掉有权记录。地图导入、校验和删除虽然在节点共享地图库执行，但若权限来自实例绑定，Control 会在任务上持久化 `scope_instance_id`，并以它统一授权任务列表、详情、取消、通知和实时事件；该字段不作为 Agent 执行目标。没有该字段的旧地图任务只允许原发起人通过当前同节点实例权限查看或操作。完成投影的成功/取消后台探测保留 1 小时，失败探测保留 24 小时；用户任务和仍带 `_projection_pending` 的终态不受该清理策略影响。

协议按主次版本兼容：主版本必须一致，Control 接受当前次版本和前一个次版本。能力协商用于 `runtime_environment_v1`、`game_installation_delete_v1`、`discover_instances`、`purge_quarantine`、`map_library_v1`、`map_hot_reload_v1` 等增量命令，避免把新任务派给旧 Agent；即使节点在任务排队后降级重连，相关任务也会留在队列等待兼容 Agent。`job_ack` 是可忽略的增量消息；不认识它的前一版本 Agent 会保持既有行为而不会断开连接。

Control 在持久化或发布前校验所有 Agent 入站消息。心跳最多 256 KiB/512 个实例；指标最多 512 KiB/512 个样本，同一批不得重复实例，数值范围与“已用不超过总量”关系必须成立；日志只接受 `stdout/stderr`，单行 1–16 KiB、消息最多 128 KiB。指标和日志时间戳只接受过去 15 分钟到未来 2 分钟。所有消息中的实例 ID 必须属于当前 WSS 节点；`job_update` 还必须匹配任务节点/实例，结果中任意嵌套 `instance_id` 都不能跨实例。任务消息文本最多 2 KiB；Agent 按完整 UTF-8 字符边界截断，并在启动时修复旧 journal 中已经超限的文本，避免无法确认的终态形成永久重放。归属校验按 Agent 连接缓存 2 秒且最多 1024 项；同一实例的并发 miss 合并为一次 SQLite 查询，未知/跨节点 ID 不做负缓存，正常实例删除会立即逐连接清除。运行进度消息最多 32 KiB、终态最多 12 MiB，重复进度丢弃、变化进度按每任务最快 250 ms 合并，终态不节流。

## 实例生命周期

- 新建实例时 Control 在 SQLite 立即事务中读取节点全部预留端口，由 Go 从 27015–27999 选择候选并写入预留。`instance_port_reservations` 以 `(node_id, port)` 为唯一键，把游戏端口与非零 SourceTV 端口放进同一互斥空间，实例删除时级联释放。停止实例修改游戏端口时，Control 继续保留旧端口并在 `pending_instance_port_changes` 中预留新端口；Agent 成功更新本地启动记录后才原子切换正式预留，失败或取消释放待变更预留。创建和待变更表之间由双向触发器阻止交叉冲突。并发创建、显式跨列冲突、运行中修改和池耗尽不会静默改号，而是返回 `409 conflict`。Agent 仍在执行配置时拒绝活动进程的端口变化，并在启动前检查节点上的实际端口占用。
- 节点、运行环境、游戏安装和实例形成 `node → environment → game_installation → instance` 关系。每个节点只有一套活动 SteamCMD；游戏安装登记节点绝对路径并最多绑定一个实例。绝对安装目录本身成为实例文件访问边界，目录内操作仍使用句柄相对访问、no-follow 与重解析点检查。添加现有目录确认平台对应的 `srcds` 普通文件存在，并在游戏根及已有配置目录做可回收的安全临时写入探测；Steam 完整校验仍是独立手动任务。结构正确但服务账号不可写的路径仍保存在 Agent 本地无秘密运行环境登记中，任务以明确的本机权限修复提示失败。节点管理员重新执行幂等 `install --config` 时，安装器以本机提权上下文重新验证这些精确游戏根并批量交给固定低权限服务账号；远程任务不能触发该权限变化。创建实例可以选择该节点已就绪且未绑定的安装；也可提交绝对路径，由 Control 创建 `pending` 临时安装并只允许同一次请求精确绑定该记录，随后 Agent 检测 `srcds`、登记安装并保存启动资料。调用者不能用后一通道绑定其他 pending 安装。未绑定安装可选择只删除 Control/Agent 登记并保留文件，或永久删除登记目录的全部文件；后一模式在 Agent 再次匹配安装 ID 与绝对路径，拒绝文件系统根、Agent 管理根和任何链接/重解析点后才隔离并删除目录。两种删除都作为持久任务与同安装的其他写操作串行化，并由 `game_installation_delete_v1` 能力门控。迁移会删除旧的根索引加相对路径记录及其绑定实例；实例创建后保持停止。
- Agent 安装和启动不扫描 SteamCMD 或游戏目录。只有进入运行环境或点击刷新才排队 `environment.inspect`，检查已知系统依赖、活动 SteamCMD 和 Agent 本地登记的游戏路径。
- 节点发现 API 将 `discover_instances` 作为节点级持久任务排队，只对配置的实例根做有界广度优先只读扫描：深度 1–8（默认 6）、最多读取 1–20000 个目录条目（默认 5000）、最多返回 200 个候选；`.quarantine`、`.staging`、回收站和系统卷目录被排除，符号链接/重解析点不会跟随。结果只包含根索引和相对路径，不暴露任意主机路径。
- 发现结果中的 `root_index + path` 只用于描述旧的受限扫描候选；新建和导入接口不再接受根索引，必须显式提交节点绝对路径。导入会再次验证活动进程的可执行路径。发现为外部运行中的实例以 `external` 状态只读接入；该状态代表所有权边界，即使外部进程随后退出也保持到显式受控重启。受控重启可提交一次性的 `existing_rcon_password`：中心加密持久化且只在派发时解封，Agent 用它向旧进程发送 `quit` 并短暂等待；未提供或优雅退出失败时，Agent 只有在重新匹配 PID 与实例内可执行路径后才强制结束进程树。随后 Agent 使用面板当前启动资料启动受管进程；旧密码不会成为实例秘密，后续 RCON 使用管理员单独保存且与 `server.cfg` 一致的凭据。
- Windows 以 `CREATE_SUSPENDED` 创建游戏进程，成功配置并加入带 `KILL_ON_JOB_CLOSE` 的 Job Object 后才恢复执行；任一步失败都会终止挂起进程并关闭句柄，因此子进程没有加入 Job 前的逃逸窗口。每个 Windows SRCDS 同时获得隐藏的新控制台及由 Windows 初始化的控制台输入句柄，标准输出和错误仍重定向到实例日志，避免后台 Agent 把空标准输入映射为 `NUL` 后触发引擎控制台错误；若引擎仍输出固定的 `GetNumberOfConsoleInputEvents` 失败，Agent 会立即把实例标为降级并让启动任务报告明确原因，而不是等待网络健康检查超时。Linux 使用进程组。启动/重启任务不能覆盖实例登记的可执行文件、参数或环境；Agent 只接受实例根中的平台固定 `srcds` 文件名，重新校验启动参数，并以受限环境变量启动进程，将 `HOME`/`USERPROFILE` 指向实例目录。可选 `-tickrate` 参数在 Control 和 Agent 两端限制为唯一的 1–1000 整数，仍作为参数数组传递。A2S/RCON 只在 Agent 当前回环、主机名解析出的回环/已分配地址及当前已分配网卡地址内探测，以兼容绑定具体接口或绑定 Linux 主机名回环（如 `127.0.1.1`）的 SRCDS；启动健康检查并发查找实际响应 A2S 的安全本机地址，再只在同一地址检查 RCON，避免多网卡或 IPv6 地址耗尽探测超时。SourceMod/Metamod 安装验证及 SourceMod 插件回退验证也使用该 A2S 地址发送唯一一次只读 RCON 命令，不再按网卡顺序消耗超时。停服先向这些本机地址上的 RCON 发送 `quit`，超时后只终止 Agent 追踪且身份匹配的进程树。
- Agent 将“管理员期望运行”与 `auto_restart` 崩溃策略分别持久化。显式 `start/restart` 记录运行意图，显式 `stop` 记录停止意图；`restart` 只允许 `running`、`degraded` 或受控接管前的 `external` 状态，停止状态必须使用 `start`。Agent 重启时只恢复两者都启用的受管实例，并重新校验路径、启动参数和端口。恢复校验/启动失败会报告 `degraded`，显式停止的实例不会因 Agent 重启被意外拉起。该机制完全在节点内执行，因此 Control 短时离线不会停止现有游戏服。
- 删除 `mode=detach` 只解除管理；`mode=quarantine` 先要求实例停止，再把目录移动到对应根的 `.quarantine/<instance-id>-<UTC timestamp>`。

隔离删除成功时，Control 在删除实例行前保存不含绝对路径的隔离墓碑（节点、根索引、严格 basename、原实例 ID/名称、隔离时间和七天到期时间）。七天是硬性最短保留期，并且 Control 的保留计时不会早于收到 Agent 结果的时间，断线后的旧任务重放只能延长而不能缩短保留期。Agent 不执行隔离区后台永久删除；到期仅表示可申请清理。管理员必须通过交互式会话创建一个只返回一次、5 分钟有效、以哈希存储并绑定当前用户与条目的确认令牌，再在同一事务中消费令牌并创建节点级 `purge_quarantine` 任务。Control 拒绝未到期、已清理或错配令牌，任务 ID 及两阶段请求进入审计/任务历史。

Agent 永久清理时只接收 Control 墓碑中的 `root_index + entry_name`，不接收任意路径或 Shell 命令。它从名称时间戳再次验证七天保留期，只允许配置根中 `.quarantine` 的直属普通目录，并在删除前重新解析边界、遍历拒绝符号链接/Windows 重解析点。目标已经不存在时任务幂等成功；任何未知名称、根索引、路径逃逸或链接都失败且不会改动其他目录。

## 配置、文件和模组

包源下载的单源总时限为 10 分钟，以容纳持续有进度的慢速网络；连接建立后的任意读取如果连续 30 秒收不到数据，Agent 会立即中止当前请求并尝试下一个官方回退或镜像源，不再等待单源总时限耗尽。

安装官方 SourceMod/Metamod 时，Agent 并行请求固定的 `api.country.is` 与 `api.ip.sb`，由服务端根据请求的公网出口 IP 返回两位国家代码。只有两个结果都有效、相同且不是 `CN` 时保留 GitHub 和 AlliedModders 官方源优先；任一结果为 `CN`、查询失败、格式异常或两个结果冲突时，均把全部已通过 GitHub 官方摘要保护的镜像提前，再回退官方源。查询结果不持久化，也不进入任务或日志。

SourceMod 安装具有强制的 Metamod:Source 前置依赖。Control 只在该实例已有成功投影且启用的 Metamod 组件时接受 SourceMod 安装请求；Agent 在解析或下载 SourceMod 包之前还会以 no-follow 方式检查 `metamod.vdf` 和当前平台的 Metamod 加载二进制，防止中心记录漂移或调用绕过。操作内部超时属于任务失败并保留具体错误，只有显式取消上下文才产生 `canceled` 终态。

结构化配置只保存面板启动资料，不生成 `left4dead2/cfg/l4d2panel.cfg`。Agent 在启动参数末尾通过固定的 `+exec server.cfg` 显式加载管理员维护的 `left4dead2/cfg/server.cfg`；文件名不可由调用者替换，密码也不会进入进程参数，`server.cfg` 是唯一游戏配置源。难度作为受控的 `+z_difficulty` 参数传递。RCON 密码是可留空的中心端加密连接凭据，必须由管理员在 `server.cfg` 中配置相同值；留空不阻止启动，进程成功时实例仍为 `running`，但 RCON 查询、广播和优雅停服不可用。API 只投影 `rcon_configured` 布尔值，不回读密码。结构化配置只保留当前值，不保存历史版本；客户端在 PATCH JSON 正文中提交 `version` 作为内部修订号做乐观并发控制。

新 Agent 通过 `server_cfg_native` 能力声明这一启动契约。涉及创建、配置、启动、重启、恢复或游戏文件维护的任务不会派发给缺少该能力的旧 Agent，而是保持排队，避免节点回滚后重新生成或注入旧配置文件。

目录和正文读取都通过 Agent 只读任务刷新缓存；首次尚无结果时文件 API 明确返回 `loading`，浏览器持续轮询而不把未完成读取显示为空目录。Agent 还以相同的 no-follow 句柄操作检测 `left4dead2/addons/sourcemod/configs/admins_simple.ini`，只有目标是实际存在的普通文件时才返回动态快捷入口。

文件 API 只开放 `cfg`、`addons`、`logs` 和它们在 `left4dead2` 下的对应目录。目录列表需要实例 `read`，读取正文和所有写操作需要 `operate`。浏览器正文写入上限 8 MiB；写入可用正文 `expected_sha256`，删除可用 `If-Match`，防止覆盖并发修改。`package_scan_v1` 一键纳管任务不接受路径参数，只枚举 `left4dead2/addons` 的直属 `.vpk` 普通文件，跳过子目录和 `l4d2panel-map-*` 地图库保留名称；扫描上限 1000 个候选，并通过实例根句柄相对的 no-follow 打开完成 512 MiB、v1/v2 结构、边界、逐项 CRC、大小和 SHA-256 校验。Control 以实例写任务串行投影尚未按路径登记的有效文件，无效文件只形成有界任务报告，Agent 不改写或删除扫描对象。模组安装入口另有能力门控的 `qq_flash_vpk_packages_v2` 导入：Control 只接收并加密保存规范化的 `https://qfile.qq.com/q/...` 分享链接，目标不可由调用者选择；Agent 先有界列出整个分享，从固定 `qfile.qq.com` 元数据接口逐文件换取固定 `multimedia.qfile.qq.com` 短期地址，并把正文暂存于 Agent 数据目录，文件正文不经过 Control。该工作流最多接受 10000 个分享/归档项、2 GiB 下载和展开数据、1000 个 VPK、3 层 ZIP/7Z/RAR 嵌套，并保留至少 1 GiB 空闲；加密归档、链接、特殊路径、超限压缩比和任意非 VPK 普通文件都会让整次导入在写入实例前失败。每个 VPK 继续执行 512 MiB 上限、v1/v2 结构、边界和逐项 CRC 校验，全部通过后才以不覆盖的唯一名称提交到固定 `left4dead2/addons`，并逐个投影为可启停、可删除的包记录。Agent 的通用安全分段下载器默认最多 4 路，QQ 闪传固定请求最多 8 路且所有任务共享节点级 8 路硬上限；大文件先用单字节 Range 探测，支持时严格校验每段 `206/Content-Range/Content-Length` 并写入同一个预分配 no-follow 临时文件，不支持或 `If-Range` 失效时从零回退单流。通用来源优先使用强 ETag，否则使用 Last-Modified 作为 `If-Range` 身份；QQ CDN 对大区间返回的 Last-Modified 会随区间偏移变化，因此 QQ 专用调用以元数据中的不可变 `physical_id` 绑定对象，没有强 ETag 时不把该时间头当作整文件身份。包删除使用独立的 `package_delete_v1` 能力门控，只接受面板登记的标准 VPK/SMX 单文件路径并在删除前复核记录的 SHA-256；成功后删除对应包记录和历史版本元数据。普通文件 API 继续拒绝包含 `.vpk` 在内的全部可执行或加载关键格式。

包安装支持最大 100 MiB 的网站上传、HTTPS URL、Workshop 项目/合集、VPK、SMX/原生 SourceMod 扩展，以及操作系统匹配的 AlliedModders SourceMod/Metamod 稳定包。上传正文在中心任务中加密，Agent 只在 `data_dir/staging` 临时落盘并在完成后清理；URL 下载使用 HTTPS、大小/重定向/地址边界和归档限制。外部 ZIP/tar 解包先写入拒绝链接的 staging；最终文件、Workshop 载荷、单文件组件和启停移动均通过 Linux dirfd 或 Windows NT `RootDirectory` 提交，提交前验证声明大小及可用的 SHA-256，no-replace 与覆盖语义均不跟随目标链接。直接上传、URL 安装及 VPK 回退验证 VPK v1/v2 结构：目录树最多 64 MiB/100000 项，只接受安全 UTF-8 相对路径和单文件内嵌数据，检查节区边界、终止符、重复/重叠条目及每项 CRC32；v2 签名区只校验长度结构，不把它当作来源真实性证明。Workshop 下载载荷不解析 VPK 签名、结构、CRC、内部路径或版本，也不施加 512 MiB VPK 校验上限，只要求 SteamCMD 固定目录中的候选是非链接普通文件并在复制前后核对 SHA-256。地图、普通模组、Workshop 和历史版本之间的 VPK 虚拟路径重叠不由面板阻断，最终覆盖关系由 Source 实际加载顺序决定；各入口原有的结构、CRC、路径和大小检查不因此放松。Workshop 合集解析最多 200 个子项目并逐项报告匿名 SteamCMD 下载失败；Agent 兼容 SteamCMD 的直接 `steamapps` 与 Linux `$HOME/Steam/steamapps` 固定布局，并把 Valve 旧式 Workshop depot 返回的 `*_legacy.bin` 作为候选载荷。Agent 通过 Valve 公开详情接口取得每个项目的标题，删除标题中的点号、路径/Windows 保留字符和控制字符并限制长度；单文件以 `<项目 ID>_<清洗后标题>.vpk` 安装，一个项目含多个候选文件时按固定排序追加 `_1`、`_2` 后缀。官方 SourceMod/Metamod 先按稳定页给出的 GitHub Releases 地址下载，失败时按同一版本和文件名回退到项目自己的 `sm.alliedmods.net`/`mms.alliedmods.net`。如果 Agent 同时从 GitHub 官方发布 API 取得了该资产的 SHA-256 和大小，官方地址仍失败后会依次尝试固定的 `gh-proxy.com`、`ghfast.top`、`ghproxy.net` 镜像；每个候选都必须与官方摘要和大小完全一致，摘要不可用时不启用第三方镜像。安装前创建检查点，必要时停服，安装后启动并用 `meta version`/`sm version` 验证，失败恢复检查点和原运行状态。第三方原生包需要显式风险确认。整套组件在写入前还把目标限制为 Metamod 的 `addons/metamod/**`、`addons/metamod.vdf`、`addons/metamod_x64.vdf`，或 SourceMod 的 `addons/sourcemod/**`、`cfg/sourcemod/**`、`addons/metamod/sourcemod.vdf`，并拒绝覆盖其他包已登记的文件。SourceMod 的 `addons/sourcemod/configs/admins_simple.ini` 若已存在则原样保留；若首次安装时不存在则安装包可以创建，但该文件永不进入组件删除清单，后续更新和卸载均保留。

`smart_plugin_archive_v1` 把原来的单个 SMX 与原生扩展上传入口合并为智能识别工作流。Control 加密持久化不超过 100 MiB 的本机 ZIP/7Z/单卷 RAR，Agent 在数据目录暂存并以 5000 文件、单文件 512 MiB、总展开 2 GiB 的上限安全解压。一个集合外层目录会被剥离，其下每个非空顶层目录投影为独立智能插件；完整 `left4dead2` 路径直接保留，散放的 SMX/SP/INC/NUT 和可判定 TXT/DLL/SO 自动映射，歧义 TXT/CFG/裸原生库必须在预览中选择受限目标。可写根固定为 `left4dead2/addons/**`、`left4dead2/cfg/**`、`left4dead2/modes/**`、`left4dead2/scripts/vscripts/**`。最终安装重新展开并核对含现有文件状态和面板路径所有权的预览指纹；未登记磁盘文件需要明确接管，其他包已登记路径不能接管。选中的所有文件先建立回滚副本再写入，任一文件失败恢复整批。每个顶层插件保存 Agent 本地清单和接管基线；同名智能插件可在目标路径集合不变且当前文件未漂移时更新。`smart_plugin_file_edit_v1` 允许详情页编辑清单内非入口的 CFG/TXT/SP/INC/NUT UTF-8 文本；Agent 复核读取时哈希，原子写入后同步本地逐文件和聚合哈希，Control 只按 Agent 的受限结果投影数据库。SourceMod 首次加载后可能自动生成 `left4dead2/cfg/sourcemod/*.cfg`；实例成功启动或停止时，Agent 只把与已启用智能插件直属 `plugins/**/<同名>.smx` 唯一对应、尚未归属其他清单且为不超过 8 MiB 的无链接 UTF-8 普通文件加入该插件清单，并由 Control 再按当前数据库入口所有权复核后投影。多个智能插件存在同名 SMX、CFG 大小写重名、文件已登记或无法安全读取时均不自动认领；删除智能插件前还会执行同一检测，因此已安全认领的自动配置会按哈希校验后一并删除。停用只移动 SMX/DLL/SO/VDF/AUTOLOAD 加载入口，静态配置、翻译、源码和数据文件保留；停用和删除要求实例停止，运行中安装或启用需要两次明确确认并标记需要重启。

启动成功后的自动配置检测先立即执行一次；仍有唯一匹配且未纳管的候选时，Agent 每 3 秒复检一次、最多复检 5 次，并在候选全部纳管或任务取消时提前结束。

整套 SourceMod/Metamod 安装成功后，Control 以单一组件记录持久化 Agent 返回的逐文件路径、大小和 SHA-256；更新复用同一记录，并保留仍未漂移的旧版文件清单，已被管理员修改的旧文件则退出组件纳管而留在原位。`component_package_delete_v1` 卸载要求实例停止、有非空清单且 Agent 在线支持该能力。Agent 先逐项验证固定组件目录、普通文件、大小和 SHA-256，再创建实例检查点，只删除该组件清单中的文件；中途失败恢复检查点。其他智能插件/手工文件和 `admins_simple.ini` 不在清单内，因此不被删除。SourceMod 记录仍存在时 Control 拒绝卸载 Metamod。旧版无清单组件必须先重装一次，不能退化为递归删目录。

覆盖非 Workshop 的单文件 VPK 或 SourceMod SMX 时，Agent 会在自己的 `data_dir/package-versions` 中先保存旧文件，并为每个目标保留最近 3 个历史快照；中心保存版本元数据和用于回退派发的 Agent 本地不透明相对定位符，专用版本列表和通用任务响应都不返回该定位符。Workshop 当前按项目/合集首次安装，不进入此版本历史。版本快照、当前文件校验、替换、回退和失败恢复都使用 no-follow 文件句柄；Linux 启停移动在提交后比较目标 `dev+inode`，Windows 直接按源句柄重命名。停用 VPK/SMX 只允许在实例状态为 `stopped` 时排队，Control 在写事务中校验状态，Agent 在移动文件前按本地进程状态再次拒绝运行中操作；启用仍可在运行中提交并返回需要重启的结果。版本回退要求包已启用且目标路径未变化，运行中的实例会先停服；Agent 校验所选快照与 SHA-256、原子替换并恢复原运行状态，SMX 还会通过 RCON 检查 SourceMod 响应。失败时恢复被替换文件和先前运行状态。回退会消费所选快照，同时把刚被替换的当前文件保存为新的历史版本。整套 SourceMod/Metamod 与第三方原生扩展也不属于此单文件版本历史。

## 地图对象库

地图导入是 `map_library_v1` 能力门控的固定节点任务，不开放任意解压或 Shell；显式路径编码和选择性展开由 `map_import_v2` 协商，Control 对旧 Agent 只派发兼容的 `auto` 请求。Control 只加密保存 QQ 闪传链接直到派发；Agent 先完整枚举最多 100000 个文件和 20 GiB 下载声明，再写入 `<data_dir>/maps/staging/<job-id>`。归档按魔数识别，使用纯 Go ZIP、7Z 和 RAR 读取器递归最多三层；只把 VPK、外部 VPK 数据块和嵌套归档写入磁盘，其他条目只读取有界签名后跳过。全部条目仍受 20 GiB 单项、100:1 压缩比、数量、路径和类型检查，选中输入另受 40 GiB 实际展开/VPK 虚拟内容与 5 GiB 磁盘预留限制；7Z 的总声明展开量和整体压缩比仍在打开条目前校验。加密、分卷、链接、特殊文件、绝对路径、`..`、设备名、ADS 和重解析点均拒绝。成功、失败或取消都会清理整棵 staging。地图导入错误还会以 `preflight/staging/metadata/download/archive/vpk/publish` 阶段、稳定错误码、可重试标志和处理建议写入任务结果，且仍遵守 `jobs.details` 正文权限。

同一链接内 VPK 按规范化容器路径稳定排序后合并，不区分大小写的重复虚拟路径由后者覆盖；固定去除 `.vtf/.mp3/.wav/.vmf/.vmx` 和无扩展名文件。输入支持 VPK v1/v2、`*_dir.vpk` 外部数据块并逐项校验 CRC 和边界；输出是确定性 VPK v1，按完整文件边界拆成不超过 1.5 GiB 的部件。`auto` 路径编码优先把 ZIP/VPK 目录组件解释为 UTF-8，仅对无效 UTF-8 使用跨平台固定的 GBK 后备解码；管理员也可显式选择 `utf8`、`gbk` 或 `big5`，用于覆盖来源错误设置的 ZIP UTF-8 标志。无法无损解码的组件仍拒绝，成功转码会形成持久警告，并在转成 UTF-8 后重新执行完整路径安全与不区分大小写的重复检查。该兼容不会放宽普通模组的 UTF-8 VPK 校验。VPK 内部名称只存在于 Source 虚拟文件系统，不按同名路径解压到宿主机，因此在继续拒绝绝对路径、`..`、分隔符和 ADS 冒号的同时，保留尾随点号、尾随空格和 Windows 设备名；所有真实 staging、归档解压及最终对象路径仍执行 Windows 安全文件名规则。每个 `maps/*.bsp` 流式查找 `stringtable_dictionary.dct`，缺失只形成持久警告。Agent 还以有界 KeyValues 解析器读取 `missions/*.txt`，并从 UTF-8/UTF-16 的 `resource/*_schinese.txt`、繁中、英文及其他语言文件依次解析显示令牌；按实际 BSP 覆盖率确定主 mission 后，投影战役名称、作者、版本、描述、模式及有序章节，并持久报告缺失引用或未引用 BSP。mission 海报引用只可解析到同一 VPK 的固定 `materials` 虚拟路径；相应 VMT/VTF 经过 CRC、大小、维度、帧数和格式限制后转换成至多 1280×720、1 MiB 的 JPEG 交给 Control，原始纹理仍按清理规则删除。节点只发布处理后对象和供终态重放的受限 JPEG sidecar，下载、原 VPK、解压目录和其他输出临时文件都随 staging 删除。

`map_roots` 的第一项始终是 `<data_dir>/maps`，管理员可为其他文件系统卷配置一个绝对根；同卷重复根在 Agent 启动时拒绝。对象按 SHA-256 保存并在同卷复用，跨卷先从健康副本原子复制和校验；未配置实例所在卷或文件系统不支持硬链接时明确失败。Linux 使用 handle-relative `linkat`，Windows 使用带 `RootDirectory` 的 NT 硬链接操作，目标固定为实例 `left4dead2/addons/workshop/l4d2panel-map-<map-id>-<part>.vpk`，L4D2 会从该 Workshop 目录识别 VPK。普通文件、QQ 模组和包接口不能触碰该保留前缀。节点注册表原子记录对象、副本、实例链接及预期关系；Linux 幂等服务安装只在注册路径全集与 inode 实际链接计数完全相等时豁免这些文件，未知硬链接仍拒绝，符号链接/Junction 规则不放松。

地图导入、发布、分配、取消、校验和删除共享节点地图锁；分配还取得实例写锁但不要求停服，也不占 SteamCMD 槽。`map_hot_reload_v1` Agent 在完成并复核硬链接后，若实例正在运行且已配置 RCON，会把游戏聊天命令 `!update_vpk` 固定转换为服务端命令 `sm_update_vpk` 并立即刷新 VPK；命令、可执行文件和参数均不接受调用者输入。未配置 RCON 时分配仍成功，结果记录跳过原因，界面在提交前警告管理员改为在游戏内手动输入 `!update_vpk`；停止实例会在下次启动时加载。新分配不再要求重启。取消由两端要求真实停止。删除必须无分配并使用一次性五分钟确认令牌；未知链接、校验不一致或部分失败保留逻辑记录为异常以便重试。校验在有健康来源时修复副本，无健康来源时标记 broken 并禁止新分配。支持 `map_library_v1` 的 Agent 每次重连完成能力协商后，Control 会为该节点每张地图补充一个去重的 `map.inventory` 校验任务，以发现本地注册表丢失和离线期间的对象异常。

相关格式和上游说明：[Valve VPK](https://developer.valvesoftware.com/wiki/VPK)、[AlliedModders SourceMod 安装](https://wiki.alliedmods.net/Installing_SourceMod)、[SteamDB App 222860](https://steamdb.info/app/222860/config/)。

## 备份、日志和传输

完整数据不经过中心长期存储：

```text
<instance-root>/<instance>/
  left4dead2/cfg/server.cfg
  left4dead2/addons/...

<instance-root>/.quarantine/          # 已解除管理且等待清理的实例目录

<agent-data-dir>/
  registry.json                       # Agent 实例注册资料
  .agent.lock                         # data_dir 单进程独占锁
  journal/v2/<job-id-sha256>.json     # 逐任务原子幂等日志
  backups/<instance-id>/*.{zip,7z}    # 节点本地实例/插件备份
  package-versions/...                # 节点本地 VPK/SMX 历史快照
  maps/registry.json                  # 地图对象、副本与实例链接证明
  maps/objects/...                    # 默认卷的只读内容寻址 VPK
  maps/staging/...                    # 导入任务临时目录，任务终态清理
  logs/instances/<instance-id>.log*   # 当前日志及最多 19 个轮转段
  staging/                            # 有边界的临时下载/恢复区
```

常规手动/计划备份使用 v4 精简清单，只包含 `cfg`、`left4dead2/cfg`、两套 `addons/sourcemod/configs` 配置树和 Control 中仍有记录且来源不是本机扫描的精确包文件；Control 在每次排队时动态发送这些相对路径，Agent 再次规范化并要求文件实际存在。地图 VPK、本机手工文件、日志、SourceMod 运行数据、游戏本体和可能含密码的 `left4dead2/cfg/l4d2panel.cfg` 不入包。清单仍记录平台、实例 ID、无秘密启动快照、地图 ID/聚合 SHA-256/名称/BSP 引用以及逐文件 SHA-256。精简恢复精确同步配置树并覆盖清单中的包文件，但不删除目标实例上的其他 addons。恢复、更新和卸载等破坏性事务前由 Agent 内部创建的 v3 检查点仍包含四个完整可移植目录，以支撑失败回滚并按最多 3 个保留。默认新实例有每日 04:00 计划，保留 7 个日备份、4 个周备份；首次每日备份会按 ISO 周复制一份周备份。Control 在同一事务中投影日备份及新周副本，并按 Agent 返回的已清理路径删除过期元数据，因此日、周和检查点都能从备份列表下载或通过 `backups.delete` 排队永久删除；重放同一终态不会产生重复记录。

插件备份是另一个仅由用户主动触发的 `plugin.backup` 写任务，要求 Agent 声明 `plugin_backup_7z_v1`。Control 从 `smart/plugin/smx/native` 插件记录及逐文件清单生成固定 payload，调用方只选择 `preserve` 或 `compact`，不能提交路径。Agent 对最多 1000 个插件、5000 个文件、单文件 512 MiB、总量 2 GiB 的清单逐项复核受限目标、no-follow 普通文件、大小和 SHA-256，并用内置 LZMA2 写入 7z；停用智能插件的入口从 Agent 私有状态目录读取，但归档名仍使用其正常目标路径。每个插件占一个经清理且消歧的顶层文件夹；`preserve` 保留 `left4dead2/...`，`compact` 去掉 `left4dead2/addons/sourcemod/` 或 `left4dead2/` 根前缀，保留 `plugins/gamedata/configs/cfg` 等结构。此类记录复用备份列表、分块下载与永久删除，但不进入定时保留轮换，也不能作为 restore 输入。

恢复必须停服并先创建完整检查点。Agent 在隔离 staging 中验证格式版本、字段边界、条目边界、清单完整性和逐文件 SHA-256；解包、最终 overlay、extras 文件/空目录删除以及旧版遗留配置的回滚均使用句柄相对操作。带启动快照的归档恢复时，端口、回环地址及中心端 RCON 凭据继续取目标实例；带地图引用的归档先按地图 ID、再按聚合哈希匹配节点已有对象并恢复分配，缺失项只返回警告。若来源启动地图属于缺失对象，则保留目标实例当前启动地图。跨平台时 Agent 先按 Metamod → SourceMod 的依赖顺序安装、启动验证目标平台原生组件，再跳过来源平台二进制并应用可移植数据。成功结果把无秘密快照返回 Control 并做幂等投影。目录同步不是文件系统原子交换；任一步失败时 Agent 使用含地图引用的恢复前检查点，并同时恢复原 registry 与旧版遗留配置。恢复任务中的 Agent 本地归档定位符以部署主密钥加密持久化并只在派发时解封，任务/API/事件视图不返回备份、周副本或检查点路径。

跨节点恢复使用目标实例上的可续传上传会话。会话初始有效期 24 小时，每个成功分块续期 24 小时；客户端必须顺序发送最大 1 MiB 的 Base64 块、当前 offset、块 SHA-256 和非空幂等键，并通过只受 `backups.restore` 保护的固定分块状态端点等待任务终态及 `received_size` 前进。该端点不借用 `jobs.read`/`jobs.details`，只返回经实例、会话和任务类型复核后的 `job_id/status`；失败或取消时浏览器丢弃该块的旧键，以新键重试同一 offset。Control 只持久化会话元数据，分块正文以部署主密钥加密进入任务且在终态清除；目标 Agent 把正文写入受限 `data_dir/backup-uploads`，逐块验证，末块再核对完整大小/SHA-256并原子完成。浏览器下载优先通过 File System Access API 逐块写盘并进行完整 SHA-256 校验；不支持该 API 时只允许最多 512 MiB 的内存聚合回退。Agent 以节点为单位串行核算上传暂存，默认最多 32 个不同会话。首次可信分块会先以严格 JSON、权限 `0600` 的 sidecar 原子持久化 `upload_id/expected_size/expected_sha256`，续块必须完全匹配；旧版无 sidecar 的 `.part/.zip` 在可信续传升级前按 20 GiB 预约。64 GiB 上限按全部会话预约大小加 sidecar/临时元数据和未知常规文件核算，每次写块前还要求磁盘足以兑现所有未完成预约并额外保留 5 GiB，避免多个大文件各占一部分后全部无法完成。配额检查在创建 sidecar、截断或写入 `.part` 前完成，拒绝不会破坏已经提交的分块。无活动未完成 `.part` 的 Agent 保留期约为 25 小时，完整 `.zip` 为 48 小时；完成包的幂等整包校验和每次 restore 校验都会续租完整会话。完成、续租、删除和清理按 UUID 将数据文件、sidecar 与原子临时文件成组处理，因此覆盖 Control 在失败后续期的 24 小时重试窗口，同时不会形成未计费孤儿。只有 ready、完整且未过期的会话可排队标准 restore；同一键重放原任务，失败或取消会把会话续期 24 小时，使用新键可直接重试而无需重传。任务成功后清理 staging。状态为 `uploading/ready/restoring/succeeded/failed/expired`，API 从不返回目标路径或正文。

浏览器另外持久化文件身份与未确认 chunk/complete/abort 的幂等键，刷新后重选同一文件即可从服务端确认偏移继续。放弃 `uploading/ready/failed` 会话与 chunk/restore 排队共享上传事务锁，活跃、`cancel_pending` 或终态尚未投影的传输会使请求安全冲突；通过检查后，Control 在同一事务把会话标为 `expired` 并创建不占实例游戏写锁的 `backup.upload.abort`。Agent 只按 UUID 调用受信任的成组清理函数，不接受路径或 Shell；对外任务正文和终态仅保留 upload ID。

实例进程日志最多为 100 MiB × 20 段；Agent 启动时及此后每小时删除超过 7 天的已关闭轮转段和停止实例当前段，运行中当前段由轮转器管理。实际保留受“7 天或约 2 GiB，先达到者”限制。清理只处理严格命名的普通文件，不跟随符号链接/重解析点。搜索最多返回 1000 行/384 KiB；日志和备份下载按最多 1 MiB 分块，每块校验 SHA-256。正文在 Control 数据库中仅以部署主密钥加密暂存，并在成功读取后删除，任务的非敏感元数据仍保留。

## 监控、更新与通知

- Agent 默认每 10 秒心跳、每 15 秒发送节点/实例指标。节点 35 秒无心跳会被标记离线。心跳按 UUID 顺序批量锁定实例，并在活跃、取消待确认或终态待投影的写任务期间保留 Control 生命周期状态；屏障清除后的下一次心跳才恢复 Agent 的稳定状态。浏览器事件日志保留 30 天；WebSocket 每页重放 500 条、单连接最多补齐 5000 条，审计事件不使用这项实时事件保留策略。
- 原始指标保留 24 小时；1 分钟聚合保留 30 天；1 小时聚合保留 1 年。指标端点每次最多返回 10000 点。
- Control 每 30 秒只对在线运行实例安排 A2S 探测，用于状态、地图和人数投影；该读探测不占实例写锁。
- Control 不运行周期性 Steam 构建探测，也不解释实例自动维护窗口。游戏安装的检测、校验修复和更新只能通过运行环境 API 手动创建持久化任务。
- 计划任务只允许 `start/stop/restart/backup/broadcast`，并按动作白名单规范化参数；不能持久化游戏安装维护、任意 RCON、URL、文件或包安装正文。计划重启到时直接执行且不会自动广播；需要通知玩家时使用独立的安全广播计划。
- 告警覆盖节点离线、崩溃循环、任务/探测失败、更新可用和磁盘 85%/95%。通知可投递到站内、强制 STARTTLS 的 SMTP，或带 `X-L4D2Panel-Timestamp` 与 `X-L4D2Panel-Signature` 的 HTTPS Webhook；Webhook 禁止代理和重定向，拨号前解析 DNS 且只连接公网地址。外部投递最多 6 次并保留每次结果。
- 公开状态页只查询显式 `public_status=true` 的实例，并只返回名称、状态、地图、模式、人数、运行时间和管理员配置的连接地址。

## 身份、权限与 API 一致性

数据库没有任何用户时，Control 仍可正常启动，并把浏览器引导到 `/setup`。管理员先在中心主机运行 `setup-key issue`；命令生成 32 字节随机值，标准输出只写一次带固定前缀的明文，SQLite 只保存 SHA-256 摘要，初始化前再次签发会原子替换旧摘要。公开状态接口返回是否已签发密钥，并只发放短期 HttpOnly/SameSite=Strict 引导 Cookie及由其派生的独立 CSRF 令牌。提交时还必须携带 `Idempotency-Key`；立即事务内重新检查用户数、以常量时间比较摘要、创建固定 `admin`／`系统管理员` 根所有者和全局绑定并删除摘要，因此并发请求只有一个成功。密钥经 Argon2id 保存为初始密码；成功响应创建普通浏览器会话并永久关闭网页引导入口。

浏览器使用 HttpOnly/Secure/SameSite=Strict Cookie；写请求另外校验 `X-CSRF-Token`。API Token 通过 Bearer 认证，不使用 CSRF，但必须有过期时间，并可限定权限、节点和实例。TOTP 和 API Token 管理只允许交互式浏览器会话；团队账号与角色绑定还要求全局所有者的 `root_security` 权限，所有者 API Token 也不能执行这些操作。新团队账号以 `pending_activation` 创建，服务端生成带固定前缀的 24 小时一次性密码，响应只返回一次且 SQLite 只保存 SHA-256 摘要。首次登录在立即事务中消费摘要并签发最长 10 分钟的 `activation` 会话；该会话只能读取自身状态、注销和提交新密码，不能访问事件流、Token、TOTP 或任何节点/实例资源。设置至少 12 字符的新密码时原子写入 Argon2id 哈希、清理激活状态和旧会话并轮换为普通会话；丢失激活会话只能由全局所有者重新签发，重新签发会使旧密码和旧激活会话失效。

角色绑定可位于全局、节点或实例，并持久化具体权限集合；角色字段仅用于填充所有者/管理员/操作员/观察者预设，后续授权以权限集合为准。匹配绑定取并集且没有显式拒绝，节点绑定覆盖其下实例，实例绑定不能获得普通节点或全局权限；共享地图的 `maps.read/import/verify/delete` 是明确例外，可由实例绑定映射到该实例所在节点，其中实例范围的删除仅限当前账号自己导入的地图，`maps.assign` 仍只作用于绑定实例。所有者 `root_security` 只能存在于不可自定义的全局所有者预设。节点/实例非查看权限分别要求同一绑定包含 `nodes.read`/`instances.read`；已加载的空绑定始终拒绝，只有创建请求真正省略权限字段时才填充预设，显式 `null` 或空数组会被拒绝。其他权限不按名称或等级隐式推出：`players.broadcast` 与允许任意命令的 `rcon.execute` 互相独立；节点发现结果只需 `nodes.discover`，不借用通用任务读取；`jobs.details` 可按已知任务独立读取脱敏正文而不授予列表；`audit.read_global` 可独立读取全局审计；创建 `start/stop/restart/backup/broadcast` 计划同时要求 `schedules.manage` 与目标动作权限；地图分配同时要求经节点或同节点实例绑定获得的 `maps.read` 和目标实例的 `maps.assign`。API Token 的 `read/operate/manage` 是附加上限，最终权限是 Token 上限、Token 资源范围和账号绑定的交集。资源列表、详情、任务、指标、通知、审计和 WebSocket 事件都会再次按具体权限与绑定范围过滤。更改账号密码、禁用账号或替换绑定会撤销其现有会话与 API Token。Control 内的登出、Token 吊销和账号权限变更还会递增授权代际，使在线事件流在通知后的下一帧前重验；每 1 秒数据库重验用于覆盖数据库提交到进程内通知之间的极短调度窗口、到期、进程外数据库变更和代际通知丢失，避免把日志速率放大成身份查询速率。

`Idempotency-Key` 在创建实例、配置更新、文件写/删及实例动作等长操作上避免重试重复执行。重放不仅匹配用户和键，还必须匹配目标资源、动作、正文和版本组成的规范化请求；不同实例、动作、负载或版本会返回 409，迁移前没有请求指纹的旧任务也不会被猜测为等价。HTTP 边界在秘密加密前用部署主密钥计算 HMAC-SHA256 指纹，因此随机加密 nonce 不破坏重试，数据库泄露也不能直接用指纹枚举低熵密码、RCON 命令、签名 URL 或文件正文。内部调度的无秘密任务使用带作用域的 SHA-256 指纹。实例结构化配置使用请求正文的 `version` 防止丢失更新；文件内容使用 SHA-256 前置条件。

已派发或运行的任务收到取消时会先进入带 `cancel_pending` 的 canceled 状态并继续占用实例写锁。Agent 的最终 succeeded/failed/canceled 结果仍可完成投影并原子清除该标记；没有 `cancel_pending` 的普通 canceled 任务不能恢复成成功或失败。

Agent 启动后独占锁定整个 `data_dir`，因此前台进程、系统服务或误启动的第二份进程不能同时读写 registry、journal 和本地状态。任务 journal v2 按任务拆分成以 Job ID SHA-256 命名的有界 JSON 文件，每次领取、进度、终态和 ACK 只原子提交对应文件；旧版 `journal/jobs.json` 在全部条目完成逐项落盘后才改名归档，中断迁移会在下一次启动逐项核对后继续，任何不一致都拒绝启动。

Agent 声明 `terminal_conflict_v1` 时，Control 若发现终态上报与不可变任务终态不一致，会持久化不含结果正文的冲突元数据并返回 `job_conflict`。Agent 在当前连接中隔离该任务、停止重复发送但继续保留本地终态；其他任务和心跳不受影响。只有交互式全局所有者可选择 `keep_control`，该幂等操作写入审计且永不改写既有终态，然后只对完全匹配的上报指纹发送 `job_ack`。若 Agent 已离线，已确认记录会在其下次重放时触发 ACK。

## 当前边界

- 单 Control、单本机 SQLite，不支持控制平面高可用、网络文件系统、Redis、Kubernetes 或共享 Depot。
- 无公开注册、计费、多租户、SSO、S3 和网页系统终端。
- 同一节点的 Agent 和全部 srcds 实例当前共用一个低权限 OS 服务账号；独立目录与 PathGuard 只约束 Agent API，不构成每实例进程沙箱或租户安全边界。相互不信任的实例/原生扩展必须拆分到不同主机或另行配置 OS 级隔离，此项在多租户生产发布前属于阻断项。
- 日志、备份与隔离文件依赖节点磁盘；中心数据库不能替代节点外备份。
- 仓库自动化覆盖单元/集成模拟、跨平台构建、Web UI 和容器构建，但不代表已在每次提交上连接 Valve 服务。发布必须另做真实 SteamCMD、A2S、RCON 和受支持系统验收。
