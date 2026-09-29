import { describe, expect, it } from "vitest";
import { instanceAddress, instancePlayers } from "./instance-display";

describe("实例连接地址与实时人数", () => {
  it("优先公开地址，正确格式化 IPv6，并隐藏通配监听地址", () => {
    expect(instanceAddress({ host: "0.0.0.0", port: 29999, node_public_address: "nc.dearxzh.asia" })).toBe("nc.dearxzh.asia:29999");
    expect(instanceAddress({ host: "192.0.2.1", port: 27015 })).toBe("192.0.2.1:27015");
    expect(instanceAddress({ host: "::", port: 27015, node_public_address: "2001:db8::1" })).toBe("[2001:db8::1]:27015");
    expect(instanceAddress({ host: "[2001:db8::1]", port: 27015 })).toBe("[2001:db8::1]:27015");
    expect(instanceAddress({ host: "0.0.0.0", port: 27015 })).toBe("未设置连接地址");
  });
  it("停止和启动中的实例不会显示缓存中的旧人数", () => {
    expect(instancePlayers({ status: "stopped", players: 5 })).toBe(0);
    expect(instancePlayers({ status: "starting", players: 5 })).toBe(0);
    expect(instancePlayers({ status: "running", players: 5 })).toBe(5);
  });
});
