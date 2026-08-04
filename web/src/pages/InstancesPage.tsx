import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Box, Filter, Gamepad2, Map, MoreHorizontal, Play, Plus, RefreshCw, Search, Square, Users } from "lucide-react";
import { useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, PageHeader, StatusBadge } from "../components/ui";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { api } from "../lib/api";
import { relativeTime } from "../lib/format";
import { maintenancePayload } from "../lib/instance-lifecycle";
import { defaultTickrate, gameDifficulties, gameModes, maxTickrate, minTickrate, officialMapGroups } from "../lib/l4d2-options";
import type { InstanceRecord } from "../lib/types";

export function InstancesPage() {
  const [createOpen, setCreateOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("all");
  const realtime = useRealtime();
  const instances = useQuery({ queryKey: ["instances"], queryFn: api.instances, refetchInterval: realtime.state === "live" ? false : 15_000 });
  const filtered = useMemo(() => (instances.data?.items || []).filter((item) => {
    const matchesSearch = `${item.name} ${item.host} ${item.map}`.toLowerCase().includes(search.trim().toLowerCase());
    return matchesSearch && (status === "all" || item.status === status);
  }), [instances.data, search, status]);

  return <div>
    <PageHeader eyebrow="GAME SERVERS" title="游戏实例" description="部署、运行和维护彼此隔离的 L4D2 专用服务器。" actions={<><Button variant="secondary" onClick={() => void instances.refetch()}><RefreshCw size={16} />刷新</Button><Button onClick={() => setCreateOpen(true)}><Plus size={17} />创建实例</Button></>} />
    <Card className="toolbar-card"><div className="search-box"><Search size={17} /><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索实例、地址或地图" aria-label="搜索实例" /></div><div className="filter-select"><Filter size={16} /><select value={status} onChange={(event) => setStatus(event.target.value)} aria-label="按状态筛选"><option value="all">全部状态</option><option value="running">运行中</option><option value="stopped">已停止</option><option value="degraded">异常</option><option value="installing">安装中</option><option value="updating">更新中</option></select></div><span className="toolbar-count">{filtered.length} 个实例</span></Card>
    {instances.isPending ? <LoadingBlock label="正在获取实例清单…" /> : instances.error ? <ErrorState error={instances.error} retry={() => void instances.refetch()} /> : filtered.length === 0 ? <Card><EmptyState title={instances.data.items.length ? "没有匹配的实例" : "还没有游戏实例"} description={instances.data.items.length ? "尝试调整搜索关键词或状态筛选。" : "先在运行环境中准备或登记游戏目录，再创建实例并手动开服。"} action={!instances.data.items.length && <Button onClick={() => setCreateOpen(true)}><Plus size={16} />创建第一个实例</Button>} /></Card> : <div className="instance-grid">{filtered.map((instance) => <InstanceCard key={instance.id} instance={instance} />)}</div>}
    <CreateInstanceModal open={createOpen} onClose={() => setCreateOpen(false)} />
  </div>;
}

function InstanceCard({ instance }: { instance: InstanceRecord }) {
  const toast = useToast();
  const client = useQueryClient();
  const action = useMutation({
    mutationFn: ({ type, payload }: { type: string; payload?: Record<string, unknown> }) => api.instanceAction(instance.id, type, payload),
    onSuccess: (_result, { type }) => { toast.push(type === "start" ? "启动任务已提交" : type === "restart" ? "重启任务已提交" : "停止任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); void client.invalidateQueries({ queryKey: ["instances"] }); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const canStart = instance.status === "stopped";
  const canStop = ["running", "degraded"].includes(instance.status);
  const showRestart = ["stopped", "running", "degraded"].includes(instance.status);
  const canRestart = ["running", "degraded"].includes(instance.status);
  const isExternal = instance.status === "external";
  const restart = () => {
    const payload = maintenancePayload(instance.status);
    if (payload && !window.confirm("确认维护重启？服务器会依次广播 10、5、1 分钟提示。")) return;
    action.mutate({ type: "restart", payload });
  };
  return <Card className={`instance-card instance-card--${instance.status}`}>
    <div className="instance-card__cover"><div className="instance-card__pattern" aria-hidden /><span className="instance-number">#{instance.id.slice(0, 6).toUpperCase()}</span><StatusBadge status={instance.status} pulse={instance.status === "running"} /></div>
    <div className="instance-card__body"><div className="instance-card__heading"><div className="server-glyph"><Gamepad2 /></div><div><Link to={`/instances/${instance.id}`}>{instance.name}</Link><span>{instance.host}:{instance.port}</span></div><Link className="icon-button" aria-label={`管理 ${instance.name}`} to={`/instances/${instance.id}`}><MoreHorizontal size={19} /></Link></div>
      <div className="instance-card__facts"><div><Map size={16} /><span>地图</span><strong>{instance.map || instance.config?.start_map || "未载入"}</strong></div><div><Box size={16} /><span>模式</span><strong>{instance.mode || instance.config?.mode || "未知"}</strong></div><div><Users size={16} /><span>玩家</span><strong>{instance.players} / {instance.max_players}</strong></div></div>
      <div className="instance-card__foot"><span>更新于 {relativeTime(instance.updated_at)}</span><div className="instance-card__foot-actions">{canStart && <Button loading={action.isPending} onClick={() => action.mutate({ type: "start" })}><Play size={15} fill="currentColor" />启动</Button>}{canStop && <Button variant="secondary" loading={action.isPending} onClick={() => action.mutate({ type: "stop" })}><Square size={14} fill="currentColor" />停止</Button>}{showRestart && <Button variant="secondary" loading={action.isPending} disabled={!canRestart} title={!canRestart ? "服务器启动后才能重启" : undefined} onClick={restart}><RefreshCw size={14} />重启</Button>}{isExternal && <Button loading={action.isPending} onClick={() => { if (window.confirm("受控接管会先停止当前外部 srcds 进程，再由 Agent 使用面板配置启动。确认继续吗？")) action.mutate({ type: "restart" }); }}><RefreshCw size={14} />受控接管</Button>}{!canStart && !canStop && !showRestart && !isExternal && <Link className="button button--secondary" to={`/instances/${instance.id}`}>查看进度</Link>}</div></div>
    </div>
  </Card>;
}

function CreateInstanceModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const environments = useQuery({ queryKey: ["environments"], queryFn: api.environments, enabled: open });
  const [name, setName] = useState("");
  const [environmentId, setEnvironmentId] = useState("");
  const [installationId, setInstallationId] = useState("");
  const [hostname, setHostname] = useState("");
  const [mode, setMode] = useState("coop");
  const [difficulty, setDifficulty] = useState("normal");
  const [rconPassword, setRCONPassword] = useState("");
  const [maxPlayers, setMaxPlayers] = useState(4);
  const [startMap, setStartMap] = useState("c1m1_hotel");
  const [port, setPort] = useState(0);
  const [tickrate, setTickrate] = useState(false);
  const [tickrateValue, setTickrateValue] = useState(defaultTickrate);
  const installations = useQuery({ queryKey: ["environments", environmentId, "game-installations"], queryFn: () => api.gameInstallations(environmentId), enabled: open && Boolean(environmentId) });
  const navigate = useNavigate();
  const toast = useToast();
  const create = useMutation({
    mutationFn: () => {
      const environment = environments.data?.items.find((item) => item.id === environmentId);
      if (!environment) throw new Error("请选择运行节点");
      if (!installationId) throw new Error("请选择可用的游戏安装");
      return api.createInstance({
        node_id: environment.node_id,
        game_installation_id: installationId,
        name: name.trim(),
        port: port || undefined,
        rcon_password: rconPassword,
        config: { hostname: hostname.trim() || name.trim(), mode, difficulty, max_players: maxPlayers, start_map: startMap, public_status: false, auto_restart: true, extra_args: tickrate ? ["-tickrate", String(tickrateValue)] : [] },
      });
    },
    onSuccess: (result) => {
      toast.push("实例绑定任务已提交");
      onClose();
      if (result.instance?.id) navigate(`/instances/${result.instance.id}`);
      else navigate("/jobs");
    },
  });
  const submit = (event: FormEvent) => { event.preventDefault(); create.mutate(); };
  const onlineEnvironments = environments.data?.items.filter((item) => item.node_status === "online" && item.agent_supported) || [];
  const readyInstallations = installations.data?.items.filter((item) => item.status === "ready" && !item.bound_instance_id) || [];
  return <Modal open={open} onClose={onClose} title="创建游戏实例" description="绑定运行节点上已安装且未被占用的游戏目录；实例创建后保持停止，由你手动开服。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button type="submit" form="create-instance" loading={create.isPending}>创建实例</Button></>}>
    <form id="create-instance" onSubmit={submit} className="form-stack">
      <div className="form-grid"><Field label="实例名称"><input value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：专家战役 #1" required /></Field><Field label="运行节点"><select value={environmentId} onChange={(event) => { setEnvironmentId(event.target.value); setInstallationId(""); }} required><option value="">选择在线运行节点</option>{onlineEnvironments.map((item) => <option value={item.id} key={item.id}>{item.node_name || item.node_id}</option>)}</select></Field></div>
      <Field label="可用游戏安装" hint="仅显示所选节点上已安装且尚未绑定实例的目录。"><select value={installationId} onChange={(event) => setInstallationId(event.target.value)} required disabled={!environmentId}><option value="">{environmentId ? "选择可用游戏安装" : "请先选择运行节点"}</option>{readyInstallations.map((item) => <option value={item.id} key={item.id}>{item.path} · 构建 {item.build_id || "未知"}</option>)}</select></Field>
      <Field label="服务器显示名称"><input value={hostname} onChange={(event) => setHostname(event.target.value)} placeholder={name || "显示在服务器浏览器中的名称"} /></Field>
      <div className="form-grid form-grid--three"><Field label="游戏模式"><select value={mode} onChange={(event) => setMode(event.target.value)}>{gameModes.map((item) => <option value={item.value} key={item.value}>{item.label}</option>)}</select></Field><Field label="难度"><select value={difficulty} onChange={(event) => setDifficulty(event.target.value)}>{gameDifficulties.map((item) => <option value={item.value} key={item.value}>{item.label}</option>)}</select></Field><Field label="最大玩家"><input type="number" min={1} max={32} value={maxPlayers} onChange={(event) => setMaxPlayers(Number(event.target.value))} /></Field></div>
      <div className="form-grid"><Field label="RCON 密码（可留空）" hint="仅作为面板连接凭据保存；需与 server.cfg 中的 rcon_password 一致。"><input type="password" value={rconPassword} onChange={(event) => setRCONPassword(event.target.value)} maxLength={256} autoComplete="new-password" /></Field><Field label="游戏端口" hint="0 表示自动"><input type="number" min={0} max={65535} value={port} onChange={(event) => setPort(Number(event.target.value))} /></Field></div>
      <Field label="初始地图"><select value={startMap} onChange={(event) => setStartMap(event.target.value)} required>{officialMapGroups.map((group) => <optgroup label={group.campaign} key={group.campaign}>{group.maps.map(([value, label]) => <option value={value} key={value}>{label} · {value}</option>)}</optgroup>)}</select></Field>
      <div className="form-grid"><label className="toggle-field"><button type="button" role="switch" aria-label="启用 Tickrate" aria-checked={tickrate} className={`switch ${tickrate ? "switch--on" : ""}`} onClick={() => setTickrate((value) => !value)}><span /></button><span><strong>启用 Tickrate</strong><small>作为 -tickrate 启动参数传递</small></span></label>{tickrate && <Field label="Tickrate 数值" hint={`${minTickrate}–${maxTickrate} 的整数`}><input type="number" min={minTickrate} max={maxTickrate} step={1} value={tickrateValue} onChange={(event) => setTickrateValue(Number(event.target.value))} required /></Field>}</div>
      <div className="form-note"><strong>创建流程</strong><span>建立独占绑定并保存启动设置。不会生成 l4d2panel.cfg、重复校验或安装游戏，也不会自动启动服务器。</span></div>
      {create.error && <div className="form-error" role="alert">{create.error.message}</div>}
    </form>
  </Modal>;
}
