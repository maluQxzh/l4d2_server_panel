import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Archive, ArrowLeft, Ban, Box, CalendarClock, ChevronRight, CircleDot, ClipboardList, Code2, Download, FileCode2, Folder, Gamepad2, HardDrive, Map, PackageOpen, Play, Plus, Power, RefreshCw, RotateCcw, Save, Search, Send, Settings2, ShieldAlert, ShieldCheck, Square, Terminal, Trash2, Users, Wrench } from "lucide-react";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, ProgressBar, StatusBadge } from "../components/ui";
import { HistoryPagination, useHistoryCursor } from "../components/HistoryPagination";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { hasInstancePermission, hasNodePermission } from "../lib/account-permissions";
import { api, ApiError } from "../lib/api";
import { dateTime, duration, jobLabel } from "../lib/format";
import { setCachedInstanceStatus } from "../lib/instance-cache";
import { buildInstanceConfigUpdate, type RCONPasswordChange } from "../lib/instance-config";
import { instanceAddress, instancePlayers } from "../lib/instance-display";
import { buildStartupArgumentPreview, canAccessInstance, lifecycleActionAllowed } from "../lib/instance-lifecycle";
import { defaultTickrate, gameDifficulties, gameModes, maxTickrate, minTickrate, officialMapGroups } from "../lib/l4d2-options";
import { liveRefreshInterval, refreshQueries } from "../lib/query-refresh";
import { summarizeRecentOperations } from "../lib/recent-operations";
import type { InstanceConfig, InstanceRecord, LogSearchEntry, Permission, Player, PlayerBan, ScheduleRecord } from "../lib/types";
import { BackupsTab } from './InstanceDetailBackups';
import { FilesTab } from './InstanceDetailFiles';
import { ModsTab, PluginsTab } from './InstanceDetailPackages';
import { OptionalUnavailable, safeBrowserFilename, saveBrowserDownload, Toggle } from './InstanceDetailShared';

const tabs = [
  { id: "overview", label: "概览", icon: CircleDot, permissions: [] },
  { id: "console", label: "控制台", icon: Terminal, permissions: ["rcon.execute", "logs.read"] },
  { id: "players", label: "玩家", icon: Users, permissions: ["players.read", "players.kick", "players.ban", "players.unban", "players.change_map", "players.broadcast"] },
  { id: "config", label: "配置", icon: Settings2, permissions: ["instances.configure"] },
  { id: "mods", label: "模组", icon: PackageOpen, permissions: ["packages.read", "packages.install", "packages.toggle", "packages.delete", "packages.rollback"] },
  { id: "plugins", label: "插件", icon: Code2, permissions: ["packages.read", "packages.install", "packages.toggle", "packages.delete", "packages.rollback"] },
  { id: "files", label: "文件", icon: Folder, permissions: ["files.list", "files.read", "files.write", "files.delete"] },
  { id: "backups", label: "备份", icon: Archive, permissions: ["backups.read", "backups.download", "backups.create", "backups.restore", "backups.delete"] },
  { id: "schedules", label: "计划任务", icon: CalendarClock, permissions: ["schedules.read", "schedules.manage"] },
  { id: "audit", label: "审计", icon: ClipboardList, permissions: ["audit.read"] },
] as const;

type TabId = typeof tabs[number]["id"];

