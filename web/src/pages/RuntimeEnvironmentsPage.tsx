import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download, FolderInput, HardDrive, Plus, RefreshCw, Settings2, ShieldAlert, Trash2, Wrench } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, PageHeader, StatusBadge } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useToast } from "../context/ToastContext";
import { api } from "../lib/api";
import { dateTime } from "../lib/format";
import { canAccessNode } from "../lib/instance-lifecycle";
import type { GameInstallationRecord, RuntimeEnvironmentRecord } from "../lib/types";

export function RuntimeEnvironmentsPage() {
  const query = useQuery({ queryKey: ["environments"], queryFn: api.environments });
  const [steamTarget, setSteamTarget] = useState<RuntimeEnvironmentRecord | null>(null);
  const [gameTarget, setGameTarget] = useState<RuntimeEnvironmentRecord | null>(null);
  return <div>
    <PageHeader eyebrow="RUNTIME ENVIRONMENTS" title="运行环境" description="集中准备 SteamCMD 和独占的 L4D2 游戏安装；Agent 不会扫描磁盘或在安装时自动下载游戏。" actions={<Button variant="secondary" onClick={() => void query.refetch()}><RefreshCw size={16} />刷新列表</Button>} />
    {query.isPending ? <LoadingBlock label="正在读取节点运行环境…" /> : query.error ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : !query.data.items.length ? <Card><EmptyState title="还没有节点运行环境" description="注册节点后会自动生成对应的运行环境。" /></Card> : <div className="environment-grid">{query.data.items.map((environment) => <EnvironmentCard key={environment.id} environment={environment} configureSteam={() => setSteamTarget(environment)} addGame={() => setGameTarget(environment)} />)}</div>}
    <SteamCMDModal environment={steamTarget} onClose={() => setSteamTarget(null)} />
    <GameInstallationModal environment={gameTarget} onClose={() => setGameTarget(null)} />
  </div>;
}

