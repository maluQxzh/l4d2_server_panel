import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, newIdempotencyKey, type PreparedPackage } from "../lib/api";
import { Button } from "./ui";
import { useToast } from "../context/ToastContext";

export function usePreparedPackages(instanceId: string, enabled = true) {
  return useQuery({ queryKey: ["instances", instanceId, "prepared-packages"], queryFn: () => api.preparedPackages(instanceId), enabled, retry: false, refetchInterval: 3000 });
}

export function PreparedPackages({ instanceId, plugins }: { instanceId: string; plugins: boolean }) {
  const query = usePreparedPackages(instanceId);
  const client = useQueryClient();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const generation = useRef(0);
  const lastSubmission = useRef<{ signature: string; key: string } | undefined>(undefined);
  useEffect(() => { generation.current++; setBusy(false); setMessage(""); lastSubmission.current = undefined; return () => { generation.current++; }; }, [instanceId, plugins]);
  const items = (query.data?.items || []).filter((item) => plugins ? !["vpk", "qq_vpk_bundle", "workshop"].includes(item.kind) : ["vpk", "qq_vpk_bundle", "workshop"].includes(item.kind));
  const queues = query.data?.queues || [];
  const active = queues.find((queue) => ["queued", "running"].includes(queue.status));
  if (!query.data?.supported && !items.length && !queues.length) return null;
  const refresh = async () => { await client.invalidateQueries({ queryKey: ["instances", instanceId] }); void client.invalidateQueries({ queryKey: ["jobs"] }); };
  const install = async (queue: PreparedPackage[]) => {
    if (busy || active || !queue.length) return;
    if (!window.confirm(`确认依次安装 ${queue.length} 个预下载包？关闭页面后仍会继续；安装期间可能重启实例。`)) return;
    const native = queue.some((item) => item.kind === "native");
    if (native && !window.confirm("其中含第三方原生扩展，会在节点执行原生代码。确认了解并接受该风险？")) return;
    const epoch = generation.current;
    const ids = queue.map((item) => item.id);
    const signature = JSON.stringify([instanceId, ids, native]);
    if (lastSubmission.current?.signature !== signature) lastSubmission.current = { signature, key: newIdempotencyKey() };
    setBusy(true);
    try {
      await api.queuePreparedInstalls(instanceId, ids, native, lastSubmission.current.key);
      if (epoch === generation.current) {
        lastSubmission.current = undefined;
        setMessage("安装队列已保存，关闭页面后仍会继续");
      }
    } catch (error) {
      if (epoch === generation.current) setMessage(error instanceof Error ? error.message : "提交失败，请刷新状态后重试");
    } finally {
      if (epoch === generation.current) { setBusy(false); void refresh(); }
    }
  };
  const ready = items.filter((item) => item.ready);
  return <section className="package-batch" aria-label="预下载包">
    <strong>预下载包</strong><p>下载不占实例写锁，从开始准备起 1 小时内有效；支持刷新后继续。安装队列保存在服务端，关闭页面后仍会继续；任一安装失败会停止后续安装。</p>
    {query.error && <p role="alert">无法刷新预下载状态，请稍后重试</p>}
    <div className="package-batch__actions"><Button variant="secondary" disabled={busy || !!active || !ready.length} onClick={() => void install(ready)}>依次安装就绪包（{ready.length}）</Button>{active && <Button variant="secondary" disabled={busy} onClick={() => { void api.stopPreparedInstallQueue(instanceId, active.id).then(refresh).catch((error: Error) => toast.push(error.message, "error")); }}>停止后续安装</Button>}</div>
    {message && <p role="status">{message}</p>}
    {queues.slice(0, 3).map((queue) => <p key={queue.id} role="status">安装队列：{queue.message || "等待实例空闲或节点上线"} · {queue.progress}%</p>)}
    {items.map((item) => <div key={item.id} className="package-batch__actions"><span>{item.name || "QQ 闪传单文件"}{item.file_count ? `（${item.file_count} 个 VPK）` : ""} · {item.queue_id ? "等待安装" : item.consume_job_id ? ({ queued: "已提交", dispatched: "已派发", running: "处理中", succeeded: "处理完成", failed: "处理失败，请重新准备", canceled: "已取消" }[item.consume_status || "queued"]) : item.ready ? "可安装" : item.status === "succeeded" ? "已过期或不可用" : item.status === "failed" ? "准备失败，请查看任务中心" : item.status === "canceled" ? "已取消" : `准备中 ${item.progress}%`}</span>
      <Button variant="secondary" disabled={busy || !!active || !item.ready} onClick={() => void install([item])}>安装 {item.name || "QQ 闪传单文件"}</Button>
      {!item.consume_job_id && !item.queue_id && item.status === "succeeded" && <Button variant="ghost" disabled={busy} onClick={() => { void api.discardPreparedPackage(instanceId, item.id).then(refresh).catch((error: Error) => toast.push(error.message, "error")); }}>丢弃 {item.name || "QQ 闪传单文件"}</Button>}
    </div>)}
  </section>;
}
