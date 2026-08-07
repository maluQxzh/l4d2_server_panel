import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { InstanceRecord } from "../lib/types";
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
  });
});