function EnvironmentCard({ environment, configureSteam, addGame }: { environment: RuntimeEnvironmentRecord; configureSteam: () => void; addGame: () => void }) {
  const { user } = useAuth();
  const toast = useToast();
  const client = useQueryClient();
  const installations = useQuery({ queryKey: ["environments", environment.id, "game-installations"], queryFn: () => api.gameInstallations(environment.id) });
  const inspect = useMutation({ mutationFn: () => api.inspectEnvironment(environment.id), onSuccess: () => { toast.push("运行环境检测任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const action = useMutation({ mutationFn: ({ item, type, location }: { item: GameInstallationRecord; type: "import" | "validate" | "update" | "reinstall"; location?: { root_index: number; path: string } }) => api.gameInstallationAction(environment.id, item.id, type, location), onSuccess: () => { toast.push("游戏安装检测或维护任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const quarantine = useMutation({ mutationFn: (item: GameInstallationRecord) => api.quarantineGameInstallation(environment.id, item.id), onSuccess: () => { toast.push("游戏安装已提交隔离，至少保留 7 天"); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const items = installations.data?.items || [];
  const canManage = canAccessNode(user, environment.node_id, "manage");
  const canRunTasks = canManage && environment.agent_supported && environment.node_status === "online";
  return <Card className="environment-card">
    <div className="card-heading"><div><span className="eyebrow">{environment.node_status}</span><h2>{environment.node_name || environment.node_id}</h2></div><StatusBadge status={environment.node_status} pulse={environment.node_status === "online"} /></div>
    {environment.node_status === "online" && !environment.agent_supported && <div className="form-warning"><ShieldAlert size={17} />当前 Agent 不支持运行环境管理，请先升级节点端。</div>}
    {environment.node_status !== "online" && <div className="form-warning"><ShieldAlert size={17} />节点当前离线；连接后才能检测能力并执行运行环境任务。</div>}
    <div className="environment-summary"><div><Wrench size={18} /><span>系统依赖</span><strong>{String(environment.system_dependencies.status || "未检测")}</strong></div><div><Settings2 size={18} /><span>SteamCMD</span><strong>{environment.steamcmd_status === "ready" ? "可用" : environment.steamcmd_status === "error" ? "异常" : "未配置"}</strong></div><div><HardDrive size={18} /><span>游戏安装</span><strong>{items.length}</strong></div></div>
    <div className="environment-path"><span>活动 SteamCMD</span><code>{environment.steamcmd_path || "未配置"}</code>{environment.last_error && <small>{environment.last_error}</small>}</div>
    <div className="environment-actions"><Button variant="secondary" disabled={!canRunTasks} loading={inspect.isPending} onClick={() => inspect.mutate()}><RefreshCw size={15} />手动检测</Button><Button variant="secondary" disabled={!canRunTasks} onClick={configureSteam}><Settings2 size={15} />配置 SteamCMD</Button><Button disabled={!canRunTasks} onClick={addGame}><Plus size={15} />添加游戏安装</Button></div>
    <div className="installation-list">{installations.isPending ? <LoadingBlock /> : items.length ? items.map((item) => <div className="installation-row" key={item.id}><div><strong>{item.path}</strong><small>根目录 #{item.root_index} · App {item.app_id} · 构建 {item.build_id || "待检测"}{item.last_checked_at ? ` · ${dateTime(item.last_checked_at)}` : ""}</small>{item.last_error && <small className="text-danger">{item.last_error}</small>}</div><StatusBadge status={item.status} /><div className="installation-row__actions"><Button variant="ghost" disabled={!canRunTasks || item.status === "quarantined" || action.isPending} onClick={() => action.mutate({ item, type: "import" })}>检测目录</Button><Button variant="ghost" disabled={!canRunTasks || item.status === "quarantined" || action.isPending} onClick={() => { const root = window.prompt("根目录索引", String(item.root_index)); if (root == null) return; const rootIndex = Number(root); if (!Number.isInteger(rootIndex) || rootIndex < 0) { toast.push("根目录索引必须是非负整数", "error"); return; } const nextPath = window.prompt("游戏安装相对路径", item.path); if (nextPath?.trim()) action.mutate({ item, type: "import", location: { root_index: rootIndex, path: nextPath.trim() } }); }}>修改路径</Button><Button variant="ghost" disabled={!canRunTasks || item.status === "quarantined" || action.isPending} onClick={() => action.mutate({ item, type: "validate" })}>Steam 校验</Button><Button variant="ghost" disabled={!canRunTasks || item.status === "quarantined" || action.isPending} onClick={() => action.mutate({ item, type: "update" })}>更新</Button><Button variant="ghost" disabled={!canRunTasks || item.status === "quarantined" || action.isPending} onClick={() => action.mutate({ item, type: "reinstall" })}>重装</Button><Button variant="danger" disabled={!canRunTasks || Boolean(item.bound_instance_id) || item.status === "quarantined" || quarantine.isPending} title={item.bound_instance_id ? "请先删除实例并解除绑定" : undefined} onClick={() => { if (window.confirm(`将 ${item.path} 移入隔离区并保留至少 7 天？`)) quarantine.mutate(item); }}><Trash2 size={14} /></Button></div></div>) : <EmptyState title="没有游戏安装" description="可以安装新的 App 222860，或登记并校验手动准备好的目录。" />}</div>
  </Card>;
}

function SteamCMDModal({ environment, onClose }: { environment: RuntimeEnvironmentRecord | null; onClose: () => void }) {
  const [path, setPath] = useState("");
  const toast = useToast();
  const client = useQueryClient();
  const action = useMutation({ mutationFn: ({ type, value }: { type: "configure" | "clear" | "install" | "repair"; value?: string }) => api.steamCMDAction(environment!.id, type, value), onSuccess: (_result, values) => { toast.push(values.type === "configure" ? "自定义 SteamCMD 校验任务已提交" : "SteamCMD 操作已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); onClose(); }, onError: (error) => toast.push(error.message, "error") });
  return <Modal open={Boolean(environment)} onClose={onClose} title="配置 SteamCMD" description="每个节点只有一套活动 SteamCMD；切换成功后，所有游戏安装维护都使用它。" footer={<Button variant="secondary" onClick={onClose}>关闭</Button>}>
    <div className="form-stack"><Field label="自定义绝对路径" hint="Windows 必须指向 steamcmd.exe；Linux 指向 steamcmd.sh 或 steamcmd。路径不能位于游戏根目录内。"><div className="inline-field"><input value={path} onChange={(event) => setPath(event.target.value)} placeholder={environment?.node_status === "online" ? "输入节点主机上的绝对路径" : "节点离线"} /><Button disabled={!path.trim() || environment?.node_status !== "online"} loading={action.isPending} onClick={() => action.mutate({ type: "configure", value: path.trim() })}>校验并启用</Button></div></Field><div className="form-note"><strong>中心端托管</strong><span>由 Agent 下载官方 SteamCMD 到自身数据目录，校验成功后切换为活动路径。</span></div><div className="modal-action-row"><Button onClick={() => action.mutate({ type: "install" })} loading={action.isPending}><Download size={15} />安装托管 SteamCMD</Button><Button variant="secondary" onClick={() => action.mutate({ type: "repair" })} loading={action.isPending}>修复托管版本</Button><Button variant="danger" onClick={() => action.mutate({ type: "clear" })} loading={action.isPending}>清除活动路径</Button></div></div>
  </Modal>;
}

function GameInstallationModal({ environment, onClose }: { environment: RuntimeEnvironmentRecord | null; onClose: () => void }) {
  const [operation, setOperation] = useState<"install" | "import">("import");
  const [rootIndex, setRootIndex] = useState(0);
  const [path, setPath] = useState("");
  const toast = useToast();
  const client = useQueryClient();
  const create = useMutation({ mutationFn: () => api.createGameInstallation(environment!.id, { root_index: rootIndex, path: path.trim(), operation }), onSuccess: () => { toast.push(operation === "install" ? "游戏安装任务已提交" : "现有游戏目录校验任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); void client.invalidateQueries({ queryKey: ["environments", environment!.id, "game-installations"] }); onClose(); }, onError: (error) => toast.push(error.message, "error") });
  const submit = (event: FormEvent) => { event.preventDefault(); create.mutate(); };
  return <Modal open={Boolean(environment)} onClose={onClose} title="添加游戏安装" description="只访问 Agent 配置的游戏根目录，不进行磁盘扫描。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button type="submit" form="game-installation-form" loading={create.isPending}>提交任务</Button></>}><form id="game-installation-form" className="form-stack" onSubmit={submit}><div className="segmented"><button type="button" className={operation === "import" ? "active" : ""} onClick={() => setOperation("import")}><FolderInput size={15} />登记现有目录</button><button type="button" className={operation === "install" ? "active" : ""} onClick={() => setOperation("install")}><Download size={15} />全新安装</button></div><div className="form-grid"><Field label="根目录索引"><input type="number" min={0} value={rootIndex} onChange={(event) => setRootIndex(Number(event.target.value))} required /></Field><Field label="相对路径"><input value={path} onChange={(event) => setPath(event.target.value)} placeholder="servers/l4d2-01" required /></Field></div><div className="form-note"><strong>独占绑定</strong><span>一个游戏安装最多绑定一个实例；实例删除后文件会保留，可再次绑定或在本页隔离。</span></div></form></Modal>;
}
