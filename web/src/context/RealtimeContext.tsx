import { useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { apiBaseURL } from "../lib/api";
import { removeCachedInstance, setCachedInstance } from "../lib/instance-cache";
import type { InstanceRecord, RealtimeEvent } from "../lib/types";

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

    const applyInstanceProjection = (update: () => void) => {
      instanceProjectionChain = instanceProjectionChain
        .then(() => queryClient.cancelQueries({ queryKey: ["instances"] }))
        .then(() => { if (!stopped) update(); });
    };

    const connect = () => {
      if (stopped) return;
      setState(attempts ? "polling" : "connecting");
      const cursor = sessionStorage.getItem("l4d2-event-cursor") || "0";
      socket = new WebSocket(websocketURL(cursor));
      socket.onopen = () => {
        attempts = 0;
        setState("live");
      };
      socket.onmessage = (message) => {
        try {
          const event = JSON.parse(message.data as string) as RealtimeEvent;
          // 指标/日志帧不会持久化，ID 为 0；不能让它们把可回放游标倒退。
          if (typeof event.id === "number" && event.id > 0) sessionStorage.setItem("l4d2-event-cursor", String(event.id));
          setLastEventAt(event.created_at || new Date().toISOString());
          listeners.current.forEach((listener) => listener(event));
          if (event.resource_type === "node") {
            void queryClient.invalidateQueries({ queryKey: ["nodes"] });
            // 实例响应投影所属节点的名称与在线状态，节点事件也必须刷新实例卡片。
            void queryClient.invalidateQueries({ queryKey: ["instances"] });
          }
          if (event.resource_type === "instance") {
            const instance = event.data?.instance as InstanceRecord | undefined;
            if (instance && event.type === "instance.updated" && instance.id === event.resource_id) {
              // The event is published from the same committed projection as the
              // API response. Write it directly so an older refetch cannot race
              // back in and overwrite a completed start/stop transition.
              applyInstanceProjection(() => setCachedInstance(queryClient, instance));
            } else if (event.type === "instance.deleted" && event.resource_id) {
              const instanceID = event.resource_id;
              applyInstanceProjection(() => removeCachedInstance(queryClient, instanceID));
            } else {
              void queryClient.invalidateQueries({ queryKey: ["instances"] });
            }
          }
          if (event.resource_type === "job" || event.type.startsWith("job")) {
            void queryClient.invalidateQueries({ queryKey: ["jobs"] });
            // 实例投影由同一事务随后发布的 instance.updated 权威事件更新；
            // 不在这里重复 refetch，避免旧请求覆盖启动/停止终态。
            void queryClient.invalidateQueries({ queryKey: ["environments"] });
            void queryClient.invalidateQueries({ queryKey: ["maps"] });
          }
          if (event.type.startsWith("notification")) void queryClient.invalidateQueries({ queryKey: ["notifications"] });
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
