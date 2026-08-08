import { describe, expect, it } from "vitest";
import { buildInstanceConfigUpdate } from "./instance-config";
import type { InstanceConfig } from "./types";

const config: InstanceConfig = {
  hostname: "专家战役",
  mode: "coop",
  difficulty: "normal",
  max_players: 8,
  start_map: "c1m1_hotel",
  public_status: false,
  auto_restart: true,
};

describe("实例配置更新载荷", () => {
  it("未编辑时不发送密码", () => {
    const payload = buildInstanceConfigUpdate(3, config, { kind: "unchanged" });
    expect(payload).not.toHaveProperty("rcon_password");
    expect(payload.config).not.toHaveProperty("server_password");
  });

  it("新密码只放在顶层字段", () => {
    const payload = buildInstanceConfigUpdate(3, config, { kind: "set", value: "new-secret" });
    expect(payload.rcon_password).toBe("new-secret");
    expect(payload.config).not.toHaveProperty("server_password");
  });

  it("显式清空会发送空字符串", () => {
    const payload = buildInstanceConfigUpdate(3, config, { kind: "clear" });
    expect(payload).toHaveProperty("rcon_password", "");
  });

  it("清除旧客户端遗留在 config 中的连接密码和地区", () => {
    const legacy = { ...config, server_password: "must-not-leak", region: "255" } as InstanceConfig;
    const payload = buildInstanceConfigUpdate(3, legacy, { kind: "unchanged" });
    expect(payload.config).not.toHaveProperty("server_password");
    expect(payload.config).not.toHaveProperty("region");
  });

  it("仅在游戏端口发生变化时附带新端口", () => {
    const unchanged = buildInstanceConfigUpdate(3, config, { kind: "unchanged" });
    const changed = buildInstanceConfigUpdate(3, config, { kind: "unchanged" }, 27016);
    expect(unchanged).not.toHaveProperty("port");
    expect(changed.port).toBe(27016);
  });
});
