import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Archive, Download, History, PackageOpen, RotateCcw, ShieldAlert, Trash2, Upload } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router";
import { FileUploadInput } from "../components/FileUploadInput";
import { Button, Card, EmptyState, Field, LoadingBlock, Modal, ProgressBar } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { hasInstancePermission } from "../lib/account-permissions";
import { api, ApiError } from "../lib/api";
import { maxBufferedBackupDownload, receiveBackupDownload, type BackupDownloadWriter, } from "../lib/backup-download";
import { backupUploadChunkSize, bytesToBase64 as encodeBackupChunk, hashBlobSHA256, readBlobSlice, sha256Hex, validateBackupUploadFile } from "../lib/backup-upload";
import { clearBackupUploadSession, loadBackupUploadSession, newBackupUploadKey, sameBackupUploadFile, saveBackupUploadSession, type PersistedBackupPlatform, type PersistedBackupUploadSession, } from "../lib/backup-upload-session";
import { bytes, dateTime } from "../lib/format";
import { canAccessInstance } from "../lib/instance-lifecycle";
import { liveRefreshInterval, refreshQueries } from "../lib/query-refresh";
import type { BackupRecord, BackupUploadRecord, InstanceRecord } from "../lib/types";
import { OptionalUnavailable, safeBrowserFilename, saveBrowserDownload, wait } from './InstanceDetailShared';

