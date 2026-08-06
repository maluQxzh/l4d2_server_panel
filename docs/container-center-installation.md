# Linux/macOS 容器中心端安装

本文说明如何使用离线 Docker 镜像部署 L4D2 Panel 中心端。中心端始终运行 Linux 容器：Linux 使用 Docker Engine 24+ 与 Docker Compose v2，macOS 使用 Docker Desktop。macOS 只作为中心端宿主机，不支持运行 Agent 或 L4D2 游戏节点。

## 部署包与架构

部署目录需要两个文件：

```text
l4d2panel-center/
├── docker-compose.yml
└── l4d2panel-control-<version>-linux-<arch>.tar.gz
```

镜像包还应附带 `.sha256` 校验文件。发布包中的 `docker-compose.yml` 来自 `deploy/docker-compose.image.yml`，离线镜像带有模板默认使用的 `l4d2panel/control:offline` 标签。

| 宿主机 | 镜像平台 |
| --- | --- |
| x86-64 Linux、Intel Mac | `linux/amd64` |
| ARM64 Linux、Apple Silicon Mac | `linux/arm64` |

确认 Docker 主机平台：

```bash
docker info --format '{{.OSType}}/{{.Architecture}}'
```

生产部署使用原生架构镜像。QEMU 只适合交叉构建和验收，不建议作为中心端长期运行依赖。

## 校验并首次启动

先进入包含 Compose 文件和镜像的永久部署目录，校验发布包：

```bash
sha256sum -c l4d2panel-control-<version>-linux-<arch>.tar.gz.sha256
```

macOS 可使用：

```bash
expected="$(awk '{print $1}' l4d2panel-control-<version>-linux-<arch>.tar.gz.sha256)"
actual="$(shasum -a 256 l4d2panel-control-<version>-linux-<arch>.tar.gz | awk '{print $1}')"
test "$expected" = "$actual"
```

校验通过后，中心端安装和启动只需要：

```bash
docker load --input l4d2panel-control-<version>-linux-<arch>.tar.gz
docker compose up -d
```

固定 Compose 同时把宿主 `/var/run/docker.sock` 读写挂载到 Control，用于设置页在线升级。Docker Socket 可以创建特权容器、挂载宿主任意路径，权限实际等同宿主 root；这不是普通应用数据卷。只应在专用或同等信任级别的 Docker 主机上部署 Control，不要与需要和面板隔离的其他容器共用同一 Docker daemon。镜像的降权、只读根文件系统与 capabilities 限制不能消除这个信任边界。

`docker load` 只把镜像导入 Docker；`docker compose up -d` 创建命名卷、容器和网络并启动 Control。首次启动自动完成：

1. 在 `app_data` 卷中生成 32 字节随机部署主密钥，并以权限 `0600` 保存为 `/app/data/control-master.key`；
2. 创建或打开 `/app/data/l4d2panel.sqlite3`；
3. 校验已有迁移名称和 SHA-256，按顺序事务执行尚未应用的迁移；
4. 检查配置、本地数据库文件系统、SQLite 锁、WAL 与数据库可用性；
5. 启动 HTTP 服务，由 Compose 健康检查持续检测 `/healthz`。

重启或重复执行 `docker compose up -d` 会复用原主密钥和数据库，不会重新初始化数据。迁移或配置检查失败时 Control 拒绝监听并由容器日志报告原因。

检查状态：

```bash
docker compose ps
curl --fail http://127.0.0.1:19090/readyz
```

## 领取初始化密钥

后台启动无法像 Windows 交互窗口一样安全显示秘密。服务就绪后单独执行本地命令领取初始化密钥：

```bash
docker compose exec -T app \
  /usr/local/bin/l4d2panel setup-key issue
```

命令只向当前终端输出 `l4d2p_setup_...`。配置 HTTPS 后访问 `PUBLIC_URL/setup`，输入该密钥并创建首个全局所有者。密钥同时是 `admin` 的初始密码，登录后应立即修改。

数据库已有所有者后，网页初始化和命令行引导都会拒绝再次创建所有者。初始化密钥不要写入 Compose、日志或版本控制。

## HTTPS 与可选配置

默认无需 `.env` 即可启动：

- 镜像：`l4d2panel/control:offline`；
- Control 端口：只发布到 `127.0.0.1:19090`；
- `PUBLIC_URL`：`https://localhost`；
- 可信代理：`127.0.0.1/32,::1/128`；
- 时区：`Asia/Hong_Kong`。

生产环境通常需要在部署目录创建 `.env`：

```dotenv
PUBLIC_URL=https://panel.example.com
CONTROL_BIND_ADDRESS=127.0.0.1
CONTROL_PORT=19090
TRUSTED_PROXIES=127.0.0.1/32,::1/128
TZ=Asia/Hong_Kong
```

Control 提供明文 HTTP，TLS 由管理员管理的反向代理终止。代理位于另一台主机时才显式设置 `CONTROL_BIND_ADDRESS=0.0.0.0`，并用宿主机防火墙把 `CONTROL_PORT` 限制为只允许该代理的精确地址访问。Compose 不安装 Docker，也不修改 nftables、iptables、UFW、firewalld 或 macOS 防火墙。

网页在线升级要求保留模板默认的 `CONTROL_IMAGE=l4d2panel/control:offline`。不要把它改成其他标签；否则以后再次执行 `docker compose up` 可能按旧标签重新创建容器，覆盖网页升级结果。需要使用自定义镜像标签时，不要使用设置页升级，应按后文的命令行流程自行管理镜像、数据库备份和恢复。

