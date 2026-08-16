import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { Job, TerminalOutcomeConflict } from "../lib/types";

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
  requested_by_display_name: "服务器管理员",
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
  beforeEach(() => {
    authState.user = { role: "owner" };
    vi.spyOn(api, "terminalOutcomeConflicts").mockResolvedValue({ items: [] });
  });
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

    fireEvent.click(screen.getByRole("button", { name: /重启实例/ }));
    expect(screen.getByText("服务器管理员")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("任务分类"), { target: { value: "background" } });
    expect(await screen.findByText("查询服务器状态", { exact: true })).toBeInTheDocument();
    expect(screen.queryByText("重启实例", { exact: true })).not.toBeInTheDocument();
    await waitFor(() => expect(jobList).toHaveBeenCalledWith({ background: "only", backgroundSummary: true }));
  });

  it("插件安装任务在正文隐藏时仍显示为安装插件", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "jobList").mockResolvedValue({ items: [{
      ...operation,
      id: "plugin-install-job",
      type: "install_mod",
      display_type: "install_plugin",
      payload: undefined,
    }] });

    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><JobsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);

    expect(await screen.findByText("安装插件", { exact: true })).toBeInTheDocument();
    expect(screen.queryByText("安装模组", { exact: true })).not.toBeInTheDocument();
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

  it("将地图导入的结构化失败显示为阶段、原因和处理建议", async () => {
    const failedImport: Job = {
      ...operation,
      id: "map-import-failed",
      type: "map.import.qq_flash",
      status: "failed",
      message: "地图归档处理失败",
      result: { failure: {
        stage: "archive",
        code: "map_archive_path_encoding_invalid",
        summary: "ZIP 内部路径编码无法识别",
        remediation: "在导入窗口改选 UTF-8、GBK 或 Big5。",
        retryable: false,
        source: "广州地图.zip",
      } },
    };
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "jobList").mockResolvedValue({ items: [failedImport] });

    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><JobsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: /导入地图/ }));

    expect(screen.getByText(/失败阶段 · 归档 · map_archive_path_encoding_invalid/)).toBeInTheDocument();
    expect(screen.getByText("ZIP 内部路径编码无法识别")).toBeInTheDocument();
    expect(screen.getByText("在导入窗口改选 UTF-8、GBK 或 Big5。")).toBeInTheDocument();
    expect(screen.getByText("广州地图.zip")).toBeInTheDocument();
  });

  it("没有任务读取权限时不请求任务列表", async () => {
    authState.user = { role: "viewer", role_bindings: [] };
    const jobList = vi.spyOn(api, "jobList").mockResolvedValue({ items: [operation] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><JobsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);
    expect(await screen.findByText("任务清单已隐藏")).toBeInTheDocument();
    expect(jobList).not.toHaveBeenCalled();
  });

  it("全局所有者可确认冲突并保留中心端结果", async () => {
    const conflict: TerminalOutcomeConflict = {
      job_id: "de13ef3b-84e0-455a-8d08-f7722e665903",
      node_id: "node-1",
      instance_id: "instance-1",
      stored_status: "succeeded",
      reported_status: "failed",
      reported_fingerprint: "a".repeat(64),
      reason: "outcome_mismatch",
      occurrences: 4,
      first_seen_at: "2026-08-12T11:25:00Z",
      last_seen_at: "2026-08-12T11:45:00Z",
    };
    vi.mocked(api.terminalOutcomeConflicts).mockResolvedValue({ items: [conflict] });
    vi.spyOn(api, "jobList").mockResolvedValue({ items: [operation] });
    const resolve = vi.spyOn(api, "resolveTerminalOutcomeConflict").mockResolvedValue({
      conflict: { ...conflict, resolution: "keep_control" },
      idempotent_replay: false,
      acknowledgement_delivered: true,
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><JobsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);

    expect(await screen.findByText("检测到任务最终结果冲突")).toBeInTheDocument();
    expect(screen.getByText(/中心端：成功 · Agent：失败/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "保留中心端结果并确认" }));
    await waitFor(() => expect(resolve).toHaveBeenCalledWith(conflict.job_id, conflict.reported_fingerprint));
  });
});
