import { describe, expect, it } from "vitest";
import { availableInstanceUpdates, fleetNetworkTotals } from "./dashboard";

describe("仪表盘汇总", () => {
  it("聚合节点最新网络累计计数并忽略非法负值", () => {
    expect(fleetNetworkTotals([
      { network_rx: 1024, network_tx: 2048 },
      { network_rx: 4096, network_tx: 8192 },
      { network_rx: -1, network_tx: Number.NaN },
    ])).toEqual({ rx: 5120, tx: 10240 });
  });

  it("只统计明确标记为可更新的实例", () => {
    expect(availableInstanceUpdates([
      { update_available: true },
      { update_available: false },
      { update_available: true },
    ])).toBe(2);
  });
});
