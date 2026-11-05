# 安全工具链升级验证（2026-11-01）

本轮将 Go 构建基线从 1.25.13 升级到 1.26.10，并定向修复前端构建与测试依赖。现有业务修改保留；没有向正在运行的中心端部署，也没有发布到 registry 或 GitHub Release。

## 固定版本与范围

| 项目 | 升级前 | 升级后 |
| --- | --- | --- |
| Go | 1.25.13 | 1.26.10 |
| Vite | 7.3.6 | 7.3.7 |
| Vitest / @vitest/mocker | 3.2.7 | 4.1.12 |
| source-map-js（传递依赖） | 1.2.1 | 1.2.2 |

Vitest 4 的依赖树不再包含旧 Tinypool；没有使用 `npm audit fix --force` 或跨主版本 overrides。`package-lock.json` 保存完整解析版本与 integrity，已通过 `npm ci` 重装验证。React、TypeScript、Node 24 与 Go 模块依赖版本未作批量升级。

Go 版本同步于 `go.mod`、Dockerfile、CI、Release workflow、PowerShell 聚合构建检查及开发说明。在项目目录中默认 `GOTOOLCHAIN=auto` 可选择并下载 1.26.10；使用 `GOTOOLCHAIN=local` 的构建机需自行安装该版本。聚合脚本在仓库目录中检查 Go 版本，避免从其他目录调用时误读旧的全局工具链。已经部署的旧二进制不会因为构建机升级而自动获得修复。

官方依据：[Go 发布记录及支持周期](https://go.dev/doc/devel/release)、[crypto/x509 修复](https://pkg.go.dev/vuln/GO-2026-6809)、[Vite 安全修复](https://github.com/vitejs/vite/security/advisories/GHSA-9jrq-w75r-8gcw)、[Vitest mocker 安全修复](https://github.com/vitest-dev/vitest/security/advisories/GHSA-qxg6-89mg-rqv8)。扫描结果取决于当天漏洞库，应在正式发布时重新检查。

## 已完成验证

- `npm audit`：0 vulnerabilities，含开发依赖。
- Go 1.26.10 下 `govulncheck@v1.6.0 ./cmd/... ./internal/...`：0 项代码可达漏洞，退出码为 0；仍报告已导入包中的 4 项、依赖模块中的 19 项未发现调用路径的漏洞，不能表述为整个依赖生态完全无漏洞。
- Windows 与 Linux/amd64 全量 Go 普通测试、Windows 全量 `go test -race ./...`、`go vet ./...`、`go mod verify`。
- SQLite 迁移/凭据生命周期和 20 节点、100 实例模拟验收。
- Vitest 4.1.12：211 项测试；Vite 7.3.7：TypeScript 与生产构建；Playwright：130 项桌面/移动端模拟接口测试。
- Windows 中心端便携 ZIP 构建、精确结构及文件哈希验证，二进制构建信息为 Go 1.26.10。
- 固定 Makefile 目标生成 Linux/amd64 与 Windows/amd64 Agent：`CGO_ENABLED=0`、`-trimpath`、版本/commit/UTC 日期；两份 LF 校验和通过，两个平台均执行 `--version`。
- Compose 两份配置检查；生产容器 `linux/amd64` 和 `linux/arm64` 构建与离线导出，SHA-256 和 Go Docker-save 归档验证均通过；ARM64 的 `--version` 使用已注册的模拟器执行。
- WSL2 Docker 内独立 Compose 项目的启动/readiness、停止后的数据库 doctor、重启主密钥持久性验证。PID 1 为 UID/GID 100:101，CapEff=0，NoNewPrivs=1；临时 Compose 项目及其卷已清理。

Linux 回归同时修正了一条测试输入：相对数据库路径改为两平台均可识别的 `data/control.sqlite3`，不修改生产路径解释规则。CI 的数据库/主密钥文件检查使用 `panel` 用户，适配已移除 DAC 绕过能力的 root exec，未放宽容器权限。

## 本地验证产物

版本标签为 `dev-security-20261101`，属于未发布、未签名的本地验证构建，包含工作区尚未提交的优化：

- `dist/security-20261101/windows/l4d2panel-center-dev-security-20261101-windows-x64.zip` 和校验和。
- `dist/agents`：固定双平台二进制、校验和与首次注册配置示例。
- `dist/container/l4d2panel-control-dev-security-20261101-linux-{amd64,arm64}.tar.gz` 和校验和。

原官方 Alpine 源和容器内 npm 下载发生网络超时。此次本地容器构建使用既有可配置的清华 Alpine HTTPS 镜像，保留 APK 签名校验；将 Windows npm 缓存中与当前锁文件 integrity 一致的公开包补入 BuildKit 缓存，随后仍运行原 Dockerfile 的 `npm ci`。没有修改默认仓库、关闭 TLS 或绕过依赖校验。

## 发布前仍需执行

Windows 主机已有运行中的固定 Control 服务，本轮没有运行会安装/升级/注销它的提升权限脚本。需要在隔离 Windows VM 上完成安装、升级及故障回滚矩阵；本轮也没有重跑真实容器在线自更新/故障回滚、原生 macOS Docker Desktop 和 SteamCMD App 222860 验收。跨平台编译、模拟接口测试和 ARM64 模拟执行不能替代这些门禁。正式发布还需签名及对应发布流程。
