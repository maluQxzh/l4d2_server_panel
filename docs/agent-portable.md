# Agent 便携、系统服务部署与升级

Windows 和 Linux Agent 始终采用二进制、`agent.yaml` 与 `data` 旁置的便携文件布局，不需要额外安装脚本。同一二进制支持两种明确模式：无子命令启动是当前账号下的前台便携进程；`install --config` 会注册为 systemd 或 Windows Service，使用专用低权限账号在后台常驻、故障重启并开机自启。显式 `run --config` 也可交给部署者选择的其他进程管理器。

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

Linux 应先限制配置权限。便携前台启动使用：

```bash
cp agent.yaml.example agent.yaml
chmod 600 agent.yaml
./l4d2panel-agent-linux-amd64
```

systemd 常驻模式使用：

```bash
./l4d2panel-agent-linux-amd64 install --config ./agent.yaml
```

非 root 运行时程序会通过 `sudo` 重启固定的安装命令。它要求 systemd 为当前 init，创建无登录的 `l4d2panel` 系统账号，只把配置、Agent 数据、地图对象根和已配置实例根交给该账号，并安装、启动及启用 `l4d2panel-agent.service`。数据、地图或实例根不得是 `/` 等宽泛目录，也不能包含 Agent 二进制。

地图对象默认保存在 `<data_dir>/maps`。实例位于其他文件系统时，必须为该文件系统配置一个绝对 `map_roots` 路径，Agent 才能在该卷保存单份只读副本并硬链接到实例：

```yaml
map_roots:
  - "/srv/l4d2panel-maps"
  - "/mnt/game-volume/l4d2panel-maps"
```

默认地图根始终存在；配置路径不得重复指向同一文件系统。不要把地图根设为实例目录或由其他程序写入的共享目录。Agent 不会退化为逐实例复制，也不会创建符号链接或 Junction。

### 接管已有的 root-owned 游戏目录

现有 SteamCMD/L4D2 目录经常由 `root` 或另一个管理员账号创建，而 systemd Agent 固定以低权限 `l4d2panel` 运行。网页首次登记这类目录时，Agent 会先保存已经通过结构检查的绝对路径，再以句柄相对的临时写入探测游戏根及现有 `left4dead2/cfg`；不可写时任务保持异常并提示在节点本机重新执行原安装命令：

```bash
cd /path/to/agent-directory
./l4d2panel-agent-linux-amd64 install --config ./agent.yaml
```

`install` 是幂等的：已运行的同一服务不会被重复注册或中断，而是以本机管理员权限读取 Agent 自己的无秘密运行环境登记，批量处理所有已登记游戏目录。每个目录都必须再次满足：绝对且非宽泛路径、不是 Agent 程序父目录、整条路径无符号链接/重解析点，并且根目录直属位置存在平台固定的普通 `srcds` 可执行文件。Linux 在改属主前还会预检整棵树，拒绝符号链接、特殊文件和嵌套挂载点，避免权限变化越过游戏目录；硬链接仅在 Agent 地图注册表能够逐个证明源对象、实例目标、文件身份与预期链接计数时豁免，任何未知或证明不完整的硬链接仍拒绝安装。Windows 只给固定虚拟服务 SID、Administrators 和 SYSTEM 配置继承的运行权限。完成后回到网页重试“检测 srcds”。

权限接管刻意不是远程 Agent 任务：Control 被攻破或节点凭据泄露时不能借此取得节点 root/Administrator 权限。通过 Agent 托管安装的新游戏目录原本就由服务账号创建，无需重新接管。自定义 SteamCMD 若由其他账号安装在只读目录中，应改用面板的托管 SteamCMD，或由节点管理员单独把专用 SteamCMD 目录授予服务账号；安装器不会递归接管 `/usr/bin` 等系统程序目录。

Windows 应在仅受节点管理员控制的永久目录中使用前台模式：

```powershell
Copy-Item .\agent.yaml.example .\agent.yaml
.\l4d2panel-agent-windows-amd64.exe
```

Windows Service 常驻模式使用：

```powershell
.\l4d2panel-agent-windows-amd64.exe install --config .\agent.yaml
```

程序会自动请求 UAC，注册延迟自动启动的 `l4d2panel-agent` 服务，使用 `NT SERVICE\l4d2panel-agent` 虚拟服务账号，并把配置、数据目录、地图对象根、实例根和二进制 ACL 限制到服务 SID、Administrators 与 SYSTEM。

前台启动和服务安装都会在需要时自动注册。成功后 Agent 原子地把 `node_id` 和长期节点凭据写回 `agent.yaml`，并删除一次性令牌。未显式配置时，运行数据和实例目录分别为同目录的 `data` 与 `data/instances`。后续启动不再需要令牌：

