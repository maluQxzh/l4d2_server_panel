import { afterEach, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { submitWhenInstanceReady } from "./instanceBusy";

afterEach(() => vi.useRealTimers());
const busy = () => new ApiError("实例被任务 occupied 占用", 409, "conflict", "instance_busy", "occupied");

it("仅对写锁冲突退避重试，解除占用后返回提交结果", async () => {
  vi.useFakeTimers();
  const submit = vi.fn().mockRejectedValueOnce(busy()).mockRejectedValueOnce(busy()).mockResolvedValue({ job_id: "next" });
  const waiting = vi.fn();
  const result = submitWhenInstanceReady(submit, () => false, waiting);
  await vi.advanceTimersByTimeAsync(749);
  expect(submit).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(submit).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(1500);
  await expect(result).resolves.toEqual({ job_id: "next" });
  expect(waiting).toHaveBeenCalledWith(expect.stringContaining("occupied"));
});

it.each([new ApiError("版本冲突", 409, "conflict"), new ApiError("没有权限", 403), new ApiError("失败", 500), new TypeError("网络中断")])("不重试语义冲突、权限或不确定的失败：%s", async (error) => {
  const submit = vi.fn().mockRejectedValue(error);
  await expect(submitWhenInstanceReady(submit, () => false, vi.fn())).rejects.toBe(error);
  expect(submit).toHaveBeenCalledTimes(1);
});

it("等待期间停止或离开页面后不再提交", async () => {
  vi.useFakeTimers();
  let stopped = false;
  const submit = vi.fn().mockRejectedValue(busy());
  const result = expect(submitWhenInstanceReady(submit, () => stopped, vi.fn())).rejects.toThrow("尚未提交的操作不会执行");
  await vi.advanceTimersByTimeAsync(0);
  stopped = true;
  await vi.advanceTimersByTimeAsync(750);
  await result;
  expect(submit).toHaveBeenCalledTimes(1);
});

it("停止后才返回的占用响应不会覆盖新页面的等待提示", async () => {
  let stopped = false;
  let reject!: (error: Error) => void;
  const waiting = vi.fn();
  const result = expect(submitWhenInstanceReady(() => new Promise((_, fail) => { reject = fail; }), () => stopped, waiting)).rejects.toThrow("尚未提交的操作不会执行");
  stopped = true;
  reject(busy());
  await result;
  expect(waiting).not.toHaveBeenCalled();
});

it("占用超过十分钟会停止而非无限重试", async () => {
  vi.useFakeTimers();
  const submit = vi.fn().mockRejectedValue(busy());
  const result = expect(submitWhenInstanceReady(submit, () => false, vi.fn())).rejects.toThrow("等待实例空闲超时");
  await vi.advanceTimersByTimeAsync(600_000);
  await result;
  expect(submit.mock.calls.length).toBeLessThan(205);
  const count = submit.mock.calls.length;
  await vi.advanceTimersByTimeAsync(60_000);
  expect(submit).toHaveBeenCalledTimes(count);
});