async function waitForBackupUploadOffset(instanceID: string, uploadID: string, jobID: string, expectedOffset: number) {
  for (let attempt = 0; attempt < 120; attempt += 1) {
    const upload = await api.backupUpload(instanceID, uploadID);
    if (upload.received_size >= expectedOffset || upload.status === "ready") return upload;
    if (["failed", "expired"].includes(upload.status)) throw new ApiError("节点未能暂存备份分片", 409, "backup_upload_failed");
    const chunk = await api.backupUploadChunkStatus(instanceID, uploadID, jobID);
    if (chunk.status === "failed" || chunk.status === "canceled") {
      throw new ApiError("节点未能暂存备份分片，可以重试当前分片", 409, "backup_upload_chunk_failed");
    }
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

export function BackupsTab({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const realtime = useRealtime();
  const canRead = canAccessInstance(user, instance, "backups.read");
  const canDownload = canAccessInstance(user, instance, "backups.download");
  const backups = useQuery({
    queryKey: ["instances", instance.id, "backups"],
    queryFn: () => api.backups(instance.id),
    enabled: canRead,
    refetchInterval: realtime.state === "live" ? liveRefreshInterval : 10_000,
  });
  const toast = useToast();
  const client = useQueryClient();
  const [uploadOpen, setUploadOpen] = useState(false);
  const [pluginBackupOpen, setPluginBackupOpen] = useState(false);
  const canCreate = canAccessInstance(user, instance, "backups.create");
  const canRestorePermission = canAccessInstance(user, instance, "backups.restore");
  const canDelete = canAccessInstance(user, instance, "backups.delete");
  const restoreReady = instance.status === "stopped" || instance.status === "degraded";
  const canRestore = canRestorePermission && restoreReady;
  const action = useMutation({ mutationFn: ({ type, backup }: { type: string; backup?: BackupRecord }) => api.instanceAction(instance.id, type, backup ? { backup_id: backup.id } : undefined), onSuccess: (_data, values) => { toast.push(values.type === "backup" ? "备份任务已提交" : "恢复任务已提交"); void refreshQueries(client, { queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const remove = useMutation({
    mutationFn: (backup: BackupRecord) => api.deleteBackup(instance.id, backup.id, crypto.randomUUID()),
    onSuccess: () => {
      toast.push("备份删除任务已提交");
      void refreshQueries(client, { queryKey: ["jobs"] });
      void refreshQueries(client, { queryKey: ["instances", instance.id, "backups"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const restoreReason = !canRestorePermission ? "当前账号没有备份恢复权限" : "请先停止实例再恢复";
  const backupButton = <Button onClick={() => action.mutate({ type: "backup" })} loading={action.isPending} disabled={!canCreate} title={!canCreate ? "当前账号没有备份创建权限" : undefined}><Archive size={16} />立即备份</Button>;
  const alternateBackupButton = (label: string) => <Button onClick={() => action.mutate({ type: "backup" })} loading={action.isPending} disabled={!canCreate}><Archive size={16} />{label}</Button>;
  const regularBackups = backups.data?.items.filter((backup) => !backup.kind?.startsWith("plugin_")) || [];
  const pluginBackups = backups.data?.items.filter((backup) => backup.kind?.startsWith("plugin_")) || [];
  const rows = (items: BackupRecord[], restorable: boolean) => <div className="backup-list">{items.map((backup) => <div className="backup-row" key={backup.id}>
    <div className="backup-icon">{restorable ? <Archive size={19} /> : <PackageOpen size={19} />}</div>
    <div><strong>{backup.name}</strong><span>{dateTime(backup.created_at)} · {restorable ? (backup.platform || "可移植数据") : (backup.kind === "plugin_preserve" ? "保留完整游戏路径" : "精简插件路径")}</span></div>
    <span>{bytes(backup.size)}</span>
    <code>{backup.checksum ? `${backup.checksum.slice(0, 12)}…` : "校验待完成"}</code>
    <div className="backup-row__actions">
      {restorable && canRestorePermission && <Button variant="secondary" disabled={!canRestore} title={canRestore ? "恢复此备份" : restoreReason} onClick={() => action.mutate({ type: "restore", backup })}><RotateCcw size={15} />恢复</Button>}
      {canDownload && <BackupDownloadButton instance={instance} backup={backup} />}
      {canDelete && <Button variant="danger" loading={remove.isPending} onClick={() => { if (window.confirm(`确认永久删除备份“${backup.name}”？节点上的归档文件会被删除且无法恢复。`)) remove.mutate(backup); }}><Trash2 size={15} />删除</Button>}
    </div>
  </div>)}</div>;
  const backupList = (items: BackupRecord[], restorable: boolean, empty: ReactNode) => backups.isPending
    ? <LoadingBlock />
    : backups.error
      ? <OptionalUnavailable error={backups.error} title="备份清单尚不可用" description="当前控制端未开放节点本地备份列表；仍可提交新的备份任务。" />
      : items.length ? rows(items, restorable) : empty;
  return <>
    <Card>
      <div className="card-heading"><div><span className="eyebrow">LOCAL SNAPSHOTS</span><h2>实例备份</h2></div><div className="backup-heading-actions"><Button variant="secondary" onClick={() => setUploadOpen(true)} disabled={!canRestore} title={canRestore ? "上传其他节点下载的 ZIP 备份并恢复" : restoreReason}><Upload size={16} />上传并恢复</Button>{backupButton}</div></div>
      {canRestorePermission && !restoreReady && <div className="form-warning backup-restore-warning"><ShieldAlert size={17} />恢复会替换实例数据，必须先停止服务器。</div>}
      {!canRead ? <EmptyState title="备份清单已隐藏" description="当前账号没有查看备份清单的权限，但仍可执行上方单独授予的创建或上传恢复操作。" /> : backupList(regularBackups, true, <EmptyState title="还没有备份" description="常规备份仅包含配置，以及通过面板上传或安装的模组和插件；不包含地图、游戏本体或节点上手动放置的文件。" action={canCreate ? alternateBackupButton("创建首个备份") : undefined} />)}
      <div className="retention-note"><History size={19} /><span><strong>备份范围与保留</strong> 常规备份只含配置和面板安装包；每日 04:00，保留 7 个日备份与 4 个周备份。破坏性操作前的完整检查点最多保留 3 个。</span></div>
    </Card>
    <Card>
      <div className="card-heading"><div><span className="eyebrow">PLUGIN ARCHIVES</span><h2>插件备份</h2></div><Button onClick={() => setPluginBackupOpen(true)} disabled={!canCreate} title={!canCreate ? "需要备份创建权限" : undefined}><PackageOpen size={16} />备份插件</Button></div>
      {!canRead ? <EmptyState title="插件备份清单已隐藏" description="当前账号没有备份查看权限；如已单独授予创建权限，仍可主动创建插件备份。" /> : backupList(pluginBackups, false, <EmptyState title="还没有插件备份" description="主动创建一个 7z 归档；每个已记录插件拥有独立文件夹，可选择保留完整游戏路径或精简为插件目录。" action={canCreate ? <Button onClick={() => setPluginBackupOpen(true)}><PackageOpen size={16} />创建首个插件备份</Button> : undefined} />)}
      <div className="retention-note plugin-backup-note"><PackageOpen size={19} /><span><strong>主动备份与保留</strong> 插件备份不会按计划自动创建或轮换；归档会一直保留到你主动删除。它仅用于下载和迁移，不会出现在实例恢复选项中。</span></div>
    </Card>
    {canRestorePermission && <BackupUploadRestoreModal open={uploadOpen} instance={instance} onClose={() => setUploadOpen(false)} />}
    <PluginBackupModal open={canCreate && pluginBackupOpen} instance={instance} onClose={() => setPluginBackupOpen(false)} />
  </>;
}

function PluginBackupModal({ open, instance, onClose }: { open: boolean; instance: InstanceRecord; onClose: () => void }) {
  const [layout, setLayout] = useState<"preserve" | "compact">("preserve");
  const toast = useToast();
  const client = useQueryClient();
  const create = useMutation({
    mutationFn: () => api.createPluginBackup(instance.id, layout),
    onSuccess: () => {
      toast.push("插件备份任务已提交");
      void refreshQueries(client, { queryKey: ["jobs"] });
      onClose();
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  return <Modal open={open} onClose={onClose} title="创建插件备份" description="把面板中已记录的插件及其关联文件打包为 7z；每个插件始终放在独立的顶层文件夹中。" footer={<><Button variant="secondary" onClick={onClose} disabled={create.isPending}>取消</Button><Button onClick={() => create.mutate()} loading={create.isPending}><Archive size={16} />开始备份</Button></>}>
    <div className="plugin-backup-layouts" role="radiogroup" aria-label="插件备份路径布局">
      <label className={layout === "preserve" ? "active" : ""}><input type="radio" name="plugin-backup-layout" value="preserve" checked={layout === "preserve"} onChange={() => setLayout("preserve")} /><span><strong>保留原路径</strong><small>插件文件夹内从 <code>left4dead2/…</code> 开始，符合智能识别上传的标准结构，可直接看出实际游戏位置。</small><code>插件名/left4dead2/addons/sourcemod/plugins/example.smx</code></span></label>
      <label className={layout === "compact" ? "active" : ""}><input type="radio" name="plugin-backup-layout" value="compact" checked={layout === "compact"} onChange={() => setLayout("compact")} /><span><strong>不保留原路径</strong><small>去掉游戏和 SourceMod 根目录，但保留 plugins、gamedata、configs、cfg 等一一对应的目录。</small><code>插件名/plugins/example.smx</code></span></label>
    </div>
  </Modal>;
}

type BackupPlatform = PersistedBackupPlatform;

function BackupUploadRestoreModal({ open, instance, onClose }: { open: boolean; instance: InstanceRecord; onClose: () => void }) {
  const { user } = useAuth();
  const canReadJobs = hasInstancePermission(user, instance, "jobs.read");
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

      const reconcileRestore = async (jobID: string) => {
        session = applyRecord(await api.backupUpload(instance.id, session.id));
        if (session.status === "restoring") {
          setProgress({ value: 100, label: "恢复任务仍在执行" });
          return { done: true, result: { job_id: jobID } };
        }
        if (session.status === "succeeded") {
          forget();
          setProgress({ value: 100, label: "恢复已完成" });
          return { done: true, result: { job_id: jobID } };
        }
        if (session.status === "failed" || session.status === "ready") {
          local = persist({ ...local!, pendingComplete: undefined, restoreJobId: jobID });
          return { done: false as const };
        }
        throw new ApiError("恢复状态与上传会话不一致", 409, "backup_restore_state_mismatch");
      };

      if (session.status === "succeeded") {
        const jobID = session.restore_job_id || local.restoreJobId || local.pendingComplete?.jobId || "";
        forget();
        setProgress({ value: 100, label: "恢复已完成" });
        return { job_id: jobID };
      }
      if (session.status === "restoring") {
        if (!session.restore_job_id) throw new ApiError("恢复中的会话缺少任务标识", 409, "backup_restore_job_missing");
        const reconciled = await reconcileRestore(session.restore_job_id);
        if (reconciled.done) return reconciled.result;
      } else if (session.status === "failed") {
        if (local.pendingComplete && !local.pendingComplete.jobId) {
          // The previous complete request may have reached the server even when its response was lost.
          return enqueueRestore();
        }
        if (!session.restore_job_id) throw new ApiError("上传校验失败，请重新创建上传会话", 409, "backup_upload_failed");
        const reconciled = await reconcileRestore(session.restore_job_id);
        if (reconciled.done) return reconciled.result;
      } else if (session.status === "ready" && local.pendingComplete?.jobId) {
        const reconciled = await reconcileRestore(local.pendingComplete.jobId);
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
        try {
          session = await waitForBackupUploadOffset(instance.id, session.id, jobID, expectedOffset);
        } catch (error) {
          if (error instanceof ApiError && error.code === "backup_upload_chunk_failed") {
            local = persist({ ...local!, pendingChunk: undefined });
          }
          throw error;
        }
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
      void refreshQueries(client, { queryKey: ["jobs"] });
      void refreshQueries(client, { queryKey: ["instances", instance.id, "backups"] });
      file.current = null;
      onClose();
      if (canReadJobs) navigate("/jobs");
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
        void refreshQueries(client, { queryKey: ["jobs"] });
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
    <div className="form-stack"><div className="form-warning"><ShieldAlert size={17} />恢复会替换实例数据，实例必须已停止。降级状态下 Agent 仍会确认无活动进程；恢复前会自动创建检查点。</div>{resumeSession && <div className="form-note backup-upload-resume"><History size={18} /><span><strong>有可恢复的上传会话</strong><small>{resumeSession.name} · {bytes(resumeSession.size)}；请重新选择同一文件，校验 SHA-256 后将从节点已确认的偏移量继续。关闭窗口或刷新页面不会丢失此会话。</small></span><Button variant="danger" onClick={abandonSession} loading={abandon.isPending} disabled={upload.isPending}>放弃会话</Button></div>}<Field label="ZIP 备份文件" hint="1 字节至 20 GiB；浏览器按切片校验和上传，不会把整个文件放入页面内存。"><FileUploadInput inputRef={fileInput} label="ZIP 备份文件" accept=".zip,application/zip" minBytes={1} maxBytes={20 * 1024 ** 3} fileName={fileMeta?.name || ""} disabled={upload.isPending || abandon.isPending} onFile={chooseFile} /></Field>{fileMeta && <div className="backup-upload-file"><Archive size={20} /><div><strong>{fileMeta.name}</strong><span>{bytes(fileMeta.size)}</span></div></div>}<Field label="备份来源平台" hint="跨系统恢复只恢复可移植数据，并重装目标平台对应的 Metamod/SourceMod 二进制。"><select value={platform} disabled={upload.isPending || abandon.isPending} onChange={(event) => setPlatform(event.target.value as BackupPlatform | "")}><option value="">请选择来源平台</option><option value="linux/amd64">Linux amd64</option><option value="windows/amd64">Windows amd64</option></select></Field>{(upload.isPending || progress.value > 0) && <div className="backup-upload-progress"><div><strong>{progress.label}</strong><span>{progress.value}%</span></div><ProgressBar value={progress.value} /></div>}{(upload.error || abandon.error) && <div className="form-error" role="alert">{(upload.error || abandon.error)?.message}</div>}</div>
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
    const pluginArchive = backup.name.toLowerCase().endsWith(".7z");
    if (picker) {
      try {
        const handle = await picker.call(window, {
          suggestedName: safeBrowserFilename(backup.name),
          types: pluginArchive
            ? [{ description: "7Z 插件备份", accept: { "application/x-7z-compressed": [".7z"] } }]
            : [{ description: "ZIP 备份", accept: { "application/zip": [".zip"] } }],
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
