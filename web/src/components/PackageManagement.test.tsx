import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StrictMode, useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "../context/ToastContext";
import { api, ApiError } from "../lib/api";
import type { InstanceRecord, PackageRecord } from "../lib/types";
import { PackageBatchControls, RenamePackageModal } from "./PackageManagement";

const instance = { id: "instance", status: "stopped" } as InstanceRecord;
const pack = (id: string, kind = "vpk", enabled = true): PackageRecord => ({ id, name: id, kind, enabled, sha256: "a".repeat(64), path: `left4dead2/addons/${id}.vpk`, file_count: 1 } as PackageRecord);
function wrap(children: React.ReactNode) {
  return render(<StrictMode><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}><ToastProvider>{children}</ToastProvider></QueryClientProvider></StrictMode>);
}
function Batch({ items, current = instance, canToggle = true, canDelete = true }: { items: PackageRecord[]; current?: InstanceRecord; canToggle?: boolean; canDelete?: boolean }) {
  const [selected, setSelected] = useState(items.map((item) => item.id));
  const [busy, setBusy] = useState(false);
  return <PackageBatchControls instance={current} items={items} selected={selected} setSelected={setSelected} canToggle={canToggle} canDelete={canDelete} busy={busy} setBusy={setBusy} />;
}
afterEach(() => vi.restoreAllMocks());

describe("包批量操作", () => {
  it("实例占用时等待并复用幂等键，不跳过当前条目", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const action = vi.spyOn(api, "instanceAction")
      .mockRejectedValueOnce(new ApiError("实例被任务 blocking 占用", 409, "conflict", "instance_busy", "blocking"))
      .mockResolvedValue({ job_id: "done" });
    vi.spyOn(api, "packageOperationStatus").mockResolvedValue({ status: "succeeded", busy: false });
    wrap(<Batch items={[pack("one"), pack("two")]} />);
    fireEvent.click(screen.getByRole("button", { name: "批量停用" }));
    await screen.findByText(/等待实例空闲/);
    expect(action).toHaveBeenCalledTimes(1);
    await screen.findByText(/批量操作结束：成功 2 项/, {}, { timeout: 3000 });
    expect(action.mock.calls.map((call) => call[2]?.package_id)).toEqual(["one", "one", "two"]);
    expect(action.mock.calls[0][3]).toBeTruthy();
    expect(action.mock.calls[0][3]).toBe(action.mock.calls[1][3]);
    expect(action.mock.calls[1][3]).not.toBe(action.mock.calls[2][3]);
  });

  it("停止等待后不提交当前条目或后续条目", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const action = vi.spyOn(api, "instanceAction").mockRejectedValue(new ApiError("忙", 409, "conflict", "instance_busy"));
    wrap(<Batch items={[pack("one"), pack("two")]} />);
    fireEvent.click(screen.getByRole("button", { name: "批量停用" }));
    await screen.findByText(/等待实例空闲/);
    fireEvent.click(screen.getByRole("button", { name: "停止后续操作" }));
    await screen.findByText("已停止后续操作；已提交的任务请到任务中心查看", {}, { timeout: 3000 });
    expect(action).toHaveBeenCalledTimes(1);
  });

  it("离开页面后停止等待，不补交操作", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const action = vi.spyOn(api, "instanceAction").mockRejectedValue(new ApiError("忙", 409, "conflict", "instance_busy"));
    const view = wrap(<Batch items={[pack("one"), pack("two")]} />);
    fireEvent.click(screen.getByRole("button", { name: "批量停用" }));
    await screen.findByText(/等待实例空闲/);
    view.unmount();
    await new Promise((resolve) => setTimeout(resolve, 800));
    expect(action).toHaveBeenCalledTimes(1);
  });

  it("复用页面切换实例后，旧批次不会继续或覆盖新页面状态", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const action = vi.spyOn(api, "instanceAction").mockRejectedValue(new ApiError("忙", 409, "conflict", "instance_busy"));
    function SwitchInstance() {
      const [id, setID] = useState("first");
      return <><button onClick={() => setID("second")}>切换实例</button><Batch current={{ ...instance, id }} items={[pack("one")]} /></>;
    }
    wrap(<SwitchInstance />);
    fireEvent.click(screen.getByRole("button", { name: "批量停用" }));
    await screen.findByText(/等待实例空闲/);
    fireEvent.click(screen.getByRole("button", { name: "切换实例" }));
    await new Promise((resolve) => setTimeout(resolve, 800));
    expect(action).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/等待实例空闲/)).not.toBeInTheDocument();
    expect(screen.queryByText(/已停止后续操作/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "批量停用" })).toBeEnabled();
  });

  it("等待前一项完成投影后才提交下一项，并保留失败条目", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const action = vi.spyOn(api, "instanceAction").mockResolvedValueOnce({ job_id: "job-1" }).mockResolvedValueOnce({ job_id: "job-2" });
    let complete!: (value: { status: "succeeded"; busy: boolean }) => void;
    vi.spyOn(api, "packageOperationStatus").mockImplementationOnce(() => new Promise((resolve) => { complete = resolve; })).mockResolvedValue({ status: "failed", busy: false });
    wrap(<Batch items={[pack("one"), pack("two")]} />);
    fireEvent.click(screen.getByRole("button", { name: "批量停用" }));
    await waitFor(() => expect(action).toHaveBeenCalledTimes(1));
    expect(screen.getByRole("button", { name: "批量删除" })).toBeDisabled();
    complete({ status: "succeeded", busy: false });
    await screen.findByText("one：停用成功");
    await screen.findByText(/two：任务失败/);
    expect(action.mock.calls.map((call) => call[2]?.package_id)).toEqual(["one", "two"]);
    expect(screen.getByText("已选 1 项")).toBeVisible();
  });

  it("删除顺序满足智能插件、SourceMod、Metamod 的依赖", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const remove = vi.spyOn(api, "deletePackage").mockResolvedValue({ job_id: "job" });
    vi.spyOn(api, "packageOperationStatus").mockResolvedValue({ status: "succeeded", busy: false });
    wrap(<Batch items={[pack("meta", "metamod"), pack("sm", "sourcemod"), pack("plugin", "smart")]} />);
    fireEvent.click(screen.getByRole("button", { name: "批量删除" }));
    await screen.findByText(/批量操作结束：成功 3 项/);
    expect(remove.mock.calls.map((call) => call[1])).toEqual(["plugin", "sm", "meta"]);
  });

  it("继续处理被明确拒绝的条目，轮询结果不确定时停止后续提交", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const remove = vi.spyOn(api, "deletePackage").mockRejectedValueOnce(new ApiError("缺少依赖", 409)).mockResolvedValue({ job_id: "uncertain" });
    vi.spyOn(api, "packageOperationStatus").mockRejectedValue(new Error("连接断开"));
    wrap(<Batch items={[pack("one"), pack("two"), pack("three")]} />);
    fireEvent.click(screen.getByRole("button", { name: "批量删除" }));
    await screen.findByText("two：连接断开");
    expect(remove).toHaveBeenCalledTimes(2);
    expect(screen.getByText("one：缺少依赖")).toBeVisible();
    expect(screen.getByText("已选 3 项")).toBeVisible();
  });

  it("运行中不能批量停用；启用智能插件保留两次运行风险确认", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValueOnce(true).mockReturnValueOnce(true).mockReturnValueOnce(false);
    const action = vi.spyOn(api, "instanceAction").mockResolvedValue({ job_id: "job" });
    wrap(<Batch current={{ ...instance, status: "running" }} items={[pack("enabled"), pack("smart", "smart", false)]} />);
    expect(screen.getByRole("button", { name: "批量停用" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "批量启用" }));
    expect(confirm).toHaveBeenCalledTimes(3);
    expect(action).not.toHaveBeenCalled();
  });

  it("遵守独立开关和删除权限，并支持取消全选", () => {
    wrap(<Batch items={[pack("one"), pack("two")]} canToggle={false} />);
    expect(screen.queryByRole("button", { name: "批量启用" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("全选当前结果"));
    expect(screen.getByText("已选 0 项")).toBeVisible();
    expect(screen.getByRole("button", { name: "批量删除" })).toBeDisabled();
  });
});

