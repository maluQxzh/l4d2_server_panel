import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { Job } from "../lib/types";
import { JobsPage } from "./JobsPage";

const operation: Job = {
  id: "operation-job",
  node_id: "node-1",
  instance_id: "instance-1",
  type: "restart",
  status: "succeeded",
  progress: 100,
  message: "operation completed",
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
});
