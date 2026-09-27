import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ChevronDown, ChevronUp, Database, HardDrive, Link2, Plus, RefreshCw, Search, ShieldCheck, Trash2, UserRound } from "lucide-react";
import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, PageHeader, ProgressBar, StatusBadge } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { hasAnyPermission, hasInstancePermission, hasMapNodePermission, hasNodePermission } from "../lib/account-permissions";
import { api } from "../lib/api";
import { bytes, dateTime } from "../lib/format";
import { mapDeduplicatedSize, mapDuplicateCounts, mapMatchesSearch, type MapDuplicateFilter } from "../lib/map-filters";
import type { InstanceRecord, MapRecord, User } from "../lib/types";

const UNASSIGNED_INSTANCE_FILTER = "__unassigned__";
const UNKNOWN_UPLOADER_FILTER = "__unknown__";

export function MapsPage() {
  const { user } = useAuth();
  const [importOpen, setImportOpen] = useState(false);
  const [assignmentMap, setAssignmentMap] = useState<MapRecord | null>(null);
  const [nodeFilter, setNodeFilter] = useState("");
  const [uploaderFilter, setUploaderFilter] = useState("");
  const [instanceFilter, setInstanceFilter] = useState("");
  const [search, setSearch] = useState("");
  const [duplicateFilter, setDuplicateFilter] = useState<MapDuplicateFilter>("");
  const realtime = useRealtime();
  const canReadMaps = hasAnyPermission(user, "maps.read");
  const canReadNodes = hasAnyPermission(user, "nodes.read");
  const canReadInstances = hasAnyPermission(user, "instances.read");
  const canReadJobs = hasAnyPermission(user, "jobs.read");
  const canImport = hasAnyPermission(user, "maps.import");
  const maps = useQuery({ queryKey: ["maps", nodeFilter], queryFn: () => api.maps(nodeFilter || undefined), enabled: canReadMaps, refetchInterval: realtime.state === "live" ? false : 10_000 });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, enabled: canReadNodes });
  const instances = useQuery({ queryKey: ["instances"], queryFn: api.instances, enabled: canReadInstances });
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: api.jobs, enabled: canReadJobs, refetchInterval: canReadJobs ? 2_000 : false });
  const mapJobs = (jobs.data?.items || []).filter((job) => job.type.startsWith("map.") && !["succeeded", "failed", "canceled"].includes(job.status));
  const mapItems = useMemo(() => maps.data?.items || [], [maps.data]);
  const totals = useMemo(() => ({ size: mapDeduplicatedSize(mapItems), assignments: mapItems.reduce((value, item) => value + (item.assignments?.length || 0), 0) }), [mapItems]);
  const uploaderOptions = useMemo(() => {
    const uploaders = new Map<string, string>();
    for (const item of mapItems) {
      const value = mapUploaderValue(item);
      if (!uploaders.has(value)) uploaders.set(value, item.created_by_display_name || "未知账号");
    }
    const duplicateLabels = new Map<string, number>();
    for (const label of uploaders.values()) duplicateLabels.set(label, (duplicateLabels.get(label) || 0) + 1);
    return [...uploaders].map(([value, label]) => ({
      value,
      label: value !== UNKNOWN_UPLOADER_FILTER && (duplicateLabels.get(label) || 0) > 1 ? `${label} · ${value.slice(0, 8)}` : label,
    })).sort((left, right) => left.label.localeCompare(right.label, "zh-CN"));
  }, [mapItems]);
  const instanceOptions = useMemo(() => {
    const visibleNodeIds = new Set(mapItems.map((item) => item.node_id));
    return (instances.data?.items || [])
      .filter((instance) => nodeFilter ? instance.node_id === nodeFilter : visibleNodeIds.has(instance.node_id))
      .sort((left, right) => left.name.localeCompare(right.name, "zh-CN"));
  }, [instances.data, mapItems, nodeFilter]);
  const duplicateCounts = useMemo(() => mapDuplicateCounts(mapItems, duplicateFilter), [mapItems, duplicateFilter]);
  const filteredMaps = useMemo(() => mapItems.filter((item) => {
    if (!mapMatchesSearch(item, search)) return false;
    if (duplicateFilter && (duplicateCounts.get(item.id) || 0) < 2) return false;
    if (uploaderFilter && mapUploaderValue(item) !== uploaderFilter) return false;
    if (instanceFilter === UNASSIGNED_INSTANCE_FILTER) return !item.assignments?.length;
    if (instanceFilter && !item.assignments?.some((assignment) => assignment.instance_id === instanceFilter)) return false;
    return true;
  }), [instanceFilter, mapItems, uploaderFilter, search, duplicateFilter, duplicateCounts]);
  const hasAdditionalFilters = Boolean(uploaderFilter || instanceFilter || search.trim() || duplicateFilter);
  const clearFilters = () => { setSearch(""); setDuplicateFilter(""); setUploaderFilter(""); setInstanceFilter(""); };

  return <div>
    <PageHeader eyebrow="SHARED MAP LIBRARY" title="地图管理" description="每个节点只保存一份地图对象，通过同卷硬链接安全分配给多个游戏实例。" actions={<>{canReadMaps && <Button variant="secondary" onClick={() => { void maps.refetch(); if (canReadJobs) void jobs.refetch(); }}><RefreshCw size={16} />刷新</Button>}{canImport && <Button onClick={() => setImportOpen(true)}><Plus size={17} />QQ 闪传导入</Button>}</>} />
    {canReadMaps && <>
    <div className="map-summary">
      <Card><Database size={19} /><div><span>逻辑地图</span><strong>{maps.data?.items.length || 0}</strong></div></Card>
      <Card><HardDrive size={19} /><div><span>地图对象占用空间</span><strong>{bytes(totals.size)}</strong></div></Card>
      <Card><Link2 size={19} /><div><span>实例分配</span><strong>{totals.assignments}</strong></div></Card>
    </div>
    <Card className="toolbar-card map-toolbar">
      <div className="map-toolbar__search-row">
        <div className="search-box"><Search size={17} aria-hidden /><input type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索地图名称、作者、来源文件或建图代码" aria-label="搜索地图" /></div>
        {hasAdditionalFilters && <Button variant="ghost" onClick={clearFilters}>清除筛选</Button>}
        <span className="toolbar-count" role="status">{hasAdditionalFilters ? `${filteredMaps.length} / ${mapItems.length}` : mapItems.length} 张地图</span>
      </div>
      <div className="map-toolbar__filters">
        {canReadNodes && <Field label="节点筛选"><select value={nodeFilter} onChange={(event) => { setNodeFilter(event.target.value); setUploaderFilter(""); setInstanceFilter(""); }}><option value="">全部可见节点</option>{(nodes.data?.items || []).map((node) => <option value={node.id} key={node.id}>{node.name}</option>)}</select></Field>}
        <Field label="上传者筛选"><select value={uploaderFilter} onChange={(event) => setUploaderFilter(event.target.value)}><option value="">全部上传者</option>{uploaderOptions.map((uploader) => <option value={uploader.value} key={uploader.value}>{uploader.label}</option>)}</select></Field>
        {canReadInstances && <Field label="实例筛选"><select value={instanceFilter} onChange={(event) => setInstanceFilter(event.target.value)}><option value="">全部实例分配</option><option value={UNASSIGNED_INSTANCE_FILTER}>未分配</option>{instanceOptions.map((instance) => <option value={instance.id} key={instance.id}>{instance.name}</option>)}</select></Field>}
        <Field label="重复地图筛选"><select value={duplicateFilter} onChange={(event) => setDuplicateFilter(event.target.value as MapDuplicateFilter)}><option value="">全部地图（含重复）</option><option value="content">仅显示内容相同</option><option value="name">仅显示名称重复（疑似）</option></select></Field>
      </div>
      {duplicateFilter && <p className="map-toolbar__hint">{duplicateFilter === "content" ? "按同一节点内最终地图文件的 SHA-256 内容摘要匹配；上传来源、名称和版本不同也不会影响判断。" : "按同一节点内地图名称匹配，仅用于发现疑似重复；名称相同不代表文件内容相同。"}</p>}
    </Card>
    </>}
    {mapJobs.length > 0 && <Card className="map-processing"><div className="card-heading"><div><span className="eyebrow">PROCESSING</span><h2>正在处理</h2></div></div>{mapJobs.map((job) => <div className="map-job" key={job.id}><div><strong>{mapJobLabel(job.type)}</strong><span>{job.message || "等待节点接收任务"}</span></div><ProgressBar value={job.progress} /><code>{job.id.slice(0, 8)}</code></div>)}</Card>}
    {!canReadMaps ? <Card><EmptyState title="地图库清单已隐藏" description="当前账号没有查看地图库的权限，但仍可向已授权节点执行单独授予的导入操作。" action={canImport ? <Button onClick={() => setImportOpen(true)}><Plus size={16} />QQ 闪传导入</Button> : undefined} /></Card> : maps.isPending ? <LoadingBlock label="正在读取地图对象库…" /> : maps.error ? <ErrorState error={maps.error} retry={() => void maps.refetch()} /> : !mapItems.length ? <Card><EmptyState title="地图对象库为空" description="提交一个 QQ 闪传链接，节点会安全解压、合并并清理其中的 VPK。" action={canImport ? <Button onClick={() => setImportOpen(true)}><Plus size={16} />导入第一张地图</Button> : undefined} /></Card> : !filteredMaps.length ? <Card><EmptyState title="没有符合筛选条件的地图" description="尝试修改搜索关键词、上传者、实例或重复地图条件，也可以使用上方“清除筛选”。" /></Card> : <div className="map-list">{filteredMaps.map((item) => <MapCard key={item.id} item={item} instances={instances.data?.items || []} user={user} duplicateLabel={duplicateFilter ? `${duplicateFilter === "content" ? "内容相同" : "名称重复"} · 共 ${duplicateCounts.get(item.id)} 张` : undefined} onAssignments={() => setAssignmentMap(item)} />)}</div>}
    <ImportMapModal open={canImport && importOpen} nodes={nodes.data?.items || []} instances={instances.data?.items || []} user={user} onClose={() => setImportOpen(false)} />
    <AssignmentModal item={assignmentMap} instances={instances.data?.items || []} user={user} onClose={() => setAssignmentMap(null)} />
  </div>;
}

