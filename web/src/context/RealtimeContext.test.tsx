import { QueryClient, QueryClientProvider, QueryObserver, type QueryKey } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider, useRealtime } from "./RealtimeContext";
import type { InstanceRecord, ListResponse, RealtimeEvent } from "../lib/types";
import { api, ApiError, onSessionExpired } from "../lib/api";

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;

  constructor(readonly url: string) {
    FakeWebSocket.instances.push(this);
  }

  close() {
    this.onclose?.(new CloseEvent("close"));
  }
}

afterEach(() => {
  cleanup();
  FakeWebSocket.instances = [];
  vi.unstubAllGlobals();
  vi.useRealTimers();
  sessionStorage.clear();
});

function emit(event: Partial<RealtimeEvent>) {
  FakeWebSocket.instances[0].onmessage?.({ data: JSON.stringify(event) } as MessageEvent);
}

function observe(client: QueryClient, queryKey: QueryKey, initialData: object = { items: [] }) {
  const fetch = vi.fn().mockResolvedValue(initialData);
  const observer = new QueryObserver(client, { queryKey, queryFn: fetch, initialData, staleTime: Infinity });
  const unsubscribe = observer.subscribe(() => {});
  return { fetch, unsubscribe };
}

function EventListener({ listener }: { listener: (event: RealtimeEvent) => void }) {
  const { subscribe } = useRealtime();
  useEffect(() => subscribe(listener), [subscribe, listener]);
  return null;
}

describe("高频事件刷新范围", () => {
  it("会话撤销停止重连，握手失败的 401 也停止重连", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const listener = vi.fn();
    const unsubscribe = onSessionExpired(listener);
    const client = new QueryClient();
    const view = render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    act(() => FakeWebSocket.instances[0].onclose?.(new CloseEvent("close", { code: 1008, reason: "session revoked" })));
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    expect(listener).toHaveBeenCalledTimes(1);
    expect(FakeWebSocket.instances).toHaveLength(1);
    view.unmount(); unsubscribe();
    const check = vi.spyOn(api, "me").mockRejectedValue(new ApiError("expired", 401));
    render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    await act(async () => FakeWebSocket.instances[1].onclose?.(new CloseEvent("close", { code: 1006 })));
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    expect(check).toHaveBeenCalledTimes(1);
    expect(FakeWebSocket.instances).toHaveLength(2);
  });
  it("日志即时送达订阅者，不刷新其他实例、子资源或回退持久游标", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeWebSocket);
    sessionStorage.setItem("l4d2-event-cursor", "123");
    const client = new QueryClient();
    const observers = [
      ["nodes"], ["instances"], ["instances", "instance-b"],
      ["instances", "instance-b", "packages"], ["instances", "instance-b", "files"],
      ["instances", "instance-b", "backups"],
    ].map((key) => observe(client, key));
    const listener = vi.fn();
    render(<QueryClientProvider client={client}><RealtimeProvider><EventListener listener={listener} /></RealtimeProvider></QueryClientProvider>);
    act(() => {
      for (let i = 0; i < 20; i++) emit({ id: 0, type: "instance.log", resource_type: "instance", resource_id: "instance-a", data: { line: `line-${i}` } });
    });
    expect(listener).toHaveBeenCalledTimes(20);
    await act(() => vi.advanceTimersByTimeAsync(1000));
    for (const observer of observers) { expect(observer.fetch).not.toHaveBeenCalled(); observer.unsubscribe(); }
    expect(sessionStorage.getItem("l4d2-event-cursor")).toBe("123");
    client.clear();
  });

  it("一批节点指标只刷新一次节点概览，实例样本不刷新资源列表", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const nodes = observe(client, ["nodes"]);
    const unrelated = [["nodes", "node-a", "quarantine"], ["instances"], ["instances", "instance-a"], ["instances", "instance-a", "packages"], ["metrics", "dashboard", "instance-a"]].map((key) => observe(client, key));
    render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    act(() => {
      for (let i = 0; i < 20; i++) emit({ id: 0, type: "metric", resource_type: "node", resource_id: `node-${i}`, data: { sample: { node_id: `node-${i}`, cpu_percent: i } } });
    });
    expect(nodes.fetch).not.toHaveBeenCalled();
    await act(() => vi.advanceTimersByTimeAsync(250));
    expect(nodes.fetch).toHaveBeenCalledTimes(1);
    act(() => {
      for (let i = 0; i < 20; i++) emit({ id: 0, type: "metric", resource_type: "node", resource_id: "node-a", data: { sample: { node_id: "node-a", instance_id: `instance-${i}` } } });
    });
    await act(() => vi.advanceTimersByTimeAsync(250));
    expect(nodes.fetch).toHaveBeenCalledTimes(1);
    for (const observer of unrelated) { expect(observer.fetch).not.toHaveBeenCalled(); observer.unsubscribe(); }
    nodes.unsubscribe(); client.clear();
  });

  it("节点状态事件只刷新节点概览、实例列表和所属节点的实例详情", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const refreshed = [observe(client, ["nodes"]), observe(client, ["instances"]), observe(client, ["instances", "instance-a"], { node_id: "node-a" })];
    const unchanged = [observe(client, ["instances", "instance-b"], { node_id: "node-b" }), observe(client, ["instances", "instance-a", "packages"]), observe(client, ["instances", "instance-a", "files"])];
    render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    act(() => {
      for (let i = 1; i <= 3; i++) emit({ id: i, type: "node.offline", resource_type: "node", resource_id: "node-a" });
    });
    await act(() => vi.advanceTimersByTimeAsync(250));
    for (const observer of refreshed) { expect(observer.fetch).toHaveBeenCalledTimes(1); observer.unsubscribe(); }
    for (const observer of unchanged) { expect(observer.fetch).not.toHaveBeenCalled(); observer.unsubscribe(); }
    client.clear();
  });

  it("任务事件合并刷新对应实例的包和备份，持续事件不推迟刷新窗口", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const refreshed = [["jobs"], ["environments"], ["maps"], ["instances", "instance-a", "packages"], ["instances", "instance-a", "backups"]].map((key) => observe(client, key));
    const other = observe(client, ["instances", "instance-b", "packages"]);
    render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    for (let i = 0; i < 5; i++) {
      act(() => {
        for (const type of ["rename_package", "plugin.backup"]) emit({ type: "job.updated", resource_type: "job", data: { job: { type, instance_id: "instance-a", status: "succeeded" } } });
      });
      await act(() => vi.advanceTimersByTimeAsync(50));
    }
    for (const observer of refreshed) expect(observer.fetch).toHaveBeenCalledTimes(1);
    expect(other.fetch).not.toHaveBeenCalled();
    act(() => emit({ type: "job.updated", resource_type: "job", data: { job: { type: "rename_package", instance_id: "instance-a", status: "succeeded" } } }));
    await act(() => vi.advanceTimersByTimeAsync(250));
    expect(refreshed[3].fetch).toHaveBeenCalledTimes(2);
    for (const observer of [...refreshed, other]) observer.unsubscribe();
    client.clear();
  });

  it("卸载后取消待刷新任务，也忽略迟到帧", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const jobs = observe(client, ["jobs"]);
    const view = render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    act(() => emit({ id: 1, type: "job.updated", resource_type: "job" }));
    view.unmount();
    act(() => emit({ id: 2, type: "job.updated", resource_type: "job" }));
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(jobs.fetch).not.toHaveBeenCalled();
    expect(sessionStorage.getItem("l4d2-event-cursor")).toBe("1");
    jobs.unsubscribe(); client.clear();
  });
});

