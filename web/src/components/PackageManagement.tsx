import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../lib/api";
import type { InstanceRecord, PackageRecord } from "../lib/types";
import { Button, Field, Modal } from "./ui";
import { useToast } from "../context/ToastContext";

export const packageToggleable = (item: PackageRecord) => item.kind === "smart" || /\.(vpk|smx)$/i.test(item.path || "");
export const packageFileRenameable = (item: PackageRecord) => ["vpk", "workshop", "plugin", "smx"].includes(item.kind) && /\.(vpk|smx)$/i.test(item.path || "");

export function packageBatchBlocker(item: PackageRecord, action: "enable" | "disable" | "delete", instance: InstanceRecord) {
  if (action !== "delete") {
    if (!packageToggleable(item)) return "整套组件不支持开关";
    if (item.enabled === (action === "enable")) return action === "enable" ? "已启用" : "已停用";
    if (action === "disable" && instance.status !== "stopped") return "请先停止实例";
    return "";
  }
  const component = item.kind === "sourcemod" || item.kind === "metamod";
  if (!component && !packageToggleable(item)) return "不支持删除此类型";
  if (!item.sha256 || (component && !item.file_count)) return "缺少安全卸载清单或校验值，请先重新安装";
  if ((component || item.kind === "smart") && instance.status !== "stopped") return "请先停止实例";
  return "";
}

async function waitPackageOperation(instanceID: string, jobID: string, canceled: () => boolean) {
  const deadline = Date.now() + 10 * 60_000;
  while (!canceled()) {
    const job = await api.packageOperationStatus(instanceID, jobID);
    if (!job.busy && ["succeeded", "failed", "canceled"].includes(job.status)) return job.status;
    if (Date.now() >= deadline) throw new Error(`任务 ${jobID} 仍未完成，请在任务中心查看后再继续`);
    await new Promise((resolve) => setTimeout(resolve, 750));
  }
  throw new Error("已停止后续操作；已提交的任务会继续执行");
}

type BatchResult = { id: string; name: string; message: string; success: boolean };

export function PackageBatchControls({ instance, items, selected, setSelected, canToggle, canDelete, busy, setBusy }: {
  instance: InstanceRecord; items: PackageRecord[]; selected: string[]; setSelected: (ids: string[]) => void;
  canToggle: boolean; canDelete: boolean; busy: boolean; setBusy: (busy: boolean) => void;
}) {
  const client = useQueryClient();
  const stopped = useRef(false);
  const all = useRef<HTMLInputElement>(null);
  const [results, setResults] = useState<BatchResult[]>([]);
  const [progress, setProgress] = useState("");
  const chosen = items.filter((item) => selected.includes(item.id));
  useEffect(() => { if (all.current) all.current.indeterminate = chosen.length > 0 && chosen.length < items.length; }, [chosen.length, items.length]);
  useEffect(() => { stopped.current = false; return () => { stopped.current = true; }; }, []);
  const run = async (action: "enable" | "disable" | "delete") => {
    if (busy || !chosen.length) return;
    const eligible = chosen.filter((item) => !packageBatchBlocker(item, action, instance));
    if (!eligible.length) return;
    const verb = action === "delete" ? "永久删除" : action === "enable" ? "启用" : "停用";
    if (!window.confirm(`确认${verb} ${eligible.length} 项？${chosen.length - eligible.length ? `另有 ${chosen.length - eligible.length} 项不满足条件，将跳过。` : ""}${action === "delete" ? "文件及版本历史会一并移除；SourceMod 的管理员配置保留。" : ""}`)) return;
    const live = action === "enable" && instance.status !== "stopped" && eligible.some((item) => item.kind === "smart");
    if (live && (!window.confirm("实例正在运行。启用只会移动插件入口文件，已加载模块不会立刻变化。仍要继续吗？") || !window.confirm("请再次确认：本次变更需要重启实例才能完整生效。"))) return;
    stopped.current = false;
    setBusy(true);
    const report: BatchResult[] = chosen.filter((item) => packageBatchBlocker(item, action, instance)).map((item) => ({ id: item.id, name: item.name, message: `跳过：${packageBatchBlocker(item, action, instance)}`, success: false }));
    setResults([...report]);
    // Remove dependent plugins before SourceMod, then Metamod.
    const rank = (item: PackageRecord) => item.kind === "metamod" ? 2 : item.kind === "sourcemod" ? 1 : 0;
    const queue = action === "delete" ? [...eligible].sort((a, b) => rank(a) - rank(b)) : eligible;
    try {
      for (const [index, item] of queue.entries()) {
        if (stopped.current) break;
        setProgress(`${verb} ${index + 1} / ${queue.length}：${item.name}`);
        let jobID = "";
        try {
          const response = action === "delete" ? await api.deletePackage(instance.id, item.id) : await api.instanceAction(instance.id, "toggle_package", { package_id: item.id, enabled: action === "enable", acknowledge_live_changes: live, acknowledge_restart_required: live });
          jobID = response.job_id;
          const status = await waitPackageOperation(instance.id, jobID, () => stopped.current);
          report.push({ id: item.id, name: item.name, message: status === "succeeded" ? `${verb}成功` : `任务${status === "failed" ? "失败" : "已取消"}，请在任务中心查看 ${jobID}`, success: status === "succeeded" });
        } catch (error) {
          report.push({ id: item.id, name: item.name, message: error instanceof Error ? error.message : "操作失败", success: false });
          // An uncertain submission or polling failure must not send more writes.
          if (jobID || !(error instanceof ApiError) || error.status >= 500) stopped.current = true;
        }
        setResults([...report]);
        await client.invalidateQueries({ queryKey: ["instances", instance.id, "packages"] });
      }
    } finally {
      setSelected(selected.filter((id) => !report.some((item) => item.id === id && item.success)));
      setProgress(stopped.current ? "已停止后续操作；已提交的任务请到任务中心查看" : `批量操作结束：成功 ${report.filter((item) => item.success).length} 项，失败或跳过 ${report.filter((item) => !item.success).length} 项`);
      setBusy(false);
      void client.invalidateQueries({ queryKey: ["jobs"] });
    }
  };
  if (!canToggle && !canDelete) return null;
  return <div className="package-batch">
    <div className="package-batch__actions">
      <label><input ref={all} type="checkbox" checked={items.length > 0 && chosen.length === items.length} disabled={busy || !items.length} onChange={(event) => setSelected(event.target.checked ? items.map((item) => item.id) : [])} />全选当前结果</label>
      <span>已选 {chosen.length} 项</span>
      {canToggle && <>{(["enable", "disable"] as const).map((action) => <Button key={action} variant="secondary" disabled={busy || !chosen.some((item) => !packageBatchBlocker(item, action, instance))} onClick={() => void run(action)}>{action === "enable" ? "批量启用" : "批量停用"}</Button>)}</>}
      {canDelete && <Button variant="danger" disabled={busy || !chosen.some((item) => !packageBatchBlocker(item, "delete", instance))} onClick={() => void run("delete")}>批量删除</Button>}
      {busy ? <Button variant="secondary" onClick={() => { stopped.current = true; }}>停止后续操作</Button> : chosen.length > 0 && <Button variant="ghost" onClick={() => setSelected([])}>取消选择</Button>}
    </div>
    {progress && <p role="status">{progress}</p>}
    {busy && <p>逐项等待任务完成；离开此页会停止提交后续操作。</p>}
    {results.length > 0 && <details open><summary>操作结果</summary><ul>{results.map((item) => <li key={item.id}>{item.name}：{item.message}</li>)}</ul></details>}
  </div>;
}