describe("重命名", () => {
  it("提交显示名称与文件名，成功后刷新并关闭", async () => {
    const rename = vi.spyOn(api, "renamePackage").mockResolvedValue({ job_id: "rename" });
    vi.spyOn(api, "packageOperationStatus").mockResolvedValue({ status: "succeeded", busy: false });
    const close = vi.fn();
    wrap(<RenamePackageModal instance={instance} item={pack("old")} onClose={close} />);
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "我的模组" } });
    fireEvent.change(screen.getByLabelText("文件名"), { target: { value: "my-mod.vpk" } });
    fireEvent.click(screen.getByRole("button", { name: "保存名称" }));
    await waitFor(() => expect(close).toHaveBeenCalled());
    expect(rename).toHaveBeenCalledWith("instance", "old", { name: "我的模组", filename: "my-mod.vpk" });
  });

  it("整包插件只改显示名称；失败保留表单和错误", async () => {
    const rename = vi.spyOn(api, "renamePackage").mockRejectedValue(new ApiError("实例忙", 409));
    const close = vi.fn();
    wrap(<RenamePackageModal instance={instance} item={pack("smart", "smart")} onClose={close} />);
    expect(screen.queryByLabelText("文件名")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "保存名称" }));
    await screen.findByRole("alert");
    expect(rename).toHaveBeenCalledWith("instance", "smart", { name: "smart" });
    expect(close).not.toHaveBeenCalled();
  });
});
