# 便携 Agent 部署与升级

便携 Agent 是 Windows 和 Linux 节点唯一推荐的安装与升级方式。节点不需要安装脚本、管理员提权、Windows Service 或 systemd；Agent 使用启动它的账号运行。需要开机自启或后台守护时，可以使用部署者自己的进程管理器，但必须保持同一低权限账号和工作目录。

## 发布产物

每次发布提供以下文件：

```text
l4d2panel-agent-linux-amd64
l4d2panel-agent-linux-amd64.sha256
l4d2panel-agent-windows-amd64.exe
l4d2panel-agent-windows-amd64.exe.sha256
agent.yaml.example
```

正式支持 amd64 Windows 10/11、Windows Server 2019+、Ubuntu 22.04/24.04 和 Debian 12。L4D2/SteamCMD 需要的 32 位运行库仍由节点管理员按操作系统安装。

## 首次部署

把对应平台二进制和 `agent.yaml.example` 放入仅由节点管理员及 Agent 运行账号控制的永久目录。不要从临时目录、下载目录或其他用户可写目录长期运行 Agent。

Linux 校验发布文件：

```bash
sha256sum -c l4d2panel-agent-linux-amd64.sha256
chmod 755 l4d2panel-agent-linux-amd64
```

Windows PowerShell 校验发布文件：

```powershell
$expected = ((Get-Content .\l4d2panel-agent-windows-amd64.exe.sha256 -Raw) -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash .\l4d2panel-agent-windows-amd64.exe -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actual -ne $expected) { throw 'Agent SHA-256 校验失败。' }
```

把示例复制为同目录的 `agent.yaml`，只修改中心地址和中心端生成的短期一次性令牌：

```yaml
central_url: "https://panel.example.com"
enrollment_token: "paste-one-time-token-here"
```

Linux 应限制配置权限：

```bash
cp agent.yaml.example agent.yaml
chmod 600 agent.yaml
./l4d2panel-agent-linux-amd64
```

Windows 应在 ACL 已限制到 Agent 运行账号、Administrators 和 SYSTEM 的目录中启动：

```powershell
Copy-Item .\agent.yaml.example .\agent.yaml
.\l4d2panel-agent-windows-amd64.exe
```

首次启动会自动注册。成功后 Agent 原子地把 `node_id` 和长期节点凭据写回 `agent.yaml`，并删除一次性令牌。未显式配置时，运行数据和实例目录分别为同目录的 `data` 与 `data/instances`。后续启动不再需要令牌：

```text
agent-directory/
  agent.yaml                  # 节点身份和配置，必须保留
  data/                       # Agent 状态、日志、备份和默认实例根，必须保留
  l4d2panel-agent-...         # 当前平台二进制，可独立升级
```

也可以在第一次启动时使用 `--central-url` 和 `--enrollment-token` 创建 `agent.yaml`，但令牌可能出现在 shell 历史或进程参数中，因此受限配置文件是默认推荐方式。

## 日常启动与停止

Agent 以前台进程运行，终端关闭或收到中断信号后停止。节点上的游戏进程仍由 Agent 的平台进程管理器管理。若使用外部守护程序，应让它：

- 以专用低权限账号运行 Agent；
- 使用 Agent 永久目录作为工作目录；
- 执行固定的 Agent 二进制，不拼接中心端提供的命令、可执行文件或环境；
- 停止时先向 Agent 发送正常终止信号，并留出最多 30 秒退出时间；
- 只授予 `agent.yaml`、`data` 和实例根所需的最小文件权限。

## Linux 升级与回滚

升级已有节点不创建新令牌，也不修改 `agent.yaml` 或 `data`。升级前先等待该节点的写任务结束，并从中心端正常停止所有运行中的实例；这会通过 RCON `quit` 完成优雅停服。不要在安装、更新、备份或恢复任务执行期间中断 Agent。Agent 进程退出时，Windows Job Object 和 Linux 父进程死亡信号可能终止仍由它管理的游戏进程，因此不能把“直接关闭 Agent”当作实例的正常停服方式。

先把新二进制及校验文件放在 Agent 永久目录下的 `upgrade` 暂存目录：

```bash
cd /path/to/agent-directory
mkdir -p upgrade
cd upgrade
sha256sum -c l4d2panel-agent-linux-amd64.sha256
chmod 755 l4d2panel-agent-linux-amd64
./l4d2panel-agent-linux-amd64 --version
cd ..
```

确认平台和版本正确后，停止当前 Agent，再执行只替换二进制的同文件系统升级：

```bash
cp -p l4d2panel-agent-linux-amd64 l4d2panel-agent-linux-amd64.previous
install -m 0755 upgrade/l4d2panel-agent-linux-amd64 l4d2panel-agent-linux-amd64.next
mv -f l4d2panel-agent-linux-amd64.next l4d2panel-agent-linux-amd64
./l4d2panel-agent-linux-amd64
```

确认节点重新上线后，可以在下一次升级时覆盖 `.previous`。若新版本启动失败，停止新进程并回滚：

```bash
mv -f l4d2panel-agent-linux-amd64 l4d2panel-agent-linux-amd64.failed
mv -f l4d2panel-agent-linux-amd64.previous l4d2panel-agent-linux-amd64
./l4d2panel-agent-linux-amd64
```

## Windows 升级与回滚

同样先等待节点任务结束并从中心端正常停止所有运行实例。若外部进程管理器配置了自动重启，应先暂停它，避免 Agent 在文件替换期间被立即拉起。然后把新 EXE 和校验文件放入同目录的 `upgrade`，先校验并查看版本：

```powershell
$stage = Join-Path $PWD 'upgrade'
$staged = Join-Path $stage 'l4d2panel-agent-windows-amd64.exe'
$checksum = Join-Path $stage 'l4d2panel-agent-windows-amd64.exe.sha256'
$expected = ((Get-Content $checksum -Raw) -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash $staged -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actual -ne $expected) { throw 'Agent SHA-256 校验失败。' }
& $staged --version
```

停止当前 Agent 后，使用同卷原子替换并同时保留上一版本：

```powershell
$current = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe'
$next = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe.next'
$previous = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe.previous'
Copy-Item $staged $next -Force
Remove-Item $previous -Force -ErrorAction SilentlyContinue
[IO.File]::Replace($next, $current, $previous, $true)
& $current
```

若新版本启动失败，确保新进程已经退出，再回滚：

```powershell
$failed = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe.failed'
Copy-Item $previous $next -Force
Remove-Item $failed -Force -ErrorAction SilentlyContinue
[IO.File]::Replace($next, $current, $failed, $true)
& $current
```

## 升级不变量

- 只替换当前平台的 Agent 二进制；绝不覆盖 `agent.yaml`、`data` 或实例目录。
- 替换前等待节点写任务结束，并通过中心端正常停止运行实例；暂停外部守护程序的自动重启。
- 新二进制必须先通过发布的 SHA-256 校验并成功输出版本。
- 已注册节点沿用现有 `node_id` 和凭据，不需要也不应申请新的注册令牌。
- 替换前保留一份上一版本二进制；新版本未重新上线前不要删除它。
- 当前没有中心端远程自更新任务。不要为了自动升级给 Agent 增加任意 URL 下载、任意可执行文件或 shell 命令能力。
