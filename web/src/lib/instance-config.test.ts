import { describe, expect, it } from "vitest";
import { buildInstanceConfigUpdate } from "./instance-config";
import type { InstanceConfig } from "./types";

const config: InstanceConfig = {
  hostname: "专家战役",
  mode: "coop",
  difficulty: "normal",
  max_players: 8,
  start_map: "c1m1_hotel",
  region: "255",
  public_status: false,
  auto_restart: true,
};

describe("实例配置密码三态", () => {
  it("未编辑时不发送密码", () => {
    const payload = buildInstanceConfigUpdate(3, config, { kind: "unchanged" });
    expect(payload).not.toHaveProperty("server_password");
    expect(payload.config).not.toHaveProperty("server_password");
  });

  it("新密码只放在顶层字段", () => {
    const payload = buildInstanceConfigUpdate(3, config, { kind: "set", value: "new-secret" });
    expect(payload.server_password).toBe("new-secret");
    expect(payload.config).not.toHaveProperty("server_password");
  });

  it("显式清空会发送空字符串", () => {
    const payload = buildInstanceConfigUpdate(3, config, { kind: "clear" });
    expect(payload).toHaveProperty("server_password", "");
  });

  it("清除旧客户端遗留在 config 中的密码", () => {
    const legacy = { ...config, server_password: "must-not-leak" } as InstanceConfig;
    const payload = buildInstanceConfigUpdate(3, legacy, { kind: "unchanged" });
    expect(payload.config).not.toHaveProperty("server_password");
  });
});
