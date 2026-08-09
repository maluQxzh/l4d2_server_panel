import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { NodeRecord } from "../lib/types";

const authState = vi.hoisted(() => ({ user: { role: "owner" } as { role: string; role_bindings?: unknown } }));
vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

import { NodesPage } from "./NodesPage";

const node: NodeRecord = {
  id: "87654321-4321-4321-4321-cba987654321",
  name: "Windows 节点",
  status: "online",
  os: "windows",
  arch: "amd64",
  agent_version: "1.0.0",
  cpu_percent: 12,
  memory_used: 1024,
  memory_total: 4096,
  disk_used: 2048,
  disk_total: 8192,
  network_rx: 0,
  network_tx: 0,
  created_at: "2026-08-09T00:00:00Z",
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><NodesPage /></RealtimeProvider></ToastProvider></QueryClientProvider>);
}

describe("节点发现权限", () => {
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => vi.restoreAllMocks());

  it("有节点发现权限时可以扫描并查看结果", async () => {
    authState.user = {
      role: "admin",
      role_bindings: [{ role: "admin", scope_type: "node", node_id: node.id, permissions: ["nodes.read", "nodes.discover"] }],
    };
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [node] });
    const discover = vi.spyOn(api, "discoverNodeInstances").mockResolvedValue({
      candidates: [{ root_index: 0, path: "D:\\Servers\\l4d2", name: "l4d2", executable: "srcds.exe", has_gameinfo: true, has_appmanifest: true, managed: false, likely_running: false, evidence: ["srcds.exe"] }],
      scanned_directories: 3,
      skipped_links: 1,
      truncated: false,
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "管理 Windows 节点" }));
    fireEvent.click(screen.getByRole("button", { name: "扫描安装" }));
    await waitFor(() => expect(discover).toHaveBeenCalledWith(node.id));
    expect(await screen.findByText("D:\\Servers\\l4d2", { exact: false })).toBeInTheDocument();
  });

  it("没有节点发现权限时隐藏扫描入口", async () => {
    authState.user = {
      role: "viewer",
      role_bindings: [{ role: "viewer", scope_type: "node", node_id: node.id, permissions: ["nodes.read"] }],
    };
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [node] });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "管理 Windows 节点" }));
    expect(screen.queryByRole("button", { name: "扫描安装" })).not.toBeInTheDocument();
  });

  it("只有接入权限时不读取节点列表并保留接入入口", async () => {
    authState.user = {
      role: "viewer",
      role_bindings: [{ role: "viewer", scope_type: "global", permissions: ["nodes.enroll"] }],
    };
    const nodes = vi.spyOn(api, "nodes").mockResolvedValue({ items: [node] });
    renderPage();

    expect(await screen.findByText("可接入新节点")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "接入节点" }).length).toBeGreaterThan(0);
    expect(nodes).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "刷新" })).not.toBeInTheDocument();
  });
});
