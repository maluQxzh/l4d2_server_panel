import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Archive,
  ArrowLeft,
  Ban,
  Box,
  CalendarClock,
  ChevronRight,
  CircleDot,
  CloudDownload,
  ClipboardList,
  Code2,
  Download,
  File,
  FileCode2,
  Folder,
  Gamepad2,
  HardDrive,
  History,
  Map,
  PackageOpen,
  Play,
  Plus,
  Power,
  RefreshCw,
  RotateCcw,
  Save,
  Search,
  Send,
  Settings2,
  ShieldAlert,
  ShieldCheck,
  Square,
  Terminal,
  Trash2,
  Upload,
  Users,
  Wrench,
} from "lucide-react";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, ProgressBar, StatusBadge } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { api, ApiError } from "../lib/api";
import { backupUploadChunkSize, bytesToBase64 as encodeBackupChunk, hashBlobSHA256, readBlobSlice, sha256Hex, validateBackupUploadFile } from "../lib/backup-upload";
import {
  clearBackupUploadSession,
  loadBackupUploadSession,
  newBackupUploadKey,
  sameBackupUploadFile,
  saveBackupUploadSession,
  type PersistedBackupPlatform,
  type PersistedBackupUploadSession,
} from "../lib/backup-upload-session";
import {
  maxBufferedBackupDownload,
  receiveBackupDownload,
  type BackupDownloadWriter,
} from "../lib/backup-download";
import { bytes, dateTime, duration, jobLabels } from "../lib/format";
import { buildInstanceConfigUpdate, type RCONPasswordChange } from "../lib/instance-config";
import { buildStartupArgumentPreview, canAccessInstance, lifecycleActionAllowed } from "../lib/instance-lifecycle";
import { defaultTickrate, gameDifficulties, gameModes, maxTickrate, minTickrate, officialMapGroups } from "../lib/l4d2-options";
import type { BackupRecord, BackupUploadRecord, InstanceConfig, InstanceRecord, LogSearchEntry, PackageRecord, Player, PlayerBan, ScheduleRecord } from "../lib/types";

const tabs = [
  { id: "overview", label: "概览", icon: CircleDot },
  { id: "console", label: "控制台", icon: Terminal },
  { id: "players", label: "玩家", icon: Users },
  { id: "config", label: "配置", icon: Settings2 },
  { id: "mods", label: "模组", icon: PackageOpen },
  { id: "plugins", label: "插件", icon: Code2 },
  { id: "files", label: "文件", icon: Folder },
  { id: "backups", label: "备份", icon: Archive },
  { id: "schedules", label: "计划任务", icon: CalendarClock },
  { id: "audit", label: "审计", icon: ClipboardList },
] as const;
type TabId = typeof tabs[number]["id"];

export function InstanceDetailPage() {
  const { instanceId = "" } = useParams();
  const [searchParams, setSearchParams] = useSearchParams();
  const active = (tabs.some((tab) => tab.id === searchParams.get("tab")) ? searchParams.get("tab") : "overview") as TabId;
  const instance = useQuery({ queryKey: ["instances", instanceId], queryFn: () => api.instance(instanceId), enabled: Boolean(instanceId) });

  if (instance.isPending) return <LoadingBlock label="正在读取实例状态…" />;
  if (instance.error) return <><Link className="back-link" to="/instances"><ArrowLeft size={16} />返回实例列表</Link><ErrorState error={instance.error} retry={() => void instance.refetch()} /></>;
  const item = instance.data;

  return <div className="instance-detail">
    <div className="detail-breadcrumb"><Link to="/instances">游戏实例</Link><ChevronRight size={14} /><span>{item.name}</span></div>
    <InstanceHero instance={item} />
    <nav className="detail-tabs" aria-label="实例管理分类">{tabs.map(({ id, label, icon: Icon }) => <button key={id} className={active === id ? "active" : ""} onClick={() => setSearchParams(id === "overview" ? {} : { tab: id })}><Icon size={17} />{label}</button>)}</nav>
    <div className="detail-content">
      {active === "overview" && <OverviewTab instance={item} />}
      {active === "console" && <ConsoleTab instance={item} />}
      {active === "players" && <PlayersTab instance={item} />}
      {active === "config" && <ConfigTab instance={item} />}
      {active === "mods" && <ModsTab instance={item} />}
      {active === "plugins" && <PluginsTab instance={item} />}
      {active === "files" && <FilesTab instance={item} />}
      {active === "backups" && <BackupsTab instance={item} />}
      {active === "schedules" && <SchedulesTab instance={item} />}
      {active === "audit" && <AuditTab instance={item} />}
    </div>
  </div>;
}

