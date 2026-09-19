import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, newIdempotencyKey, type PreparedPackage } from "../lib/api";
import { submitWhenInstanceReady } from "../lib/instanceBusy";
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
  const stopped = useRef(false);
  useEffect(() => { generation.current++; stopped.current = false; setBusy(false); setMessage(""); return () => { generation.current++; stopped.current = true; }; }, [instanceId, plugins]);
  const items = (query.data?.items || []).filter((item) => plugins ? item.kind !== "vpk" : item.kind === "vpk");
  if (!query.data?.supported && !items.length) return null;
  const refresh = async () => { await client.invalidateQueries({ queryKey: ["instances", instanceId] }); void client.invalidateQueries({ queryKey: ["jobs"] }); };
  const install = async (queue: PreparedPackage[]) => {
    if (busy || !queue.length) return;
    if (!window.confirm(`确认依次安装 ${queue.length} 个预下载包？安装期间可能重启正在运行的实例。`)) return;
    const native = queue.some((item) => item.kind === "native");
    if (native && !window.confirm("其中含第三方原生扩展，会在节点执行原生代码。确认了解并接受该风险？")) return;
    const epoch = generation.current;
    const canceled = () => stopped.current || epoch !== generation.current;
    stopped.current = false; setBusy(true);
    try {
      for (const item of queue) {
        if (canceled()) break;
        setMessage(`准备安装 ${item.name || "QQ 闪传单文件"}`);
        const key = newIdempotencyKey();
        const response = await submitWhenInstanceReady(() => api.installPreparedPackage(instanceId, item.id, native, key), canceled, (text) => setMessage(`${item.name || "QQ 闪传单文件"}：${text}`));
        const deadline = Date.now() + 10 * 60_000;
        while (!canceled()) {
          const state = await api.preparedPackages(instanceId);
          if (canceled()) break;
          const current = state.items.find((p) => p.id === item.id && p.consume_job_id === response.job_id);
          if (current && !current.busy && ["succeeded", "failed", "canceled"].includes(current.consume_status || "")) {
            if (current.consume_status !== "succeeded") throw new Error(`${item.name || "QQ 闪传单文件"} 安装失败或已取消；请查看任务 ${response.job_id}，重新准备后再试`);
            break;
          }
          if (Date.now() >= deadline) throw new Error(`安装仍未结束，已停止后续提交；请查看任务 ${response.job_id}`);
          setMessage(`正在安装 ${item.name || "QQ 闪传单文件"}，等待任务完成`);
          await new Promise((resolve) => setTimeout(resolve, 1000));
        }
        if (!canceled()) await refresh();
      }
      if (epoch === generation.current) setMessage(canceled() ? "已停止后续提交，已提交任务继续执行" : "所选预下载包已安装完成");
    } catch (error) {
      if (epoch === generation.current) setMessage(error instanceof Error ? error.message : "安装失败");
    } finally {
      if (epoch === generation.current) { setBusy(false); void refresh(); }
    }
  };
  const ready = items.filter((item) => item.ready);
  return <section className="package-batch" aria-label="预下载包">
    <strong>预下载包</strong><p>下载不占实例写锁，从开始准备起 1 小时内有效；支持刷新后继续。安装会逐项等待，离开此页停止后续提交。</p>
    {query.error && <p role="alert">无法刷新预下载状态，请稍后重试</p>}
    <div className="package-batch__actions"><Button variant="secondary" disabled={busy || !ready.length} onClick={() => void install(ready)}>依次安装就绪包（{ready.length}）</Button>{busy && <Button variant="secondary" onClick={() => { stopped.current = true; }}>停止后续安装</Button>}</div>
    {message && <p role="status">{message}</p>}
    {items.map((item) => <div key={item.id} className="package-batch__actions"><span>{item.name || "QQ 闪传单文件"} · {item.consume_job_id ? ({ queued: "已提交", dispatched: "已派发", running: "处理中", succeeded: "处理完成", failed: "处理失败，请重新准备", canceled: "已取消" }[item.consume_status || "queued"]) : item.ready ? "可安装" : item.status === "succeeded" ? "已过期或不可用" : item.status === "failed" ? "准备失败，请查看任务中心" : item.status === "canceled" ? "已取消" : `准备中 ${item.progress}%`}</span>
      <Button variant="secondary" disabled={busy || !item.ready} onClick={() => void install([item])}>安装 {item.name || "QQ 闪传单文件"}</Button>
      {!item.consume_job_id && item.status === "succeeded" && <Button variant="ghost" disabled={busy} onClick={() => { void api.discardPreparedPackage(instanceId, item.id).then(refresh).catch((error: Error) => toast.push(error.message, "error")); }}>丢弃 {item.name || "QQ 闪传单文件"}</Button>}
    </div>)}
  </section>;
}