export function InstanceDetailPage() {
  const { user } = useAuth();
  const realtime = useRealtime();
  const { instanceId = "" } = useParams();
  const [searchParams, setSearchParams] = useSearchParams();
  const instance = useQuery({
    queryKey: ["instances", instanceId],
    queryFn: () => api.instance(instanceId),
    enabled: Boolean(instanceId),
    refetchInterval: realtime.state === "live" ? liveRefreshInterval : 5_000,
  });

  if (instance.isPending) return <LoadingBlock label="正在读取实例状态…" />;
  if (instance.error) return <><Link className="back-link" to="/instances"><ArrowLeft size={16} />返回实例列表</Link><ErrorState error={instance.error} retry={() => void instance.refetch()} /></>;
  const item = instance.data;
  const visibleTabs = tabs.filter((tab) => tab.permissions.length === 0 || tab.permissions.some((permission) => hasInstancePermission(user, item, permission as Permission)));
  const requestedTab = searchParams.get("tab");
  const active = (visibleTabs.some((tab) => tab.id === requestedTab) ? requestedTab : "overview") as TabId;

  return <div className="instance-detail">
    <div className="detail-breadcrumb"><Link to="/instances">游戏实例</Link><ChevronRight size={14} /><span>{item.name}</span></div>
    <InstanceHero instance={item} />
    <nav className="detail-tabs" aria-label="实例管理分类">{visibleTabs.map(({ id, label, icon: Icon }) => <button key={id} className={active === id ? "active" : ""} onClick={() => setSearchParams(id === "overview" ? {} : { tab: id })}><Icon size={17} />{label}</button>)}</nav>
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
    onMutate: async (type) => {
      await client.cancelQueries({ queryKey: ["instances"] });
      if (type === "start") setCachedInstanceStatus(client, instance.id, "starting");
      if (type === "stop" || type === "restart") setCachedInstanceStatus(client, instance.id, "stopping");
    },
    onSuccess: (_result, type) => {
      setRestartOpen(false);
      toast.push(`${actionLabel(type)}任务已提交`);
      void refreshQueries(client, { queryKey: ["jobs"] });
    },
    onError: (error) => {
      toast.push(error.message, "error");
      void refreshQueries(client, { queryKey: ["instances"] });
    },
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
    onMutate: async () => {
      await client.cancelQueries({ queryKey: ["instances"] });
      setCachedInstanceStatus(client, instance.id, "stopping");
    },
    onSuccess: () => {
      setTakeoverOpen(false);
      toast.push("受控重启与接管任务已提交");
      void refreshQueries(client, { queryKey: ["jobs"] });
    },
    onError: (error) => {
      toast.push(error.message, "error");
      void refreshQueries(client, { queryKey: ["instances"] });
    },
  });
  const closeTakeover = () => {
    if (takeoverPassword.current) takeoverPassword.current.value = "";
    setTakeoverOpen(false);
  };
  return <><header className="instance-hero">
    <div className="instance-hero__identity"><div className="instance-hero__glyph"><Gamepad2 /></div><div><div className="instance-hero__title"><h1>{instance.name}</h1><StatusBadge status={instance.status} pulse={instance.status === "running" && !nodeUnavailable} />{nodeUnavailable && <span className={`status status--${instance.node_status} instance-node-status`} title="无法确认节点上实例的实时状态"><span className="status__dot" aria-hidden />{nodeStatusLabel}</span>}{instance.status === "running" && !instance.rcon_configured && <span className="instance-rcon-warning">RCON 密码为空</span>}</div><p><code>{instanceAddress(instance)}</code><span>·</span><span>{instance.map || instance.config.start_map}</span><span>·</span><span>{instancePlayers(instance)}/{instance.max_players} 玩家</span>{instance.status === "external" && <><span>·</span><strong>受控重启前只读</strong></>}</p></div></div>
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
  const { user } = useAuth();
  const canReadJobs = hasInstancePermission(user, instance, "jobs.read");
  const canReadEnvironment = hasNodePermission(user, instance.node_id, "environments.read");
  const jobs = useQuery({ queryKey: ["jobs", "instance", instance.id, "operations"], queryFn: () => api.instanceJobs(instance.id, "exclude"), enabled: canReadJobs });
  const recentOperations = summarizeRecentOperations(jobs.data?.items || []);
  return <div className="overview-layout">
    <div className="overview-main">
      <section className="detail-stat-grid"><Card><span>在线玩家</span><strong>{instancePlayers(instance)}<small> / {instance.max_players}</small></strong><div className="mini-progress"><ProgressBar value={(instancePlayers(instance) / Math.max(instance.max_players, 1)) * 100} /></div></Card><Card><span>当前地图</span><strong className="text-value">{instance.map || "未载入"}</strong><small><Map size={14} />{instance.mode || instance.config.mode}</small></Card><Card><span>连续运行</span><strong className="text-value">{duration(instance.started_at)}</strong><small><Power size={14} />PID {instance.pid || "—"}</small></Card><Card><span>游戏构建</span><strong className="text-value">{instance.build_id || "待检测"}</strong><small><Box size={14} />当前启动配置</small></Card></section>
      <InstanceBindingCard instance={instance} />
      {canReadJobs && <Card><div className="card-heading"><div><span className="eyebrow">RECENT OPERATIONS</span><h2>最近操作</h2></div><Link to="/jobs">全部任务 <ChevronRight size={15} /></Link></div>{jobs.isPending ? <LoadingBlock /> : recentOperations.length ? <div className="detail-job-list">{recentOperations.map(({ job, occurrences }) => <div key={job.id}><div className={`job-icon job-icon--${job.status}`}><Code2 size={15} /></div><div><strong>{jobLabel(job)}</strong><small>{job.message || dateTime(job.created_at)}{occurrences > 1 ? ` · 共 ${occurrences} 次` : ""}</small></div><StatusBadge status={job.status} />{["queued", "dispatched", "running"].includes(job.status) && <div className="job-inline-progress"><ProgressBar value={job.progress} /><span>{job.progress}%</span></div>}</div>)}</div> : <EmptyState title="还没有操作记录" description="启动、备份或其他实例操作的进度会显示在这里。" />}</Card>}
    </div>
    <aside className="overview-side"><Card><div className="card-heading"><div><span className="eyebrow">SERVER PROFILE</span><h2>实例资料</h2></div></div><div className="detail-pairs detail-pairs--stack"><div><span>所属节点</span><strong>{instance.node_name || instance.node_id}</strong></div><div><span>游戏安装</span><code>{instance.game_installation_id || "尚未绑定"}</code></div><div><span>安装路径</span><code>{instance.path || "等待 Agent 校验"}</code></div><div><span>游戏端口</span><strong>{instance.port}</strong></div><div><span>SourceTV 端口</span><strong>{instance.tv_port || "未启用"}</strong></div><div><span>自动重启</span><strong>{instance.config.auto_restart ? "已启用" : "已关闭"}</strong></div><div><span>公开状态</span><strong>{instance.config.public_status ? "可见" : "隐藏"}</strong></div></div></Card><StartupArgumentsCard instance={instance} />{canReadEnvironment && <Card className="maintenance-card"><Wrench size={22} /><div><strong>游戏文件维护</strong><span>SteamCMD、更新和校验修复统一在运行环境中执行</span></div><Link to="/environments">打开运行环境 <ChevronRight size={14} /></Link></Card>}</aside>
  </div>;
}

