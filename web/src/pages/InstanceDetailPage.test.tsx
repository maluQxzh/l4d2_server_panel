import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { InstanceRecord } from "../lib/types";

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ user: { role: "owner" } }),
}));

import { InstanceDetailPage } from "./InstanceDetailPage";

const instance: InstanceRecord = {
  id: "12345678-1234-1234-1234-123456789abc",
  node_id: "87654321-4321-4321-4321-cba987654321",
  node_name: "广州游戏节点 A",
  node_status: "online",
  node_public_address: "game.example.com",
  name: "绝境求生",
  status: "stopped",
  host: "0.0.0.0",
  port: 27015,
  tv_port: 27020,
  players: 0,
  max_players: 8,
  update_available: false,
  rcon_configured: true,
  version: 4,
  config: {
    hostname: "绝境求生",
    mode: "coop",
    difficulty: "normal",
    max_players: 8,
    start_map: "c2m1_highway",
    public_status: false,
    auto_restart: true,
  },
  created_at: "2026-08-07T00:00:00Z",
  updated_at: "2026-08-07T00:00:00Z",
};

function renderConfigPage(item: InstanceRecord) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  vi.spyOn(api, "instance").mockResolvedValue(item);
  vi.spyOn(api, "maps").mockResolvedValue({ items: [] });
  const router = createMemoryRouter(
    [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
    { initialEntries: [`/instances/${item.id}?tab=config`] },
  );
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <RouterProvider router={router} />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("游戏实例配置中的端口", () => {
  afterEach(() => vi.restoreAllMocks());

  it("停止状态把新端口随配置一起提交", async () => {
    const update = vi.spyOn(api, "updateInstance").mockResolvedValue({ instance: { ...instance, version: 5 } });
    renderConfigPage(instance);

    const port = await screen.findByLabelText(/游戏端口/);
    expect(port).toBeEnabled();
    fireEvent.change(port, { target: { value: "27016" } });
    fireEvent.click(screen.getByRole("button", { name: "保存配置" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(instance.id, expect.objectContaining({
      version: 4,
      port: 27016,
      config: expect.objectContaining({ hostname: "绝境求生" }),
    })));
  });

  it("运行中只禁用端口，仍允许保存其他配置", async () => {
    const running = { ...instance, status: "running" as const };
    const update = vi.spyOn(api, "updateInstance").mockResolvedValue({ instance: { ...running, version: 5 } });
    renderConfigPage(running);

    expect(await screen.findByLabelText(/游戏端口/)).toBeDisabled();
    fireEvent.change(screen.getByLabelText("服务器名称"), { target: { value: "运行中改名" } });
    fireEvent.click(screen.getByRole("button", { name: "保存配置" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(instance.id, expect.not.objectContaining({ port: expect.anything() })));
  });
});