function InstanceHero({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const toast = useToast();
  const client = useQueryClient();
  const [takeoverOpen, setTakeoverOpen] = useState(false);
  const [restartOpen, setRestartOpen] = useState(false);
  const [restartPlayers, setRestartPlayers] = useState(0);
  const takeoverPassword = useRef<HTMLInputElement>(null);
  const allows = (permission: Parameters<typeof canAccessInstance>[2]) => canAccessInstance(user, instance, permission);
  const canStart = lifecycleActionAllowed("start", instance.status, allows);
  const canStop = lifecycleActionAllowed("stop", instance.status, allows);
  const canRestart = lifecycleActionAllowed("restart", instance.status, allows);
  const nodeUnavailable = instance.node_status !== "online";
  const nodeStatusLabel = instance.node_status === "offline" ? "节点离线" : instance.node_status === "revoked" ? "节点已吊销" : "节点待接入";
  const disabledReason = !canStart && !canStop && !canRestart ? "当前账号没有对应的实例生命周期权限" : "当前实例状态不允许此操作";
  const action = useMutation({
    mutationFn: (type: string) => api.instanceAction(instance.id, type),
    onSuccess: (_result, type) => { setRestartOpen(false); toast.push(`${actionLabel(type)}任务已提交`); void client.invalidateQueries({ queryKey: ["jobs"] }); void client.invalidateQueries({ queryKey: ["instances", instance.id] }); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const restartCheck = useMutation({
    mutationFn: () => api.instance(instance.id),
    onSuccess: (current) => {
      if (current.players > 0) {
        setRestartPlayers(current.players);
        setRestartOpen(true);
        return;
      }
      action.mutate("restart");
    },
    onError: (error) => toast.push(`无法检测在线玩家：${error.message}`, "error"),
  });
  const takeover = useMutation({
    mutationFn: () => {
      const input = takeoverPassword.current;
      if (!input?.value) throw new ApiError("请输入现有服务器的 RCON 密码", 400, "rcon_password_required");
      // instanceAction serializes the body synchronously. Clear the uncontrolled
      // field immediately so the one-time secret is never retained in React or
      // React Query state/devtools.
      const request = api.instanceAction(instance.id, "restart", { existing_rcon_password: input.value });
      input.value = "";
      return request;
    },
    onSuccess: () => {
      setTakeoverOpen(false);
      toast.push("受控重启与接管任务已提交");
      void client.invalidateQueries({ queryKey: ["jobs"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const closeTakeover = () => {
    if (takeoverPassword.current) takeoverPassword.current.value = "";
    setTakeoverOpen(false);
  };
  return <><header className="instance-hero">
    <div className="instance-hero__identity"><div className="instance-hero__glyph"><Gamepad2 /></div><div><div className="instance-hero__title"><h1>{instance.name}</h1><StatusBadge status={instance.status} pulse={instance.status === "running" && !nodeUnavailable} />{nodeUnavailable && <span className={`status status--${instance.node_status} instance-node-status`} title="无法确认节点上实例的实时状态"><span className="status__dot" aria-hidden />{nodeStatusLabel}</span>}{instance.status === "running" && !instance.rcon_configured && <span className="instance-rcon-warning">RCON 密码为空</span>}</div><p><code>{instance.host}:{instance.port}</code><span>·</span><span>{instance.map || instance.config.start_map}</span><span>·</span><span>{instance.players}/{instance.max_players} 玩家</span>{instance.status === "external" && <><span>·</span><strong>受控重启前只读</strong></>}</p></div></div>
    <div className="instance-actions">
      {instance.status === "external" ? <Button onClick={() => setTakeoverOpen(true)} disabled={!canRestart} title={!canRestart ? disabledReason : undefined} loading={takeover.isPending}><RefreshCw size={16} />受控重启并接管</Button> : <>
        {instance.status === "stopped" ? <Button onClick={() => action.mutate("start")} disabled={!canStart} title={!canStart ? disabledReason : undefined} loading={action.isPending}><Play size={16} fill="currentColor" />启动</Button> : <Button variant="secondary" onClick={() => action.mutate("stop")} disabled={!canStop} title={!canStop ? disabledReason : undefined} loading={action.isPending}><Square size={14} fill="currentColor" />停止</Button>}
        <Button variant="secondary" onClick={() => restartCheck.mutate()} disabled={!canRestart} title={!canRestart ? instance.status === "stopped" ? "服务器启动后才能重启" : disabledReason : undefined} loading={action.isPending || restartCheck.isPending}><RefreshCw size={16} />重启</Button>
      </>}
    </div>
  </header><Modal open={restartOpen} onClose={() => setRestartOpen(false)} title="服务器内仍有玩家" description={`检测到当前有 ${restartPlayers} 名玩家在线。`} footer={<><Button variant="secondary" onClick={() => setRestartOpen(false)}>取消</Button><Button onClick={() => action.mutate("restart")} loading={action.isPending}>仍然重启</Button></>}>
    <div className="form-stack"><div className="form-warning"><ShieldAlert size={17} />立即重启会断开所有在线玩家的连接，且不会发送游戏内广播。</div><div className="form-note"><strong>确认后立即执行</strong><span>Agent 会先通过 RCON 请求退出，失败或超时后再终止受管进程树并重新启动。</span></div></div>
  </Modal><Modal open={takeoverOpen} onClose={closeTakeover} title="受控重启并接管" description="现有 RCON 密码仅用于本次优雅停服，不会保存到浏览器状态或审计日志。" footer={<><Button variant="secondary" onClick={closeTakeover}>取消</Button><Button form="external-takeover-form" type="submit" loading={takeover.isPending}>确认重启并接管</Button></>}>
    <form id="external-takeover-form" onSubmit={(event) => { event.preventDefault(); takeover.mutate(); }} className="form-stack">
      <Field label="现有 RCON 密码" hint="请输入外部 srcds 当前使用的密码；接管完成后将改用中心端保存的 RCON 凭据。"><input ref={takeoverPassword} type="password" autoComplete="off" required /></Field>
      <div className="form-warning"><ShieldAlert size={17} />Agent 会先通过 RCON 请求退出，超时后才会终止外部进程并以受控模式重新启动。</div>
    </form>
  </Modal></>;
}

function OverviewTab({ instance }: { instance: InstanceRecord }) {
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: api.jobs });
  const instanceJobs = (jobs.data?.items || []).filter((job) => job.instance_id === instance.id).slice(0, 5);
  return <div className="overview-layout">
    <div className="overview-main">
      <section className="detail-stat-grid"><Card><span>在线玩家</span><strong>{instance.players}<small> / {instance.max_players}</small></strong><div className="mini-progress"><ProgressBar value={(instance.players / Math.max(instance.max_players, 1)) * 100} /></div></Card><Card><span>当前地图</span><strong className="text-value">{instance.map || "未载入"}</strong><small><Map size={14} />{instance.mode || instance.config.mode}</small></Card><Card><span>连续运行</span><strong className="text-value">{duration(instance.started_at)}</strong><small><Power size={14} />PID {instance.pid || "—"}</small></Card><Card><span>游戏构建</span><strong className="text-value">{instance.build_id || "待检测"}</strong><small><Box size={14} />当前启动配置</small></Card></section>
      <InstanceBindingCard instance={instance} />
      <Card><div className="card-heading"><div><span className="eyebrow">RECENT OPERATIONS</span><h2>最近操作</h2></div><Link to="/jobs">全部任务 <ChevronRight size={15} /></Link></div>{jobs.isPending ? <LoadingBlock /> : instanceJobs.length ? <div className="detail-job-list">{instanceJobs.map((job) => <div key={job.id}><div className={`job-icon job-icon--${job.status}`}><Code2 size={15} /></div><div><strong>{jobLabels[job.type] || job.type}</strong><small>{job.message || dateTime(job.created_at)}</small></div><StatusBadge status={job.status} />{["queued", "dispatched", "running"].includes(job.status) && <div className="job-inline-progress"><ProgressBar value={job.progress} /><span>{job.progress}%</span></div>}</div>)}</div> : <EmptyState title="还没有操作记录" description="启动、更新或备份实例后，任务进度会显示在这里。" />}</Card>
    </div>
    <aside className="overview-side"><Card><div className="card-heading"><div><span className="eyebrow">SERVER PROFILE</span><h2>实例资料</h2></div></div><div className="detail-pairs detail-pairs--stack"><div><span>所属节点</span><strong>{instance.node_name || instance.node_id}</strong></div><div><span>游戏安装</span><code>{instance.game_installation_id || "尚未绑定"}</code></div><div><span>安装路径</span><code>{instance.path || "等待 Agent 校验"}</code></div><div><span>游戏端口</span><strong>{instance.port}</strong></div><div><span>SourceTV 端口</span><strong>{instance.tv_port || "未启用"}</strong></div><div><span>自动重启</span><strong>{instance.config.auto_restart ? "已启用" : "已关闭"}</strong></div><div><span>公开状态</span><strong>{instance.config.public_status ? "可见" : "隐藏"}</strong></div></div></Card><StartupArgumentsCard instance={instance} /><Card className="maintenance-card"><Wrench size={22} /><div><strong>游戏文件维护</strong><span>SteamCMD、更新、校验和重装统一在运行环境中执行</span></div><Link to="/environments">打开运行环境 <ChevronRight size={14} /></Link></Card></aside>
  </div>;
}

function InstanceBindingCard({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const toast = useToast();
  const navigate = useNavigate();
  const canManage = canAccessInstance(user, instance, "instances.delete");
  const remove = useMutation({
    mutationFn: () => api.deleteInstance(instance.id, "detach"),
    onSuccess: () => { toast.push("实例已提交解除绑定，游戏文件将保留"); navigate("/jobs"); },
    onError: (error) => toast.push(error.message, "error"),
  });
  return <Card className="lifecycle-card">
    <div className="card-heading"><div><span className="eyebrow">GAME INSTALLATION</span><h2>游戏安装绑定</h2></div><Link to="/environments">运行环境 <ChevronRight size={15} /></Link></div>
    <div className="lifecycle-actions">
      <div><HardDrive size={19} /><span><strong>{instance.game_installation_id ? "已绑定独占游戏安装" : "游戏安装未就绪"}</strong><small>{instance.path || "请到运行环境检查目录状态"}</small></span><Button variant="secondary" onClick={() => navigate("/environments")}>查看环境</Button></div>
      <div className="lifecycle-danger"><Trash2 size={19} /><span><strong>删除实例并解除绑定</strong><small>保留全部游戏文件；之后可再次绑定或在运行环境中隔离删除</small></span><Button variant="danger" loading={remove.isPending} disabled={!canManage} title={!canManage ? "当前角色没有实例管理权限" : undefined} onClick={() => { if (window.confirm(`删除实例“${instance.name}”并保留游戏文件？`)) remove.mutate(); }}>删除实例</Button></div>
    </div>
  </Card>;
}

function StartupArgumentsCard({ instance }: { instance: InstanceRecord }) {
  const args = buildStartupArgumentPreview(instance);
  return <Card className="startup-preview"><div className="card-heading"><div><span className="eyebrow">ARGV PREVIEW</span><h2>启动参数预览</h2></div><Terminal size={19} /></div><ol aria-label="启动参数数组">{args.map((argument, index) => <li key={`${index}-${argument}`}><span>{index}</span><code>{JSON.stringify(argument)}</code></li>)}</ol><p><ShieldCheck size={14} />面板下次受控启动会逐项传递这些参数，不经过 Shell；历史敏感参数会隐藏其值并标记为不会传递。</p></Card>;
}

function ConsoleTab({ instance }: { instance: InstanceRecord }) {
  const realtime = useRealtime();
  const toast = useToast();
  const [lines, setLines] = useState<Array<{ at: string; stream: string; line: string }>>([]);
  const [command, setCommand] = useState("");
  const send = useMutation({ mutationFn: () => api.instanceAction(instance.id, "rcon", { command }), onSuccess: () => { setLines((value) => [...value, { at: new Date().toISOString(), stream: "command", line: `> ${command}` }]); setCommand(""); toast.push("RCON 命令已发送"); }, onError: (error) => toast.push(error.message, "error") });
  useEffect(() => realtime.subscribe((event) => {
    if (event.resource_id !== instance.id || !["log", "console"].some((word) => event.type.includes(word))) return;
    const data = event.data || {};
    setLines((current) => [...current.slice(-499), { at: String(data.at || event.created_at), stream: String(data.stream || "stdout"), line: String(data.line || data.message || "") }]);
  }), [instance.id, realtime]);
  const submit = (event: FormEvent) => { event.preventDefault(); if (command.trim()) send.mutate(); };
  return <div className="console-stack"><Card className="console-card"><div className="console-toolbar"><div><span className={`health-dot health-dot--${realtime.state === "live" ? "online" : "pending"}`} /><strong>{realtime.state === "live" ? "实时日志" : "连接降级"}</strong><small>{realtime.state === "live" ? "输出会自动滚动" : "实时输出暂不可用，命令仍可提交"}</small></div><Button variant="ghost" onClick={() => setLines([])}><Trash2 size={15} />清屏</Button></div><div className="terminal-output" role="log" aria-live="polite">{lines.length ? lines.map((line, index) => <div key={`${line.at}-${index}`} className={`terminal-line terminal-line--${line.stream}`}><time>{new Date(line.at).toLocaleTimeString("zh-CN", { hour12: false })}</time><span>{line.line}</span></div>) : <div className="terminal-placeholder"><Terminal size={27} /><strong>等待服务器输出</strong><span>启动实例后，Agent 推送的控制台日志会出现在这里。</span></div>}</div><form className="command-line" onSubmit={submit}><span>RCON</span><input value={command} onChange={(event) => setCommand(event.target.value)} placeholder="输入 Source RCON 命令，例如 status" aria-label="RCON 命令" disabled={instance.status === "stopped"} /><Button type="submit" loading={send.isPending} disabled={!command.trim() || instance.status === "stopped"}><Send size={16} />发送</Button></form><p className="console-note">这里只接受游戏 RCON 命令，不提供主机 Shell。密码类参数不会写入审计日志。</p></Card><LogHistoryCard instance={instance} /></div>;
}

function LogHistoryCard({ instance }: { instance: InstanceRecord }) {
  const toast = useToast();
  const [keyword, setKeyword] = useState("");
  const [since, setSince] = useState("");
  const [until, setUntil] = useState("");
  const [result, setResult] = useState<{ items: LogSearchEntry[]; matched: number; truncated: boolean } | null>(null);
  const [downloadProgress, setDownloadProgress] = useState(0);
  const search = useMutation({
    mutationFn: () => api.searchLogs(instance.id, { keyword: keyword.trim() || undefined, since: since ? new Date(since).toISOString() : undefined, until: until ? new Date(until).toISOString() : undefined, limit: 500 }),
    onSuccess: (data) => setResult(data),
    onError: (error) => toast.push(error.message, "error"),
  });
  const download = useMutation({
    mutationFn: async () => {
      const parts: BlobPart[] = [];
      let offset = 0;
      let expectedSha = "";
      for (;;) {
        const chunk = await api.downloadLogChunk(instance.id, 0, offset, expectedSha);
        if (offset === 0) expectedSha = chunk.fileSha256;
        if (expectedSha && chunk.fileSha256 !== expectedSha) throw new ApiError("日志在下载过程中发生轮转，请重试", 409, "log_changed");
        parts.push(chunk.blob);
        offset = chunk.nextOffset;
        setDownloadProgress(chunk.totalSize > 0 ? Math.round((offset / chunk.totalSize) * 100) : 100);
        if (chunk.eof) break;
      }
      return new Blob(parts, { type: "text/plain;charset=utf-8" });
    },
    onSuccess: (blob) => { saveBrowserDownload(blob, `${safeBrowserFilename(instance.name)}-${new Date().toISOString().slice(0, 10)}.log`); setDownloadProgress(0); toast.push("日志下载已完成"); },
    onError: (error) => { setDownloadProgress(0); toast.push(error.message, "error"); },
  });
  return <Card className="log-history-card"><div className="card-heading"><div><span className="eyebrow">LOCAL LOG ARCHIVE</span><h2>节点日志检索</h2></div><Button variant="secondary" onClick={() => download.mutate()} loading={download.isPending}><Download size={15} />{download.isPending ? `下载 ${downloadProgress}%` : "下载当前日志"}</Button></div><form className="log-search-form" onSubmit={(event) => { event.preventDefault(); search.mutate(); }}><Field label="关键词"><input value={keyword} onChange={(event) => setKeyword(event.target.value)} maxLength={256} placeholder="错误、玩家名或地图" /></Field><Field label="开始时间"><input type="datetime-local" value={since} onChange={(event) => setSince(event.target.value)} /></Field><Field label="结束时间"><input type="datetime-local" value={until} onChange={(event) => setUntil(event.target.value)} /></Field><Button type="submit" loading={search.isPending}><Search size={15} />检索</Button></form>{result ? <div className="log-search-results"><div className="log-search-summary"><span>匹配 {result.matched} 行</span>{result.truncated && <em>结果过多，仅显示最近 500 行</em>}</div>{result.items.length ? <div className="log-search-lines" role="log">{result.items.map((entry, index) => <div key={`${entry.at || "legacy"}-${index}`}><time>{entry.at ? dateTime(entry.at) : "旧格式"}</time><span className={`log-stream log-stream--${entry.stream}`}>{entry.stream}</span><code>{entry.message}</code></div>)}</div> : <EmptyState title="没有匹配日志" description="调整时间范围或关键词后重试。" />}</div> : <p className="log-search-hint">日志保留在节点本地，搜索结果只在当前请求中加密传输，不会长期复制到控制端。</p>}</Card>;
}

function PlayersTab({ instance }: { instance: InstanceRecord }) {
  const serverActive = instance.status === "running" || instance.status === "degraded";
  const canQueryRCON = serverActive && instance.rcon_configured;
  const players = useQuery({ queryKey: ["instances", instance.id, "players"], queryFn: () => api.players(instance.id), enabled: canQueryRCON, refetchInterval: canQueryRCON ? 15_000 : false });
  const probeJobs = useQuery({ queryKey: ["jobs", instance.id, "rcon.players"], queryFn: () => api.instanceJobs(instance.id), enabled: canQueryRCON, refetchInterval: canQueryRCON ? 2_000 : false });
  const bans = useQuery({ queryKey: ["instances", instance.id, "bans"], queryFn: () => api.bans(instance.id), refetchInterval: 30_000 });
  const toast = useToast();
  const client = useQueryClient();
  const [selected, setSelected] = useState<Player | null>(null);
  const action = useMutation({ mutationFn: ({ type, payload }: { type: string; payload: Record<string, unknown> }) => api.instanceAction(instance.id, type, payload), onSuccess: (_data, values) => { toast.push(values.type === "kick_player" ? "已提交踢出操作" : "已提交解封操作"); void client.invalidateQueries({ queryKey: ["instances", instance.id, "bans"] }); }, onError: (error) => toast.push(error.message, "error") });
  const latestProbe = probeJobs.data?.items.find((job) => job.instance_id === instance.id && job.type === "rcon.players");
  const handledProbe = useRef("");
  useEffect(() => {
    if (latestProbe?.status !== "succeeded" || handledProbe.current === latestProbe.id) return;
    handledProbe.current = latestProbe.id;
    void players.refetch();
  }, [latestProbe?.id, latestProbe?.status, players]);

  const playerItems = players.data?.items || [];
  let playerContent: ReactNode;
  if (!serverActive) {
    playerContent = <div className="form-warning"><ShieldAlert size={17} />服务器尚未启动，RCON 查询只在运行中生效。</div>;
  } else if (!instance.rcon_configured) {
    playerContent = <div className="form-warning"><ShieldAlert size={17} />中心端尚未保存 RCON 密码。请设置与 server.cfg 完全一致的密码后重试。</div>;
  } else if (players.isPending) {
    playerContent = <LoadingBlock label="正在发起 RCON 玩家查询…" />;
  } else if (players.error) {
    playerContent = <ErrorState error={players.error} retry={() => void players.refetch()} />;
  } else {
    playerContent = <>
      {latestProbe?.status === "failed" && <div className="form-error" role="alert">RCON 查询失败：{latestProbe.message || "请确认 server.cfg 密码已在当前游戏进程中生效，并检查任务详情。"}</div>}
      {latestProbe && ["queued", "dispatched", "running"].includes(latestProbe.status) && <div className="form-note"><RefreshCw className="spin" size={16} />正在通过节点查询最新玩家信息…</div>}
      {playerItems.length ? <div className="data-table-wrap"><table className="data-table"><thead><tr><th>玩家</th><th>Steam ID</th><th>分数</th><th>延迟</th><th>连接时间</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>{playerItems.map((player) => <tr key={player.steam_id || player.name}><td><strong>{player.name}</strong></td><td><code>{player.steam_id || "—"}</code></td><td>{player.score ?? "—"}</td><td>{player.ping != null ? `${player.ping} ms` : "—"}</td><td>{dateTime(player.connected_at)}</td><td className="table-actions"><Button variant="ghost" onClick={() => action.mutate({ type: "kick_player", payload: { steam_id: player.steam_id, name: player.name } })}><Power size={14} />踢出</Button><Button variant="ghost" onClick={() => setSelected(player)} disabled={!player.steam_id}><Ban size={14} />封禁</Button></td></tr>)}</tbody></table></div> : <EmptyState title="服务器里暂时没人" description="查询成功后，在线玩家会显示在这里；若上方出现错误，请按错误信息检查 RCON。" />}
    </>;
  }

  return <div className="players-stack"><Card><div className="card-heading"><div><span className="eyebrow">CONNECTED PLAYERS</span><h2>在线玩家</h2></div><span className="table-count">{playerItems.length} / {instance.max_players}</span></div>{playerContent}</Card><Card><div className="card-heading"><div><span className="eyebrow">ACTIVE BANS</span><h2>封禁记录</h2></div><span className="table-count">{bans.data?.items.length || 0} 条</span></div>{bans.isPending ? <LoadingBlock /> : bans.error ? <ErrorState error={bans.error} retry={() => void bans.refetch()} /> : bans.data.items.length ? <div className="data-table-wrap"><table className="data-table"><thead><tr><th>Steam ID</th><th>原因</th><th>创建时间</th><th>到期时间</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>{bans.data.items.map((ban) => <BanRow key={ban.id} ban={ban} unban={() => action.mutate({ type: "unban_player", payload: { steam_id: ban.steam_id } })} busy={action.isPending} />)}</tbody></table></div> : <EmptyState title="没有有效封禁" description="限时封禁到期后会自动从清单移除；永久封禁需要手动解除。" />}</Card><BanPlayerModal instance={instance} player={selected} onClose={() => setSelected(null)} /></div>;
}

function BanRow({ ban, unban, busy }: { ban: PlayerBan; unban: () => void; busy: boolean }) {
  return <tr><td><code>{ban.steam_id}</code></td><td>{ban.reason || "管理员封禁"}</td><td>{dateTime(ban.created_at)}</td><td>{ban.expires_at ? dateTime(ban.expires_at) : "永久"}</td><td className="table-actions"><Button variant="ghost" onClick={() => { if (window.confirm(`确认解除 ${ban.steam_id} 的封禁？`)) unban(); }} loading={busy}><RotateCcw size={14} />解封</Button></td></tr>;
}

function BanPlayerModal({ instance, player, onClose }: { instance: InstanceRecord; player: Player | null; onClose: () => void }) {
  const toast = useToast();
  const client = useQueryClient();
  const [durationMinutes, setDurationMinutes] = useState(0);
  const [reason, setReason] = useState("违反服务器规则");
  useEffect(() => { if (player) { setDurationMinutes(0); setReason("违反服务器规则"); } }, [player]);
  const ban = useMutation({ mutationFn: () => api.instanceAction(instance.id, "ban_player", { steam_id: player?.steam_id, name: player?.name, duration_minutes: durationMinutes, reason: reason.trim() }), onSuccess: () => { toast.push(durationMinutes ? "限时封禁任务已提交" : "永久封禁任务已提交"); void client.invalidateQueries({ queryKey: ["instances", instance.id, "bans"] }); onClose(); }, onError: (error) => toast.push(error.message, "error") });
  return <Modal open={Boolean(player)} onClose={onClose} title={`封禁 ${player?.name || "玩家"}`} description={`${player?.steam_id || "缺少 Steam ID"}；0 分钟表示永久封禁。`} footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button variant="danger" onClick={() => ban.mutate()} loading={ban.isPending} disabled={!player?.steam_id || durationMinutes < 0 || durationMinutes > 525600 || !reason.trim()}><Ban size={15} />确认封禁</Button></>}><div className="form-stack"><Field label="时长"><select value={durationMinutes} onChange={(event) => setDurationMinutes(Number(event.target.value))}><option value={0}>永久</option><option value={60}>1 小时</option><option value={1440}>1 天</option><option value={10080}>7 天</option><option value={43200}>30 天</option></select></Field><Field label="原因"><input value={reason} onChange={(event) => setReason(event.target.value)} maxLength={200} /></Field><div className="form-warning"><ShieldAlert size={17} />封禁通过 RCON 写入服务器 ban 列表，并在任务成功后记录到不可越权查看的实例范围清单。</div></div></Modal>;
}

function ConfigTab({ instance }: { instance: InstanceRecord }) {
	const maps = useQuery({ queryKey: ["maps", instance.node_id], queryFn: () => api.maps(instance.node_id) });
	const assignedMapOptions = (maps.data?.items || []).filter((item) => item.assignments?.some((assignment) => assignment.instance_id === instance.id)).flatMap((item) => (item.bsps || []).filter((bsp) => Boolean(bsp.start_name)).map((bsp) => ({ value: bsp.start_name!, label: item.name })));
  const [config, setConfig] = useState<InstanceConfig>(() => ({
    ...instance.config,
    cvars: { ...instance.config.cvars },
    extra_args: [...(instance.config.extra_args || [])],
  }));
  const [port, setPort] = useState(instance.port);
  const [advanced, setAdvanced] = useState(false);
  const [extraArgs, setExtraArgs] = useState(
    (instance.config.extra_args || []).join(" "),
  );
  const [cvars, setCvars] = useState(
    Object.entries(instance.config.cvars || {})
      .map(([key, value]) => `${key} ${value}`)
      .join("\n"),
  );
  const [passwordChange, setPasswordChange] = useState<RCONPasswordChange>({
    kind: "unchanged",
  });
  const toast = useToast();
  const client = useQueryClient();
  const publicAddress = instance.node_public_address?.trim() || "";
  const publicStatusBlocked = config.public_status && !publicAddress;
  const portChanged = port !== instance.port;
  const portEditable = instance.status === "stopped";
  const portInvalid = !Number.isInteger(port) || port < 1 || port > 65535;
  const portChangeBlocked = portChanged && !portEditable;
  const save = useMutation({
    mutationFn: () => {
      if (publicStatusBlocked)
        throw new ApiError(
          "请先为所属节点设置公开连接地址",
          400,
          "public_address_required",
        );
      if (portInvalid)
        throw new ApiError("游戏端口必须是 1–65535 的整数", 400, "invalid_config");
      if (portChangeBlocked)
        throw new ApiError("实例运行中不能修改游戏端口", 409, "conflict");
      return api.updateInstance(
        instance.id,
        buildInstanceConfigUpdate(
          instance.version,
          {
            ...config,
            extra_args: splitArgs(extraArgs),
            cvars: parseCVars(cvars),
          },
          passwordChange,
          portChanged ? port : undefined,
        ),
      );
    },
    onSuccess: (result) => {
      setPasswordChange({ kind: "unchanged" });
      client.setQueryData(["instances", instance.id], result.instance);
      toast.push("配置已保存");
      void client.invalidateQueries({ queryKey: ["instances", instance.id] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const tickrateEnabled = /(^|\s)-tickrate(?:\s|=|$)/i.test(extraArgs);
  const tickrateValue = readTickrate(extraArgs) ?? defaultTickrate;
  const passwordValue =
    passwordChange.kind === "set" ? passwordChange.value : "";
  const passwordHint =
    passwordChange.kind === "unchanged"
      ? instance.rcon_configured
        ? "RCON 密码已保存；不输入会保持不变。该值需与 server.cfg 一致；刚修改 server.cfg 时需重启服务器后才会生效。"
        : "当前未保存 RCON 密码；可留空，但 RCON 功能将不可用。"
      : passwordChange.kind === "clear"
        ? "保存后将清除面板保存的 RCON 凭据。"
        : "保存后将替换面板使用的 RCON 凭据。";
  const changePublicStatus = (checked: boolean) => {
    if (checked && !publicAddress) {
      toast.push("请先在节点页设置公开连接地址", "error");
      return;
    }
    setConfig({ ...config, public_status: checked });
  };
  const discard = () => {
    setConfig({
      ...instance.config,
      cvars: { ...instance.config.cvars },
      extra_args: [...(instance.config.extra_args || [])],
    });
    setExtraArgs((instance.config.extra_args || []).join(" "));
    setPort(instance.port);
    setCvars(
      Object.entries(instance.config.cvars || {})
        .map(([key, value]) => `${key} ${value}`)
        .join("\n"),
    );
    setPasswordChange({ kind: "unchanged" });
  };
  return (
    <div className="config-layout">
      <Card>
        <div className="card-heading">
          <div>
            <span className="eyebrow">SERVER CONFIG</span>
            <h2>结构化配置</h2>
          </div>
        </div>
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            save.mutate();
          }}
        >
          <div className="form-grid">
            <Field label="服务器名称">
              <input
                value={config.hostname}
                onChange={(event) =>
                  setConfig({ ...config, hostname: event.target.value })
                }
                required
              />
            </Field>
            <Field
              label="游戏端口"
              hint={portEditable
                ? "仅停止状态可修改；范围 1–65535，不能与 SourceTV 或同节点其他实例端口冲突。"
                : "实例运行中不能修改游戏端口；停止实例后可编辑。"}
            >
              <input
                type="number"
                min={1}
                max={65535}
                step={1}
                value={port}
                disabled={!portEditable}
                onChange={(event) => setPort(Number(event.target.value))}
                required
              />
            </Field>
          </div>
          <div className="secret-config-field">
            <Field label="RCON 密码" hint={passwordHint}>
              <input
                type="password"
                value={passwordValue}
                disabled={passwordChange.kind === "clear"}
                placeholder={
                  passwordChange.kind === "unchanged"
                    ? instance.rcon_configured ? "已保存（输入新密码可替换）" : "尚未设置"
                    : ""
                }
                onChange={(event) =>
                  setPasswordChange(
                    event.target.value
                      ? { kind: "set", value: event.target.value }
                      : { kind: "unchanged" },
                  )
                }
                autoComplete="new-password"
              />
            </Field>
            <div className="secret-config-actions">
              <button
                type="button"
                aria-pressed={passwordChange.kind === "clear"}
                onClick={() => setPasswordChange({ kind: "clear" })}
              >
                清除 RCON 密码
              </button>
              {passwordChange.kind !== "unchanged" && (
                <button
                  type="button"
                  onClick={() => setPasswordChange({ kind: "unchanged" })}
                >
                  保持现有 RCON 密码
                </button>
              )}
            </div>
          </div>
          <div className="form-grid form-grid--three">
            <Field label="模式">
              <select
                value={config.mode}
                onChange={(event) =>
                  setConfig({ ...config, mode: event.target.value })
                }
              >
                {gameModes.map((item) => <option value={item.value} key={item.value}>{item.label}</option>)}
              </select>
            </Field>
            <Field label="难度">
              <select
                value={config.difficulty}
                onChange={(event) =>
                  setConfig({ ...config, difficulty: event.target.value })
                }
              >
                {gameDifficulties.map((item) => <option value={item.value} key={item.value}>{item.label}</option>)}
              </select>
            </Field>
            <Field label="最大玩家">
              <input
                type="number"
                min={1}
                max={32}
                value={config.max_players}
                onChange={(event) =>
                  setConfig({
                    ...config,
                    max_players: Number(event.target.value),
                  })
                }
              />
            </Field>
          </div>
          <div>
            <Field label="初始地图">
              <select
                value={config.start_map}
                onChange={(event) =>
                  setConfig({ ...config, start_map: event.target.value })
                }
              >
                {!officialMapGroups.some((group) => group.maps.some(([value]) => value === config.start_map)) && !assignedMapOptions.some((item) => item.value === config.start_map) && <option value={config.start_map}>{config.start_map}</option>}
                {officialMapGroups.map((group) => <optgroup label={group.campaign} key={group.campaign}>{group.maps.map(([value, label]) => <option value={value} key={value}>{label} · {value}</option>)}</optgroup>)}
				{assignedMapOptions.length > 0 && <optgroup label="已分配地图">{assignedMapOptions.map((item) => <option value={item.value} key={`${item.label}-${item.value}`}>{item.label} · {item.value}</option>)}</optgroup>}
              </select>
            </Field>
          </div>
          <Field label="Steam 组 ID">
            <input
              value={config.steam_group_id || ""}
              onChange={(event) =>
                setConfig({ ...config, steam_group_id: event.target.value })
              }
            />
          </Field>
          <div className="toggle-grid">
            <Toggle
              checked={config.auto_restart}
              onChange={(checked) =>
                setConfig({ ...config, auto_restart: checked })
              }
              label="崩溃自动重启"
              description="Agent 在中心离线时仍会执行"
            />
            <Toggle
              checked={config.public_status}
              onChange={changePublicStatus}
              label="公开状态页"
              description={
                publicAddress
                  ? `玩家连接地址：${publicAddress}:${port}`
                  : "需要先为所属节点设置公开连接地址"
              }
            />
          </div>
          {!publicAddress && (
            <div className="form-warning">
              <ShieldAlert size={17} />
              <span>
                公开状态页需要可供玩家访问的节点地址。请先前往{" "}
                <Link to="/nodes">节点管理</Link> 设置域名或公网 IP。
              </span>
            </div>
          )}
          <button
            type="button"
            className="advanced-toggle"
            onClick={() => setAdvanced((value) => !value)}
          >
            <Code2 size={16} />
            高级配置
            <ChevronRight size={16} className={advanced ? "rotate" : ""} />
          </button>
          {advanced && (
            <div className="advanced-fields">
              <div className="form-grid">
                <Toggle checked={tickrateEnabled} onChange={(checked) => setExtraArgs(writeTickrate(extraArgs, checked ? tickrateValue : null))} label="启用 Tickrate" description="作为 -tickrate 启动参数传递" />
                {tickrateEnabled && <Field label="Tickrate 数值" hint={`${minTickrate}–${maxTickrate} 的整数`}><input type="number" min={minTickrate} max={maxTickrate} step={1} value={tickrateValue} onChange={(event) => setExtraArgs(writeTickrate(extraArgs, Number(event.target.value)))} required /></Field>}
              </div>
              <Field
                label="额外启动参数"
                hint="参数会按数组安全传递，不经过 Shell。"
              >
                <textarea
                  rows={3}
                  value={extraArgs}
                  onChange={(event) => setExtraArgs(event.target.value)}
                  placeholder="+sv_lan 0"
                />
              </Field>
              <Field
                label="自定义 CVar"
                hint="每行一个变量，例如 sv_consistency 1"
              >
                <textarea
                  className="code-input"
                  rows={8}
                  value={cvars}
                  onChange={(event) => setCvars(event.target.value)}
                />
              </Field>
            </div>
          )}
          <div className="form-actions">
            <Button
              type="submit"
              loading={save.isPending}
              disabled={publicStatusBlocked || portInvalid || portChangeBlocked}
            >
              <Save size={16} />
              保存配置
            </Button>
            <Button type="button" variant="secondary" onClick={discard}>
              放弃更改
            </Button>
          </div>
        </form>
      </Card>
      <Card className="config-note">
        <FileCode2 size={21} />
        <div>
          <strong>游戏加载 server.cfg</strong>
          <span>
            面板不再生成或注入 l4d2panel.cfg；启动时只固定加载你维护的 server.cfg。RCON 密码仅作为中心端连接凭据保存，请在 server.cfg 中配置相同值。
          </span>
        </div>
      </Card>
    </div>
  );
}

function ModsTab({ instance }: { instance: InstanceRecord }) {
  return <PackagesTab instance={instance} section="mods" />;
}

function PluginsTab({ instance }: { instance: InstanceRecord }) {
  return <PackagesTab instance={instance} section="plugins" />;
}

type PackageSection = "mods" | "plugins";

function PackagesTab({ instance, section }: { instance: InstanceRecord; section: PackageSection }) {
  const [open, setOpen] = useState(false);
  const packages = useQuery({ queryKey: ["instances", instance.id, "packages"], queryFn: () => api.packages(instance.id), refetchInterval: 15_000 });
  const pluginSection = section === "plugins";
  const items = packages.data?.items.filter((item) => pluginSection ? item.kind !== "vpk" && item.kind !== "workshop" : item.kind === "vpk" || item.kind === "workshop") || [];
  const noun = pluginSection ? "插件" : "模组";
  const description = pluginSection ? "支持 SourceMod 插件、SourceMod/Metamod 整包与第三方原生扩展。" : "支持 VPK、Workshop 项目与合集，以及 QQ闪传 VPK 批量导入。";
  return <Card><div className="card-heading"><div><span className="eyebrow">{pluginSection ? "SERVER PLUGINS" : "ADD-ONS"}</span><h2>{pluginSection ? "插件与服务端组件" : "模组"}</h2></div><Button onClick={() => setOpen(true)}><Plus size={16} />安装{noun}</Button></div>{packages.isPending ? <LoadingBlock /> : packages.error ? <OptionalUnavailable error={packages.error} title={`${noun}清单尚不可用`} description={`当前控制端未开放${noun}清单接口；安装任务仍可通过面板提交。`} action={<Button onClick={() => setOpen(true)}><Plus size={16} />提交安装任务</Button>} /> : items.length ? <div className="package-grid">{items.map((item) => <PackageCard key={item.id} item={item} instanceId={instance.id} />)}</div> : <EmptyState title={`还没有安装${noun}`} description={description} action={<Button onClick={() => setOpen(true)}><Plus size={16} />安装第一个{noun}</Button>} />}<InstallPackageModal open={open} instance={instance} section={section} onClose={() => setOpen(false)} /></Card>;
}

function PackageCard({ item, instanceId }: { item: PackageRecord; instanceId: string }) {
  const toast = useToast();
  const client = useQueryClient();
  const [selectedVersion, setSelectedVersion] = useState("");
  const toggle = useMutation({ mutationFn: () => api.instanceAction(instanceId, "toggle_package", { package_id: item.id, enabled: !item.enabled }), onSuccess: () => { toast.push(item.enabled ? "组件停用任务已提交" : "组件启用任务已提交"); void client.invalidateQueries({ queryKey: ["instances", instanceId, "packages"] }); }, onError: (error) => toast.push(error.message, "error") });
  const toggleable = /\.(?:vpk|smx)$/i.test(item.path || "");
  const deletable = toggleable && Boolean(item.sha256);
  const remove = useMutation({
    mutationFn: () => api.deletePackage(instanceId, item.id),
    onSuccess: () => {
      toast.push("组件删除任务已提交");
      void client.invalidateQueries({ queryKey: ["jobs"] });
      void client.invalidateQueries({ queryKey: ["instances", instanceId, "packages"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const versions = useQuery({
    queryKey: ["instances", instanceId, "packages", item.id, "versions"],
    queryFn: () => api.packageVersions(instanceId, item.id),
    enabled: toggleable,
    refetchInterval: toggleable ? 15_000 : false,
  });
  useEffect(() => {
    if (!versions.data?.items.length) {
      setSelectedVersion("");
      return;
    }
    if (!versions.data.items.some((version) => version.id === selectedVersion)) setSelectedVersion(versions.data.items[0].id);
  }, [selectedVersion, versions.data]);
  const rollback = useMutation({
    mutationFn: () => api.rollbackPackageVersion(instanceId, item.id, selectedVersion),
    onSuccess: () => {
      toast.push("版本回退任务已提交；Agent 会停服、校验并恢复原运行状态");
      void client.invalidateQueries({ queryKey: ["instances", instanceId, "packages"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const selected = versions.data?.items.find((version) => version.id === selectedVersion);
  return <div className="package-card">
    <div className={`package-icon package-icon--${item.kind}`}><PackageOpen size={20} /></div>
    <div className="package-card__identity"><strong>{item.name}</strong><span>{item.kind.toUpperCase()} · {item.version || "版本未知"}</span>{item.sha256 && <code>{item.sha256.slice(0, 16)}…</code>}</div>
    <div className="package-card__controls">
      <div className="package-card__actions">
        {toggleable ? <button className={`switch ${item.enabled ? "switch--on" : ""}`} disabled={toggle.isPending || remove.isPending} onClick={() => toggle.mutate()} aria-label={`${item.enabled ? "停用" : "启用"} ${item.name}`}><span /></button> : <span className="package-installed">已安装</span>}
        {deletable && <button className="package-delete-button" disabled={remove.isPending || toggle.isPending} aria-label={`删除 ${item.name}`} title="删除组件" onClick={() => {
          if (window.confirm(`确认永久删除 ${item.name}？文件及面板中的版本历史会一并移除；运行中的服务器可能需要重启后才能完全卸载。`)) remove.mutate();
        }}><Trash2 size={14} /></button>}
      </div>
      {toggleable && versions.data?.items.length ? <div className="package-version-rollback">
        <select value={selectedVersion} onChange={(event) => setSelectedVersion(event.target.value)} aria-label={`${item.name} 可回退版本`} disabled={!item.enabled || rollback.isPending}>
          {versions.data.items.map((version) => <option value={version.id} key={version.id}>{version.version || "未标记"} · {version.sha256.slice(0, 10)} · {dateTime(version.created_at)}</option>)}
        </select>
        <button className="package-rollback-button" disabled={!item.enabled || !selectedVersion || rollback.isPending} title={!item.enabled ? "请先启用组件" : `恢复 ${selected?.version || "选中版本"}`} onClick={() => {
          if (selected && window.confirm(`确认将 ${item.name} 回退到 ${selected.version || selected.sha256.slice(0, 12)}？当前文件会作为新的可回退版本保留。`)) rollback.mutate();
        }}><RotateCcw size={13} />{rollback.isPending ? "提交中" : "回退"}</button>
      </div> : toggleable && versions.error ? <small className="package-version-error">版本历史暂不可用</small> : null}
    </div>
  </div>;
}

function InstallPackageModal({ open, instance, section, onClose }: { open: boolean; instance: InstanceRecord; section: PackageSection; onClose: () => void }) {
  const pluginSection = section === "plugins";
  const [kind, setKind] = useState(pluginSection ? "plugin" : "workshop");
  const [source, setSource] = useState("");
  const [upload, setUpload] = useState<{ name: string; size: number; contentBase64: string } | null>(null);
  const [risk, setRisk] = useState(false);
  const toast = useToast();
  const client = useQueryClient();
  const official = kind === "sourcemod" || kind === "metamod";
  const qqFlash = kind === "qq_flash";
  const riskRequired = ["native", "sourcemod_url", "metamod_url"].includes(kind);
  const showRisk = riskRequired || kind === "plugin";
  const nativePlugin = kind === "plugin" && /\.(?:dll|so)(?:$|\?)/i.test(upload?.name || source.trim());
  const needsRisk = riskRequired || nativePlugin;
  const canUpload = !official && kind !== "workshop" && !qqFlash;
  const hasSource = Boolean(source.trim() || upload);
  const accept = kind === "vpk_url" ? ".vpk" : kind === "plugin" || kind === "native" ? ".smx,.dll,.so" : ".zip,.tar,.gz,.bz2,.xz";
  const chooseUpload = async (file?: File) => {
    if (!file) return;
    if (file.size === 0 || file.size > 8 * 1024 * 1024) {
      toast.push("上传包必须大于 0 且不能超过 8 MiB", "error");
      return;
    }
    try {
      const contentBase64 = bytesToBase64(new Uint8Array(await file.arrayBuffer()));
      setUpload({ name: file.name, size: file.size, contentBase64 });
      setSource("");
    } catch {
      toast.push("无法读取所选文件", "error");
    }
  };
  const resetAndClose = () => {
    setSource("");
    setUpload(null);
    setRisk(false);
    onClose();
  };
  const install = useMutation({
    mutationFn: () => qqFlash
      ? api.importQQFlashFile(instance.id, source)
      : api.instanceAction(instance.id, "install_package", {
        kind,
        source: source.trim() || undefined,
        name: upload?.name,
        content_base64: upload?.contentBase64,
        acknowledge_native_risk: risk,
      }),
    onSuccess: () => {
      toast.push(qqFlash ? "QQ闪传 VPK 导入任务已提交" : `${pluginSection ? "插件" : "模组"}安装任务已提交`);
      void client.invalidateQueries({ queryKey: ["jobs"] });
      void client.invalidateQueries({ queryKey: ["instances", instance.id, "packages"] });
      resetAndClose();
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  return <Modal open={open} onClose={resetAndClose} title={pluginSection ? "安装插件或服务端组件" : "安装模组"} description={qqFlash ? "供玩家通过 QQ闪传提交 VPK；节点暂存并检查完整分享，安全解压后再安装，不经过 Control 或浏览器。" : "HTTPS 下载和本机上传都会经过大小、哈希、路径与内容格式检查。"} footer={<><Button variant="secondary" onClick={resetAndClose}>取消</Button><Button onClick={() => install.mutate()} loading={install.isPending} disabled={(!official && !hasSource) || (needsRisk && !risk)}>{qqFlash ? <CloudDownload size={16} /> : <Download size={16} />}{qqFlash ? "导入 VPK" : "提交安装"}</Button></>}>
    <div className="form-stack">
      <Field label="类型"><select value={kind} onChange={(event) => { setKind(event.target.value); setSource(""); setUpload(null); setRisk(false); }}>{pluginSection ? <><option value="plugin">SourceMod 插件文件</option><option value="sourcemod">官方 SourceMod 稳定版</option><option value="metamod">官方 Metamod:Source 稳定版</option><option value="sourcemod_url">第三方 SourceMod 整包</option><option value="metamod_url">第三方 Metamod 整包</option><option value="native">第三方原生扩展</option></> : <><option value="workshop">Workshop 项目 / 合集 ID</option><option value="vpk_url">VPK 文件</option><option value="qq_flash">QQ闪传（玩家上传 VPK）</option></>}</select></Field>
      {qqFlash ? <>
        <Field label="固定目标目录"><input value="left4dead2/addons" readOnly /></Field>
        <Field label="QQ闪传分享链接" hint="可直接粘贴 QQ 分享文本；支持直接 VPK，以及最多嵌套 3 层的 ZIP、7Z、RAR 压缩包。"><textarea rows={4} value={source} onChange={(event) => setSource(event.target.value)} placeholder="链接：https://qfile.qq.com/q/Example123" spellCheck={false} /></Field>
        <div className="form-note"><CloudDownload size={18} /><span><strong>仅接收经过校验的 VPK</strong><small>节点会先完整暂存分享，限制下载与解压总量，拒绝加密包、危险路径和任何非 VPK 文件。成功后每个 VPK 会单独出现在本页，可启停和删除。</small></span></div>
      </> : <>
        {official ? <div className="form-note"><ShieldCheck size={18} /><span><strong>自动解析官方稳定版</strong><small>Agent 只接受 AlliedModders 官方白名单地址，并按节点系统选择压缩包。</small></span></div> : <Field label={kind === "workshop" ? "项目或合集 ID" : "HTTPS 来源"} hint={canUpload ? "填写 URL，或在下方从本机选择一个文件" : undefined}><input value={source} onChange={(event) => { setSource(event.target.value); setUpload(null); }} placeholder={kind === "workshop" ? "例如：123456789" : "https://example.org/plugin.zip"} /></Field>}
        {canUpload && <Field label="从本机上传" hint="单个文件上限 8 MiB；载荷会加密保存，并只发送给目标节点"><input type="file" accept={accept} onChange={(event) => { void chooseUpload(event.target.files?.[0]); event.currentTarget.value = ""; }} /></Field>}
        {upload && <div className="binary-file-note"><Upload size={20} /><div><strong>{upload.name}</strong><span>{bytes(upload.size)} · 已准备安全上传</span></div><Button variant="secondary" onClick={() => setUpload(null)}>移除</Button></div>}
        {showRisk && <label className="risk-check"><input type="checkbox" checked={risk} onChange={(event) => setRisk(event.target.checked)} /><ShieldAlert size={19} /><span><strong>我了解第三方原生代码风险</strong><small>{needsRisk ? "此包可能包含或覆盖节点执行的二进制扩展，必须确认后安装。" : "普通 SMX 不要求确认；DLL/SO 原生扩展必须确认。"}</small></span></label>}
        <div className="form-note"><strong>自动回退</strong><span>组件升级前会创建快照；重启后的版本检测失败时，Agent 恢复上一版本。</span></div>
      </>}
    </div>
  </Modal>;
}

function FilesTab({ instance }: { instance: InstanceRecord }) {
  const [path, setPath] = useState("");
  const [editor, setEditor] = useState<{ path: string; isNew: boolean } | null>(null);
  const [serverShortcut, setServerShortcut] = useState(false);
  const files = useQuery({ queryKey: ["instances", instance.id, "files", path], queryFn: () => api.files(instance.id, path) });
  const allowedRoots = ["left4dead2/addons", "left4dead2/cfg"];
  useEffect(() => {
    if (!serverShortcut || path !== "left4dead2/cfg" || files.isPending || files.error) return;
    const serverPath = "left4dead2/cfg/server.cfg";
    const exists = files.data.items.some((item) => item.type === "file" && item.path.toLowerCase() === serverPath);
    setEditor({ path: serverPath, isNew: !exists });
    setServerShortcut(false);
  }, [files.data, files.error, files.isPending, path, serverShortcut]);
  if (files.isPending) return <LoadingBlock label="正在读取实例目录…" />;
  if (files.error) return <OptionalUnavailable error={files.error} title="文件浏览暂不可用" description="当前控制端未开放受限文件列表接口。Agent 仍会强制执行实例根目录边界。" />;
  const newPath = `${path || "left4dead2/cfg"}/new-file.cfg`;
  return <><Card><div className="card-heading"><div><span className="eyebrow">SANDBOXED FILES</span><h2>文件管理</h2></div><Button variant="secondary" onClick={() => setEditor({ path: newPath, isNew: true })}><Upload size={16} />上传或新建</Button></div><div className="file-browser"><aside><button onClick={() => { setServerShortcut(true); setPath("left4dead2/cfg"); }}><FileCode2 size={15} />server.cfg</button>{allowedRoots.map((root) => <button className={path === root ? "active" : ""} onClick={() => setPath(root)} key={root}><Folder size={15} />{root}</button>)}</aside><div className="file-list"><div className="file-path"><button onClick={() => setPath("")}>实例目录</button>{path.split("/").filter(Boolean).map((part, index, all) => <span key={`${part}-${index}`}><ChevronRight size={13} /><button onClick={() => setPath(all.slice(0, index + 1).join("/"))}>{part}</button></span>)}</div>{files.data.items.length ? files.data.items.map((item) => <button className="file-row" key={item.path} onClick={() => item.type === "directory" ? setPath(item.path) : setEditor({ path: item.path, isNew: false })}><span className={`file-type file-type--${item.type}`}>{item.type === "directory" ? <Folder size={18} /> : <File size={18} />}</span><strong>{item.name}</strong><span>{item.type === "file" ? bytes(item.size) : "文件夹"}</span><time>{dateTime(item.modified_at)}</time><ChevronRight size={15} /></button>) : <EmptyState title="目录为空" description="这个受限目录中还没有文件。" />}</div></div></Card><FileEditorModal instance={instance} editor={editor} onClose={() => { setEditor(null); void files.refetch(); }} /></>;
}

function FileEditorModal({ instance, editor, onClose }: { instance: InstanceRecord; editor: { path: string; isNew: boolean } | null; onClose: () => void }) {
  const toast = useToast();
  const client = useQueryClient();
  const [relative, setRelative] = useState("");
  const [content, setContent] = useState("");
  const [binary, setBinary] = useState<string | null>(null);
  const file = useQuery({
    queryKey: ["instances", instance.id, "file-content", editor?.path],
    queryFn: () => api.fileContent(instance.id, editor?.path || ""),
    enabled: Boolean(editor && !editor.isNew),
    refetchInterval: (query) => query.state.data?.status === "loading" ? 1_000 : false,
  });
  useEffect(() => {
    if (!editor) return;
    setRelative(editor.path);
    setContent("");
    setBinary(null);
  }, [editor]);
  useEffect(() => {
    if (!file.data || file.data.status === "loading") return;
    if (file.data.content != null) {
      setContent(file.data.content);
      setBinary(null);
    } else if (file.data.content_base64 != null) {
      setBinary(file.data.content_base64);
    }
  }, [file.data]);
  const save = useMutation({
    mutationFn: () => api.writeFile(instance.id, relative, content, editor?.isNew ? undefined : file.data?.sha256, binary || undefined),
    onSuccess: (result) => {
      toast.push("文件写入任务已提交");
      void client.invalidateQueries({ queryKey: ["jobs"] });
      onClose();
      return result;
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteFile(instance.id, relative, file.data?.sha256),
    onSuccess: () => { toast.push("文件删除任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); onClose(); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const loadUpload = async (selected: File) => {
    if (selected.size > 8 * 1024 * 1024) { toast.push("单个文件不能超过 8 MiB", "error"); return; }
    setRelative(`${relative.split("/").slice(0, -1).join("/")}/${selected.name}`.replace(/^\//, ""));
    const raw = new Uint8Array(await selected.arrayBuffer());
    const text = new TextDecoder("utf-8", { fatal: false }).decode(raw);
    const looksTextual = !raw.includes(0) && !text.includes("�");
    if (looksTextual) { setContent(text); setBinary(null); } else { setContent(""); setBinary(bytesToBase64(raw)); }
  };
  const download = () => {
    const encoded = file.data?.content_base64;
    if (!encoded) return;
    const raw = Uint8Array.from(atob(encoded), (character) => character.charCodeAt(0));
    const url = URL.createObjectURL(new Blob([raw]));
    const anchor = document.createElement("a");
    anchor.href = url; anchor.download = relative.split("/").pop() || "download"; anchor.click();
    URL.revokeObjectURL(url);
  };
  const loading = Boolean(editor && !editor.isNew && (file.isPending || file.data?.status === "loading"));
  return <Modal open={Boolean(editor)} onClose={onClose} title={editor?.isNew ? "上传或新建文件" : "编辑受限文件"} description="Agent 会再次校验实例边界、重解析点和 8 MiB 大小限制。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button>{!editor?.isNew && <Button variant="danger" onClick={() => { if (window.confirm(`确认删除 ${relative}？`)) remove.mutate(); }} loading={remove.isPending}><Trash2 size={15} />删除</Button>}<Button onClick={() => save.mutate()} loading={save.isPending} disabled={!relative || loading}><Save size={16} />提交写入</Button></>}>
    {loading ? <LoadingBlock label="正在从节点安全读取文件…" /> : file.error ? <ErrorState error={file.error} retry={() => void file.refetch()} /> : <div className="form-stack"><Field label="实例内相对路径"><input value={relative} onChange={(event) => setRelative(event.target.value)} disabled={!editor?.isNew} /></Field>{editor?.isNew && <Field label="从本机选择文件" hint="文本文件可继续编辑；二进制文件会以 Base64 安全传输"><input type="file" onChange={(event) => { const selected = event.target.files?.[0]; if (selected) void loadUpload(selected); }} /></Field>}{binary != null ? <div className="binary-file-note"><File size={20} /><div><strong>二进制文件</strong><span>{binary.length ? `Base64 载荷约 ${bytes(Math.floor(binary.length * 0.75))}` : "空文件"}</span></div>{!editor?.isNew && <Button variant="secondary" onClick={download}><Download size={15} />下载</Button>}</div> : <Field label="文件内容" hint="保存使用读取时的 SHA-256 做并发保护，文件被外部修改时会拒绝覆盖。"><textarea className="file-editor" rows={18} value={content} onChange={(event) => setContent(event.target.value)} spellCheck={false} /></Field>}{file.data?.sha256 && <div className="file-digest"><span>SHA-256</span><code>{file.data.sha256}</code></div>}</div>}
  </Modal>;
}

function bytesToBase64(value: Uint8Array) {
  let binary = "";
  const chunk = 0x8000;
  for (let offset = 0; offset < value.length; offset += chunk) {
    binary += String.fromCharCode(...value.subarray(offset, offset + chunk));
  }
  return btoa(binary);
}

function wait(milliseconds: number) {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

async function waitForBackupUploadJob(jobID: string) {
  for (let attempt = 0; attempt < 600; attempt += 1) {
    const job = await api.job(jobID);
    if (["succeeded", "failed", "canceled"].includes(job.status)) return job;
    await wait(500);
  }
  throw new ApiError("等待备份分片传输超时", 408, "backup_upload_timeout");
}

async function waitForBackupUploadOffset(instanceID: string, uploadID: string, expectedOffset: number) {
  for (let attempt = 0; attempt < 120; attempt += 1) {
    const upload = await api.backupUpload(instanceID, uploadID);
    if (upload.received_size >= expectedOffset || upload.status === "ready") return upload;
    if (["failed", "expired"].includes(upload.status)) throw new ApiError("节点未能暂存备份分片", 409, "backup_upload_failed");
    await wait(250);
  }
  throw new ApiError("备份分片已执行，但中心端偏移量未及时更新", 408, "backup_upload_projection_timeout");
}

async function waitForBackupUploadReady(instanceID: string, uploadID: string) {
  for (let attempt = 0; attempt < 120; attempt += 1) {
    const upload = await api.backupUpload(instanceID, uploadID);
    if (upload.status === "ready") return upload;
    if (["failed", "expired"].includes(upload.status)) throw new ApiError("节点未能校验完整备份", 409, "backup_upload_failed");
    await wait(250);
  }
  throw new ApiError("备份已上传，但节点整包校验等待超时", 408, "backup_upload_validation_timeout");
}

async function waitForBackupRestoreProjection(instanceID: string, uploadID: string, expected: "succeeded" | "failed") {
  for (let attempt = 0; attempt < 120; attempt += 1) {
    const upload = await api.backupUpload(instanceID, uploadID);
    if (upload.status === expected || upload.status === "expired") return upload;
    await wait(250);
  }
  throw new ApiError("恢复任务已结束，但上传会话状态尚未收敛", 408, "backup_restore_projection_timeout");
}

function BackupsTab({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const backups = useQuery({ queryKey: ["instances", instance.id, "backups"], queryFn: () => api.backups(instance.id) });
  const toast = useToast();
  const client = useQueryClient();
  const [uploadOpen, setUploadOpen] = useState(false);
  const canCreate = canAccessInstance(user, instance, "backups.create");
  const canRestorePermission = canAccessInstance(user, instance, "backups.restore");
  const restoreReady = instance.status === "stopped" || instance.status === "degraded";
  const canRestore = canRestorePermission && restoreReady;
  const action = useMutation({ mutationFn: ({ type, backup }: { type: string; backup?: BackupRecord }) => api.instanceAction(instance.id, type, backup ? { backup_id: backup.id } : undefined), onSuccess: (_data, values) => { toast.push(values.type === "backup" ? "备份任务已提交" : "恢复任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const restoreReason = !canRestorePermission ? "当前账号没有备份恢复权限" : "请先停止实例再恢复";
  const backupButton = <Button onClick={() => action.mutate({ type: "backup" })} loading={action.isPending} disabled={!canCreate} title={!canCreate ? "当前账号没有备份创建权限" : undefined}><Archive size={16} />立即备份</Button>;
  const alternateBackupButton = (label: string) => <Button onClick={() => action.mutate({ type: "backup" })} loading={action.isPending} disabled={!canCreate}><Archive size={16} />{label}</Button>;
  return <><Card><div className="card-heading"><div><span className="eyebrow">LOCAL SNAPSHOTS</span><h2>实例备份</h2></div><div className="backup-heading-actions"><Button variant="secondary" onClick={() => setUploadOpen(true)} disabled={!canRestore} title={canRestore ? "上传其他节点下载的 ZIP 备份并恢复" : restoreReason}><Upload size={16} />上传并恢复</Button>{backupButton}</div></div>{!canRestore && <div className="form-warning backup-restore-warning"><ShieldAlert size={17} />{!canRestorePermission ? "当前账号没有备份恢复权限。" : "恢复会替换实例数据，必须先停止服务器。"}</div>}{backups.isPending ? <LoadingBlock /> : backups.error ? <OptionalUnavailable error={backups.error} title="备份清单尚不可用" description="当前控制端未开放节点本地备份列表；仍可提交新的备份任务。" action={alternateBackupButton("创建备份")} /> : backups.data.items.length ? <div className="backup-list">{backups.data.items.map((backup) => <div className="backup-row" key={backup.id}><div className="backup-icon"><Archive size={19} /></div><div><strong>{backup.name}</strong><span>{dateTime(backup.created_at)} · {backup.platform || "可移植数据"}</span></div><span>{bytes(backup.size)}</span><code>{backup.checksum ? `${backup.checksum.slice(0, 12)}…` : "校验待完成"}</code><Button variant="secondary" disabled={!canRestore} title={canRestore ? "恢复此备份" : restoreReason} onClick={() => action.mutate({ type: "restore", backup })}><RotateCcw size={15} />恢复</Button><BackupDownloadButton instance={instance} backup={backup} /></div>)}</div> : <EmptyState title="还没有备份" description="备份保存在节点本地，包含配置、封禁、插件与 VPK，不包含游戏本体。" action={alternateBackupButton("创建首个备份")} />}<div className="retention-note"><History size={19} /><span><strong>默认保留策略</strong> 每日 04:00，保留 7 个日备份与 4 个周备份。恢复前会自动创建检查点。</span></div></Card><BackupUploadRestoreModal open={uploadOpen} instance={instance} onClose={() => setUploadOpen(false)} /></>;
}

type BackupPlatform = PersistedBackupPlatform;

function BackupUploadRestoreModal({ open, instance, onClose }: { open: boolean; instance: InstanceRecord; onClose: () => void }) {
  const toast = useToast();
  const client = useQueryClient();
  const navigate = useNavigate();
  const file = useRef<File | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const persisted = useRef<PersistedBackupUploadSession | null>(loadBackupUploadSession(instance.id));
  const [resumeSession, setResumeSession] = useState<PersistedBackupUploadSession | null>(persisted.current);
  const [fileMeta, setFileMeta] = useState<{ name: string; size: number } | null>(null);
  const [platform, setPlatform] = useState<BackupPlatform | "">(persisted.current?.platform || "");
  const [progress, setProgress] = useState({ value: 0, label: persisted.current ? "发现未完成会话，请重选同一文件以继续" : "等待选择备份" });
  const persist = (next: PersistedBackupUploadSession) => {
    const updated = { ...next, updatedAt: new Date().toISOString() };
    persisted.current = updated;
    saveBackupUploadSession(updated);
    setResumeSession(updated);
    return updated;
  };
  const forget = () => {
    persisted.current = null;
    setResumeSession(null);
    clearBackupUploadSession(instance.id);
  };
  const resetEphemeral = () => {
    file.current = null;
    if (fileInput.current) fileInput.current.value = "";
    setFileMeta(null);
    setPlatform(persisted.current?.platform || "");
    setProgress({ value: 0, label: persisted.current ? "会话已保留，请重选同一文件以继续" : "等待选择备份" });
    upload.reset();
  };
  useEffect(() => {
    const stored = loadBackupUploadSession(instance.id);
    persisted.current = stored;
    setResumeSession(stored);
    if (open) {
      if (!file.current) setPlatform(stored?.platform || "");
      setProgress({ value: 0, label: stored ? "发现未完成会话，请重选同一文件以继续" : "等待选择备份" });
    }
    if (!stored?.uploadId) return;
    let canceled = false;
    void api.backupUpload(instance.id, stored.uploadId).then((record) => {
      if (canceled) return;
      // Do not let a slow reconciliation response overwrite a new file
      // selection or discard chunk/complete idempotency keys persisted while
      // this request was in flight.
      const current = persisted.current;
      if (!current || current.createKey !== stored.createKey || current.uploadId !== stored.uploadId) return;
      const sameIdentity = record.instance_id === instance.id && record.name === current.name && record.size === current.size
        && record.sha256 === current.sha256 && record.platform === current.platform;
      if (!sameIdentity || record.status === "succeeded" || record.status === "expired") {
        clearBackupUploadSession(instance.id);
        persisted.current = null;
        setResumeSession(null);
        if (open) setProgress({ value: record.status === "succeeded" ? 100 : 0, label: record.status === "succeeded" ? "恢复已完成" : "上传会话已过期，请重新上传" });
        return;
      }
      const refreshed = {
        ...current,
        expiresAt: record.expires_at,
        restoreJobId: record.restore_job_id,
        updatedAt: new Date().toISOString(),
      };
      persisted.current = refreshed;
      saveBackupUploadSession(refreshed);
      setResumeSession(refreshed);
    }).catch(() => {
      // A transient read failure must not discard the only resumable client-side session.
    });
    return () => { canceled = true; };
  }, [instance.id, open]);
  const close = () => {
    if (upload.isPending || abandon.isPending) {
      toast.push(abandon.isPending ? "正在排队清理上传会话，请稍候" : "分块上传正在进行，完成前不能关闭此窗口", "error");
      return;
    }
    resetEphemeral();
    onClose();
  };
  const chooseFile = (selected?: File) => {
    file.current = null;
    setFileMeta(null);
    upload.reset();
    const error = selected ? validateBackupUploadFile(selected.name, selected.size) : "请选择 ZIP 备份文件";
    if (error || !selected) {
      if (error) toast.push(error, "error");
      if (fileInput.current) fileInput.current.value = "";
      return;
    }
    file.current = selected;
    setFileMeta({ name: selected.name, size: selected.size });
    setProgress({ value: 0, label: resumeSession ? "已选择文件；开始后将校验是否可续传" : "已选择备份" });
  };
  const upload = useMutation({
    mutationFn: async () => {
      const selected = file.current;
      if (!selected) throw new ApiError("请选择 ZIP 备份文件", 400, "backup_file_required");
      if (!platform) throw new ApiError("请选择备份来源平台", 400, "backup_platform_required");
      const validation = validateBackupUploadFile(selected.name, selected.size);
      if (validation) throw new ApiError(validation, 400, "invalid_backup_file");

      setProgress({ value: 1, label: "正在分片计算 SHA-256…" });
      const digest = await hashBlobSHA256(selected, (processed, total) => {
        setProgress({ value: Math.max(1, Math.round((processed / Math.max(total, 1)) * 15)), label: `正在校验 ${bytes(processed)} / ${bytes(total)}` });
      });
      const identity = { name: selected.name, size: selected.size, sha256: digest, platform };
      let local = persisted.current;
      if (!local || !sameBackupUploadFile(local, identity)) {
        local = persist({
          version: 1,
          instanceId: instance.id,
          createKey: newBackupUploadKey(`backup-upload:${instance.id}:${digest.slice(0, 16)}`),
          ...identity,
          updatedAt: new Date().toISOString(),
        });
      }

      const applyRecord = (record: BackupUploadRecord) => {
        if (record.instance_id !== instance.id || record.name !== identity.name || record.size !== identity.size
          || record.sha256 !== identity.sha256 || record.platform !== identity.platform) {
          forget();
          throw new ApiError("持久化上传会话与所选文件不一致，已停止续传", 409, "backup_upload_identity_mismatch");
        }
        local = persist({
          ...local!,
          uploadId: record.id,
          expiresAt: record.expires_at,
          restoreJobId: record.restore_job_id,
        });
        return record;
      };
      const createUpload = async () => {
        const created = await api.createBackupUpload(instance.id, identity, local!.createKey);
        return applyRecord(created.upload);
      };
      let session: BackupUploadRecord;
      if (local.uploadId) {
        setProgress({ value: 16, label: "正在确认已保存的上传会话…" });
        try {
          session = applyRecord(await api.backupUpload(instance.id, local.uploadId));
        } catch (error) {
          if (!(error instanceof ApiError) || error.status !== 404) throw error;
          local = persist({
            version: 1,
            instanceId: instance.id,
            createKey: newBackupUploadKey(`backup-upload:${instance.id}:${digest.slice(0, 16)}`),
            ...identity,
            updatedAt: new Date().toISOString(),
          });
          session = await createUpload();
        }
      } else {
        session = await createUpload();
      }
      if (session.status === "expired") {
        local = persist({
          version: 1,
          instanceId: instance.id,
          createKey: newBackupUploadKey(`backup-upload:${instance.id}:${digest.slice(0, 16)}`),
          ...identity,
          updatedAt: new Date().toISOString(),
        });
        session = await createUpload();
        if (session.status === "expired") {
          forget();
          throw new ApiError("上传会话已过期且无法重新创建", 409, "backup_upload_expired");
        }
      }

      const enqueueRestore = async (mayRetryTerminalReplay = true): Promise<{ job_id: string }> => {
        let pending = local!.pendingComplete;
        if (!pending) {
          pending = { idempotencyKey: newBackupUploadKey(`backup-complete:${session.id}:${digest.slice(0, 16)}`) };
          local = persist({ ...local!, pendingComplete: pending });
        }
        setProgress({ value: 96, label: local!.restoreJobId ? "正在重试恢复…" : "整包校验通过，正在排队恢复…" });
        const completed = await api.completeBackupUpload(instance.id, session.id, pending.idempotencyKey);
        local = persist({
          ...local!,
          restoreJobId: completed.job_id,
          pendingComplete: { ...pending, jobId: completed.job_id },
        });
        if (completed.status === "failed" || completed.status === "canceled") {
          session = applyRecord(await waitForBackupRestoreProjection(instance.id, session.id, "failed"));
          local = persist({ ...local!, pendingComplete: undefined, restoreJobId: completed.job_id });
          if (mayRetryTerminalReplay) {
            setProgress({ value: 96, label: "已确认上次恢复失败，正在使用新的幂等键重试…" });
            return enqueueRestore(false);
          }
          throw new ApiError("恢复重试任务立即失败，请检查任务详情", 409, "backup_restore_retry_failed");
        }
        if (completed.status === "succeeded") {
          session = applyRecord(await waitForBackupRestoreProjection(instance.id, session.id, "succeeded"));
          forget();
          setProgress({ value: 100, label: "恢复已完成" });
          return { job_id: completed.job_id };
        }
        setProgress({ value: 100, label: "恢复任务已排队；会话已保留到任务结束" });
        return { job_id: completed.job_id };
      };

      const reconcileRestoreJob = async (jobID: string) => {
        const job = await api.job(jobID);
        if (["queued", "dispatched", "running"].includes(job.status)) {
          setProgress({ value: 100, label: "恢复任务仍在执行" });
          return { done: true, result: { job_id: jobID } };
        }
        const expected = job.status === "succeeded" ? "succeeded" : "failed";
        session = applyRecord(await waitForBackupRestoreProjection(instance.id, session.id, expected));
        if (job.status === "succeeded" && session.status === "succeeded") {
          forget();
          setProgress({ value: 100, label: "恢复已完成" });
          return { done: true, result: { job_id: jobID } };
        }
        if (job.status === "failed" || job.status === "canceled") {
          local = persist({ ...local!, pendingComplete: undefined, restoreJobId: jobID });
          return { done: false as const };
        }
        throw new ApiError("恢复任务状态与上传会话不一致", 409, "backup_restore_state_mismatch");
      };

      if (session.status === "succeeded") {
        const jobID = session.restore_job_id || local.restoreJobId || local.pendingComplete?.jobId || "";
        forget();
        setProgress({ value: 100, label: "恢复已完成" });
        return { job_id: jobID };
      }
      if (session.status === "restoring") {
        if (!session.restore_job_id) throw new ApiError("恢复中的会话缺少任务标识", 409, "backup_restore_job_missing");
        const reconciled = await reconcileRestoreJob(session.restore_job_id);
        if (reconciled.done) return reconciled.result;
      } else if (session.status === "failed") {
        if (local.pendingComplete && !local.pendingComplete.jobId) {
          // The previous complete request may have reached the server even when its response was lost.
          return enqueueRestore();
        }
        if (!session.restore_job_id) throw new ApiError("上传校验失败，请重新创建上传会话", 409, "backup_upload_failed");
        const reconciled = await reconcileRestoreJob(session.restore_job_id);
        if (reconciled.done) return reconciled.result;
      } else if (session.status === "ready" && local.pendingComplete?.jobId) {
        const reconciled = await reconcileRestoreJob(local.pendingComplete.jobId);
        if (reconciled.done) return reconciled.result;
      }

      if (session.status === "failed") {
        setProgress({ value: 96, label: "已复用校验通过的备份，正在重试恢复…" });
        return enqueueRestore();
      }
      if (session.status !== "uploading" && session.status !== "ready") {
        throw new ApiError("上传会话当前无法继续", 409, "backup_upload_not_resumable");
      }
      let offset = session.received_size;
      if (offset > selected.size) throw new ApiError("服务端上传偏移量超过文件大小", 409, "invalid_backup_upload_offset");
      if (local.pendingChunk && offset >= local.pendingChunk.offset + local.pendingChunk.length) {
        local = persist({ ...local, pendingChunk: undefined });
      }

      while (offset < selected.size) {
        const slice = selected.slice(offset, Math.min(selected.size, offset + backupUploadChunkSize));
        const raw = new Uint8Array(await readBlobSlice(slice));
        const chunkDigest = sha256Hex(raw);
        const expectedOffset = offset + raw.length;
        setProgress({ value: 15 + Math.round((offset / selected.size) * 78), label: `正在上传 ${bytes(offset)} / ${bytes(selected.size)}` });
        let pending = local.pendingChunk;
        if (pending && offset >= pending.offset + pending.length) {
          local = persist({ ...local, pendingChunk: undefined });
          pending = undefined;
        }
        if (!pending || pending.offset !== offset || pending.length !== raw.length || pending.sha256 !== chunkDigest) {
          pending = {
            offset,
            length: raw.length,
            sha256: chunkDigest,
            idempotencyKey: newBackupUploadKey(`backup-chunk:${session.id}:${offset}:${chunkDigest.slice(0, 16)}`),
          };
          local = persist({ ...local, pendingChunk: pending });
        }
        let jobID = pending.jobId;
        if (!jobID) {
          const queued = await api.uploadBackupChunk(instance.id, session.id, {
            offset, chunk_sha256: chunkDigest, content_base64: encodeBackupChunk(raw),
          }, pending.idempotencyKey);
          jobID = queued.job_id;
          pending = { ...pending, jobId: jobID };
          local = persist({ ...local, pendingChunk: pending });
        }
        const chunkJob = await waitForBackupUploadJob(jobID);
        if (chunkJob.status === "failed" || chunkJob.status === "canceled") {
          local = persist({ ...local, pendingChunk: undefined });
          throw new ApiError(chunkJob.message || "备份分片传输失败；下次重试将使用新的幂等键", 409, "backup_upload_job_failed");
        }
        session = await waitForBackupUploadOffset(instance.id, session.id, expectedOffset);
        applyRecord(session);
        offset = session.received_size;
        local = persist({ ...local, pendingChunk: undefined });
        setProgress({ value: 15 + Math.round((offset / selected.size) * 78), label: `已上传 ${bytes(offset)} / ${bytes(selected.size)}` });
      }
      if (session.status !== "ready") session = applyRecord(await waitForBackupUploadReady(instance.id, session.id));
      return enqueueRestore();
    },
    onSuccess: () => {
      toast.push("跨节点备份已上传，恢复任务已提交");
      void client.invalidateQueries({ queryKey: ["jobs"] });
      void client.invalidateQueries({ queryKey: ["instances", instance.id, "backups"] });
      file.current = null;
      onClose();
      navigate("/jobs");
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const abandon = useMutation({
    mutationFn: async () => {
      const current = persisted.current;
      if (!current?.uploadId) return { localOnly: true, job_id: "" };
      let pending = current.pendingAbort;
      if (!pending) {
        pending = { idempotencyKey: newBackupUploadKey(`backup-abort:${current.uploadId}`) };
        persist({ ...current, pendingAbort: pending });
      }
      const queued = await api.abortBackupUpload(instance.id, current.uploadId, pending.idempotencyKey);
      persist({ ...persisted.current!, pendingAbort: { ...pending, jobId: queued.job_id } });
      return { localOnly: false, job_id: queued.job_id };
    },
    onSuccess: (result) => {
      forget();
      file.current = null;
      if (fileInput.current) fileInput.current.value = "";
      setFileMeta(null);
      setPlatform("");
      setProgress({ value: 0, label: "等待选择备份" });
      upload.reset();
      if (!result.localOnly) {
        toast.push("上传会话已放弃，节点暂存清理任务已排队");
        void client.invalidateQueries({ queryKey: ["jobs"] });
      } else {
        toast.push("本地上传记录已清除");
      }
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const abandonSession = () => {
    const current = persisted.current;
    if (!current) return;
    if (!window.confirm(`确认放弃 ${current.name} 的上传会话？已上传的节点暂存分块将异步清理，且不能继续续传。`)) return;
    abandon.mutate();
  };
  const restoreReady = instance.status === "stopped" || instance.status === "degraded";
  return <Modal open={open} onClose={close} title="上传备份并恢复" description="把从其他节点下载的 ZIP 备份按 1 MiB 分块上传到当前节点，整包校验通过后自动排队恢复。" footer={<><Button variant="secondary" onClick={close} disabled={upload.isPending || abandon.isPending}>取消</Button><Button onClick={() => upload.mutate()} loading={upload.isPending} disabled={abandon.isPending || !fileMeta || !platform || !restoreReady}><Upload size={16} />开始上传并恢复</Button></>}>
    <div className="form-stack"><div className="form-warning"><ShieldAlert size={17} />恢复会替换实例数据，实例必须已停止。降级状态下 Agent 仍会确认无活动进程；恢复前会自动创建检查点。</div>{resumeSession && <div className="form-note backup-upload-resume"><History size={18} /><span><strong>有可恢复的上传会话</strong><small>{resumeSession.name} · {bytes(resumeSession.size)}；请重新选择同一文件，校验 SHA-256 后将从节点已确认的偏移量继续。关闭窗口或刷新页面不会丢失此会话。</small></span><Button variant="danger" onClick={abandonSession} loading={abandon.isPending} disabled={upload.isPending}>放弃会话</Button></div>}<Field label="ZIP 备份文件" hint="1 字节至 20 GiB；浏览器按切片校验和上传，不会把整个文件放入页面内存。"><input ref={fileInput} type="file" accept=".zip,application/zip" disabled={upload.isPending || abandon.isPending} onChange={(event) => chooseFile(event.currentTarget.files?.[0])} /></Field>{fileMeta && <div className="backup-upload-file"><Archive size={20} /><div><strong>{fileMeta.name}</strong><span>{bytes(fileMeta.size)}</span></div></div>}<Field label="备份来源平台" hint="跨系统恢复只恢复可移植数据，并重装目标平台对应的 Metamod/SourceMod 二进制。"><select value={platform} disabled={upload.isPending || abandon.isPending} onChange={(event) => setPlatform(event.target.value as BackupPlatform | "")}><option value="">请选择来源平台</option><option value="linux/amd64">Linux amd64</option><option value="windows/amd64">Windows amd64</option></select></Field>{(upload.isPending || progress.value > 0) && <div className="backup-upload-progress"><div><strong>{progress.label}</strong><span>{progress.value}%</span></div><ProgressBar value={progress.value} /></div>}{(upload.error || abandon.error) && <div className="form-error" role="alert">{(upload.error || abandon.error)?.message}</div>}</div>
  </Modal>;
}

function BackupDownloadButton({ instance, backup }: { instance: InstanceRecord; backup: BackupRecord }) {
  const toast = useToast();
  const [progress, setProgress] = useState(0);
  const download = useMutation({
    mutationFn: ({ writer }: { writer?: BackupDownloadWriter }) => receiveBackupDownload({
      expectedSize: backup.size,
      expectedSHA256: backup.checksum,
      fetchChunk: (offset) => api.downloadBackupChunk(instance.id, backup.id, offset),
      writer,
      onProgress: setProgress,
    }),
    onSuccess: (blob) => { if (blob) saveBrowserDownload(blob, safeBrowserFilename(backup.name)); setProgress(0); toast.push("备份下载已完成"); },
    onError: (error) => { setProgress(0); toast.push(error.message, "error"); },
  });
  const begin = async () => {
    const picker = (window as Window & { showSaveFilePicker?: SaveFilePicker }).showSaveFilePicker;
    if (picker) {
      try {
        const handle = await picker.call(window, {
          suggestedName: safeBrowserFilename(backup.name),
          types: [{ description: "ZIP 备份", accept: { "application/zip": [".zip"] } }],
        });
        download.mutate({ writer: await handle.createWritable() });
      } catch (error) {
        if (error instanceof DOMException && error.name === "AbortError") return;
        toast.push(error instanceof Error ? error.message : "无法创建备份下载文件", "error");
      }
      return;
    }
    if (backup.size > maxBufferedBackupDownload) {
      toast.push("当前浏览器不支持流式保存，无法安全下载超过 512 MiB 的备份；请使用支持文件保存 API 的 Chromium 浏览器", "error");
      return;
    }
    if (backup.size > 128 * 1024 * 1024 && !window.confirm(`该备份大小为 ${bytes(backup.size)}，浏览器需要临时占用相近内存。仍要下载吗？`)) return;
    download.mutate({});
  };
  return <Button variant="ghost" onClick={() => void begin()} loading={download.isPending}><Download size={15} />{download.isPending ? `${progress}%` : "下载"}</Button>;
}

interface SaveFilePicker {
  (options: { suggestedName: string; types: Array<{ description: string; accept: Record<string, string[]> }> }): Promise<{
    createWritable(): Promise<BackupDownloadWriter>;
  }>;
}

function SchedulesTab({ instance }: { instance: InstanceRecord }) {
  const [open, setOpen] = useState(false);
  const schedules = useQuery({ queryKey: ["instances", instance.id, "schedules"], queryFn: () => api.schedules(instance.id) });
  return <Card><div className="card-heading"><div><span className="eyebrow">AUTOMATION</span><h2>计划任务</h2></div><Button onClick={() => setOpen(true)}><Plus size={16} />新增计划</Button></div>{schedules.isPending ? <LoadingBlock /> : schedules.error ? <OptionalUnavailable error={schedules.error} title="计划清单尚不可用" description="当前控制端未开放计划任务列表；仍可提交新的 Agent 计划。" action={<Button onClick={() => setOpen(true)}><Plus size={16} />创建计划</Button>} /> : schedules.data.items.length ? <div className="schedule-list">{schedules.data.items.map((schedule) => <ScheduleRow key={schedule.id} instanceId={instance.id} schedule={schedule} />)}</div> : <EmptyState title="还没有自定义计划" description="默认备份策略仍会运行。你可以安排重启、更新、备份或广播。" action={<Button onClick={() => setOpen(true)}><Plus size={16} />新增计划</Button>} />}<ScheduleModal open={open} instance={instance} onClose={() => setOpen(false)} /></Card>;
}

function ScheduleRow({ instanceId, schedule }: { instanceId: string; schedule: ScheduleRecord }) {
  const client = useQueryClient();
  const toast = useToast();
  const refresh = () => void client.invalidateQueries({ queryKey: ["instances", instanceId, "schedules"] });
  const toggle = useMutation({ mutationFn: () => api.setScheduleEnabled(instanceId, schedule.id, !schedule.enabled), onSuccess: () => { refresh(); toast.push(schedule.enabled ? "计划已停用" : "计划已启用"); }, onError: (error) => toast.push(error.message, "error") });
  const remove = useMutation({ mutationFn: () => api.deleteSchedule(instanceId, schedule.id), onSuccess: () => { refresh(); toast.push("计划已删除"); }, onError: (error) => toast.push(error.message, "error") });
  return <div className="schedule-row"><div className={`schedule-icon ${schedule.enabled ? "schedule-icon--active" : ""}`}><CalendarClock size={18} /></div><div><strong>{schedule.name}</strong><span>{actionLabel(schedule.action)} · <code>{schedule.cron}</code></span></div><div><small>下次运行</small><strong>{schedule.enabled ? dateTime(schedule.next_run_at) : "暂停"}</strong></div><span className={`status status--${schedule.enabled ? "online" : "offline"}`}>{schedule.enabled ? "已启用" : "已停用"}</span><div className="schedule-actions"><Button variant="ghost" aria-label={schedule.enabled ? `停用 ${schedule.name}` : `启用 ${schedule.name}`} onClick={() => toggle.mutate()} loading={toggle.isPending}><Power size={14} />{schedule.enabled ? "停用" : "启用"}</Button><Button variant="danger" aria-label={`删除 ${schedule.name}`} onClick={() => { if (window.confirm(`确认删除计划“${schedule.name}”？`)) remove.mutate(); }} loading={remove.isPending}><Trash2 size={14} />删除</Button></div></div>;
}

function ScheduleModal({ open, instance, onClose }: { open: boolean; instance: InstanceRecord; onClose: () => void }) {
  const [name, setName] = useState(""); const [cron, setCron] = useState("0 4 * * *"); const [action, setAction] = useState("backup"); const toast = useToast();
  const client = useQueryClient();
  const create = useMutation({ mutationFn: () => api.instanceAction(instance.id, "create_schedule", { name, cron, action, enabled: true }), onSuccess: () => { toast.push("计划任务已提交"); void client.invalidateQueries({ queryKey: ["instances", instance.id, "schedules"] }); onClose(); }, onError: (error) => toast.push(error.message, "error") });
  return <Modal open={open} onClose={onClose} title="新增计划任务" description="计划按中心端时区执行，长操作仍遵循实例写锁。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button onClick={() => create.mutate()} loading={create.isPending} disabled={!name || !cron}>创建计划</Button></>}><div className="form-stack"><Field label="计划名称"><input value={name} onChange={(event) => setName(event.target.value)} placeholder="每日备份" /></Field><div className="form-grid"><Field label="执行动作"><select value={action} onChange={(event) => setAction(event.target.value)}><option value="backup">创建备份</option><option value="restart">重启实例</option><option value="broadcast">聊天广播</option></select></Field><Field label="Cron 表达式" hint="分 时 日 月 周"><input value={cron} onChange={(event) => setCron(event.target.value)} /></Field></div><div className="form-note"><strong>不会自动广播</strong><span>计划重启到时会直接执行；如需提前通知玩家，请单独创建聊天广播计划。</span></div></div></Modal>;
}

function AuditTab({ instance }: { instance: InstanceRecord }) {
  const audit = useQuery({ queryKey: ["audit", instance.id], queryFn: () => api.audit(instance.id) });
  if (audit.isPending) return <LoadingBlock label="正在读取不可编辑的审计记录…" />;
  if (audit.error) return <ErrorState error={audit.error} retry={() => void audit.refetch()} />;
  return <Card><div className="card-heading"><div><span className="eyebrow">AUDIT TRAIL</span><h2>审计记录</h2></div><span className="table-count">{audit.data.items.length} 条</span></div>{audit.data.items.length ? <div className="audit-timeline">{audit.data.items.map((event) => <div className="audit-event" key={event.id}><span className="audit-event__line" /><div className="audit-event__dot" /><time>{dateTime(event.created_at)}</time><div><strong>{event.actor_name || "系统"} · {event.action}</strong><span>{event.resource_type} {event.resource_id ? `#${event.resource_id.slice(0, 8)}` : ""}{event.remote_ip ? ` · ${event.remote_ip}` : ""}</span>{event.metadata && Object.keys(event.metadata).length > 0 && <code>{JSON.stringify(event.metadata)}</code>}</div></div>)}</div> : <EmptyState title="还没有审计记录" description="对实例的写操作、登录身份和来源地址会记录在这里。" />}</Card>;
}

function OptionalUnavailable({ error, title, description, action }: { error: unknown; title: string; description: string; action?: ReactNode }) {
  if (error instanceof ApiError && [404, 501].includes(error.status)) return <EmptyState title={title} description={description} action={action} />;
  return <ErrorState error={error} />;
}

function Toggle({ checked, onChange, label, description }: { checked: boolean; onChange: (value: boolean) => void; label: string; description: string }) {
  return <label className="toggle-field"><button type="button" role="switch" aria-label={label} aria-checked={checked} className={`switch ${checked ? "switch--on" : ""}`} onClick={() => onChange(!checked)}><span /></button><span><strong>{label}</strong><small>{description}</small></span></label>;
}

function splitArgs(value: string) {
  return value.match(/(?:[^\s"]+|"[^"]*")+/g)?.map((item) => item.replace(/^"|"$/g, "")) || [];
}

function readTickrate(value: string) {
  const args = splitArgs(value);
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    const inline = argument.match(/^-tickrate(?:=|\s+)(\d+)$/i);
    if (inline) return Number(inline[1]);
    if (argument.toLowerCase() === "-tickrate" && /^\d+$/.test(args[index + 1] || "")) return Number(args[index + 1]);
  }
  return null;
}

function writeTickrate(value: string, tickrate: number | null) {
  const args = splitArgs(value);
  const result: string[] = [];
  for (let index = 0; index < args.length; index += 1) {
    if (/^-tickrate(?:=|\s|$)/i.test(args[index])) {
      if (args[index].toLowerCase() === "-tickrate" && /^\d+$/.test(args[index + 1] || "")) index += 1;
      continue;
    }
    result.push(args[index]);
  }
  if (tickrate !== null) result.push("-tickrate", String(tickrate));
  return result.map((item) => /\s/.test(item) ? `"${item.replaceAll('"', '\\"')}"` : item).join(" ");
}

function parseCVars(value: string) {
  return value.split("\n").reduce<Record<string, string>>((result, line) => {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("//") || trimmed.startsWith("#")) return result;
    const [key, ...rest] = trimmed.split(/\s+/);
    if (key && rest.length) result[key] = rest.join(" ");
    return result;
  }, {});
}

function safeBrowserFilename(value: string) {
  return value.trim().replace(/[\\/:*?"<>|\u0000-\u001f]/g, "_") || "download.bin";
}

function saveBrowserDownload(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function actionLabel(type: string) {
  return ({ start: "启动", stop: "停止", restart: "重启", validate: "文件校验", backup: "备份", restore: "恢复", update: "更新", reinstall: "重新安装", broadcast: "广播" } as Record<string, string>)[type] || type;
}
