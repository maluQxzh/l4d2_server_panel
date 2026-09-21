import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api, ApiError, type PreparedPackage } from "../lib/api";
import { ToastProvider } from "../context/ToastContext";
import { PreparedPackages } from "./PreparedPackages";

const pack = (id: string): PreparedPackage => ({ id, name: `${id}.vpk`, kind: "vpk", status: "succeeded", progress: 100, ready: true });
const wrap = () => render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}><ToastProvider><PreparedPackages instanceId="instance" plugins={false} /></ToastProvider></QueryClientProvider>);
afterEach(() => vi.restoreAllMocks());

it("官方组件展示固定版本且无需第三方风险确认", async () => {
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, official_extensions_supported: true, items: [{ ...pack("official"), name: "meta.zip", kind: "metamod", official: true, version: "1.12" }] });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
  const queue = vi.spyOn(api, "queuePreparedInstalls").mockResolvedValue({ job_id: "batch" });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ToastProvider><PreparedPackages instanceId="instance" plugins /></ToastProvider></QueryClientProvider>);
  await screen.findByText("meta.zip（官方 1.12） · 可安装");
  fireEvent.click(screen.getByRole("button", { name: "安装 meta.zip" }));
  await waitFor(() => expect(queue).toHaveBeenCalledWith("instance", ["official"], false, expect.any(String)));
  expect(confirm).toHaveBeenCalledTimes(1);
});

it("组件整包必须再次确认原生风险才提交持久队列", async () => {
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, extensions_supported: true, items: [{ ...pack("extension"), kind: "metamod", name: "meta.zip" }] });
  const confirm = vi.spyOn(window, "confirm").mockReturnValueOnce(true).mockReturnValueOnce(false);
  const queue = vi.spyOn(api, "queuePreparedInstalls").mockResolvedValue({ job_id: "batch" });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ToastProvider><PreparedPackages instanceId="instance" plugins /></ToastProvider></QueryClientProvider>);
  fireEvent.click(await screen.findByRole("button", { name: "安装 meta.zip" }));
  expect(confirm).toHaveBeenCalledTimes(2);
  expect(queue).not.toHaveBeenCalled();
  confirm.mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "安装 meta.zip" }));
  await waitFor(() => expect(queue).toHaveBeenCalledWith("instance", ["extension"], true, expect.any(String)));
});

it("VPK 文件包在模组列表展示文件数并可与单文件一起排队", async () => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, vpk_bundles_supported: true, items: [pack("one"), { ...pack("bundle"), kind: "qq_vpk_bundle", name: "QQ 闪传 VPK 文件包", file_count: 3 }] });
  const install = vi.spyOn(api, "queuePreparedInstalls").mockResolvedValue({ job_id: "batch" });
  wrap();
  expect(await screen.findByText("QQ 闪传 VPK 文件包（3 个 VPK） · 可安装")).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "依次安装就绪包（2）" }));
  await waitFor(() => expect(install).toHaveBeenCalledWith("instance", ["one", "bundle"], false, expect.any(String)));
});

it("一次提交完整清单，离开页面不取消服务端队列", async () => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  const items = [pack("one"), pack("two")];
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, items });
  const install = vi.spyOn(api, "queuePreparedInstalls").mockResolvedValue({ job_id: "batch" });
  const stop = vi.spyOn(api, "stopPreparedInstallQueue");
  const view = wrap();
  fireEvent.click(await screen.findByRole("button", { name: "依次安装就绪包（2）" }));
  await screen.findByText("安装队列已保存，关闭页面后仍会继续");
  expect(install).toHaveBeenCalledTimes(1);
  expect(install).toHaveBeenCalledWith("instance", ["one", "two"], false, expect.any(String));
  view.unmount();
  expect(stop).not.toHaveBeenCalled();
});

it("刷新恢复服务端等待和停止入口，无需任务列表权限", async () => {
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, items: [{ ...pack("one"), ready: false, queue_id: "batch" }], queues: [{ id: "batch", status: "queued", progress: 0, message: "" }] });
  const stop = vi.spyOn(api, "stopPreparedInstallQueue").mockResolvedValue({});
  wrap();
  expect(await screen.findByText("one.vpk · 等待安装")).toBeVisible();
  expect(screen.getByRole("button", { name: "安装 one.vpk" })).toBeDisabled();
  expect(screen.queryByRole("button", { name: "丢弃 one.vpk" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "停止后续安装" }));
  await waitFor(() => expect(stop).toHaveBeenCalledWith("instance", "batch"));
});

it("提交响应丢失后重试使用同一幂等键", async () => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, items: [pack("one")] });
  const install = vi.spyOn(api, "queuePreparedInstalls").mockRejectedValueOnce(new Error("网络中断")).mockResolvedValue({ job_id: "batch" });
  wrap();
  fireEvent.click(await screen.findByRole("button", { name: "依次安装就绪包（1）" }));
  await screen.findByText("网络中断");
  fireEvent.click(screen.getByRole("button", { name: "依次安装就绪包（1）" }));
  await screen.findByText("安装队列已保存，关闭页面后仍会继续");
  expect(install.mock.calls[0][3]).toBe(install.mock.calls[1][3]);
  // Once accepted, a later install of released waiting items is a new intent.
  fireEvent.click(screen.getByRole("button", { name: "依次安装就绪包（1）" }));
  await waitFor(() => expect(install).toHaveBeenCalledTimes(3));
  expect(install.mock.calls[2][3]).not.toBe(install.mock.calls[1][3]);
});

it("丢弃就绪包无需安装且失败显示提示", async () => {
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, items: [pack("one")] });
  const discard = vi.spyOn(api, "discardPreparedPackage").mockRejectedValue(new ApiError("权限已撤销", 403));
  wrap();
  fireEvent.click(await screen.findByRole("button", { name: "丢弃 one.vpk" }));
  await waitFor(() => expect(discard).toHaveBeenCalledWith("instance", "one"));
  await screen.findByText("权限已撤销");
});

it("QQ 分享未解析名称时显示准备占位且禁止安装", async () => {
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, qq_flash_supported: true, items: [{ ...pack("one"), name: "", status: "running", ready: false, progress: 10 }] });
  wrap();
  expect(await screen.findByRole("button", { name: "安装 QQ 闪传单文件" })).toBeDisabled();
  expect(screen.getByText("QQ 闪传单文件 · 准备中 10%")).toBeVisible();
});


it("Workshop 准备显示在模组列表并加入持久安装队列", async () => {
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, workshop_supported: true, items: [{ ...pack("workshop"), kind: "workshop", name: "Workshop 文件包", file_count: 2 }] });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  const queue = vi.spyOn(api, "queuePreparedInstalls").mockResolvedValue({ job_id: "queue" });
  wrap();
  await screen.findByText("Workshop 文件包（2 个 VPK） · 可安装");
  fireEvent.click(screen.getByRole("button", { name: "安装 Workshop 文件包" }));
  await waitFor(() => expect(queue).toHaveBeenCalledWith("instance", ["workshop"], false, expect.any(String)));
});