describe("实时节点事件", () => {
  it("实例投影不会取消仍在加载的包清单，首次实例列表会重新获取", async () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    let oldList!: (value: ListResponse<InstanceRecord>) => void;
    let finishPackages!: (value: string[]) => void;
    const instance = { id: "instance-a", node_id: "node-a", status: "stopped" } as InstanceRecord;
    const list = vi.fn().mockImplementationOnce(() => new Promise((resolve) => { oldList = resolve; })).mockResolvedValue({ items: [instance] });
    const listObserver = new QueryObserver(client, { queryKey: ["instances"], queryFn: list });
    const packageObserver = new QueryObserver(client, { queryKey: ["instances", instance.id, "packages"], queryFn: () => new Promise<string[]>((resolve) => { finishPackages = resolve; }) });
    const unsubscribeList = listObserver.subscribe(() => {});
    const unsubscribePackages = packageObserver.subscribe(() => {});
    render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    await act(async () => FakeWebSocket.instances[0].onmessage?.({ data: JSON.stringify({ id: 1, type: "instance.updated", resource_type: "instance", resource_id: instance.id, data: { instance } }) } as MessageEvent));
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    finishPackages(["installed"]);
    oldList({ items: [] });
    await waitFor(() => expect(client.getQueryData(["instances", instance.id, "packages"])).toEqual(["installed"]));
    expect(client.getQueryData(["instances"])).toEqual({ items: [instance] });
    unsubscribeList(); unsubscribePackages();
  });

  it("重新连通后刷新已挂载资源，重命名和恢复任务也更新包清单", async () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(<QueryClientProvider client={client}><RealtimeProvider><span>ready</span></RealtimeProvider></QueryClientProvider>);
    act(() => FakeWebSocket.instances[0].onopen?.(new Event("open")));
    for (const key of ["maps", "nodes", "instances", "jobs", "environments", "notifications"]) expect(invalidate).toHaveBeenCalledWith({ queryKey: [key] });
    invalidate.mockClear();
    for (const type of ["rename_package", "restore"]) {
      act(() => FakeWebSocket.instances[0].onmessage?.({ data: JSON.stringify({ type: "job.updated", resource_type: "job", data: { job: { type, instance_id: "instance-a", status: "succeeded" } } }) } as MessageEvent));
      await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances", "instance-a", "packages"], exact: false }));
      invalidate.mockClear();
    }
  });
  it("节点状态变化时同步刷新实例节点投影", async () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <RealtimeProvider><span>ready</span></RealtimeProvider>
      </QueryClientProvider>,
    );

    expect(FakeWebSocket.instances).toHaveLength(1);
    act(() => {
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({ id: 1, type: "node.offline", resource_type: "node", resource_id: "node-a", created_at: "2026-08-07T00:00:00Z" }),
      } as MessageEvent);
    });

    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["nodes"], exact: true }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances"], exact: true });
  });

  it("任务更新时刷新运行环境投影", async () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <RealtimeProvider><span>ready</span></RealtimeProvider>
      </QueryClientProvider>,
    );

    act(() => {
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({ id: 2, type: "job.updated", resource_type: "job", resource_id: "job-a", created_at: "2026-08-09T00:00:00Z" }),
      } as MessageEvent);
    });

    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["environments"], exact: false }));
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ["instances"] });
  });

  it("模组扫描终态刷新实例包清单", async () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <RealtimeProvider><span>ready</span></RealtimeProvider>
      </QueryClientProvider>,
    );

    act(() => {
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({
          id: 3,
          type: "job.updated",
          resource_type: "job",
          resource_id: "scan-job",
          data: { job: { id: "scan-job", instance_id: "instance-a", type: "scan_packages", status: "succeeded" } },
          created_at: "2026-08-10T00:00:00Z",
        }),
      } as MessageEvent);
    });

    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances", "instance-a", "packages"], exact: false }));
  });

  it("插件备份任务更新时刷新实例备份清单", async () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <RealtimeProvider><span>ready</span></RealtimeProvider>
      </QueryClientProvider>,
    );

    act(() => {
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({
          id: 4,
          type: "job.updated",
          resource_type: "job",
          resource_id: "plugin-backup-job",
          data: { job: { id: "plugin-backup-job", instance_id: "instance-a", type: "plugin.backup", status: "succeeded" } },
          created_at: "2026-08-12T00:00:00Z",
        }),
      } as MessageEvent);
    });

    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances", "instance-a", "backups"], exact: false }));
  });

  it("实例终态事件直接同步详情与列表缓存", async () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const running = {
      id: "instance-a", node_id: "node-a", node_name: "Node", node_status: "online", name: "Server",
      status: "running", host: "0.0.0.0", port: 27015, players: 1, max_players: 4, update_available: false,
      rcon_configured: true, version: 1, config: { hostname: "Server", mode: "coop", difficulty: "normal", max_players: 4, start_map: "c1m1_hotel", public_status: false, auto_restart: true },
      created_at: "2026-08-09T00:00:00Z", updated_at: "2026-08-09T00:00:00Z",
    } satisfies InstanceRecord;
    const stopping = { ...running, status: "stopping", updated_at: "2026-08-09T00:00:30Z" } satisfies InstanceRecord;
    const stopped = { ...running, status: "stopped", players: 0, updated_at: "2026-08-09T00:01:00Z" } satisfies InstanceRecord;
    client.setQueryData<InstanceRecord>(["instances", running.id], running);
    client.setQueryData<ListResponse<InstanceRecord>>(["instances"], { items: [running] });
    render(
      <QueryClientProvider client={client}>
        <RealtimeProvider><span>ready</span></RealtimeProvider>
      </QueryClientProvider>,
    );

    await act(async () => {
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({ id: 3, type: "instance.updated", resource_type: "instance", resource_id: running.id, data: { instance: stopping }, created_at: stopping.updated_at }),
      } as MessageEvent);
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({ id: 4, type: "instance.updated", resource_type: "instance", resource_id: running.id, data: { instance: stopped }, created_at: stopped.updated_at }),
      } as MessageEvent);
      await Promise.resolve();
    });

    expect(client.getQueryData<InstanceRecord>(["instances", running.id])).toEqual(stopped);
    expect(client.getQueryData<ListResponse<InstanceRecord>>(["instances"])?.items).toEqual([stopped]);
  });
});