function mapUploaderValue(item: MapRecord) {
  return item.created_by || UNKNOWN_UPLOADER_FILTER;
}

function MapCard({ item, instances, user, duplicateLabel, onAssignments }: { item: MapRecord; instances: InstanceRecord[]; user: User | null; duplicateLabel?: string; onAssignments: () => void }) {
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
  const assigned = (item.assignments || []).map((assignment) => ({ ...assignment, instance: instances.find((value) => value.id === assignment.instance_id) }));
  const modes = item.modes || [];
  const primaryMode = modes.find((mode) => mode.name === "coop") || modes[0];
  const poster = item.poster_url || "/map-default-poster.jpg";
  const detailsId = `map-details-${item.id}`;
  const canAssign = instances.some((instance) => instance.node_id === item.node_id && hasInstancePermission(user, instance, "maps.assign"));
  const canVerify = hasMapNodePermission(user, instances, item.node_id, "maps.verify");
  const canDelete = hasNodePermission(user, item.node_id, "maps.delete")
    || item.created_by === user?.id && hasMapNodePermission(user, instances, item.node_id, "maps.delete");
  return <Card className="map-card">
    <div className="map-card__hero">
      <div className="map-card__poster"><img src={poster} alt={`${item.name} 地图海报`} onError={(event) => { if (!event.currentTarget.src.endsWith("/map-default-poster.jpg")) event.currentTarget.src = "/map-default-poster.jpg"; }} /><div className="map-card__poster-shade" /><span>{item.poster_url ? "MISSION POSTER" : "DEFAULT POSTER"}</span></div>
      <div className="map-card__profile">
        <div className="map-card__identity"><span className="eyebrow">CUSTOM CAMPAIGN</span><h2>{item.name}</h2><small>{item.source_name || "QQ 闪传"} · {dateTime(item.created_at)}</small>
          <div className="map-card__uploader"><UserRound size={12} aria-hidden /><span>上传者</span><strong>{item.created_by_display_name || "未知账号"}</strong></div>
          {duplicateLabel && <span className="map-card__duplicate">{duplicateLabel}</span>}
          <dl className="map-card__metadata">
            <div><dt>作者：</dt><dd>{item.author || "未提供"}</dd></div>
            <div><dt>版本：</dt><dd>{item.version || "未提供"}</dd></div>
            <div><dt>模式：</dt><dd>{modes.length ? modes.map((mode) => mapModeName(mode.name)).join(" / ") : "未声明"}</dd></div>
            <div className="map-card__metadata-size"><dt>处理后 VPK 大小：</dt><dd>{bytes(item.total_size)}</dd></div>
          </dl>
        </div>
        <section className="map-card__assigned-summary">
          <div className="map-card__assigned-heading"><span><Link2 size={15} aria-hidden />已分配实例</span><strong>{assigned.length ? `${assigned.length} 个实例` : "尚未分配"}</strong></div>
          {assigned.length ? <div className="map-assignment-list map-assignment-list--summary">{assigned.map((assignment) => <div key={assignment.instance_id}><Link2 size={13} aria-hidden /><span><strong>{assignment.instance?.name || assignment.instance_id.slice(0, 8)}</strong><small>{assignment.restart_required ? "等待 VPK 刷新" : "已生效"}</small></span></div>)}</div> : <p className="map-muted">点击“分配实例”添加同一节点上的游戏实例。</p>}
        </section>
        <StatusBadge status={item.health} />
      </div>
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
    <div className="map-card__footer"><Button className="map-card__toggle" variant="ghost" aria-expanded={expanded} aria-controls={detailsId} onClick={() => setExpanded((value) => !value)}>{expanded ? <ChevronUp size={14} /> : <ChevronDown size={14} />}{expanded ? "收起详情" : "展开详情"}</Button><div>{canAssign && <Button variant="secondary" onClick={onAssignments}><Link2 size={14} />分配实例</Button>}{canVerify && <Button variant="secondary" loading={verify.isPending} onClick={() => verify.mutate()}><ShieldCheck size={14} />校验</Button>}{canDelete && <Button variant="danger" loading={remove.isPending} disabled={Boolean(item.assignments?.length)} onClick={() => remove.mutate()}><Trash2 size={14} />永久删除</Button>}</div></div>
  </Card>;
}

