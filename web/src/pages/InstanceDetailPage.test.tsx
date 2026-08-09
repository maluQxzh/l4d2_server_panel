import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { InstanceRecord } from "../lib/types";

const authState = vi.hoisted(() => ({ user: { role: "owner" } as { role: string; role_bindings?: unknown } }));

vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

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
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => {
    vi.restoreAllMocks();
    localStorage.clear();
  });

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

  it("只显示已授权标签，且创建备份不依赖备份清单权限", async () => {
    authState.user = {
      role: "operator",
      role_bindings: [{
        role: "operator",
        scope_type: "instance",
        instance_id: instance.id,
        permissions: ["instances.read", "backups.create"],
      }],
    };
    localStorage.setItem(`l4d2panel.backup-upload.v1:${instance.id}`, JSON.stringify({
      version: 1,
      instanceId: instance.id,
      createKey: "persisted-create-key",
      uploadId: "22222222-2222-4222-8222-222222222222",
      name: "old.zip",
      size: 1024,
      sha256: "a".repeat(64),
      platform: "linux/amd64",
      updatedAt: "2026-08-09T00:00:00Z",
    }));
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    const backups = vi.spyOn(api, "backups").mockResolvedValue({ items: [] });
    const reconcileUpload = vi.spyOn(api, "backupUpload").mockRejectedValue(new Error("must not be queried"));
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=backups`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><RouterProvider router={router} /></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "实例备份" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "立即备份" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "控制台" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "玩家" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "配置" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "文件" })).not.toBeInTheDocument();
    expect(backups).not.toHaveBeenCalled();
    expect(reconcileUpload).not.toHaveBeenCalled();
  });
});