## 主密钥兼容与恢复

新部署不需要手工生成 `MASTER_KEY`。如果 `.env` 显式提供有效 `MASTER_KEY` 且卷内尚无 `control-master.key`，Control 会用该值创建持久密钥文件，便于旧部署迁移。文件已经存在时，显式环境变量必须与它一致，否则中心端拒绝启动。

主密钥用于加密 RCON 密码、SMTP 凭据、Webhook 密钥等可恢复秘密。数据库备份必须与对应主密钥一起保存。安全导出主密钥：

```bash
umask 077
docker compose exec -T app cat /app/data/control-master.key \
  > control-master.key.backup
```

备份文件属于高敏感秘密，应离线加密保管，不得提交 Git 或发送到普通日志。恢复时可在首次启动恢复卷之前，把原密钥填入受保护 `.env` 的 `MASTER_KEY`，Control 会验证并持久化它。

## 数据和日常操作

Compose 使用三个命名卷：

- `app_data`：SQLite、部署主密钥及中心端数据；
- `app_spool`：受控临时传输数据。
- `app_updates`：容器升级状态、短期工作文件和停服快照。

SQLite 文件、WAL 和 SHM 必须放在容器宿主机的本地持久卷，不得改为 NFS、CIFS、SMB 等网络文件系统。只允许一个 Control 实例访问数据库。

```bash
docker compose ps
docker compose logs --tail=200 app
docker compose restart app
docker compose stop app
docker compose start app
```

停止 Control 后创建一致性数据库备份：

```bash
docker compose stop app
docker compose run --rm -T app \
  backup-database --output /app/data/control-backup.sqlite3
docker compose start app
```

随后同时导出数据库备份和 `control-master.key`，并按同一恢复点保存。`docker compose down` 默认保留命名卷；`docker compose down -v` 会永久删除数据库和主密钥，除非明确清空部署且已验证备份，否则不要使用。

## 设置页在线升级

全局所有者可以在“设置 → 中心端更新”上传与宿主架构匹配的官方 `.tar.gz` 离线镜像包，最大 1 GiB。上传前仍应在管理员终端校验发布方提供的 `.sha256` 或签名；包内 OCI 摘要只证明内容没有自相矛盾，不能证明发布者身份。

Control 自动完成以下流程：

1. 限制上传大小和归档展开规模，拒绝链接、路径穿越、重复项和额外文件；
2. 要求归档只含一个固定标签 `l4d2panel/control:offline` 的 Linux 镜像，校验全部 blob SHA-256、产品标签、版本及 `amd64/arm64` 架构；
3. 拒绝降级，通过 Docker Engine API 加载镜像，并直接用经过复核的不可变镜像 ID 创建目标容器；
4. 启动独立更新 Worker，先停止当前 Control，再快照 SQLite、WAL/SHM 和 `control-master.key`；
5. 保留旧容器，复制其 Compose 标签、端口、命名卷、网络、重启策略和安全配置，以目标镜像重建同名 Control；
6. 等待新容器启动、自动执行前向迁移，并从容器内部检查 `/readyz`；成功后删除旧容器，失败则删除新容器、恢复快照和 `l4d2panel/control:offline` 的旧镜像指向，再重新启动旧容器。

上传请求返回后网页短暂断线是正常现象，刷新后可继续查看持久化在 `app_updates` 卷中的结果。一次只能运行一个中心端升级。当前中心端是 `dev` 或其他非语义版本开发构建时，可以替换为任意受信任的开发构建或正式版本；开发标签本身没有可靠的先后顺序，因此不做降级比较。当前中心端已经是正式语义版本时，只允许同版重装或升级，不能切回开发标签或在线降级。

如果现有部署来自旧 Compose，必须先用新发布包中的 `docker-compose.yml` 替换模板并执行一次 `docker compose up -d`，确认容器具有 `app_updates` 卷和 `/var/run/docker.sock` 挂载，设置页才会显示在线升级可用。

## 命令行离线升级与手工恢复

升级前创建并导出数据库与主密钥备份，然后校验、加载新镜像。发布包继续使用 `l4d2panel/control:offline` 标签时执行：

```bash
docker load --input l4d2panel-control-<new-version>-linux-<arch>.tar.gz
docker compose up -d --force-recreate
docker compose ps
curl --fail http://127.0.0.1:19090/readyz
```

这一路径绕过内置 Worker，不提供自动快照和回滚。迁移只向前执行；新版本迁移数据库后，不保证旧镜像兼容新结构。安全恢复必须同时恢复旧镜像、匹配的升级前数据库和同一主密钥，不能只切换镜像标签。

## 故障排查

```bash
docker compose ps
docker compose logs --tail=200 app
```

- `exec format error`：镜像架构与主机不匹配。
- `MASTER_KEY does not match`：`.env` 的显式密钥与卷内持久密钥不同；不要删除正确密钥来绕过。
- `MASTER_KEY_FILE` 错误：密钥文件不是普通文件、权限无法收紧或内容损坏。
- migration checksum/unknown migration：数据库与镜像版本链不一致；恢复匹配备份，不要修改历史迁移。
- `/readyz` 失败：检查数据库卷、迁移、端口占用和最近日志。
- 浏览器 Cookie 丢失：确认最终地址使用 HTTPS、`PUBLIC_URL` 正确并保持 `COOKIE_SECURE=true`。

从源码构建镜像的流程见仓库根目录 `README.md`；Windows 原生 ZIP 中心端见 `docs/windows-center-installation.md`。
