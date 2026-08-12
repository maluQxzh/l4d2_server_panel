import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "./RealtimeContext";
import type { InstanceRecord, ListResponse } from "../lib/types";

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
  FakeWebSocket.instances = [];
  vi.unstubAllGlobals();
});

describe("实时节点事件", () => {
  it("节点状态变化时同步刷新实例节点投影", () => {
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

    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["nodes"] });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances"] });
  });

  it("任务更新时刷新运行环境投影", () => {
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

    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["environments"] });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ["instances"] });
  });

  it("模组扫描终态刷新实例包清单", () => {
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

    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances", "instance-a", "packages"] });
  });

  it("插件备份任务更新时刷新实例备份清单", () => {
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

    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances", "instance-a", "backups"] });
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