function InstanceBindingCard({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const toast = useToast();
  const navigate = useNavigate();
  const canManage = canAccessInstance(user, instance, "instances.delete");
  const canReadEnvironment = hasNodePermission(user, instance.node_id, "environments.read");
  const canReadJobs = hasInstancePermission(user, instance, "jobs.read");
  const remove = useMutation({
    mutationFn: () => api.deleteInstance(instance.id, "detach"),
    onSuccess: () => { toast.push("实例已提交解除绑定，游戏文件将保留"); navigate(canReadJobs ? "/jobs" : "/instances"); },
    onError: (error) => toast.push(error.message, "error"),
  });
  return <Card className="lifecycle-card">
    <div className="card-heading"><div><span className="eyebrow">GAME INSTALLATION</span><h2>游戏安装绑定</h2></div>{canReadEnvironment && <Link to="/environments">运行环境 <ChevronRight size={15} /></Link>}</div>
    <div className="lifecycle-actions">
      <div><HardDrive size={19} /><span><strong>{instance.game_installation_id ? "已绑定独占游戏安装" : "游戏安装未就绪"}</strong><small>{instance.path || "请联系有运行环境权限的管理员检查目录状态"}</small></span>{canReadEnvironment && <Button variant="secondary" onClick={() => navigate("/environments")}>查看环境</Button>}</div>
      <div className="lifecycle-danger"><Trash2 size={19} /><span><strong>删除实例并解除绑定</strong><small>保留全部游戏文件；之后可再次绑定或在运行环境中隔离删除</small></span><Button variant="danger" loading={remove.isPending} disabled={!canManage} title={!canManage ? "当前角色没有实例管理权限" : undefined} onClick={() => { if (window.confirm(`删除实例“${instance.name}”并保留游戏文件？`)) remove.mutate(); }}>删除实例</Button></div>
    </div>
  </Card>;
}

function StartupArgumentsCard({ instance }: { instance: InstanceRecord }) {
  const args = buildStartupArgumentPreview(instance);
  return <Card className="startup-preview"><div className="card-heading"><div><span className="eyebrow">ARGV PREVIEW</span><h2>启动参数预览</h2></div><Terminal size={19} /></div><ol aria-label="启动参数数组">{args.map((argument, index) => <li key={`${index}-${argument}`}><span>{index}</span><code>{JSON.stringify(argument)}</code></li>)}</ol><p><ShieldCheck size={14} />面板下次受控启动会逐项传递这些参数，不经过 Shell；历史敏感参数会隐藏其值并标记为不会传递。</p></Card>;
}

function ConsoleTab({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const realtime = useRealtime();
  const toast = useToast();
  const [lines, setLines] = useState<Array<{ at: string; stream: string; line: string }>>([]);
  const [command, setCommand] = useState("");
  const canExecuteRCON = hasInstancePermission(user, instance, "rcon.execute");
  const canReadLogs = hasInstancePermission(user, instance, "logs.read");
  const send = useMutation({ mutationFn: () => api.instanceAction(instance.id, "rcon", { command }), onSuccess: () => { setLines((value) => [...value, { at: new Date().toISOString(), stream: "command", line: `> ${command}` }]); setCommand(""); toast.push("RCON 命令已发送"); }, onError: (error) => toast.push(error.message, "error") });
  useEffect(() => {
    if (!canReadLogs) return;
    return realtime.subscribe((event) => {
    if (event.resource_id !== instance.id || !["log", "console"].some((word) => event.type.includes(word))) return;
    const data = event.data || {};
    setLines((current) => [...current.slice(-499), { at: String(data.at || event.created_at), stream: String(data.stream || "stdout"), line: String(data.line || data.message || "") }]);
    });
  }, [canReadLogs, instance.id, realtime]);
  const submit = (event: FormEvent) => { event.preventDefault(); if (canExecuteRCON && command.trim()) send.mutate(); };
  return <div className="console-stack"><Card className="console-card"><div className="console-toolbar"><div><span className={`health-dot health-dot--${realtime.state === "live" ? "online" : "pending"}`} /><strong>{canReadLogs ? realtime.state === "live" ? "实时日志" : "连接降级" : "RCON 控制台"}</strong><small>{canReadLogs ? realtime.state === "live" ? "输出会自动滚动" : "实时输出暂不可用，命令仍可提交" : "当前权限不包含日志读取"}</small></div>{canReadLogs && <Button variant="ghost" onClick={() => setLines([])}><Trash2 size={15} />清屏</Button>}</div>{canReadLogs ? <div className="terminal-output" role="log" aria-live="polite">{lines.length ? lines.map((line, index) => <div key={`${line.at}-${index}`} className={`terminal-line terminal-line--${line.stream}`}><time>{new Date(line.at).toLocaleTimeString("zh-CN", { hour12: false })}</time><span>{line.line}</span></div>) : <div className="terminal-placeholder"><Terminal size={27} /><strong>等待服务器输出</strong><span>启动实例后，Agent 推送的控制台日志会出现在这里。</span></div>}</div> : <div className="form-note"><ShieldAlert size={17} />日志输出已按权限隐藏。</div>}{canExecuteRCON && <form className="command-line" onSubmit={submit}><span>RCON</span><input value={command} onChange={(event) => setCommand(event.target.value)} placeholder="输入 Source RCON 命令，例如 status" aria-label="RCON 命令" disabled={instance.status === "stopped"} /><Button type="submit" loading={send.isPending} disabled={!command.trim() || instance.status === "stopped"}><Send size={16} />发送</Button></form>}<p className="console-note">这里只接受游戏 RCON 命令，不提供主机 Shell。密码类参数不会写入审计日志。</p></Card>{canReadLogs && <LogHistoryCard instance={instance} />}</div>;
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
  const { user } = useAuth();
  const canReadPlayers = hasInstancePermission(user, instance, "players.read");
  const canKick = hasInstancePermission(user, instance, "players.kick");
  const canBan = hasInstancePermission(user, instance, "players.ban");
  const canUnban = hasInstancePermission(user, instance, "players.unban");
  const canChangeMap = hasInstancePermission(user, instance, "players.change_map");
  const canBroadcast = hasInstancePermission(user, instance, "players.broadcast");
  const serverActive = instance.status === "running" || instance.status === "degraded";
  const canQueryRCON = canReadPlayers && serverActive && instance.node_status === "online" && instance.rcon_configured;
  const players = useQuery({
    queryKey: ["instances", instance.id, "players", instance.started_at, serverActive],
    queryFn: () => api.players(instance.id),
    enabled: canQueryRCON,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return canQueryRCON && (status === "loading" || status === "refreshing") ? 750 : canQueryRCON ? 15_000 : false;
    },
  });
  const bans = useQuery({ queryKey: ["instances", instance.id, "bans"], queryFn: () => api.bans(instance.id), enabled: canReadPlayers, refetchInterval: canReadPlayers ? 30_000 : false });
  const toast = useToast();
  const client = useQueryClient();
  const [selected, setSelected] = useState<Player | null>(null);
  const action = useMutation({ mutationFn: ({ type, payload }: { type: string; payload: Record<string, unknown> }) => api.instanceAction(instance.id, type, payload), onSuccess: (_data, values) => { toast.push(values.type === "kick_player" ? "已提交踢出操作" : "已提交解封操作"); void refreshQueries(client, { queryKey: ["instances", instance.id, "bans"] }); }, onError: (error) => toast.push(error.message, "error") });
  const playerItems = canQueryRCON ? players.data?.items || [] : [];
  const playerStatus = players.data?.status || "ready";
  let playerContent: ReactNode;
  if (!canReadPlayers) {
    playerContent = <EmptyState title="玩家清单已隐藏" description="当前账号没有查看在线玩家和封禁清单的权限。" />;
  } else if (!serverActive) {
    playerContent = <div className="form-warning"><ShieldAlert size={17} />服务器尚未启动，RCON 查询只在运行中生效。</div>;
  } else if (instance.node_status !== "online") {
    playerContent = <div className="form-warning"><ShieldAlert size={17} />节点不在线，暂时无法查询在线玩家。</div>;
  } else if (!instance.rcon_configured) {
    playerContent = <div className="form-warning"><ShieldAlert size={17} />中心端尚未保存 RCON 密码。请设置与 server.cfg 完全一致的密码后重试。</div>;
  } else if (players.error) {
    playerContent = <ErrorState error={players.error} retry={() => void players.refetch()} />;
  } else if (players.isPending || ((playerStatus === "loading" || playerStatus === "refreshing") && !playerItems.length)) {
    playerContent = <LoadingBlock label={playerStatus === "refreshing" ? "正在查询最新玩家信息…" : "正在发起 RCON 玩家查询…"} />;
  } else {
    playerContent = <>
      {playerStatus === "failed" && <div className="form-error" role="alert">{players.data?.message || "RCON 玩家查询失败，请检查 RCON 配置后重试。"}</div>}
      {playerStatus === "refreshing" && <div className="form-note"><RefreshCw className="spin" size={16} />正在通过节点查询最新玩家信息，当前先显示上次结果…</div>}
      {playerItems.length ? <div className="data-table-wrap"><table className="data-table"><thead><tr><th>玩家</th><th>Steam ID</th><th>分数</th><th>延迟</th><th>连接时间</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>{playerItems.map((player) => <tr key={player.steam_id || player.name}><td><strong>{player.name}</strong></td><td><code>{player.steam_id || "—"}</code></td><td>{player.score ?? "—"}</td><td>{player.ping != null ? `${player.ping} ms` : "—"}</td><td>{dateTime(player.connected_at)}</td><td className="table-actions">{canKick && <Button variant="ghost" onClick={() => action.mutate({ type: "kick_player", payload: { steam_id: player.steam_id, name: player.name } })}><Power size={14} />踢出</Button>}{canBan && <Button variant="ghost" onClick={() => setSelected(player)} disabled={!player.steam_id}><Ban size={14} />封禁</Button>}</td></tr>)}</tbody></table></div> : playerStatus === "failed" ? null : <EmptyState title="服务器里暂时没人" description="最新 RCON 查询已完成，当前没有在线玩家。" />}
    </>;
  }

  return <div className="players-stack">{(canChangeMap || canBroadcast) && <PlayerCommandsCard instance={instance} canChangeMap={canChangeMap} canBroadcast={canBroadcast} />}<Card><div className="card-heading"><div><span className="eyebrow">CONNECTED PLAYERS</span><h2>在线玩家</h2></div>{canReadPlayers && <span className="table-count">{playerItems.length} / {instance.max_players}</span>}</div>{playerContent}</Card>{canReadPlayers && <Card><div className="card-heading"><div><span className="eyebrow">ACTIVE BANS</span><h2>封禁记录</h2></div><span className="table-count">{bans.data?.items.length || 0} 条</span></div>{bans.isPending ? <LoadingBlock /> : bans.error ? <ErrorState error={bans.error} retry={() => void bans.refetch()} /> : bans.data.items.length ? <div className="data-table-wrap"><table className="data-table"><thead><tr><th>Steam ID</th><th>原因</th><th>创建时间</th><th>到期时间</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>{bans.data.items.map((ban) => <BanRow key={ban.id} ban={ban} unban={canUnban ? () => action.mutate({ type: "unban_player", payload: { steam_id: ban.steam_id } }) : undefined} busy={action.isPending} />)}</tbody></table></div> : <EmptyState title="没有有效封禁" description="限时封禁到期后会自动从清单移除；永久封禁需要手动解除。" />}</Card>}<BanPlayerModal instance={instance} player={canBan ? selected : null} onClose={() => setSelected(null)} /></div>;
}

function PlayerCommandsCard({ instance, canChangeMap, canBroadcast }: { instance: InstanceRecord; canChangeMap: boolean; canBroadcast: boolean }) {
  const toast = useToast();
  const [mapName, setMapName] = useState(instance.map || instance.config.start_map || "");
  const [message, setMessage] = useState("");
  const ready = ["running", "degraded"].includes(instance.status) && instance.rcon_configured;
  const validMap = /^[A-Za-z0-9_-]+$/.test(mapName);
  const validMessage = Boolean(message.trim()) && [...message.trim()].length <= 200 && !/[\0\r\n;"\\]/.test(message);
  const command = useMutation({
    mutationFn: ({ type, payload }: { type: "change_map" | "broadcast"; payload: Record<string, unknown> }) => api.instanceAction(instance.id, type, payload),
    onSuccess: (_result, values) => {
      if (values.type === "broadcast") setMessage("");
      toast.push(values.type === "change_map" ? "切换地图任务已提交" : "聊天广播任务已提交");
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  return <Card><div className="card-heading"><div><span className="eyebrow">GAMEPLAY CONTROL</span><h2>游戏内管理</h2></div></div>{!ready && <div className="form-warning"><ShieldAlert size={17} />服务器需要处于运行状态并配置 RCON 后才能执行。</div>}<div className="form-grid">{canChangeMap && <Field label="切换地图" hint="只允许地图代码中的字母、数字、下划线和连字符"><div className="inline-field"><input value={mapName} maxLength={128} onChange={(event) => setMapName(event.target.value.trim())} placeholder="c2m1_highway" /><Button variant="secondary" disabled={!ready || !validMap} loading={command.isPending} onClick={() => command.mutate({ type: "change_map", payload: { map: mapName } })}><Map size={15} />切换</Button></div></Field>}{canBroadcast && <Field label="聊天广播" hint="1–200 个字符；不允许换行、分号、引号或反斜杠"><div className="inline-field"><input value={message} maxLength={200} onChange={(event) => setMessage(event.target.value)} placeholder="服务器将在 5 分钟后重启" /><Button variant="secondary" disabled={!ready || !validMessage} loading={command.isPending} onClick={() => command.mutate({ type: "broadcast", payload: { message: message.trim() } })}><Send size={15} />广播</Button></div></Field>}</div></Card>;
}

function BanRow({ ban, unban, busy }: { ban: PlayerBan; unban?: () => void; busy: boolean }) {
  return <tr><td><code>{ban.steam_id}</code></td><td>{ban.reason || "管理员封禁"}</td><td>{dateTime(ban.created_at)}</td><td>{ban.expires_at ? dateTime(ban.expires_at) : "永久"}</td><td className="table-actions">{unban && <Button variant="ghost" onClick={() => { if (window.confirm(`确认解除 ${ban.steam_id} 的封禁？`)) unban(); }} loading={busy}><RotateCcw size={14} />解封</Button>}</td></tr>;
}

function BanPlayerModal({ instance, player, onClose }: { instance: InstanceRecord; player: Player | null; onClose: () => void }) {
  const toast = useToast();
  const client = useQueryClient();
  const [durationMinutes, setDurationMinutes] = useState(0);
  const [reason, setReason] = useState("违反服务器规则");
  useEffect(() => { if (player) { setDurationMinutes(0); setReason("违反服务器规则"); } }, [player]);
  const ban = useMutation({ mutationFn: () => api.instanceAction(instance.id, "ban_player", { steam_id: player?.steam_id, name: player?.name, duration_minutes: durationMinutes, reason: reason.trim() }), onSuccess: () => { toast.push(durationMinutes ? "限时封禁任务已提交" : "永久封禁任务已提交"); void refreshQueries(client, { queryKey: ["instances", instance.id, "bans"] }); onClose(); }, onError: (error) => toast.push(error.message, "error") });
  return <Modal open={Boolean(player)} onClose={onClose} title={`封禁 ${player?.name || "玩家"}`} description={`${player?.steam_id || "缺少 Steam ID"}；0 分钟表示永久封禁。`} footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button variant="danger" onClick={() => ban.mutate()} loading={ban.isPending} disabled={!player?.steam_id || durationMinutes < 0 || durationMinutes > 525600 || !reason.trim()}><Ban size={15} />确认封禁</Button></>}><div className="form-stack"><Field label="时长"><select value={durationMinutes} onChange={(event) => setDurationMinutes(Number(event.target.value))}><option value={0}>永久</option><option value={60}>1 小时</option><option value={1440}>1 天</option><option value={10080}>7 天</option><option value={43200}>30 天</option></select></Field><Field label="原因"><input value={reason} onChange={(event) => setReason(event.target.value)} maxLength={200} /></Field><div className="form-warning"><ShieldAlert size={17} />封禁通过 RCON 写入服务器 ban 列表，并在任务成功后记录到不可越权查看的实例范围清单。</div></div></Modal>;
}

function ConfigTab({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const canConfigure = hasInstancePermission(user, instance, "instances.configure");
  const canReadMaps = hasNodePermission(user, instance.node_id, "maps.read");
	const maps = useQuery({ queryKey: ["maps", instance.node_id], queryFn: () => api.maps(instance.node_id), enabled: canReadMaps, refetchInterval: liveRefreshInterval });
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
      if (!canConfigure)
        throw new ApiError("当前账号没有实例配置权限", 403, "forbidden");
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
      void refreshQueries(client, { queryKey: ["instances", instance.id] });
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
          {!canConfigure && <div className="form-warning"><ShieldAlert size={17} />当前账号只能查看配置，不能保存更改。</div>}
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
              disabled={!canConfigure || publicStatusBlocked || portInvalid || portChangeBlocked}
              title={!canConfigure ? "当前账号没有实例配置权限" : undefined}
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

function SchedulesTab({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const [open, setOpen] = useState(false);
  const canRead = hasInstancePermission(user, instance, "schedules.read");
  const canManage = hasInstancePermission(user, instance, "schedules.manage");
  const hasSchedulableAction = (["instances.start", "instances.stop", "instances.restart", "backups.create", "players.broadcast"] as const).some((permission) => hasInstancePermission(user, instance, permission));
  const schedules = useQuery({ queryKey: ["instances", instance.id, "schedules"], queryFn: () => api.schedules(instance.id), enabled: canRead, refetchInterval: liveRefreshInterval });
  const createButton = canManage && hasSchedulableAction ? <Button onClick={() => setOpen(true)}><Plus size={16} />新增计划</Button> : undefined;
  return <Card><div className="card-heading"><div><span className="eyebrow">AUTOMATION</span><h2>计划任务</h2></div>{createButton}</div>{canManage && !hasSchedulableAction && <div className="form-warning"><ShieldAlert size={17} />创建计划还需要所选动作的权限：启动、停止、重启、备份或聊天广播。</div>}{!canRead ? <EmptyState title="计划清单已隐藏" description="当前账号没有查看计划任务清单的权限，但仍可执行单独授予的创建操作。" action={createButton} /> : schedules.isPending ? <LoadingBlock /> : schedules.error ? <OptionalUnavailable error={schedules.error} title="计划清单尚不可用" description="当前控制端未开放计划任务列表；仍可提交新的 Agent 计划。" action={createButton} /> : schedules.data.items.length ? <div className="schedule-list">{schedules.data.items.map((schedule) => <ScheduleRow key={schedule.id} instanceId={instance.id} schedule={schedule} canManage={canManage} />)}</div> : <EmptyState title="还没有自定义计划" description="默认备份策略仍会运行。你可以安排启动、停止、重启、备份或广播。" action={createButton} />}<ScheduleModal open={canManage && open} instance={instance} onClose={() => setOpen(false)} /></Card>;
}

function ScheduleRow({ instanceId, schedule, canManage }: { instanceId: string; schedule: ScheduleRecord; canManage: boolean }) {
  const client = useQueryClient();
  const toast = useToast();
  const refresh = () => void refreshQueries(client, { queryKey: ["instances", instanceId, "schedules"] });
  const toggle = useMutation({ mutationFn: () => api.setScheduleEnabled(instanceId, schedule.id, !schedule.enabled), onSuccess: () => { refresh(); toast.push(schedule.enabled ? "计划已停用" : "计划已启用"); }, onError: (error) => toast.push(error.message, "error") });
  const remove = useMutation({ mutationFn: () => api.deleteSchedule(instanceId, schedule.id), onSuccess: () => { refresh(); toast.push("计划已删除"); }, onError: (error) => toast.push(error.message, "error") });
  const supported = ["start", "stop", "restart", "backup", "broadcast"].includes(schedule.action);
  return <div className="schedule-row"><div className={`schedule-icon ${schedule.enabled ? "schedule-icon--active" : ""}`}><CalendarClock size={18} /></div><div><strong>{schedule.name}</strong><span>{actionLabel(schedule.action)} · <code>{schedule.cron}</code>{!supported && " · 已停止支持"}</span></div><div><small>下次运行</small><strong>{supported && schedule.enabled ? dateTime(schedule.next_run_at) : "暂停"}</strong></div><span className={`status status--${supported && schedule.enabled ? "online" : "offline"}`}>{supported && schedule.enabled ? "已启用" : "已停用"}</span>{canManage && <div className="schedule-actions">{supported && <Button variant="ghost" aria-label={schedule.enabled ? `停用 ${schedule.name}` : `启用 ${schedule.name}`} onClick={() => toggle.mutate()} loading={toggle.isPending}><Power size={14} />{schedule.enabled ? "停用" : "启用"}</Button>}<Button variant="danger" aria-label={`删除 ${schedule.name}`} onClick={() => { if (window.confirm(`确认删除计划“${schedule.name}”？`)) remove.mutate(); }} loading={remove.isPending}><Trash2 size={14} />删除</Button></div>}</div>;
}

function ScheduleModal({ open, instance, onClose }: { open: boolean; instance: InstanceRecord; onClose: () => void }) {
  const { user } = useAuth();
  const availableActions = ([
    { value: "start", label: "启动实例", permission: "instances.start" },
    { value: "stop", label: "停止实例", permission: "instances.stop" },
    { value: "backup", label: "创建备份", permission: "backups.create" },
    { value: "restart", label: "重启实例", permission: "instances.restart" },
    { value: "broadcast", label: "聊天广播", permission: "players.broadcast" },
  ] as const).filter((item) => hasInstancePermission(user, instance, item.permission));
  const [name, setName] = useState(""); const [cron, setCron] = useState("0 4 * * *"); const [action, setAction] = useState("backup"); const [message, setMessage] = useState(""); const toast = useToast();
  const client = useQueryClient();
  useEffect(() => { if (!availableActions.some((item) => item.value === action)) setAction(availableActions[0]?.value || ""); }, [action, availableActions]);
  const create = useMutation({ mutationFn: () => api.instanceAction(instance.id, "create_schedule", { name, cron, action, enabled: true, message: action === "broadcast" ? message.trim() : undefined, payload: {} }), onSuccess: () => { toast.push("计划任务已提交"); setMessage(""); void refreshQueries(client, { queryKey: ["instances", instance.id, "schedules"] }); onClose(); }, onError: (error) => toast.push(error.message, "error") });
  const validMessage = action !== "broadcast" || Boolean(message.trim()) && [...message.trim()].length <= 200 && !/[\0\r\n;"\\]/.test(message);
  return <Modal open={open} onClose={onClose} title="新增计划任务" description="计划按中心端时区执行，长操作仍遵循实例写锁。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button onClick={() => create.mutate()} loading={create.isPending} disabled={!name || !cron || !action || !validMessage}>创建计划</Button></>}><div className="form-stack"><Field label="计划名称"><input value={name} onChange={(event) => setName(event.target.value)} placeholder="每日备份" /></Field><div className="form-grid"><Field label="执行动作"><select value={action} onChange={(event) => setAction(event.target.value)}>{availableActions.map((item) => <option value={item.value} key={item.value}>{item.label}</option>)}</select></Field><Field label="Cron 表达式" hint="分 时 日 月 周"><input value={cron} onChange={(event) => setCron(event.target.value)} /></Field></div>{action === "broadcast" && <Field label="广播内容" hint="1–200 个字符，不允许换行、分号、引号或反斜杠"><input value={message} maxLength={200} onChange={(event) => setMessage(event.target.value)} /></Field>}<div className="form-note"><strong>不会自动广播</strong><span>计划重启到时会直接执行；如需提前通知玩家，请单独创建聊天广播计划。</span></div></div></Modal>;
}

function AuditTab({ instance }: { instance: InstanceRecord }) {
  const pager = useHistoryCursor(instance.id);
  const audit = useQuery({ queryKey: ["audit", instance.id, pager.cursor], queryFn: () => pager.cursor ? api.audit(instance.id, pager.cursor) : api.audit(instance.id) });
  if (audit.isPending) return <LoadingBlock label="正在读取不可编辑的审计记录…" />;
  if (audit.error) return <ErrorState error={audit.error} retry={() => void audit.refetch()} />;
  return <Card><div className="card-heading"><div><span className="eyebrow">AUDIT TRAIL</span><h2>审计记录</h2></div><span className="table-count">{audit.data.items.length} 条</span></div>{audit.data.items.length ? <div className="audit-timeline">{audit.data.items.map((event) => <div className="audit-event" key={event.id}><span className="audit-event__line" /><div className="audit-event__dot" /><time>{dateTime(event.created_at)}</time><div><strong>{event.actor_name || "系统"} · {event.action}</strong><span>{event.resource_type} {event.resource_id ? `#${event.resource_id.slice(0, 8)}` : ""}{event.remote_ip ? ` · ${event.remote_ip}` : ""}</span>{event.metadata && Object.keys(event.metadata).length > 0 && <code>{JSON.stringify(event.metadata)}</code>}</div></div>)}</div> : <EmptyState title="还没有审计记录" description="对实例的写操作、登录身份和来源地址会记录在这里。" />}<HistoryPagination pager={pager} next={audit.data?.next_cursor} loading={audit.isFetching} /></Card>;
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

function actionLabel(type: string) {
  return ({ start: "启动", stop: "停止", restart: "重启", validate: "文件校验", backup: "备份", restore: "恢复", update: "更新", broadcast: "广播" } as Record<string, string>)[type] || type;
}
