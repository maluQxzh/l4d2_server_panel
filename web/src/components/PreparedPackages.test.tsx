import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api, ApiError, type PreparedPackage } from "../lib/api";
import { ToastProvider } from "../context/ToastContext";
import { PreparedPackages } from "./PreparedPackages";

const pack = (id: string): PreparedPackage => ({ id, name: `${id}.vpk`, kind: "vpk", status: "succeeded", progress: 100, ready: true });
const wrap = () => render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}><ToastProvider><PreparedPackages instanceId="instance" plugins={false} /></ToastProvider></QueryClientProvider>);
afterEach(() => vi.restoreAllMocks());

it("刷新后的准备记录可以依次安装，写锁冲突复用幂等键", async () => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  const items = [pack("one"), pack("two")];
  vi.spyOn(api, "preparedPackages").mockImplementation(async () => ({ supported: true, items }));
  const install = vi.spyOn(api, "installPreparedPackage")
    .mockRejectedValueOnce(new ApiError("busy", 409, "conflict", "instance_busy"))
    .mockImplementation(async (_instance, id) => {
      const item = items.find((p) => p.id === id)!;
      item.ready = false; item.consume_job_id = `job-${id}`; item.consume_status = "succeeded"; item.busy = false;
      return { job_id: `job-${id}` };
    });
  wrap();
  fireEvent.click(await screen.findByRole("button", { name: "依次安装就绪包（2）" }));
  await screen.findByText(/等待实例空闲/);
  await screen.findByText("所选预下载包已安装完成", {}, { timeout: 3000 });
  expect(install.mock.calls.map((call) => call[1])).toEqual(["one", "one", "two"]);
  expect(install.mock.calls[0][3]).toBe(install.mock.calls[1][3]);
  expect(install.mock.calls[1][3]).not.toBe(install.mock.calls[2][3]);
});

it("结果未同步时不提交下一包，离开页面终止后续安装", async () => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  const items = [pack("one"), pack("two")];
  vi.spyOn(api, "preparedPackages").mockResolvedValue({ supported: true, items });
  const install = vi.spyOn(api, "installPreparedPackage").mockImplementation(async () => {
    items[0].consume_job_id = "job"; items[0].consume_status = "succeeded"; items[0].busy = true;
    return { job_id: "job" };
  });
  const view = wrap();
  fireEvent.click(await screen.findByRole("button", { name: "依次安装就绪包（2）" }));
  await screen.findByText(/正在安装 one.vpk/);
  expect(install).toHaveBeenCalledTimes(1);
  view.unmount();
  await new Promise((resolve) => setTimeout(resolve, 1050));
  expect(install).toHaveBeenCalledTimes(1);
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
