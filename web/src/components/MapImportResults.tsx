import { useQueries, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { api, ApiError } from "../lib/api";
import { acknowledgeMapImport, pendingMapImports, subscribeMapImports } from "../lib/map-import-tracker";
import { Button, Modal } from "./ui";

// Mounted above the routes so navigating away cannot lose a completion notice.
// Only job IDs are persisted; results are fetched with the current user's scope.
export function MapImportResults({ userId }: { userId: string }) {
  const client = useQueryClient();
  const [ids, setIds] = useState(() => pendingMapImports(userId));
  useEffect(() => subscribeMapImports(() => setIds(pendingMapImports(userId))), [userId]);
  const queries = useQueries({ queries: ids.map((id) => ({
    queryKey: ["map-import-result", userId, id],
    queryFn: () => api.mapImportResult(id),
    refetchInterval: (query: { state: { data?: { ready: boolean }; error: Error | null } }) => query.state.data?.ready || unavailable(query.state.error) ? false : 2_000,
    retry: (count: number, error: Error) => !unavailable(error) && count < 2,
  })) });
  const index = queries.findIndex((query) => query.data?.ready || unavailable(query.error));
  const query = queries[index];
  const result = query?.data?.ready && !unavailable(query.error) ? query.data : undefined;
  const jobId = ids[index];
  useEffect(() => {
    if (result?.ready) void client.invalidateQueries({ queryKey: ["maps"] });
  }, [client, result?.ready, jobId]);
  if (!jobId) return null;
  const close = () => acknowledgeMapImport(userId, jobId);
  const missing = result?.missing_dictionaries || [];
  const warnings = result?.warnings || [];
  const hasWarnings = missing.length > 0 || warnings.length > 0;
  const title = !result ? "无法读取地图导入结果" : result.status === "failed" ? "地图导入失败" : result.status === "canceled" ? "地图导入已取消" : result.duplicate ? "地图已存在" : hasWarnings ? "地图导入完成，但有检测警告" : "地图导入完成";
  return <Modal open title={title} onClose={close} footer={<Button onClick={close}>知道了</Button>}>
    <div className="form-stack">
      {result?.name && <strong>{result.name}</strong>}
      {!result ? <p>此任务已不可用，或当前账号已失去该节点的地图导入权限。</p> : result.status === "succeeded" ? <>
        <p>{result.duplicate ? "服务器检测到相同内容的地图，已复用已有地图，本次没有新增地图记录。" : "服务器检测完成，地图已加入地图库。"}</p>
        {missing.length > 0 && <div className="form-warning"><div><strong>以下 BSP 缺少字典</strong><ul>{missing.map((path) => <li key={path}>{path}</li>)}</ul></div></div>}
        {warnings.length > 0 && <section className="map-warnings"><h3>检测警告（{warnings.length}）</h3>{warnings.map((warning, warningIndex) => <p key={warningIndex}><strong>{warning.code}</strong>：{warning.message}{warning.virtual_path && <><br /><code>{warning.virtual_path}</code></>}</p>)}</section>}
        {!hasWarnings && <p>未发现检测警告。</p>}
      </> : <p>{result.status === "failed" ? "服务器未能完成地图导入，请确认分享有效、内容完整且只包含一张地图后重试。具有任务详情权限的账号可在任务中心查看失败原因。" : "本次地图导入已取消。"}</p>}
    </div>
  </Modal>;
}

function unavailable(error: Error | null) {
  return error instanceof ApiError && [403, 404].includes(error.status);
}
