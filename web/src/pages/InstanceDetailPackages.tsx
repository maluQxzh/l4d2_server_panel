import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, CloudDownload, Download, FileCode2, PackageOpen, Plus, RotateCcw, Search, ShieldAlert, ShieldCheck, Trash2, Upload } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { FileUploadInput } from "../components/FileUploadInput";
import { PackageBatchControls, RenamePackageModal } from "../components/PackageManagement";
import { PreparedPackages, usePreparedPackages } from "../components/PreparedPackages";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useToast } from "../context/ToastContext";
import { hasInstancePermission } from "../lib/account-permissions";
import { api, ApiError, newIdempotencyKey } from "../lib/api";
import { bytes, dateTime } from "../lib/format";
import { filterAndSortPackages, type PackageSort } from "../lib/package-list";
import { refreshQueries } from "../lib/query-refresh";
import { scanPollInterval } from "../lib/task-polling";
import type { InstanceRecord, MapPathEncoding, PackageRecord, SmartPluginFilePlan, SmartPluginPlan, SmartPluginSelection } from "../lib/types";
import { FileEditorModal, type FileEditorTarget } from './InstanceDetailFiles';
import { bytesToBase64, OptionalUnavailable } from './InstanceDetailShared';

export function ModsTab({ instance }: { instance: InstanceRecord }) {
  return <PackagesTab instance={instance} section="mods" />;
}

export function PluginsTab({ instance }: { instance: InstanceRecord }) {
  return <PackagesTab instance={instance} section="plugins" />;
}

type PackageSection = "mods" | "plugins";

