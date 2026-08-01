import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Box, Filter, Gamepad2, Map, MoreHorizontal, Play, Plus, RefreshCw, Search, Server, Square, Users } from "lucide-react";
import { useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, PageHeader, StatusBadge } from "../components/ui";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { api } from "../lib/api";
import { relativeTime } from "../lib/format";
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
    {instances.isPending ? <LoadingBlock label="正在获取实例清单…" /> : instances.error ? <ErrorState error={instances.error} retry={() => void instances.refetch()} /> : filtered.length === 0 ? <Card><EmptyState title={instances.data.items.length ? "没有匹配的实例" : "还没有游戏实例"} description={instances.data.items.length ? "尝试调整搜索关键词或状态筛选。" : "选择一个在线节点，面板会自动分配端口并安装 L4D2。"} action={!instances.data.items.length && <Button onClick={() => setCreateOpen(true)}><Plus size={16} />创建第一个实例</Button>} /></Card> : <div className="instance-grid">{filtered.map((instance) => <InstanceCard key={instance.id} instance={instance} />)}</div>}
    <CreateInstanceModal open={createOpen} onClose={() => setCreateOpen(false)} />
  </div>;
}

function InstanceCard({ instance }: { instance: InstanceRecord }) {
  const toast = useToast();
  const client = useQueryClient();
  const action = useMutation({
    mutationFn: (type: string) => api.instanceAction(instance.id, type),
    onSuccess: (_result, type) => { toast.push(type === "start" ? "启动任务已提交" : type === "restart" ? "受控重启任务已提交" : "停止任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const canStart = instance.status === "stopped";
  const canStop = ["running", "degraded"].includes(instance.status);
  const isExternal = instance.status === "external";
  return <Card className={`instance-card instance-card--${instance.status}`}>
    <div className="instance-card__cover"><div className="instance-card__pattern" aria-hidden /><span className="instance-number">#{instance.id.slice(0, 6).toUpperCase()}</span><StatusBadge status={instance.status} pulse={instance.status === "running"} /></div>
    <div className="instance-card__body"><div className="instance-card__heading"><div className="server-glyph"><Gamepad2 /></div><div><Link to={`/instances/${instance.id}`}>{instance.name}</Link><span>{instance.host}:{instance.port}</span></div><Link className="icon-button" aria-label={`管理 ${instance.name}`} to={`/instances/${instance.id}`}><MoreHorizontal size={19} /></Link></div>
      <div className="instance-card__facts"><div><Map size={16} /><span>地图</span><strong>{instance.map || instance.config?.start_map || "未载入"}</strong></div><div><Box size={16} /><span>模式</span><strong>{instance.mode || instance.config?.mode || "未知"}</strong></div><div><Users size={16} /><span>玩家</span><strong>{instance.players} / {instance.max_players}</strong></div></div>
      <div className="instance-card__foot"><span>更新于 {relativeTime(instance.updated_at)}</span>{canStart && <Button loading={action.isPending} onClick={() => action.mutate("start")}><Play size={15} fill="currentColor" />启动</Button>}{canStop && <Button variant="secondary" loading={action.isPending} onClick={() => action.mutate("stop")}><Square size={14} fill="currentColor" />停止</Button>}{isExternal && <Button loading={action.isPending} onClick={() => { if (window.confirm("受控接管会先停止当前外部 srcds 进程，再由 Agent 使用面板配置启动。确认继续吗？")) action.mutate("restart"); }}><RefreshCw size={14} />受控接管</Button>}{!canStart && !canStop && !isExternal && <Link className="button button--secondary" to={`/instances/${instance.id}`}>查看进度</Link>}</div>
    </div>
  </Card>;
}

function CreateInstanceModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, enabled: open });
  const [name, setName] = useState("");
  const [nodeId, setNodeId] = useState("");
  const [hostname, setHostname] = useState("");
  const [mode, setMode] = useState("coop");
  const [maxPlayers, setMaxPlayers] = useState(4);
  const [startMap, setStartMap] = useState("c1m1_hotel");
  const [port, setPort] = useState(0);
  const [installMode, setInstallMode] = useState<"install" | "import">("install");
  const [path, setPath] = useState("");
  const navigate = useNavigate();
  const toast = useToast();
  const create = useMutation({
    mutationFn: () => api.createInstance({
      node_id: nodeId,
      name: name.trim(),
      port: port || undefined,
      operation: installMode,
      path: installMode === "import" ? path : undefined,
      root_index: installMode === "import" ? 0 : undefined,
      config: { hostname: hostname.trim() || name.trim(), mode, difficulty: "normal", max_players: maxPlayers, start_map: startMap, region: "255", public_status: false, auto_restart: true },
    }),
    onSuccess: (result) => {
      toast.push("实例创建任务已提交");
      onClose();
      if (result.instance?.id) navigate(`/instances/${result.instance.id}`);
      else navigate("/jobs");
    },
  });
  const submit = (event: FormEvent) => { event.preventDefault(); create.mutate(); };
  const onlineNodes = nodes.data?.items.filter((node) => node.status === "online") || [];
  return <Modal open={open} onClose={onClose} title="创建游戏实例" description="每个实例使用独立目录，端口留空时由节点自动分配。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button type="submit" form="create-instance" loading={create.isPending}>提交部署任务</Button></>}>
    <form id="create-instance" onSubmit={submit} className="form-stack">
      <div className="segmented"><button type="button" className={installMode === "install" ? "active" : ""} onClick={() => setInstallMode("install")}>全新安装</button><button type="button" className={installMode === "import" ? "active" : ""} onClick={() => setInstallMode("import")}>导入现有实例</button></div>
      <div className="form-grid"><Field label="实例名称"><input value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：专家战役 #1" required /></Field><Field label="运行节点"><select value={nodeId} onChange={(event) => setNodeId(event.target.value)} required><option value="">选择在线节点</option>{onlineNodes.map((node) => <option value={node.id} key={node.id}>{node.name} · {node.os}</option>)}</select></Field></div>
      {nodes.isSuccess && !onlineNodes.length && <div className="form-warning"><Server size={17} />当前没有在线节点，请先接入或恢复一个节点。</div>}
      {installMode === "import" && <Field label="现有安装目录" hint="路径必须位于该 Agent 配置的实例根目录内。"><input value={path} onChange={(event) => setPath(event.target.value)} placeholder="instances/existing-server" required /></Field>}
      <Field label="服务器显示名称"><input value={hostname} onChange={(event) => setHostname(event.target.value)} placeholder={name || "显示在服务器浏览器中的名称"} /></Field>
      <div className="form-grid form-grid--three"><Field label="游戏模式"><select value={mode} onChange={(event) => setMode(event.target.value)}><option value="coop">战役</option><option value="versus">对抗</option><option value="survival">生还者</option><option value="scavenge">清道夫</option><option value="realism">写实</option></select></Field><Field label="最大玩家"><input type="number" min={1} max={32} value={maxPlayers} onChange={(event) => setMaxPlayers(Number(event.target.value))} /></Field><Field label="游戏端口" hint="0 表示自动"><input type="number" min={0} max={65535} value={port} onChange={(event) => setPort(Number(event.target.value))} /></Field></div>
      <Field label="初始地图"><input value={startMap} onChange={(event) => setStartMap(event.target.value)} placeholder="c1m1_hotel" required /></Field>
      <div className="form-note"><strong>部署流程</strong><span>检查端口与磁盘 → 安装 App 222860 → 生成 RCON 密钥 → 启动并完成 A2S/RCON 健康检查。</span></div>
      {create.error && <div className="form-error" role="alert">{create.error.message}</div>}
    </form>
  </Modal>;
}
