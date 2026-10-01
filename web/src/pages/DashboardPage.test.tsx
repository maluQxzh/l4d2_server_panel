import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { api } from "../lib/api";
import type { InstanceRecord, MetricPoint } from "../lib/types";

const authState = vi.hoisted(() => ({ user: { role: "owner" } as { role: string; role_bindings?: unknown } }));
vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

import { DashboardPage } from "./DashboardPage";

const instances = [
  { id: "empty", name: "空服", status: "running", players: 0 },
  { id: "occupied", name: "多人绝境01", status: "running", players: 3 },
  { id: "stopped", name: "已停止服", status: "stopped", players: 9 },
  ...Array.from({ length: 4 }, (_, index) => ({ id: `extra-${index}`, name: `额外服${index}`, status: "running", players: index === 3 ? 2 : 0 })),
].map((item) => ({ ...item, node_id: "node-1", host: "127.0.0.1", port: 27015, max_players: 10 })) as InstanceRecord[];

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><MemoryRouter><DashboardPage /></MemoryRouter></RealtimeProvider></QueryClientProvider>);
  return client;
}

describe("仪表盘玩家统计", () => {
  beforeEach(() => {
    authState.user = { role: "owner" };
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: instances });
    vi.spyOn(api, "jobs").mockResolvedValue({ items: [] });
    vi.spyOn(api, "metrics").mockResolvedValue({ items: [], resolution: "1m", from: "2026-09-30T00:00:00Z", to: "2026-10-01T00:00:00Z" });
  });
  afterEach(() => vi.restoreAllMocks());

  it("汇总全部可见实例而不是第一台或前六台，并随实例更新同步", async () => {
    const client = mount();
    const total = await screen.findByLabelText("集群当前在线玩家");
    expect(within(total).getByText("5", { exact: false, selector: "strong" })).toBeInTheDocument();
    expect(total).toHaveTextContent("全部 7 个可见实例合计");
    expect(await screen.findByText("暂无历史玩家数据")).toBeInTheDocument();
    expect(document.querySelector(".recharts-wrapper")).toBeNull();
    act(() => client.setQueryData(["instances"], { items: instances.map((item) => item.id === "occupied" ? { ...item, players: 4 } : item) }));
    await waitFor(() => expect(total.querySelector("strong")).toHaveTextContent("6 人"));
    expect(screen.getByRole("link", { name: /多人绝境01/ })).toHaveTextContent("4/10");
  });

  it("单实例历史为零也不覆盖当前集群人数，并标明历史所属实例", async () => {
    vi.mocked(api.metrics).mockResolvedValue({ items: [{ instance_id: "empty", collected_at: "2026-10-01T00:00:00Z", players: 0 } as MetricPoint], resolution: "1m", from: "2026-09-30T00:00:00Z", to: "2026-10-01T00:00:00Z" });
    mount();
    expect(await screen.findByText("空服 · 近 24 小时玩家趋势（单实例）")).toBeInTheDocument();
    expect(screen.getByLabelText("集群当前在线玩家").querySelector("strong")).toHaveTextContent("5 人");
    await waitFor(() => expect(api.metrics).toHaveBeenCalledWith(expect.objectContaining({ instanceId: "empty" })));
  });

  it("历史读取失败时提示失败并保留实时总人数", async () => {
    vi.mocked(api.metrics).mockRejectedValue(new Error("unavailable"));
    mount();
    expect(await screen.findByText("历史数据暂时无法读取")).toBeInTheDocument();
    expect(screen.getByLabelText("集群当前在线玩家").querySelector("strong")).toHaveTextContent("5 人");
  });

  it("仅有实例查看权限也能查看当前总人数，不请求历史指标或伪造节点容量", async () => {
    authState.user = { role: "viewer", role_bindings: [{ role: "viewer", scope_type: "global", permissions: ["instances.read"] }] };
    mount();
    expect((await screen.findByLabelText("集群当前在线玩家")).querySelector("strong")).toHaveTextContent("5 人");
    expect(api.metrics).not.toHaveBeenCalled();
    expect(api.nodes).not.toHaveBeenCalled();
    expect(screen.queryByText(/近 24 小时玩家趋势/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText("节点网络累计流量")).not.toBeInTheDocument();
  });

  it("仅有节点查看权限时不显示虚假的零玩家", async () => {
    authState.user = { role: "viewer", role_bindings: [{ role: "viewer", scope_type: "global", permissions: ["nodes.read"] }] };
    mount();
    expect(await screen.findByRole("heading", { name: "集群容量" })).toBeInTheDocument();
    expect(screen.queryByLabelText("集群当前在线玩家")).not.toBeInTheDocument();
    expect(api.instances).not.toHaveBeenCalled();
    expect(api.metrics).not.toHaveBeenCalled();
  });
});
