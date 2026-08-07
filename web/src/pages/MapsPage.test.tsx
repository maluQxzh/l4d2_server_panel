import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { InstanceRecord, MapRecord } from "../lib/types";
import { MapsPage } from "./MapsPage";

const map: MapRecord = {
  id: "12345678-1234-1234-1234-123456789abc",
  node_id: "87654321-4321-4321-4321-cba987654321",
  name: "Claustrophobia",
  author: "Sean C. Wood",
  version: "1.2",
  description: "L4D2 version of Claustrophobia",
  health: "healthy",
  aggregate_sha256: "4241ce300a7bfdc6aabbccddeeff00112233445566778899aabbccddeeff0011",
  total_size: 188_743_680,
  source_type: "qq_flash",
  source_name: "l4d2_claustrophobia.zip",
  removed_files: 1_182,
  removed_bytes: 245_471_642,
  artifacts: [{ id: "artifact-1", ordinal: 1, size: 188_743_680, sha256: "a".repeat(64) }],
  bsps: [{ virtual_path: "maps/claustrophobia1.bsp", start_name: "claustrophobia1", dictionary_present: true }],
  modes: [{ name: "coop", chapters: [{ ordinal: 1, bsp_name: "claustrophobia1", display_name: "Onyx St. Sewer" }] }],
  assignments: [{
    map_id: "12345678-1234-1234-1234-123456789abc",
    instance_id: "instance-1",
    restart_required: true,
    created_at: "2026-08-07T13:46:00Z",
    updated_at: "2026-08-07T13:46:00Z",
  }],
  created_at: "2026-08-07T13:46:00Z",
  updated_at: "2026-08-07T13:46:00Z",
};

const assignedInstance = {
  id: "instance-1",
  node_id: map.node_id,
  name: "专家战役 #1",
  status: "running",
} as InstanceRecord;

describe("地图管理卡片", () => {
  it("使用三项总览并默认折叠次要地图信息", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "maps").mockResolvedValue({ items: [map] });
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [assignedInstance] });
    vi.spyOn(api, "jobs").mockResolvedValue({ items: [] });

    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <MapsPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("heading", { name: "Claustrophobia" })).toBeInTheDocument();
    expect(screen.getByText("逻辑地图")).toBeInTheDocument();
    expect(screen.getByText("地图占用空间")).toBeInTheDocument();
    expect(screen.getByText("实例分配", { selector: ".map-summary span" })).toBeInTheDocument();
    expect(screen.queryByText("处理后容量")).not.toBeInTheDocument();
    expect(screen.queryByText("已清理空间")).not.toBeInTheDocument();

    expect(screen.getByText("处理后的 VPK 大小")).toBeInTheDocument();
    expect(screen.queryByText("VPK 部件")).not.toBeInTheDocument();
    expect(screen.queryByText("已清理文件")).not.toBeInTheDocument();
    expect(screen.getByText("Sean C. Wood")).toBeInTheDocument();
    expect(screen.getByText("1.2")).toBeInTheDocument();
    expect(screen.getByText("合作")).toBeInTheDocument();
    expect(screen.getByText("已分配实例")).toBeInTheDocument();
    expect(screen.getByText("专家战役 #1")).toBeInTheDocument();
    expect(screen.getByText("等待重启生效")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "立即重启" })).toBeInTheDocument();

    const toggle = screen.getByRole("button", { name: "展开详情" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(screen.getAllByText("claustrophobia1.bsp")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "收起详情" })).toHaveAttribute("aria-expanded", "true");
  });
});
