import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { acknowledgeMapImport, mapImportTrackingTTL, maxTrackedMapImports, pendingMapImports, trackMapImport } from "./map-import-tracker";
beforeEach(() => { localStorage.clear(); vi.useFakeTimers(); vi.setSystemTime(new Date("2026-11-01T00:00:00Z")); });
afterEach(() => vi.useRealTimers());

it("每账号最多保留 64 条，读取和重复追踪不延长有效期", () => {
  for (let i = 0; i < 100; i++) trackMapImport("bounded", `job-${i}`);
  expect(pendingMapImports("bounded")).toHaveLength(maxTrackedMapImports);
  expect(pendingMapImports("bounded")[0]).toBe("job-36");
  vi.advanceTimersByTime(mapImportTrackingTTL - 1);
  trackMapImport("bounded", "job-99");
  expect(pendingMapImports("bounded")).toHaveLength(64);
  vi.advanceTimersByTime(1);
  expect(pendingMapImports("bounded")).toEqual([]);
});

it("旧字符串记录一次迁移，拒绝畸形记录并按账号隔离", () => {
  localStorage.setItem("l4d2-map-imports:legacy", JSON.stringify(["old", "old", 3, { id: "future", trackedAt: Date.now() + 1 }]));
  expect(pendingMapImports("legacy")).toEqual(["old"]);
  expect(JSON.parse(localStorage.getItem("l4d2-map-imports:legacy")!)[0].trackedAt).toBe(Date.now());
  trackMapImport("other", "other-job");
  acknowledgeMapImport("legacy", "old");
  expect(pendingMapImports("other")).toEqual(["other-job"]);
});