function asBSPFile(value: string) {
  return value.toLowerCase().endsWith(".bsp") ? value : `${value}.bsp`;
}

function mapModeName(value: string) {
  const names: Record<string, string> = { coop: "合作", realism: "写实", versus: "对抗", survival: "生还者", scavenge: "清道夫" };
  return names[value] || value;
}

function ImportMapModal({ open, nodes, instances, user, onClose }: { open: boolean; nodes: Array<{ id: string; name: string; status: string }>; instances: InstanceRecord[]; user: User | null; onClose: () => void }) {
  const [nodeId, setNodeId] = useState("");
  const [shareUrl, setShareUrl] = useState("");
  const toast = useToast();
  const client = useQueryClient();
  const submit = useMutation({ mutationFn: () => api.importMap(nodeId, shareUrl.trim()), onSuccess: () => { toast.push("地图导入任务已提交"); setShareUrl(""); onClose(); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const handleSubmit = (event: FormEvent) => { event.preventDefault(); submit.mutate(); };
  const eligibleNodes = useMemo(() => {
    const result = new Map<string, { id: string; name: string; status: string }>();
    for (const node of nodes) {
      if (node.status === "online" && hasMapNodePermission(user, instances, node.id, "maps.import")) result.set(node.id, node);
    }
    for (const instance of instances) {
      if (instance.node_status === "online" && hasInstancePermission(user, instance, "maps.import") && !result.has(instance.node_id)) {
        result.set(instance.node_id, { id: instance.node_id, name: instance.node_name || `实例 ${instance.name} 所在节点`, status: instance.node_status });
      }
    }
    return [...result.values()];
  }, [instances, nodes, user]);
  return <Modal open={open} onClose={onClose} title="从 QQ 闪传导入地图" description="一个分享链接会生成一张独立逻辑地图；下载源和中间文件不会保留。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button type="submit" form="import-map" loading={submit.isPending} disabled={!nodeId}>开始导入</Button></>}><form id="import-map" className="form-stack" onSubmit={handleSubmit}><Field label="目标节点"><select value={nodeId} onChange={(event) => setNodeId(event.target.value)} required><option value="">选择在线节点</option>{eligibleNodes.map((node) => <option value={node.id} key={node.id}>{node.name}</option>)}</select></Field>{!eligibleNodes.length && <div className="form-warning"><AlertTriangle size={16} />没有在线且允许导入地图的节点。</div>}<Field label="QQ 闪传链接" hint="仅接受 https://qfile.qq.com/q/...；链接不会写入数据库或任务回显。"><input value={shareUrl} onChange={(event) => setShareUrl(event.target.value)} placeholder="https://qfile.qq.com/q/..." required /></Field><div className="form-note"><strong>处理规则</strong><span>最多递归 3 层 ZIP/7Z/RAR；归档只落盘 VPK 与嵌套归档；同名虚拟路径后者覆盖。</span></div></form></Modal>;
}

function AssignmentModal({ item, instances, user, onClose }: { item: MapRecord | null; instances: InstanceRecord[]; user: User | null; onClose: () => void }) {
  const eligible = useMemo(() => instances.filter((instance) => instance.node_id === item?.node_id && hasInstancePermission(user, instance, "maps.assign")), [instances, item?.node_id, user]);
  const current = useMemo(() => new Set((item?.assignments || []).map((assignment) => assignment.instance_id)), [item]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  useEffect(() => setSelected(new Set(current)), [current, item?.id]);
  const toast = useToast();
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: async () => {
      if (!item) return { manualReload: false };
      const add = eligible.filter((instance) => selected.has(instance.id) && !current.has(instance.id));
      const remove = eligible.filter((instance) => !selected.has(instance.id) && current.has(instance.id));
      const runningRemoval = remove.find((instance) => instance.status !== "stopped");
      if (runningRemoval) throw new Error(`实例“${runningRemoval.name}”运行中，无法取消分配`);
      const manualReload = add.filter((instance) => ["running", "degraded", "external"].includes(instance.status) && !instance.rcon_configured);
      if (manualReload.length && !window.confirm(`实例“${manualReload.map((instance) => instance.name).join("、")}”未配置 RCON。地图仍会分配，但无法自动执行热更新；请在游戏聊天窗口手动输入 !update_vpk 使其立即生效。仍要继续吗？`)) {
        throw new Error("已取消分配");
      }
      await Promise.all([...add.map((instance) => api.assignMap(item.id, instance.id)), ...remove.map((instance) => api.unassignMap(item.id, instance.id))]);
      return { manualReload: manualReload.length > 0 };
    },
    onSuccess: ({ manualReload }) => { toast.push(manualReload ? "地图分配任务已提交；请在未配置 RCON 的实例内输入 !update_vpk" : "地图分配任务已提交；运行中的实例会自动刷新 VPK"); onClose(); void client.invalidateQueries({ queryKey: ["jobs"] }); },
    onError: (error) => { if (error.message !== "已取消分配") toast.push(error.message, "error"); },
  });
  return <Modal open={Boolean(item)} onClose={onClose} title={`分配地图${item ? ` · ${item.name}` : ""}`} description="分配不要求停服；运行中会自动执行 VPK 热更新，取消分配仍只允许已停止实例。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button loading={save.isPending} onClick={() => save.mutate()}>提交变更</Button></>}><div className="map-instance-picker">{eligible.length ? eligible.map((instance) => { const assigned = current.has(instance.id); const cannotRemove = assigned && instance.status !== "stopped"; return <label key={instance.id}><input type="checkbox" checked={selected.has(instance.id)} disabled={cannotRemove} onChange={(event) => setSelected((value) => { const next = new Set(value); if (event.target.checked) next.add(instance.id); else next.delete(instance.id); return next; })} /><span><strong>{instance.name}</strong><small>{instance.status === "stopped" ? "已停止 · 下次启动时加载" : assigned ? "运行中 · 必须停服后才能取消" : instance.rcon_configured ? "运行中 · 分配后自动刷新 VPK" : "运行中 · 需手动输入 !update_vpk"}</small></span></label>; }) : <EmptyState title="没有可分配实例" description="该节点上暂无你可以管理的游戏实例。" />}</div></Modal>;
}

function mapJobLabel(type: string) {
  const labels: Record<string, string> = { "map.import.qq_flash": "导入并处理地图", "map.assign": "分配地图", "map.unassign": "取消地图分配", "map.verify": "校验地图", "map.delete": "永久删除地图" };
  return labels[type] || type;
}
