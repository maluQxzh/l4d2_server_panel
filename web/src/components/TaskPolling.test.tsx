import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api } from "../lib/api";
import { refreshQueries } from "../lib/query-refresh";
import { usePreparedPackages } from "./PreparedPackages";
import { scanPollInterval } from "../lib/task-polling";

afterEach(() => { cleanup(); vi.useRealTimers(); });
it("空闲准备列表停止快速轮询，事件刷新后活动任务恢复快速轮询", async () => {
  vi.useFakeTimers();
  let active = false;
  const fetch = vi.spyOn(api, "preparedPackages").mockImplementation(async () => ({ supported: true, items: active ? [{ id: "a", name: "a", kind: "vpk", status: "running", progress: 0 }] : [] }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  function Probe() { usePreparedPackages("instance"); return null; }
  render(<QueryClientProvider client={client}><Probe /></QueryClientProvider>);
  await act(() => vi.advanceTimersByTimeAsync(10_000));
  expect(fetch).toHaveBeenCalledTimes(1);
  active = true;
  await act(async () => { await refreshQueries(client, { queryKey: ["instances", "instance", "prepared-packages"] }); });
  expect(fetch).toHaveBeenCalledTimes(2);
  await act(() => vi.advanceTimersByTimeAsync(3000));
  expect(fetch).toHaveBeenCalledTimes(3);
});
it("终态投影尚未完成的扫描仍轮询，完全完成后降频", () => {
  expect(scanPollInterval({ scan: { status: "succeeded", busy: true } })).toBe(3000);
  expect(scanPollInterval({ scan: { status: "succeeded", busy: false } })).toBe(30_000);
  expect(scanPollInterval({ scan: null })).toBe(30_000);
});