```text
agent-directory/
  agent.yaml                  # 节点身份和配置，必须保留
  data/                       # Agent 状态、日志、备份、默认地图根和默认实例根，必须保留
  l4d2panel-agent-...         # 当前平台二进制，可独立升级
```

也可以在第一次启动时使用 `--central-url` 和 `--enrollment-token` 创建 `agent.yaml`，但令牌可能出现在 shell 历史或进程参数中，因此受限配置文件是默认推荐方式。

## 日常启动与停止

无子命令的便携 Agent 以前台进程运行，终端关闭或收到中断信号后停止。内置服务模式使用以下同一组跨平台命令；Linux 在需要时调用 `sudo`，Windows 在需要时请求 UAC：

```text
l4d2panel-agent status  --config /absolute/path/to/agent.yaml
l4d2panel-agent start   --config /absolute/path/to/agent.yaml
l4d2panel-agent stop    --config /absolute/path/to/agent.yaml
l4d2panel-agent restart --config /absolute/path/to/agent.yaml
l4d2panel-agent uninstall --config /absolute/path/to/agent.yaml
```

`install` 可以幂等重试：属于当前旁置部署且已经运行的服务会重新同步配置、数据、地图根、实例根及已登记游戏安装的服务账号权限后返回成功；同名服务若指向其他二进制、配置或账号则拒绝接管。`uninstall` 只注销系统服务，保留二进制、`agent.yaml`、`data`、地图对象、游戏目录和节点身份。

节点上的游戏进程仍由 Agent 的平台进程管理器管理。若使用显式 `run --config` 交给其他守护程序，应让它：

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

确认平台和版本正确后，停止当前 Agent。前台模式使用 `Ctrl+C`；systemd 模式使用：

```bash
./l4d2panel-agent-linux-amd64 stop --config ./agent.yaml
```

再执行只替换二进制的同文件系统升级，并按原模式重新启动：

```bash
cp -p l4d2panel-agent-linux-amd64 l4d2panel-agent-linux-amd64.previous
install -m 0755 upgrade/l4d2panel-agent-linux-amd64 l4d2panel-agent-linux-amd64.next
mv -f l4d2panel-agent-linux-amd64.next l4d2panel-agent-linux-amd64
# 前台模式：./l4d2panel-agent-linux-amd64
# systemd 模式：./l4d2panel-agent-linux-amd64 start --config ./agent.yaml
```

确认节点重新上线后，可以在下一次升级时覆盖 `.previous`。若新版本启动失败，停止新进程并回滚：

```bash
mv -f l4d2panel-agent-linux-amd64 l4d2panel-agent-linux-amd64.failed
mv -f l4d2panel-agent-linux-amd64.previous l4d2panel-agent-linux-amd64
# 按原前台或 systemd 模式重新启动
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

停止当前 Agent。前台模式用 `Ctrl+C`；Windows Service 模式使用：

```powershell
.\l4d2panel-agent-windows-amd64.exe stop --config .\agent.yaml
```

然后使用同卷原子替换并同时保留上一版本，最后按原模式重新启动：

```powershell
$current = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe'
$next = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe.next'
$previous = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe.previous'
Copy-Item $staged $next -Force
Remove-Item $previous -Force -ErrorAction SilentlyContinue
[IO.File]::Replace($next, $current, $previous, $true)
# 前台模式：& $current
# Windows Service 模式：& $current start --config .\agent.yaml
```

若新版本启动失败，确保新进程已经退出，再回滚：

```powershell
$failed = Join-Path $PWD 'l4d2panel-agent-windows-amd64.exe.failed'
Copy-Item $previous $next -Force
Remove-Item $failed -Force -ErrorAction SilentlyContinue
[IO.File]::Replace($next, $current, $failed, $true)
# 按原前台或 Windows Service 模式重新启动
```

## 升级不变量

- 只替换当前平台的 Agent 二进制；绝不覆盖 `agent.yaml`、`data` 或实例目录。
- 替换前等待节点写任务结束，并通过中心端正常停止运行实例；停止内置系统服务，或暂停外部守护程序的自动重启。
- 新二进制必须先通过发布的 SHA-256 校验并成功输出版本。
- 已注册节点沿用现有 `node_id` 和凭据，不需要也不应申请新的注册令牌。
- 替换前保留一份上一版本二进制；新版本未重新上线前不要删除它。
- 当前没有中心端远程自更新任务。不要为了自动升级给 Agent 增加任意 URL 下载、任意可执行文件或 shell 命令能力。
