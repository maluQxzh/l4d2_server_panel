import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { InstanceRecord } from "../lib/types";

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ user: { role: "owner" } }),
}));

import { InstancesPage } from "./InstancesPage";

const instance: InstanceRecord = {
  id: "12345678-1234-1234-1234-123456789abc",
  node_id: "87654321-4321-4321-4321-cba987654321",
  node_name: "广州游戏节点 A",
  node_status: "offline",
  name: "绝境求生",
  status: "running",
  host: "0.0.0.0",
  port: 27015,
  players: 0,
  max_players: 8,
  update_available: false,
  rcon_configured: true,
  version: 1,
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

describe("游戏实例卡片", () => {
  afterEach(() => vi.restoreAllMocks());

  it("保留离线节点的实例，并同时显示最后实例状态和所属节点", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [instance] });
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <InstancesPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("link", { name: "绝境求生" })).toBeInTheDocument();
    expect(screen.getByText("运行中", { selector: ".status" })).toBeInTheDocument();
    expect(screen.getByText("节点离线", { selector: ".status" })).toBeInTheDocument();
    expect(screen.getByText("所属节点：广州游戏节点 A")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "修改端口" })).not.toBeInTheDocument();
  });

  it("创建实例时不再要求单独填写服务器显示名称", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [] });
    vi.spyOn(api, "environments").mockResolvedValue({ items: [] });
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <InstancesPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "创建实例" }));
    expect(screen.queryByText("服务器显示名称")).not.toBeInTheDocument();
  });

  it("没有在线玩家时检测后直接重启", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [instance] });
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    const restart = vi.spyOn(api, "instanceAction").mockResolvedValue({ job_id: "restart-job" });
    const confirm = vi.spyOn(window, "confirm");
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <InstancesPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "重启" }));
    await waitFor(() => expect(restart).toHaveBeenCalledWith(instance.id, "restart", undefined));
    expect(confirm).not.toHaveBeenCalled();
    expect(restart.mock.calls[0][2]).toBeUndefined();
  });

  it("有在线玩家时警告并允许确认重启", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [instance] });
    vi.spyOn(api, "instance").mockResolvedValue({ ...instance, players: 3 });
    const restart = vi.spyOn(api, "instanceAction").mockResolvedValue({ job_id: "restart-job" });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <InstancesPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "重启" }));
    await waitFor(() => expect(confirm).toHaveBeenCalledWith(expect.stringContaining("3 名玩家在线")));
    await waitFor(() => expect(restart).toHaveBeenCalledWith(instance.id, "restart", undefined));
  });
});
