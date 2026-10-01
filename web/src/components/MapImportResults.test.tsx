import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "../lib/api";
import { pendingMapImports, trackMapImport } from "../lib/map-import-tracker";
import { MapImportResults } from "./MapImportResults";

function Location() {
  const location = useLocation();
  return <output aria-label="当前位置">{location.pathname}{location.search}</output>;
}

function mount(userId = "uploader") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><MapImportResults userId={userId} /><Location /></MemoryRouter></QueryClientProvider>);
}

describe("地图导入完成提示", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

  it("从持久任务恢复并直接显示重复地图、缺失字典和警告，确认后不重复提示", async () => {
    trackMapImport("uploader", "import-1");
    vi.spyOn(api, "mapImportResult").mockResolvedValue({ job_id: "import-1", status: "succeeded", ready: true, duplicate: true, name: "测试地图", missing_dictionaries: ["maps/test.bsp"], warnings: [{ code: "missing_stringtable_dictionary", message: "BSP 不包含字典" }] });
    const view = mount();
    expect(await screen.findByRole("dialog", { name: "地图已存在" })).toBeInTheDocument();
    expect(screen.getByText(/本次没有新增地图记录/)).toBeInTheDocument();
    expect(screen.getByText("maps/test.bsp")).toBeInTheDocument();
    expect(screen.getByText(/BSP 不包含字典/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "查看已有地图" })).not.toBeInTheDocument();
    view.unmount();
    const restored = mount();
    expect(await screen.findByRole("dialog", { name: "地图已存在" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "知道了" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(pendingMapImports("uploader")).toEqual([]);
    restored.unmount();
    mount();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("展示原地图上传信息并按 ID 跳转，跳转后确认提示", async () => {
    trackMapImport("uploader", "duplicate");
    const createdAt = "2025-08-07T13:46:00Z";
    vi.spyOn(api, "mapImportResult").mockResolvedValue({ job_id: "duplicate", status: "succeeded", ready: true, duplicate: true, name: "本次导入名称", existing_map: {
      id: "original-map", name: "原地图名称", created_by_display_name: "原上传者", created_at: createdAt,
      source_name: "original.zip", author: "地图作者甲", version: "1.2", total_size: 1024,
    } });
    mount();
    expect(await screen.findByText("原上传者")).toBeInTheDocument();
    expect(screen.getByText(new Date(createdAt).toLocaleString("zh-CN", { hour12: false }))).toBeInTheDocument();
    expect(screen.getByText("原地图名称")).toBeInTheDocument();
    expect(screen.queryByText("本次导入名称")).not.toBeInTheDocument();
    expect(screen.getByText("original.zip")).toBeInTheDocument();
    expect(screen.getByText("地图作者甲")).toBeInTheDocument();
    expect(screen.getByText("1.2")).toBeInTheDocument();
    expect(screen.getByText("1 KB")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "查看已有地图" }));
    expect(screen.getByLabelText("当前位置")).toHaveTextContent("/maps?map=original-map");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(pendingMapImports("uploader")).toEqual([]);
  });

  it("原上传账号已删除时显示未知账号", async () => {
    trackMapImport("uploader", "duplicate");
    vi.spyOn(api, "mapImportResult").mockResolvedValue({ job_id: "duplicate", status: "succeeded", ready: true, duplicate: true, existing_map: {
      id: "original-map", name: "地图", created_at: "2026-08-07T13:46:00Z", total_size: 1024,
    } });
    mount();
    expect(await screen.findByText("未知账号")).toBeInTheDocument();
  });

  it("全局监听新提交任务，待服务端投影完成后显示警告", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const fetch = vi.spyOn(api, "mapImportResult")
      .mockResolvedValueOnce({ job_id: "import-2", status: "succeeded", ready: false })
      .mockResolvedValue({ job_id: "import-2", status: "succeeded", ready: true, warnings: [{ code: "mission_warning", message: "章节信息缺失" }] });
    const view = mount();
    act(() => trackMapImport("uploader", "import-2"));
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await act(async () => vi.advanceTimersByTime(2_000));
    expect(await screen.findByRole("dialog", { name: "地图导入完成，但有检测警告" })).toBeInTheDocument();
    expect(screen.getByText(/章节信息缺失/)).toBeInTheDocument();
    view.unmount();
    vi.useRealTimers();
  });

  it("仅跟踪当前上传账号的任务，并依次提示失败与权限失效", async () => {
    trackMapImport("another-user", "other-import");
    trackMapImport("uploader", "failed-import");
    trackMapImport("uploader", "revoked-import");
    const fetch = vi.spyOn(api, "mapImportResult").mockImplementation(async (id) => {
      if (id === "revoked-import") throw new ApiError("forbidden", 403);
      return { job_id: id, status: "failed", ready: true };
    });
    mount();
    expect(await screen.findByRole("dialog", { name: "地图导入失败" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "知道了" }));
    expect(await screen.findByRole("dialog", { name: "无法读取地图导入结果" })).toBeInTheDocument();
    expect(fetch).not.toHaveBeenCalledWith("other-import");
  });

  it("处理中失去权限时不会把缓存的未完成状态误报为成功", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    trackMapImport("uploader", "revoked-while-running");
    const fetch = vi.spyOn(api, "mapImportResult")
      .mockResolvedValueOnce({ job_id: "revoked-while-running", status: "running", ready: false })
      .mockRejectedValue(new ApiError("forbidden", 403));
    const view = mount();
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    await act(async () => vi.advanceTimersByTime(2_000));
    expect(await screen.findByRole("dialog", { name: "无法读取地图导入结果" })).toBeInTheDocument();
    expect(screen.queryByText("地图导入完成")).not.toBeInTheDocument();
    view.unmount();
  });
});
