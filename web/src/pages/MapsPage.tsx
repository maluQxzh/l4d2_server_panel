import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ChevronDown, ChevronUp, Database, HardDrive, Link2, Plus, RefreshCw, RotateCw, ShieldCheck, Trash2 } from "lucide-react";
import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, PageHeader, ProgressBar, StatusBadge } from "../components/ui";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { api } from "../lib/api";
import { bytes, dateTime } from "../lib/format";
import type { InstanceRecord, MapRecord } from "../lib/types";

export function MapsPage() {
  const [importOpen, setImportOpen] = useState(false);
  const [assignmentMap, setAssignmentMap] = useState<MapRecord | null>(null);
  const [nodeFilter, setNodeFilter] = useState("");
  const realtime = useRealtime();
  const maps = useQuery({ queryKey: ["maps", nodeFilter], queryFn: () => api.maps(nodeFilter || undefined), refetchInterval: realtime.state === "live" ? false : 10_000 });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes });
  const instances = useQuery({ queryKey: ["instances"], queryFn: api.instances });
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: api.jobs, refetchInterval: 2_000 });
  const mapJobs = (jobs.data?.items || []).filter((job) => job.type.startsWith("map.") && !["succeeded", "failed", "canceled"].includes(job.status));
  const totals = useMemo(() => (maps.data?.items || []).reduce((value, item) => ({ size: value.size + item.total_size, assignments: value.assignments + (item.assignments?.length || 0) }), { size: 0, assignments: 0 }), [maps.data]);

  return <div>
    <PageHeader eyebrow="SHARED MAP LIBRARY" title="地图管理" description="每个节点只保存一份地图对象，通过同卷硬链接安全分配给多个游戏实例。" actions={<><Button variant="secondary" onClick={() => { void maps.refetch(); void jobs.refetch(); }}><RefreshCw size={16} />刷新</Button><Button onClick={() => setImportOpen(true)}><Plus size={17} />QQ 闪传导入</Button></>} />
    <div className="map-summary">
      <Card><Database size={19} /><div><span>逻辑地图</span><strong>{maps.data?.items.length || 0}</strong></div></Card>
      <Card><HardDrive size={19} /><div><span>地图占用空间</span><strong>{bytes(totals.size)}</strong></div></Card>
      <Card><Link2 size={19} /><div><span>实例分配</span><strong>{totals.assignments}</strong></div></Card>
    </div>
    <Card className="toolbar-card map-toolbar"><Field label="节点筛选"><select value={nodeFilter} onChange={(event) => setNodeFilter(event.target.value)}><option value="">全部可见节点</option>{(nodes.data?.items || []).map((node) => <option value={node.id} key={node.id}>{node.name}</option>)}</select></Field><span className="toolbar-count">{maps.data?.items.length || 0} 张地图</span></Card>
    {mapJobs.length > 0 && <Card className="map-processing"><div className="card-heading"><div><span className="eyebrow">PROCESSING</span><h2>正在处理</h2></div></div>{mapJobs.map((job) => <div className="map-job" key={job.id}><div><strong>{mapJobLabel(job.type)}</strong><span>{job.message || "等待节点接收任务"}</span></div><ProgressBar value={job.progress} /><code>{job.id.slice(0, 8)}</code></div>)}</Card>}
    {maps.isPending ? <LoadingBlock label="正在读取地图对象库…" /> : maps.error ? <ErrorState error={maps.error} retry={() => void maps.refetch()} /> : !maps.data.items.length ? <Card><EmptyState title="地图对象库为空" description="提交一个 QQ 闪传链接，节点会安全解压、合并并清理其中的 VPK。" action={<Button onClick={() => setImportOpen(true)}><Plus size={16} />导入第一张地图</Button>} /></Card> : <div className="map-list">{maps.data.items.map((item) => <MapCard key={item.id} item={item} instances={instances.data?.items || []} onAssignments={() => setAssignmentMap(item)} />)}</div>}
    <ImportMapModal open={importOpen} nodes={nodes.data?.items || []} onClose={() => setImportOpen(false)} />
    <AssignmentModal item={assignmentMap} instances={instances.data?.items || []} onClose={() => setAssignmentMap(null)} />
  </div>;
}

