import { QueryClient } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import { removeCachedInstance, setCachedInstanceStatus } from "./instance-cache";
import type { InstanceRecord, ListResponse } from "./types";

const instance = {
  id: "instance-a", node_id: "node-a", node_name: "Node", node_status: "online", name: "Server",
  status: "running", host: "0.0.0.0", port: 27015, players: 1, max_players: 4, update_available: false,
  rcon_configured: true, version: 1, config: { hostname: "Server", mode: "coop", difficulty: "normal", max_players: 4, start_map: "c1m1_hotel", public_status: false, auto_restart: true },
  created_at: "2026-08-09T00:00:00Z", updated_at: "2026-08-09T00:00:00Z",
} satisfies InstanceRecord;

describe("实例实时缓存", () => {
  it("启动和停止过渡态同时写入详情与列表", () => {
    const client = new QueryClient();
    client.setQueryData<InstanceRecord>(["instances", instance.id], instance);
    client.setQueryData<ListResponse<InstanceRecord>>(["instances"], { items: [instance] });

    setCachedInstanceStatus(client, instance.id, "stopping");

    expect(client.getQueryData<InstanceRecord>(["instances", instance.id])?.status).toBe("stopping");
    expect(client.getQueryData<ListResponse<InstanceRecord>>(["instances"])?.items[0].status).toBe("stopping");
  });

  it("删除事件同时清理详情与列表", () => {
    const client = new QueryClient();
    client.setQueryData<InstanceRecord>(["instances", instance.id], instance);
    client.setQueryData<ListResponse<InstanceRecord>>(["instances"], { items: [instance], total: 1 });

    removeCachedInstance(client, instance.id);

    expect(client.getQueryData(["instances", instance.id])).toBeUndefined();
    expect(client.getQueryData<ListResponse<InstanceRecord>>(["instances"])).toEqual({ items: [], total: 0 });
  });
});
