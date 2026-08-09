import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { GameInstallationRecord, RuntimeEnvironmentRecord } from "../lib/types";

const authState = vi.hoisted(() => ({ user: { role: "viewer" } as { role: string; role_bindings?: unknown } }));
vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

import { RuntimeEnvironmentsPage } from "./RuntimeEnvironmentsPage";

const environment: RuntimeEnvironmentRecord = {
  id: "environment-1",
  node_id: "node-1",
  node_name: "维护节点",
  node_status: "online",
  agent_supported: true,
  system_dependencies: { status: "ready" },
  steamcmd_source: "managed",
  steamcmd_status: "ready",
  created_at: "2026-08-09T00:00:00Z",
  updated_at: "2026-08-09T00:00:00Z",
};

const installation: GameInstallationRecord = {
  id: "installation-1",
  environment_id: environment.id,
  node_id: environment.node_id,
  path: "D:\\Servers\\l4d2",
  app_id: 222860,
  status: "ready",
  created_at: "2026-08-09T00:00:00Z",
  updated_at: "2026-08-09T00:00:00Z",
};

describe("运行环境游戏安装维护", () => {
  afterEach(() => vi.restoreAllMocks());

  it("environments.manage 用户可以提交游戏安装重装", async () => {
    authState.user = {
      role: "admin",
      role_bindings: [{
        role: "admin",
        scope_type: "node",
        node_id: environment.node_id,
        permissions: ["nodes.read", "environments.read", "environments.manage"],
      }],
    };
    vi.spyOn(api, "environments").mockResolvedValue({ items: [environment] });
    vi.spyOn(api, "gameInstallations").mockResolvedValue({ items: [installation] });
    const reinstall = vi.spyOn(api, "gameInstallationAction").mockResolvedValue({ job_id: "job-reinstall" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><RuntimeEnvironmentsPage /></ToastProvider></QueryClientProvider>);

    const button = await screen.findByRole("button", { name: "重装" });
    expect(button).toBeEnabled();
    fireEvent.click(button);
    await waitFor(() => expect(reinstall).toHaveBeenCalledWith(environment.id, installation.id, "reinstall", undefined));
  });
});
