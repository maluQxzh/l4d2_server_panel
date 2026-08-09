import { describe, expect, it } from "vitest";
import { summarizeRecentOperations } from "./recent-operations";
import type { Job } from "./types";

function job(id: string, type: string, status: Job["status"], message = "operation completed"): Job {
  return {
    id, node_id: "node", instance_id: "instance", type, status, progress: status === "succeeded" ? 100 : 0,
    message, created_at: `2026-08-09T00:00:0${id.length}Z`,
  };
}

describe("实例最近操作摘要", () => {
  it("隐藏诊断探测并折叠相同的成功操作", () => {
    const result = summarizeRecentOperations([
      job("probe-1", "a2s.info", "succeeded"),
      job("stop-2", "stop", "succeeded"),
      job("stop-1", "stop", "succeeded"),
      job("start-1", "start", "succeeded"),
    ]);

    expect(result.map(({ job: item, occurrences }) => [item.id, occurrences])).toEqual([
      ["stop-2", 2],
      ["start-1", 1],
    ]);
  });

  it("不会折叠执行中、失败或取消的任务", () => {
    const result = summarizeRecentOperations([
      job("running-2", "backup", "running"),
      job("running-1", "backup", "running"),
      job("failed-2", "backup", "failed", "disk full"),
      job("failed-1", "backup", "failed", "disk full"),
      job("canceled-1", "backup", "canceled"),
    ]);

    expect(result.map(({ job: item }) => item.id)).toEqual(["running-2", "running-1", "failed-2", "failed-1", "canceled-1"]);
  });
});