export function RenamePackageModal({ instance, item, onClose }: { instance: InstanceRecord; item: PackageRecord; onClose: () => void }) {
  const client = useQueryClient();
  const toast = useToast();
  const [name, setName] = useState(item.name);
  const [filename, setFilename] = useState(item.path?.split("/").pop() || "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const stopped = useRef(false);
  useEffect(() => { stopped.current = false; return () => { stopped.current = true; }; }, []);
  const fileRenameable = packageFileRenameable(item);
  return <Modal open title={`重命名 ${item.name}`} onClose={() => { if (!busy) onClose(); }}>
    <form className="form-stack" onSubmit={async (event) => {
      event.preventDefault(); setBusy(true); setError("");
      try {
        const result = await api.renamePackage(instance.id, item.id, { name: name.trim(), ...(fileRenameable && instance.status === "stopped" ? { filename } : {}) });
        const status = await waitPackageOperation(instance.id, result.job_id, () => stopped.current);
        if (status !== "succeeded") throw new Error(`重命名${status === "failed" ? "失败" : "已取消"}，请在任务中心查看 ${result.job_id}`);
        await client.invalidateQueries({ queryKey: ["instances", instance.id, "packages"] });
        toast.push("重命名成功"); onClose();
      } catch (error) { setError(error instanceof Error ? error.message : "重命名失败"); }
      finally { setBusy(false); }
    }}>
      <Field label="显示名称"><input value={name} maxLength={240} required disabled={busy} onChange={(event) => setName(event.target.value)} /></Field>
      {fileRenameable ? <Field label="文件名" hint={instance.status !== "stopped" ? "修改实际文件名需要先停止实例；当前仍可修改显示名称。" : "保留 .vpk / .smx 扩展名，不包含目录；版本历史会同步迁移。"}><input aria-label="文件名" value={filename} maxLength={240} required disabled={busy || instance.status !== "stopped"} onChange={(event) => setFilename(event.target.value)} /></Field> : <p>修改面板显示名称，插件内部文件及加载路径保持原样。</p>}
      {error && <p role="alert" className="form-error">{error}</p>}
      <div className="form-actions"><Button type="submit" loading={busy} disabled={!name.trim()}>保存名称</Button><Button variant="secondary" disabled={busy} onClick={onClose}>取消</Button></div>
    </form>
  </Modal>;
}