function PackagesTab({ instance, section }: { instance: InstanceRecord; section: PackageSection }) {
  const { user } = useAuth();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<PackageSort>("installed_desc");
  const [selected, setSelected] = useState<string[]>([]);
  const [batchBusy, setBatchBusy] = useState(false);
  const canDelete = hasInstancePermission(user, instance, "packages.delete");
  const canRead = hasInstancePermission(user, instance, "packages.read");
  const canInstall = hasInstancePermission(user, instance, "packages.install");
  const canToggle = hasInstancePermission(user, instance, "packages.toggle");
  const toast = useToast();
  const client = useQueryClient();
  const packages = useQuery({ queryKey: ["instances", instance.id, "packages"], queryFn: () => api.packages(instance.id), enabled: canRead, refetchInterval: canRead ? 15_000 : false });
  const pluginSection = section === "plugins";
  const scanState = useQuery({ queryKey: ["instances", instance.id, "package-scan"], queryFn: () => api.installedPackageScan(instance.id), enabled: canInstall && !pluginSection, retry: false, refetchInterval: (query) => scanPollInterval(query.state.data) });
  const scanStatus = scanState.data?.scan;
  useEffect(() => {
    if (scanStatus?.outcome === "applied") void refreshQueries(client, { queryKey: ["instances", instance.id, "packages"] });
  }, [client, instance.id, scanStatus?.job_id, scanStatus?.outcome]);
  const scanMessage = !scanStatus ? "" : scanStatus.busy
    ? `扫描处理中 ${scanStatus.progress}%，其他操作可以继续`
    : scanStatus.status === "succeeded"
      ? scanStatus.outcome === "stale" ? "扫描期间实例发生变更，结果已作废；请在操作完成后重新扫描" : `扫描完成，新增纳管 ${scanStatus.registered_count} 个模组`
      : scanStatus.status === "failed" ? "扫描失败，请查看任务中心" : scanStatus.status === "canceled" ? "扫描已取消" : "扫描等待中";

  const sectionItems = packages.data?.items.filter((item) => pluginSection ? item.kind !== "vpk" && item.kind !== "workshop" : item.kind === "vpk" || item.kind === "workshop") || [];
  const items = filterAndSortPackages(sectionItems, search, sort);
  const metamodInstalled = packages.data ? packages.data.items.some((item) => item.kind === "metamod" && item.enabled) : undefined;
  const noun = pluginSection ? "插件" : "模组";
  const disablingBlocked = canToggle && instance.status !== "stopped" && sectionItems.some((item) => item.enabled && (item.kind === "smart" || /\.(?:vpk|smx)$/i.test(item.path || "")));
  const scan = useMutation({
    mutationFn: () => api.scanInstalledPackages(instance.id),
    onSuccess: () => {
      toast.push("扫描任务已提交；期间可以继续操作实例，发生变更时会丢弃旧结果");
      void refreshQueries(client, { queryKey: ["instances", instance.id, "package-scan"] });
      void refreshQueries(client, { queryKey: ["jobs"] });
      void refreshQueries(client, { queryKey: ["instances", instance.id, "packages"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const description = pluginSection ? "智能识别 ZIP、7Z、RAR 中的 SourceMod 插件、配置、脚本与原生扩展，也支持 SourceMod/Metamod 整包。" : "支持 VPK、Workshop 项目与合集、QQ闪传批量导入，以及扫描 left4dead2/addons 中已有的 VPK。";
  const installButton = canInstall ? <Button onClick={() => setOpen(true)}><Plus size={16} />安装{noun}</Button> : undefined;
  const headingActions = canInstall ? <div className="package-heading-actions">{!pluginSection && <Button variant="secondary" loading={scan.isPending} disabled={scanStatus?.busy} onClick={() => scan.mutate()} title="扫描 left4dead2/addons 直属目录中的 VPK"><Search size={16} />扫描已安装</Button>}{installButton}</div> : undefined;
  return <Card><div className="card-heading"><div><span className="eyebrow">{pluginSection ? "SERVER PLUGINS" : "ADD-ONS"}</span><h2>{pluginSection ? "插件与服务端组件" : "模组"}</h2></div>{headingActions}</div>{canInstall && !pluginSection && scanMessage && <p role="status">{scanMessage}</p>}{canRead && !packages.isPending && !packages.error && sectionItems.length > 0 && <div className="package-toolbar"><div className="search-box"><Search size={16} /><input value={search} disabled={batchBusy} onChange={(event) => { setSearch(event.target.value); setSelected([]); }} placeholder={`按名称搜索${noun}`} aria-label={`按名称搜索${noun}`} /></div><div className="filter-select"><select value={sort} onChange={(event) => setSort(event.target.value as PackageSort)} aria-label={`${noun}排序`}><option value="installed_desc">添加时间：最新优先</option><option value="installed_asc">添加时间：最早优先</option><option value="name_asc">名称：升序</option><option value="name_desc">名称：降序</option></select></div><span className="toolbar-count">{items.length} / {sectionItems.length}</span></div>}{canRead && <PackageBatchControls instance={instance} items={items} selected={selected} setSelected={setSelected} canToggle={canToggle} canDelete={canDelete} busy={batchBusy} setBusy={setBatchBusy} />}{disablingBlocked && <div className="package-runtime-warning" role="status"><ShieldAlert size={16} /><span>当前实例未停止，不能停用已启用的{noun}；请先停止实例。</span></div>}{!canRead ? <EmptyState title={`${noun}清单已隐藏`} description={`当前账号没有查看${noun}清单的权限，但仍可执行上方单独授予的安装操作。`} /> : packages.isPending ? <LoadingBlock /> : packages.error ? <OptionalUnavailable error={packages.error} title={`${noun}清单尚不可用`} description={`当前控制端未开放${noun}清单接口；安装任务仍可通过面板提交。`} /> : items.length ? <div className="package-grid">{items.map((item) => <PackageCard key={item.id} item={item} instance={instance} busy={batchBusy} selected={selected.includes(item.id)} onSelect={canToggle || canDelete ? (checked) => setSelected((ids) => checked ? [...ids, item.id] : ids.filter((id) => id !== item.id)) : undefined} />)}</div> : sectionItems.length ? <EmptyState title={`没有匹配的${noun}`} description={`请更换搜索名称，当前共纳管 ${sectionItems.length} 个${noun}。`} /> : <EmptyState title={`还没有安装${noun}`} description={description} />}{canInstall && <PreparedPackages instanceId={instance.id} plugins={pluginSection} />}<InstallPackageModal open={canInstall && open} instance={instance} section={section} metamodInstalled={metamodInstalled} smartPackages={sectionItems.filter((item) => item.kind === "smart")} onClose={() => setOpen(false)} /></Card>;
}

function PackageCard({ item, instance, busy, selected: checked, onSelect }: { item: PackageRecord; instance: InstanceRecord; busy: boolean; selected: boolean; onSelect?: (checked: boolean) => void }) {
  const { user } = useAuth();
  const instanceId = instance.id;
  const canToggle = hasInstancePermission(user, instance, "packages.toggle");
  const canDelete = hasInstancePermission(user, instance, "packages.delete");
  const canRollback = hasInstancePermission(user, instance, "packages.rollback");
  const toast = useToast();
  const client = useQueryClient();
  const [selectedVersion, setSelectedVersion] = useState("");
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [renameOpen, setRenameOpen] = useState(false);
  const canRename = hasInstancePermission(user, instance, "packages.install");
  const [editor, setEditor] = useState<FileEditorTarget | null>(null);
  const toggle = useMutation({ mutationFn: (liveAcknowledged: boolean) => api.instanceAction(instanceId, "toggle_package", { package_id: item.id, enabled: !item.enabled, acknowledge_live_changes: liveAcknowledged, acknowledge_restart_required: liveAcknowledged }), onSuccess: () => { toast.push(item.enabled ? "组件停用任务已提交" : "组件启用任务已提交"); void refreshQueries(client, { queryKey: ["instances", instanceId, "packages"] }); }, onError: (error) => toast.push(error.message, "error") });
  const toggleable = item.kind === "smart" || /\.(?:vpk|smx)$/i.test(item.path || "");
  const serverComponent = item.kind === "sourcemod" || item.kind === "metamod";
  const rollbackable = item.kind !== "smart" && toggleable;
  const disablingBlocked = item.enabled && instance.status !== "stopped";
  const deletable = serverComponent || (toggleable && Boolean(item.sha256));
  const componentInventoryMissing = serverComponent && (!item.sha256 || !item.file_count);
  const managedDeleteBlocked = (item.kind === "smart" || serverComponent) && instance.status !== "stopped";
  const remove = useMutation({
    mutationFn: (options?: { allowChangedFiles?: boolean; preserveChangedConfig?: boolean }) => options ? api.deletePackage(instanceId, item.id, undefined, options) : api.deletePackage(instanceId, item.id),
    onSuccess: () => {
      toast.push("组件删除任务已提交");
      void refreshQueries(client, { queryKey: ["jobs"] });
      void refreshQueries(client, { queryKey: ["instances", instanceId, "packages"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const versions = useQuery({
    queryKey: ["instances", instanceId, "packages", item.id, "versions"],
    queryFn: () => api.packageVersions(instanceId, item.id),
    enabled: rollbackable && canRollback,
    refetchInterval: rollbackable && canRollback ? 15_000 : false,
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
      void refreshQueries(client, { queryKey: ["instances", instanceId, "packages"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const selected = versions.data?.items.find((version) => version.id === selectedVersion);
  return <><fieldset disabled={busy} className={`package-card ${checked ? "package-card--selected" : ""}`}>
    <div className={`package-icon package-icon--${item.kind}`}>{onSelect ? <input type="checkbox" checked={checked} aria-label={`选择 ${item.name}`} onChange={(event) => onSelect(event.target.checked)} /> : <PackageOpen size={20} />}</div>
    <button type="button" className="package-card__identity" onClick={() => setDetailsOpen(true)} aria-label={`查看 ${item.name} 详情`}><strong>{item.name}</strong><span>{item.kind === "smart" ? `智能识别 · ${item.file_count || 0} 个文件` : `${item.kind.toUpperCase()} · ${item.version || "版本未知"}`}</span>{item.sha256 && <code>{item.sha256.slice(0, 16)}…</code>}{item.restart_required && <span className="package-restart-required">需要重启实例完成加载</span>}<span className="package-card__detail-hint">查看详情 <ChevronRight size={12} /></span></button>
    <div className="package-card__controls">
      <div className="package-card__actions">
        {canRename && <button type="button" className="package-rollback-button" aria-label={`重命名 ${item.name}`} disabled={remove.isPending || toggle.isPending} onClick={() => setRenameOpen(true)}>重命名</button>}
        {toggleable && canToggle ? <button className={`switch ${item.enabled ? "switch--on" : ""}`} disabled={toggle.isPending || remove.isPending || disablingBlocked} title={disablingBlocked ? "请先停止实例，再停用组件" : undefined} onClick={() => {
          if (item.kind === "smart" && !item.enabled && instance.status !== "stopped") {
            if (!window.confirm("实例正在运行。现在启用只会移动插件入口文件，已加载模块不会立刻变化。仍要继续吗？")) return;
            if (!window.confirm("请再次确认：本次变更需要重启实例才能完整生效。")) return;
            toggle.mutate(true);
            return;
          }
          toggle.mutate(false);
        }} aria-label={`${item.enabled ? "停用" : "启用"} ${item.name}`}><span /></button> : <span className="package-installed">{item.enabled ? "已启用" : "已停用"}</span>}
        {deletable && canDelete && <button className="package-delete-button" disabled={remove.isPending || toggle.isPending || managedDeleteBlocked || componentInventoryMissing} aria-label={`删除 ${item.name}`} title={componentInventoryMissing ? "请先重新安装一次该组件，以建立安全卸载清单" : managedDeleteBlocked ? "请先停止实例，再删除该组件" : "删除组件"} onClick={() => {
          const componentNotice = item.kind === "sourcemod" ? "admins_simple.ini 和由其他插件包纳管的文件会保留。" : item.kind === "metamod" ? "如果仍安装 SourceMod，必须先删除 SourceMod。" : "";
          if (!window.confirm(`确认永久删除 ${item.name}？${componentNotice}文件及面板中的版本历史会一并移除。`)) return;
          if (item.kind === "smart") {
            const preserveConfig = window.confirm("如果发现已修改的配置文件：点击“确定”保留配置，点击“取消”一并删除。异常的程序文件会在确认后删除。");
            remove.mutate({ allowChangedFiles: true, preserveChangedConfig: preserveConfig });
          } else {
            remove.mutate(undefined);
          }
        }}><Trash2 size={14} /></button>}
      </div>
      {canRollback && rollbackable && versions.data?.items.length ? <div className="package-version-rollback">
        <select value={selectedVersion} onChange={(event) => setSelectedVersion(event.target.value)} aria-label={`${item.name} 可回退版本`} disabled={!item.enabled || rollback.isPending}>
          {versions.data.items.map((version) => <option value={version.id} key={version.id}>{version.version || "未标记"} · {version.sha256.slice(0, 10)} · {dateTime(version.created_at)}</option>)}
        </select>
        <button className="package-rollback-button" disabled={!item.enabled || !selectedVersion || rollback.isPending} title={!item.enabled ? "请先启用组件" : `恢复 ${selected?.version || "选中版本"}`} onClick={() => {
          if (selected && window.confirm(`确认将 ${item.name} 回退到 ${selected.version || selected.sha256.slice(0, 12)}？当前文件会作为新的可回退版本保留。`)) rollback.mutate();
        }}><RotateCcw size={13} />{rollback.isPending ? "提交中" : "回退"}</button>
      </div> : canRollback && rollbackable && versions.error ? <small className="package-version-error">版本历史暂不可用</small> : null}
    </div>
  </fieldset>{renameOpen && <RenamePackageModal instance={instance} item={item} onClose={() => setRenameOpen(false)} />}<PackageDetailsModal open={detailsOpen} item={item} instance={instance} onClose={() => setDetailsOpen(false)} onEdit={(file) => {
    setDetailsOpen(false);
    setEditor({ path: file.path, isNew: false, packageId: item.id, packageName: item.name });
  }} /><FileEditorModal instance={instance} editor={editor} onClose={() => {
    setEditor(null);
    void refreshQueries(client, { queryKey: ["instances", instanceId, "packages"] });
    void refreshQueries(client, { queryKey: ["instances", instanceId, "packages", item.id, "details"] });
  }} /></>;
}

function PackageDetailsModal({ open, item, instance, onClose, onEdit }: { open: boolean; item: PackageRecord; instance: InstanceRecord; onClose: () => void; onEdit: (file: { path: string }) => void }) {
  const { user } = useAuth();
  const canEdit = hasInstancePermission(user, instance, "files.read") && hasInstancePermission(user, instance, "files.write");
  const details = useQuery({
    queryKey: ["instances", instance.id, "packages", item.id, "details"],
    queryFn: () => api.packageDetails(instance.id, item.id),
    enabled: open,
  });
  return <Modal open={open} onClose={onClose} title={item.name} description="显示该组件由面板记录的所有实例内文件位置。" footer={<Button variant="secondary" onClick={onClose}>关闭</Button>}>
    {details.isPending ? <LoadingBlock label="正在读取插件文件清单…" /> : details.error ? <ErrorState error={details.error} retry={() => void details.refetch()} /> : <div className="package-details">
      <div className="package-details__summary"><span>{item.kind === "smart" ? "智能识别插件" : item.kind.toUpperCase()}</span><span>{details.data?.files.length || 0} 个已记录文件</span>{item.sha256 && <code>{item.sha256}</code>}</div>
      {details.data?.files.length ? <div className="package-file-list">{details.data.files.map((file) => <div className="package-file-row" key={file.path}>
        <div className={`file-type file-type--file`}><FileCode2 size={17} /></div>
        <div><strong>{file.path.split("/").pop()}</strong><code>{file.path}</code><span>{file.entrypoint ? "加载入口" : "随附文件"} · {file.size > 0 ? bytes(file.size) : "大小未记录"}</span></div>
        {file.editable && canEdit ? <Button variant="secondary" onClick={() => onEdit(file)}><FileCode2 size={14} />编辑</Button> : <span className="package-file-row__state">{file.entrypoint ? "受包管理保护" : file.editable ? "无编辑权限" : "只读"}</span>}
      </div>)}</div> : <EmptyState title="没有逐文件清单" description="该组件由旧版整包或单文件流程安装，面板没有保存可枚举的文件清单。" />}
    </div>}
  </Modal>;
}

type SmartPluginChoice = { selected: boolean; packageId?: string; overrides: Record<string, string>; takeovers: string[]; preserveExisting: string[] };

function smartPluginFileState(file: SmartPluginFilePlan, choice: SmartPluginChoice) {
  const target = file.ambiguous ? choice.overrides[file.id]?.trim() || "" : file.target_path || file.suggested_path || "";
  const pathState = file.path_states?.[target];
  const existing = pathState?.existing ?? file.existing;
  const owner = pathState?.managed_package_id ?? file.managed_package_id;
  const sameOwner = Boolean(owner && choice.packageId && owner === choice.packageId);
  const otherOwner = Boolean(owner && owner !== choice.packageId);
  const takeoverRequired = existing && !owner;
  const takeoverConfirmed = choice.takeovers.includes(file.id);
  const preserveConfirmed = choice.preserveExisting.includes(file.id);
  let blocker = "";
  if (file.ambiguous && !target) blocker = "需要确认安装路径";
  else if (otherOwner) blocker = "目标路径已由另一个面板包管理";
  else if (takeoverRequired && !takeoverConfirmed && !preserveConfirmed) blocker = "目标文件已存在，请选择接管并覆盖或接管但不覆盖";
  return { target, existing, owner, sameOwner, otherOwner, takeoverRequired, takeoverConfirmed, preserveConfirmed, blocker };
}

function InstallPackageModal({ open, instance, section, metamodInstalled, smartPackages, onClose }: { open: boolean; instance: InstanceRecord; section: PackageSection; metamodInstalled?: boolean; smartPackages: PackageRecord[]; onClose: () => void }) {
  const pluginSection = section === "plugins";
  const [kind, setKind] = useState(pluginSection ? "smart" : "workshop");
  const [transport, setTransport] = useState<"default" | "qq_flash">("default");
  const [source, setSource] = useState("");
  const [upload, setUpload] = useState<{ name: string; size: number; contentBase64: string } | null>(null);
  const [pathEncoding, setPathEncoding] = useState<MapPathEncoding>("auto");
  const [risk, setRisk] = useState(false);
  const [smartPlan, setSmartPlan] = useState<SmartPluginPlan | null>(null);
  const [smartChoices, setSmartChoices] = useState<Record<string, SmartPluginChoice>>({});
  const smartQueueSubmission = useRef<{ signature: string; key: string } | undefined>(undefined);
  const toast = useToast();
  const client = useQueryClient();
  const official = kind === "sourcemod" || kind === "metamod";
  const qqFlashVPK = kind === "qq_flash";
  const qqFlashPlugin = pluginSection && transport === "qq_flash";
  const smart = kind === "smart";
  const riskRequired = ["native", "sourcemod_url", "metamod_url"].includes(kind) || (qqFlashPlugin && ["sourcemod", "metamod"].includes(kind));
  const smartNativeRisk = smartPlan?.folders.some((folder) => smartChoices[folder.id]?.selected && folder.native_risk) || false;
  const showRisk = riskRequired || smartNativeRisk;
  const needsRisk = riskRequired || smartNativeRisk;
  const sourceModBlocked = (kind === "sourcemod" || kind === "sourcemod_url") && metamodInstalled === false;
  const canUpload = !official && kind !== "workshop" && !qqFlashVPK && !qqFlashPlugin;
  const hasSource = Boolean(source.trim() || upload);
  const accept = kind === "vpk_url" ? ".vpk" : kind === "smx" ? ".smx" : smart ? ".zip,.7z,.rar" : kind === "native" ? ".dll,.so" : ".zip,.tar,.gz,.bz2,.xz";
  const clearSmartPlan = () => {
    setSmartPlan(null);
    setSmartChoices({});
    smartQueueSubmission.current = undefined;
    setRisk(false);
  };
  const chooseUpload = async (file?: File) => {
    if (!file) return;
    if (file.size === 0 || file.size > 100 * 1024 * 1024) {
      toast.push("上传包必须大于 0 且不能超过 100 MiB", "error");
      return;
    }
    try {
      const contentBase64 = bytesToBase64(new Uint8Array(await file.arrayBuffer()));
      setUpload({ name: file.name, size: file.size, contentBase64 });
      setSource("");
      clearSmartPlan();
    } catch {
      toast.push("无法读取所选文件", "error");
    }
  };
  const resetAndClose = () => {
    setSource("");
    setUpload(null);
    setTransport("default");
    setPathEncoding("auto");
    setRisk(false);
    setSmartPlan(null);
    setSmartChoices({});
    onClose();
  };
  const inspection = useMutation({
    mutationFn: () => {
      if (qqFlashPlugin) {
        if (!source.trim()) throw new ApiError("请先填写 QQ 闪传分享链接", 400, "archive_required");
        return api.inspectPluginArchive(instance.id, { source: source.trim(), source_transport: "qq_flash", path_encoding: pathEncoding });
      }
      if (!upload) throw new ApiError("请先选择 ZIP、7Z 或 RAR 压缩包", 400, "archive_required");
      return api.inspectPluginArchive(instance.id, { name: upload.name, content_base64: upload.contentBase64, path_encoding: pathEncoding });
    },
    onSuccess: (plan) => {
      const choices: typeof smartChoices = {};
      for (const folder of plan.folders) {
        const overrides: Record<string, string> = {};
        for (const file of folder.files) if (file.ambiguous && file.suggested_path) overrides[file.id] = file.suggested_path;
        const existing = smartPackages.find((item) => item.name.localeCompare(folder.name, undefined, { sensitivity: "accent" }) === 0);
        choices[folder.id] = { selected: true, packageId: existing?.id, overrides, takeovers: [], preserveExisting: [] };
      }
      setSmartPlan(plan);
      setSmartChoices(choices);
      smartQueueSubmission.current = undefined;
      setRisk(false);
      toast.push(`已识别 ${plan.folders.length} 个插件文件夹、${plan.file_count} 个文件`);
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const prepared = usePreparedPackages(instance.id, open);
  const selectedSmartFolders = smartPlan?.folders.filter((folder) => smartChoices[folder.id]?.selected) || [];
  const smartBlockers = selectedSmartFolders.flatMap((folder) => {
    const choice = smartChoices[folder.id];
    return folder.files.flatMap((file) => {
      const state = smartPluginFileState(file, choice);
      return state.blocker ? [{ folder: folder.name, path: state.target || file.source_path, reason: state.blocker }] : [];
    });
  });
  const smartReady = selectedSmartFolders.length > 0 && smartBlockers.length === 0;
  const durableSmart = Boolean(smart && smartPlan?.preview_id && smartPlan.inspection_job_id && prepared.data?.smart_plugins_supported);
  const queueSmart = (liveAcknowledged: boolean) => {
    const id = smartPlan!.inspection_job_id!;
    const options = {
      acknowledge_live_changes: liveAcknowledged,
      acknowledge_restart_required: liveAcknowledged,
      smart_packages: { [id]: selectedSmartFolders.map((folder): SmartPluginSelection => ({
        folder_id: folder.id, package_id: smartChoices[folder.id].packageId,
        overrides: smartChoices[folder.id].overrides, takeovers: smartChoices[folder.id].takeovers,
        preserve_existing: smartChoices[folder.id].preserveExisting,
      })) },
    };
    const signature = JSON.stringify([instance.id, id, risk, options]);
    if (smartQueueSubmission.current?.signature !== signature) smartQueueSubmission.current = { signature, key: newIdempotencyKey() };
    return api.queuePreparedInstalls(instance.id, [id], risk, smartQueueSubmission.current.key, options);
  };
  const install = useMutation({
    mutationFn: (liveAcknowledged: boolean) => durableSmart ? queueSmart(liveAcknowledged) : qqFlashVPK
      ? api.importQQFlashFile(instance.id, source)
      : api.instanceAction(instance.id, "install_package", {
        kind,
        source: source.trim() || undefined,
        source_transport: qqFlashPlugin ? "qq_flash" : undefined,
        name: upload?.name,
        content_base64: upload?.contentBase64,
        path_encoding: smart ? pathEncoding : undefined,
        acknowledge_native_risk: risk,
        inspection_fingerprint: smartPlan?.fingerprint,
        smart_packages: smartPlan ? selectedSmartFolders.map((folder): SmartPluginSelection => ({
          folder_id: folder.id,
          package_id: smartChoices[folder.id].packageId,
          overrides: smartChoices[folder.id].overrides,
          takeovers: smartChoices[folder.id].takeovers,
          preserve_existing: smartChoices[folder.id].preserveExisting,
        })) : undefined,
        acknowledge_live_changes: liveAcknowledged,
        acknowledge_restart_required: liveAcknowledged,
      }),
    onSuccess: () => {
      toast.push(durableSmart ? "智能插件安装队列已保存，关闭页面后仍会继续" : qqFlashVPK ? "QQ闪传 VPK 导入任务已提交" : `${pluginSection ? "插件" : "模组"}安装任务已提交`);
      void refreshQueries(client, { queryKey: ["jobs"] });
      void refreshQueries(client, { queryKey: ["instances", instance.id, "packages"] });
      void refreshQueries(client, { queryKey: ["instances", instance.id, "prepared-packages"] });
      resetAndClose();
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const submitInstall = () => {
    if (smart && instance.status !== "stopped") {
      if (!window.confirm("实例正在运行。文件可以安全写入，但当前已加载的插件或扩展不会立即替换。仍要继续吗？")) return;
      if (!window.confirm("请再次确认：安装完成后会标记“需要重启”，请稍后重启实例使全部变更生效。")) return;
      install.mutate(true);
      return;
    }
    install.mutate(false);
  };
  const smartSourceReady = qqFlashPlugin ? Boolean(source.trim()) : Boolean(upload);
  const ordinarySourceReady = official ? (!qqFlashPlugin || Boolean(source.trim())) : hasSource;
  const qqPrepare = qqFlashVPK || qqFlashPlugin;
  const singleFileQQPlugin = qqFlashPlugin && ["smx", "native"].includes(kind);
  const qqBundlePrepare = qqFlashVPK && prepared.data?.vpk_bundles_supported;
  const officialPrepare = official && !qqFlashPlugin;
  const extensionPrepare = ["sourcemod_url", "metamod_url"].includes(kind) || (qqFlashPlugin && ["sourcemod", "metamod"].includes(kind));
  const canPrepare = prepared.data?.supported && ((officialPrepare && prepared.data.official_extensions_supported) || (extensionPrepare && prepared.data.extensions_supported && (!qqPrepare || prepared.data.qq_flash_supported)) || (["vpk_url", "qq_flash", "smx", "native"].includes(kind) && (!qqPrepare || prepared.data.qq_flash_supported)) || (kind === "workshop" && prepared.data.workshop_supported));
  const prepare = useMutation({
    mutationFn: () => api.preparePackage(instance.id, officialPrepare ? { kind, official: true } : { kind: qqBundlePrepare ? "qq_vpk_bundle" : kind === "vpk_url" || qqFlashVPK ? "vpk" : extensionPrepare ? kind.replace(/_url$/, "") : kind, name: qqPrepare ? undefined : upload?.name, source: source.trim() || undefined, content_base64: qqPrepare ? undefined : upload?.contentBase64, source_transport: qqPrepare ? "qq_flash" : undefined }),
    onSuccess: () => {
      toast.push("预下载任务已提交，可继续准备其他包；完成后在预下载列表安装");
      void refreshQueries(client, { queryKey: ["instances", instance.id, "prepared-packages"] });
      void refreshQueries(client, { queryKey: ["jobs"] });
      resetAndClose();
    },
    onError: (error: Error) => toast.push(error.message, "error"),
  });
  const primaryDisabled = singleFileQQPlugin || sourceModBlocked || (smart ? !smartSourceReady || !smartPlan || !smartReady : !ordinarySourceReady) || (needsRisk && !risk);
  const defaultTransportLabel = smart ? "本机上传" : official ? "官方下载" : "HTTPS / 本机上传";
  return <Modal open={open} onClose={() => { if (!prepare.isPending) resetAndClose(); }} title={pluginSection ? "安装插件或服务端组件" : "安装模组"} description={smart ? `${qqFlashPlugin ? "通过 QQ 闪传获取" : "上传"} ZIP、7Z 或 RAR；目标 Agent 会先识别每个插件文件夹和目标路径，确认预览后再整批安装。` : (qqFlashVPK || qqFlashPlugin) ? "节点直接从 QQ 闪传暂存并检查完整分享，文件正文不经过 Control 或浏览器。" : "HTTPS 下载和本机上传都会经过大小、哈希、路径与内容格式检查。"} footer={<><Button variant="secondary" onClick={resetAndClose} disabled={prepare.isPending}>取消</Button>{canPrepare && <Button variant="secondary" onClick={() => prepare.mutate()} loading={prepare.isPending} disabled={(!officialPrepare && !hasSource) || install.isPending}>预下载，稍后安装</Button>}{smart && !smartPlan ? <Button onClick={() => inspection.mutate()} loading={inspection.isPending} disabled={!smartSourceReady}><Search size={16} />开始识别</Button> : <Button onClick={submitInstall} loading={install.isPending} disabled={primaryDisabled || prepare.isPending}>{qqFlashVPK || qqFlashPlugin ? <CloudDownload size={16} /> : <Download size={16} />}{qqFlashVPK ? "导入 VPK" : smart ? `${durableSmart ? "排队安装" : "安装"} ${selectedSmartFolders.length} 个插件` : "提交安装"}</Button>}</>}>
    <div className="form-stack">
      <Field label="类型"><select disabled={prepare.isPending} value={kind} onChange={(event) => { setKind(event.target.value); setTransport("default"); setSource(""); setUpload(null); setPathEncoding("auto"); clearSmartPlan(); }}>{pluginSection ? <><option value="smart">智能识别</option><option value="smx">单文件 SMX 插件</option><option value="native">单文件原生扩展</option><option value="sourcemod">官方 SourceMod 稳定版</option><option value="metamod">官方 Metamod:Source 稳定版</option><option value="sourcemod_url">第三方 SourceMod 整包</option><option value="metamod_url">第三方 Metamod 整包</option></> : <><option value="workshop">Workshop 项目 / 合集 ID</option><option value="vpk_url">VPK 文件</option><option value="qq_flash">QQ闪传（玩家上传 VPK）</option></>}</select></Field>
      {pluginSection && <Field label="传输方式"><select disabled={prepare.isPending} value={transport} onChange={(event) => { setTransport(event.target.value as "default" | "qq_flash"); setSource(""); setUpload(null); setPathEncoding("auto"); clearSmartPlan(); }}><option value="default">{defaultTransportLabel}</option><option value="qq_flash">QQ 闪传</option></select></Field>}
      {qqPrepare && ["qq_flash", "smx", "native"].includes(kind) && <div className="form-note"><CloudDownload size={18} /><span><strong>{qqBundlePrepare ? "多文件和压缩包可先预下载" : "单文件可先预下载"}</strong><small>{qqBundlePrepare ? "支持多个 VPK 或仅含 VPK 的 ZIP、7Z、RAR；总下载和展开分别最多 2 GiB、最多 1000 个 VPK，单文件最多 512 MiB。准备不占实例写锁，完成后可加入持久安装队列。" : prepared.data?.qq_flash_supported ? "分享须恰好包含一个与所选类型一致的文件，最大 512 MiB；预下载完成后到列表安装。压缩包和多文件 VPK 分享请使用原导入流程。" : "请更新并连接 Agent 以使用 QQ 闪传单文件预下载。"}</small></span></div>}
      {officialPrepare && prepared.data?.official_extensions_supported && <div className="form-note"><CloudDownload size={18} /><span><strong>官方稳定版可先预下载</strong><small>按节点系统解析当前官方稳定版。准备完成后保存该版本，安装时使用同一份归档，不再下载或切换版本；仍会检查依赖、验证安装并在失败时恢复。</small></span></div>}
      {extensionPrepare && prepared.data?.extensions_supported && <div className="form-note"><CloudDownload size={18} /><span><strong>组件整包可先预下载</strong><small>下载不占实例写锁。上传最多 100 MiB，HTTPS / QQ 单个 ZIP 或 tar.gz 最多 512 MiB。安装前仍需确认原生代码风险；SourceMod 须先安装并启用 Metamod。正式安装会校验归档、按需停服并验证，失败自动恢复。</small></span></div>}
      {kind === "workshop" && prepared.data?.workshop_supported && <div className="form-note"><CloudDownload size={18} /><span><strong>Workshop 可先预下载</strong><small>项目或合集下载不占实例写锁，完成后可加入持久安装队列。快照最多 8 GiB、1000 个文件，从开始准备起 1 小时内有效，Agent 重启后仍可使用。</small></span></div>}
      {sourceModBlocked && <div className="form-note"><ShieldAlert size={18} /><span><strong>请先安装 Metamod:Source</strong><small>SourceMod 依赖 Metamod；Metamod 安装并验证成功后才能提交 SourceMod 安装。</small></span></div>}
      {smart && <Field label="压缩包路径编码" hint="自动模式优先使用 UTF-8，并在 ZIP 旧式文件名无效时回退 GBK；仍不正确时可显式选择原始编码。"><select aria-label="压缩包路径编码" value={pathEncoding} onChange={(event) => { setPathEncoding(event.target.value as MapPathEncoding); clearSmartPlan(); }}><option value="auto">自动（推荐）</option><option value="utf8">UTF-8</option><option value="gbk">GBK（简体中文旧压缩包）</option><option value="big5">Big5（繁体中文旧压缩包）</option></select></Field>}
      {qqFlashVPK ? <>
        <Field label="固定目标目录"><input value="left4dead2/addons" readOnly /></Field>
        <Field label="QQ闪传分享链接" hint="可直接粘贴 QQ 分享文本；支持直接 VPK，以及最多嵌套 3 层的 ZIP、7Z、RAR 压缩包。"><textarea disabled={prepare.isPending} rows={4} value={source} onChange={(event) => setSource(event.target.value)} placeholder="链接：https://qfile.qq.com/q/Example123" spellCheck={false} /></Field>
        <div className="form-note"><CloudDownload size={18} /><span><strong>仅接收经过校验的 VPK</strong><small>节点会先完整暂存分享，限制下载与解压总量，拒绝加密包、危险路径和任何非 VPK 文件。成功后每个 VPK 会单独出现在本页，可启停和删除。</small></span></div>
      </> : <>
        {qqFlashPlugin ? <>
          <Field label="QQ 闪传分享链接" hint="分享中必须只有一个实际包文件；智能识别支持 ZIP、7Z、RAR，组件整包会继续按所选类型校验。"><textarea disabled={prepare.isPending} rows={4} value={source} onChange={(event) => { setSource(event.target.value); clearSmartPlan(); }} placeholder="链接：https://qfile.qq.com/q/Example123" spellCheck={false} /></Field>
          <div className="form-note"><CloudDownload size={18} /><span><strong>节点直连下载</strong><small>最多下载 2 GiB，并保留至少 1 GiB 可用空间；下载后的包仍经过现有路径、归档和安装校验。</small></span></div>
        </> : official ? <div className="form-note"><ShieldCheck size={18} /><span><strong>自动解析官方稳定版</strong><small>Agent 只接受 AlliedModders 官方白名单地址，并按节点系统选择压缩包。</small></span></div> : !smart && <Field label={kind === "workshop" ? "项目或合集 ID" : "HTTPS 来源"} hint={canUpload ? "填写 URL，或在下方从本机选择一个文件" : undefined}><input disabled={prepare.isPending} value={source} onChange={(event) => { setSource(event.target.value); setUpload(null); clearSmartPlan(); }} placeholder={kind === "workshop" ? "例如：123456789" : "https://example.org/plugin.zip"} /></Field>}
        {canUpload && <Field label={smart ? "插件压缩包" : "从本机上传"} hint={smart ? "支持 ZIP、7Z、RAR；单个文件上限 100 MiB。可包含多个顶层插件文件夹" : "单个文件上限 100 MiB；载荷会加密保存，并只发送给目标节点"}><FileUploadInput key={kind} label={smart ? "插件压缩包" : "从本机上传"} accept={accept} minBytes={1} maxBytes={100 * 1024 * 1024} fileName={upload?.name || ""} disabled={install.isPending || inspection.isPending || prepare.isPending} onFile={chooseUpload} /></Field>}
        {upload && <div className="binary-file-note"><Upload size={20} /><div><strong>{upload.name}</strong><span>{bytes(upload.size)} · 已准备安全上传</span></div><Button variant="secondary" disabled={prepare.isPending} onClick={() => { setUpload(null); clearSmartPlan(); }}>移除</Button></div>}
        {durableSmart && <p>确认后加入安装队列，实例忙时自动等待；关闭页面后仍会继续。预检开始后一小时内有效。</p>}
        {smartPlan && <div className="smart-plugin-plan">
          <div className="smart-plugin-plan__summary"><strong>识别完成</strong><span>{smartPlan.format.toUpperCase()} · 路径编码 {smartPlan.path_encoding.toUpperCase()} · {smartPlan.folders.length} 个插件文件夹 · {smartPlan.file_count} 个文件 · 解压后 {bytes(smartPlan.expanded_bytes)}</span></div>
          {smartPlan.folders.map((folder) => {
            const choice = smartChoices[folder.id];
            if (!choice) return null;
            return <details className="smart-plugin-folder" open={folder.ambiguous_files > 0 || folder.existing_files > 0} key={folder.id}>
              <summary><label onClick={(event) => event.stopPropagation()}><input type="checkbox" checked={choice.selected} onChange={(event) => setSmartChoices((current) => ({ ...current, [folder.id]: { ...current[folder.id], selected: event.target.checked } }))} /><strong>{folder.name}</strong></label><span>{choice.packageId ? "更新已有插件 · " : ""}{folder.files.length} 个文件{folder.native_risk ? " · 含原生扩展" : ""}{folder.ambiguous_files ? ` · ${folder.ambiguous_files} 项路径需核对` : ""}</span></summary>
              <div className="smart-plugin-files">
                {folder.files.map((file) => {
                  const state = smartPluginFileState(file, choice);
                  return <div className={`smart-plugin-file ${file.ambiguous || state.existing ? "smart-plugin-file--attention" : ""}`} key={file.id}>
                    <div><code>{file.source_path}</code><span>{file.reason}{file.entrypoint ? " · 加载入口" : ""}</span></div>
                    {file.ambiguous ? <Field label="安装路径"><input list={`smart-paths-${file.id}`} value={state.target} disabled={!choice.selected} onChange={(event) => setSmartChoices((current) => ({ ...current, [folder.id]: { ...current[folder.id], overrides: { ...current[folder.id].overrides, [file.id]: event.target.value }, takeovers: current[folder.id].takeovers.filter((id) => id !== file.id), preserveExisting: current[folder.id].preserveExisting.filter((id) => id !== file.id) } }))} /><datalist id={`smart-paths-${file.id}`}>{file.choices?.map((path) => <option value={path} key={path} />)}</datalist></Field> : <code className="smart-plugin-target">→ {state.target}</code>}
                    {state.otherOwner ? <div className="smart-plugin-managed-conflict">该路径已由另一个面板包管理，不能覆盖。请取消选择此插件或调整歧义路径。</div> : state.sameOwner ? <div className="smart-plugin-update-path">已登记为此插件的文件，将作为更新替换。</div> : state.takeoverRequired && <><label className="smart-plugin-takeover"><input type="checkbox" disabled={!choice.selected} checked={state.takeoverConfirmed} onChange={(event) => setSmartChoices((current) => ({ ...current, [folder.id]: { ...current[folder.id], preserveExisting: current[folder.id].preserveExisting.filter((id) => id !== file.id), takeovers: event.target.checked ? [...current[folder.id].takeovers, file.id] : current[folder.id].takeovers.filter((id) => id !== file.id) } }))} />彻底接管并覆盖实例中的已有文件（删除插件时会删除该文件，不恢复原文件）</label><label className="smart-plugin-takeover"><input type="checkbox" disabled={!choice.selected} checked={state.preserveConfirmed} onChange={(event) => setSmartChoices((current) => ({ ...current, [folder.id]: { ...current[folder.id], takeovers: current[folder.id].takeovers.filter((id) => id !== file.id), preserveExisting: event.target.checked ? [...current[folder.id].preserveExisting, file.id] : current[folder.id].preserveExisting.filter((id) => id !== file.id) } }))} />接管但不覆盖（保留现有内容并纳入插件管理，卸载时按插件删除规则处理）</label></>}
                  </div>;
                })}
              </div>
            </details>;
          })}
        </div>}
        {smartPlan && smartBlockers.length > 0 && <div className="smart-plugin-blockers" role="alert"><ShieldAlert size={18} /><div><strong>还有 {smartBlockers.length} 项需要处理，暂时无法安装</strong><ul>{smartBlockers.slice(0, 8).map((blocker, index) => <li key={`${blocker.folder}-${blocker.path}-${index}`}><span>{blocker.folder}</span><code>{blocker.path}</code><em>{blocker.reason}</em></li>)}</ul>{smartBlockers.length > 8 && <small>另外还有 {smartBlockers.length - 8} 项，请检查上方带提示的插件文件夹。</small>}</div></div>}
        {showRisk && <label className="risk-check"><input type="checkbox" checked={risk} onChange={(event) => setRisk(event.target.checked)} /><ShieldAlert size={19} /><span><strong>我了解第三方原生代码风险</strong><small>{needsRisk ? "此包可能包含或覆盖节点执行的二进制扩展，必须确认后安装。" : "普通 SMX 不要求确认；DLL/SO 原生扩展必须确认。"}</small></span></label>}
        <div className="form-note"><strong>{smart ? "整批原子安装" : "自动回退"}</strong><span>{smart ? "任一插件文件写入失败会回滚整批；停用只移动 SMX、DLL、SO、VDF、AUTOLOAD 等加载入口，配置和翻译等支持文件保留。" : "组件升级前会创建快照；重启后的版本检测失败时，Agent 恢复上一版本。"}</span></div>
      </>}
    </div>
  </Modal>;
}