function MapCard({ item, instances, onAssignments }: { item: MapRecord; instances: InstanceRecord[]; onAssignments: () => void }) {
  const [expanded, setExpanded] = useState(false);
  const toast = useToast();
  const client = useQueryClient();
  const verify = useMutation({ mutationFn: () => api.verifyMap(item.id), onSuccess: () => { toast.push("地图校验任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const remove = useMutation({
    mutationFn: async () => {
      if (item.assignments?.length) throw new Error("请先取消全部实例分配");
      if (!window.confirm(`永久删除“${item.name}”及其所有无引用卷副本？此操作不可恢复。`)) throw new Error("已取消删除");
      const confirmation = await api.createMapDeleteConfirmation(item.id);
      return api.deleteMap(item.id, confirmation.confirmation_token);
    },
    onSuccess: () => { toast.push("永久删除任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); },
    onError: (error) => { if (error.message !== "已取消删除") toast.push(error.message, "error"); },
  });
  const restart = useMutation({ mutationFn: (instanceId: string) => api.instanceAction(instanceId, "restart"), onSuccess: () => { toast.push("实例重启任务已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const assigned = (item.assignments || []).map((assignment) => ({ ...assignment, instance: instances.find((value) => value.id === assignment.instance_id) }));
  const modes = item.modes || [];
  const primaryMode = modes.find((mode) => mode.name === "coop") || modes[0];
  const poster = item.poster_url || "/map-default-poster.jpg";
  const detailsId = `map-details-${item.id}`;
  return <Card className="map-card">
    <div className="map-card__hero">
      <div className="map-card__poster"><img src={poster} alt={`${item.name} 地图海报`} onError={(event) => { if (!event.currentTarget.src.endsWith("/map-default-poster.jpg")) event.currentTarget.src = "/map-default-poster.jpg"; }} /><div className="map-card__poster-shade" /><span>{item.poster_url ? "MISSION POSTER" : "DEFAULT POSTER"}</span></div>
      <div className="map-card__profile">
        <div className="map-card__title"><div><span className="eyebrow">CUSTOM CAMPAIGN</span><h2>{item.name}</h2><small>{item.source_name || "QQ 闪传"} · {dateTime(item.created_at)}</small></div><StatusBadge status={item.health} /></div>
        <div className="map-card__size"><HardDrive size={17} aria-hidden /><span><small>处理后的 VPK 大小</small><strong>{bytes(item.total_size)}</strong></span></div>
      </div>
    </div>
    <div className="map-card__snapshot">
      <dl className="map-card__facts">
        <div><dt>战役</dt><dd>{item.name}</dd></div>
        <div><dt>作者</dt><dd>{item.author || "未提供"}</dd></div>
        <div><dt>版本</dt><dd>{item.version || "未提供"}</dd></div>
        <div><dt>模式</dt><dd>{modes.length ? modes.map((mode) => mapModeName(mode.name)).join(" / ") : "未声明"}</dd></div>
      </dl>
      <section className="map-card__assigned-summary">
        <div className="map-card__assigned-heading"><Link2 size={16} aria-hidden /><span><small>已分配实例</small><strong>{assigned.length ? `${assigned.length} 个实例` : "尚未分配"}</strong></span></div>
        {assigned.length ? <div className="map-assignment-list map-assignment-list--summary">{assigned.map((assignment) => <div key={assignment.instance_id}><span><strong>{assignment.instance?.name || assignment.instance_id.slice(0, 8)}</strong><small>{assignment.restart_required ? "等待重启生效" : "已生效"}</small></span>{assignment.restart_required && <Button variant="ghost" disabled={restart.isPending} onClick={() => restart.mutate(assignment.instance_id)}><RotateCw size={13} />立即重启</Button>}</div>)}</div> : <p className="map-muted">可以通过右下角“分配实例”将地图链接到同一节点上的游戏实例。</p>}
      </section>
    </div>
    {expanded && <div className="map-card__details" id={detailsId}>
        {item.description && <p className="map-card__description">{item.description}</p>}
        <div className="map-card__campaign"><h3>章节{primaryMode ? ` · ${mapModeName(primaryMode.name)}` : ""}</h3>{primaryMode ? <div className="map-chapter-route">{primaryMode.chapters.map((chapter, index) => <div className="map-chapter" key={`${primaryMode.name}-${chapter.ordinal}`}><span>{chapter.ordinal}</span><div><strong>{chapter.display_name}</strong><code>{asBSPFile(chapter.bsp_name)}</code></div>{index < primaryMode.chapters.length - 1 && <i aria-hidden="true">→</i>}</div>)}</div> : <p className="map-muted">Mission 文件未提供章节顺序</p>}</div>
        <div className="map-card__content">
          <section><h3>全部 BSP 建图代码</h3><div className="map-bsp-list">{(item.bsps || []).map((bsp) => <span key={bsp.virtual_path} className={!bsp.dictionary_present ? "warning" : ""}><code>{asBSPFile(bsp.start_name || bsp.virtual_path.replace(/^maps\//i, ""))}</code>{!bsp.dictionary_present && <small><AlertTriangle size={12} />缺少字典</small>}</span>)}</div></section>
          {(item.warnings?.length || 0) > 0 && <section className="map-warnings"><h3>处理警告</h3>{item.warnings!.map((warning, index) => <div key={`${warning.code}-${index}`}><AlertTriangle size={14} /><span><strong>{warning.message}</strong>{warning.virtual_path && <code>{warning.virtual_path}</code>}</span></div>)}</section>}
        </div>
        <code className="map-card__checksum" title={item.aggregate_sha256}>SHA-256 · {item.aggregate_sha256.slice(0, 16)}…</code>
      </div>}
    <div className="map-card__footer"><Button className="map-card__toggle" variant="ghost" aria-expanded={expanded} aria-controls={detailsId} onClick={() => setExpanded((value) => !value)}>{expanded ? <ChevronUp size={14} /> : <ChevronDown size={14} />}{expanded ? "收起详情" : "展开详情"}</Button><div><Button variant="secondary" onClick={onAssignments}><Link2 size={14} />分配实例</Button><Button variant="secondary" loading={verify.isPending} onClick={() => verify.mutate()}><ShieldCheck size={14} />校验</Button><Button variant="danger" loading={remove.isPending} disabled={Boolean(item.assignments?.length)} onClick={() => remove.mutate()}><Trash2 size={14} />永久删除</Button></div></div>
  </Card>;
}

function asBSPFile(value: string) {
  return value.toLowerCase().endsWith(".bsp") ? value : `${value}.bsp`;
}

function mapModeName(value: string) {
  const names: Record<string, string> = { coop: "合作", realism: "写实", versus: "对抗", survival: "生还者", scavenge: "清道夫" };
  return names[value] || value;
}

function ImportMapModal({ open, nodes, onClose }: { open: boolean; nodes: Array<{ id: string; name: string; status: string }>; onClose: () => void }) {
  const [nodeId, setNodeId] = useState("");
  const [shareUrl, setShareUrl] = useState("");
  const toast = useToast();
  const client = useQueryClient();
  const submit = useMutation({ mutationFn: () => api.importMap(nodeId, shareUrl.trim()), onSuccess: () => { toast.push("地图导入任务已提交"); setShareUrl(""); onClose(); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const handleSubmit = (event: FormEvent) => { event.preventDefault(); submit.mutate(); };
  return <Modal open={open} onClose={onClose} title="从 QQ 闪传导入地图" description="一个分享链接会生成一张独立逻辑地图；下载源和中间文件不会保留。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button type="submit" form="import-map" loading={submit.isPending}>开始导入</Button></>}><form id="import-map" className="form-stack" onSubmit={handleSubmit}><Field label="目标节点"><select value={nodeId} onChange={(event) => setNodeId(event.target.value)} required><option value="">选择在线节点</option>{nodes.filter((node) => node.status === "online").map((node) => <option value={node.id} key={node.id}>{node.name}</option>)}</select></Field><Field label="QQ 闪传链接" hint="仅接受 https://qfile.qq.com/q/...；链接不会写入数据库或任务回显。"><input value={shareUrl} onChange={(event) => setShareUrl(event.target.value)} placeholder="https://qfile.qq.com/q/..." required /></Field><div className="form-note"><strong>处理规则</strong><span>最多递归 3 层 ZIP/7Z/RAR；同名虚拟路径后者覆盖；缺失 BSP 字典仅警告，不阻止导入。</span></div></form></Modal>;
}

function AssignmentModal({ item, instances, onClose }: { item: MapRecord | null; instances: InstanceRecord[]; onClose: () => void }) {
  const eligible = useMemo(() => instances.filter((instance) => instance.node_id === item?.node_id), [instances, item?.node_id]);
  const current = useMemo(() => new Set((item?.assignments || []).map((assignment) => assignment.instance_id)), [item]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  useEffect(() => setSelected(new Set(current)), [current, item?.id]);
  const toast = useToast();
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: async () => {
      if (!item) return;
      const add = eligible.filter((instance) => selected.has(instance.id) && !current.has(instance.id));
      const remove = eligible.filter((instance) => !selected.has(instance.id) && current.has(instance.id));
      const runningRemoval = remove.find((instance) => instance.status !== "stopped");
      if (runningRemoval) throw new Error(`实例“${runningRemoval.name}”运行中，无法取消分配`);
      await Promise.all([...add.map((instance) => api.assignMap(item.id, instance.id)), ...remove.map((instance) => api.unassignMap(item.id, instance.id))]);
    },
    onSuccess: () => { toast.push("地图分配任务已提交；运行中的实例可选择稍后重启"); onClose(); void client.invalidateQueries({ queryKey: ["jobs"] }); },
    onError: (error) => toast.push(error.message, "error"),
  });
  return <Modal open={Boolean(item)} onClose={onClose} title={`分配地图${item ? ` · ${item.name}` : ""}`} description="分配不要求停服；取消分配只允许已停止实例。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button loading={save.isPending} onClick={() => save.mutate()}>提交变更</Button></>}><div className="map-instance-picker">{eligible.length ? eligible.map((instance) => { const assigned = current.has(instance.id); const cannotRemove = assigned && instance.status !== "stopped"; return <label key={instance.id}><input type="checkbox" checked={selected.has(instance.id)} disabled={cannotRemove} onChange={(event) => setSelected((value) => { const next = new Set(value); if (event.target.checked) next.add(instance.id); else next.delete(instance.id); return next; })} /><span><strong>{instance.name}</strong><small>{instance.status === "stopped" ? "已停止" : assigned ? "运行中 · 必须停服后才能取消" : "运行中 · 分配后需要重启"}</small></span></label>; }) : <EmptyState title="没有可分配实例" description="该节点上暂无你可以管理的游戏实例。" />}</div></Modal>;
}

function mapJobLabel(type: string) {
  const labels: Record<string, string> = { "map.import.qq_flash": "导入并处理地图", "map.assign": "分配地图", "map.unassign": "取消地图分配", "map.verify": "校验地图", "map.delete": "永久删除地图" };
  return labels[type] || type;
}
