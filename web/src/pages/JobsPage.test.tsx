import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { Job } from "../lib/types";

const authState = vi.hoisted(() => ({ user: { role: "owner" } as { role: string; role_bindings?: unknown } }));

vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

import { JobsPage } from "./JobsPage";

const operation: Job = {
  id: "operation-job",
  node_id: "node-1",
  instance_id: "instance-1",
  type: "restart",
  status: "succeeded",
  progress: 100,
  message: "operation completed",
  payload: { map: "c2m1_highway" },
  result: { message: "ok" },
  requested_by: "owner",
  created_at: "2026-08-07T14:00:00Z",
};

const probe: Job = {
  id: "probe-job",
  node_id: "node-1",
  instance_id: "instance-1",
  type: "a2s.info",
  status: "succeeded",
  progress: 100,
  message: "operation completed",
  created_at: "2026-08-07T14:01:00Z",
};

describe("任务中心", () => {
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => vi.restoreAllMocks());

  it("默认展示重要任务并将后台探测放入独立视图", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const jobList = vi.spyOn(api, "jobList").mockImplementation(async (options = {}) => {
      const { background } = options;
      return {
        items: background === "only" ? [probe] : [operation],
        background_summary: { targets: 1, active: 0, failed: 0, latest_at: probe.created_at },
      };
    });

    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <JobsPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByText("重启实例", { exact: true })).toBeInTheDocument();
    expect(screen.queryByText("查询服务器状态", { exact: true })).not.toBeInTheDocument();
    expect(screen.getByText("1 个目标")).toBeInTheDocument();
    expect(jobList).toHaveBeenCalledWith({ background: "exclude", backgroundSummary: true });

    fireEvent.change(screen.getByLabelText("任务分类"), { target: { value: "background" } });
    expect(await screen.findByText("查询服务器状态", { exact: true })).toBeInTheDocument();
    expect(screen.queryByText("重启实例", { exact: true })).not.toBeInTheDocument();
    await waitFor(() => expect(jobList).toHaveBeenCalledWith({ background: "only", backgroundSummary: true }));
  });

  it("取消按钮严格跟随任务所属实例范围", async () => {
    authState.user = {
      role: "operator",
      role_bindings: [{
        role: "operator",
        scope_type: "instance",
        instance_id: "different-instance",
        permissions: ["instances.read", "jobs.read", "jobs.cancel"],
      }],
    };
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "jobList").mockResolvedValue({ items: [{ ...operation, status: "running", progress: 30 }] });

    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><JobsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: /重启实例/ }));
    expect(screen.queryByRole("button", { name: "取消任务" })).not.toBeInTheDocument();
    expect(screen.queryByText("任务参数")).not.toBeInTheDocument();
    expect(screen.queryByText('{"map":"c2m1_highway"}')).not.toBeInTheDocument();
  });

  it("仅在任务详情权限匹配所属实例时显示脱敏正文和结果", async () => {
    authState.user = {
      role: "operator",
      role_bindings: [{
        role: "operator",
        scope_type: "instance",
        instance_id: "instance-1",
        permissions: ["jobs.read", "jobs.details"],
      }],
    };
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "jobList").mockResolvedValue({ items: [operation] });

    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><JobsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: /重启实例/ }));
    expect(screen.getByText("任务参数")).toBeInTheDocument();
    expect(screen.getByText('{"map":"c2m1_highway"}')).toBeInTheDocument();
    expect(screen.getByText('{"message":"ok"}')).toBeInTheDocument();
  });

  it("没有任务读取权限时不请求任务列表", async () => {
    authState.user = { role: "viewer", role_bindings: [] };
    const jobList = vi.spyOn(api, "jobList").mockResolvedValue({ items: [operation] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><JobsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);
    expect(await screen.findByText("任务清单已隐藏")).toBeInTheDocument();
    expect(jobList).not.toHaveBeenCalled();
  });
});
