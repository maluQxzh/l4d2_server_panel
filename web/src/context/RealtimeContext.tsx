import { refreshQueries } from "../lib/query-refresh";
import { useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { apiBaseURL } from "../lib/api";
import { removeCachedInstance, setCachedInstance } from "../lib/instance-cache";
import type { InstanceRecord, Job, ListResponse, RealtimeEvent } from "../lib/types";

type ConnectionState = "connecting" | "live" | "polling";
type Listener = (event: RealtimeEvent) => void;

const RealtimeContext = createContext<{
  state: ConnectionState;
  lastEventAt?: string;
  subscribe: (listener: Listener) => () => void;
} | null>(null);

function websocketURL(cursor: string) {
  const configured = import.meta.env.VITE_WS_URL as string | undefined;
  if (configured) return `${configured}${configured.includes("?") ? "&" : "?"}cursor=${encodeURIComponent(cursor)}`;
  const apiPath = apiBaseURL().startsWith("http") ? new URL(apiBaseURL()) : new URL(apiBaseURL(), window.location.origin);
  apiPath.protocol = apiPath.protocol === "https:" ? "wss:" : "ws:";
  apiPath.pathname = `${apiPath.pathname.replace(/\/$/, "")}/events`;
  apiPath.search = `?cursor=${encodeURIComponent(cursor)}`;
  return apiPath.toString();
}

export function RealtimeProvider({ children, enabled = true }: { children: ReactNode; enabled?: boolean }) {
  const queryClient = useQueryClient();
  const [state, setState] = useState<ConnectionState>("connecting");
  const [lastEventAt, setLastEventAt] = useState<string>();
  const listeners = useRef(new Set<Listener>());

  useEffect(() => {
    if (!enabled) return;
    let socket: WebSocket | undefined;
    let retryTimer: number | undefined;
    let attempts = 0;
    let stopped = false;
    let instanceProjectionChain = Promise.resolve();

    const applyInstanceProjection = (id: string, update: () => void) => {
      instanceProjectionChain = instanceProjectionChain
        .then(async () => {
          if (stopped) return;
          const listed = queryClient.getQueryData<ListResponse<InstanceRecord>>(["instances"])?.items.some((instance) => instance.id === id);
          // Cancel only the two projections being replaced. Canceling the whole
          // prefix strands pending files/packages/backups with no replacement.
          await Promise.all([
            queryClient.cancelQueries({ queryKey: ["instances"], exact: true }),
            queryClient.cancelQueries({ queryKey: ["instances", id], exact: true }),
          ]);
          if (stopped) return;
          update();
          if (!listed) void refreshQueries(queryClient, { queryKey: ["instances"], exact: true });
        });
    };

    const connect = () => {
      if (stopped) return;
      setState(attempts ? "polling" : "connecting");
      const cursor = sessionStorage.getItem("l4d2-event-cursor") || "0";
      socket = new WebSocket(websocketURL(cursor));
      socket.onopen = () => {
        if (stopped) return;
        attempts = 0;
        setState("live");
        // A socket may reconnect after an event was missed or outside retention.
        for (const root of ["nodes", "instances", "jobs", "maps", "environments", "notifications"]) {
          void refreshQueries(queryClient, { queryKey: [root] });
        }
      };
      socket.onmessage = (message) => {
        try {
          const event = JSON.parse(message.data as string) as RealtimeEvent;
          // 指标/日志帧不会持久化，ID 为 0；不能让它们把可回放游标倒退。
          if (typeof event.id === "number" && event.id > 0) sessionStorage.setItem("l4d2-event-cursor", String(event.id));
          setLastEventAt(event.created_at || new Date().toISOString());
          listeners.current.forEach((listener) => listener(event));
          if (event.resource_type === "node") {
            void refreshQueries(queryClient, { queryKey: ["nodes"] });
            // 实例响应投影所属节点的名称与在线状态，节点事件也必须刷新实例卡片。
            void refreshQueries(queryClient, { queryKey: ["instances"] });
          }
          if (event.resource_type === "instance") {
            const instance = event.data?.instance as InstanceRecord | undefined;
            if (instance && event.type === "instance.updated" && instance.id === event.resource_id) {
              // The event is published from the same committed projection as the
              // API response. Write it directly so an older refetch cannot race
              // back in and overwrite a completed start/stop transition.
              applyInstanceProjection(instance.id, () => setCachedInstance(queryClient, instance));
            } else if (event.type === "instance.deleted" && event.resource_id) {
              const instanceID = event.resource_id;
              applyInstanceProjection(instanceID, () => removeCachedInstance(queryClient, instanceID));
            } else {
              void refreshQueries(queryClient, { queryKey: ["instances"] });
            }
          }
          if (event.resource_type === "job" || event.type.startsWith("job")) {
            void refreshQueries(queryClient, { queryKey: ["jobs"] });
            const job = event.data?.job as Job | undefined;
            if (job?.instance_id && ["install_mod", "install_package", "qq_flash_import", "scan_packages", "toggle_package", "rename_package", "rollback_package", "delete_package", "restore"].includes(job.type)) {
              void refreshQueries(queryClient, { queryKey: ["instances", job.instance_id, "packages"] });
            }
            if (job?.instance_id && ["backup", "plugin.backup", "backup.delete"].includes(job.type)) {
              void refreshQueries(queryClient, { queryKey: ["instances", job.instance_id, "backups"] });
            }
            // 实例投影由同一事务随后发布的 instance.updated 权威事件更新；
            // 不在这里重复 refetch，避免旧请求覆盖启动/停止终态。
            void refreshQueries(queryClient, { queryKey: ["environments"] });
            void refreshQueries(queryClient, { queryKey: ["maps"] });
          }
          if (event.type.startsWith("notification")) void refreshQueries(queryClient, { queryKey: ["notifications"] });
        } catch {
          // Ignore malformed event frames; a later valid cursor keeps the stream usable.
        }
      };
      socket.onerror = () => socket?.close();
      socket.onclose = () => {
        if (stopped) return;
        attempts += 1;
        setState("polling");
        const delay = Math.min(30_000, 1_000 * 2 ** Math.min(attempts, 5));
        retryTimer = window.setTimeout(connect, delay);
      };
    };
    connect();
    return () => {
      stopped = true;
      if (retryTimer) window.clearTimeout(retryTimer);
      socket?.close();
    };
  }, [enabled, queryClient]);

  const value = useMemo(() => ({
    state,
    lastEventAt,
    subscribe: (listener: Listener) => {
      listeners.current.add(listener);
      return () => listeners.current.delete(listener);
    },
  }), [lastEventAt, state]);

  return <RealtimeContext.Provider value={value}>{children}</RealtimeContext.Provider>;
}

export function useRealtime() {
  const value = useContext(RealtimeContext);
  if (!value) throw new Error("useRealtime 必须在 RealtimeProvider 内使用");
  return value;
}
