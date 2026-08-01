import { describe, expect, it } from "vitest";
import { buildStartupArgumentPreview, canAccessInstance, canAccessNode, lifecycleActionAllowed, maintenancePayload } from "./instance-lifecycle";
import type { InstanceRecord, User } from "./types";

const instance = {
  id: "instance-a", node_id: "node-a", port: 27015, tv_port: 27020,
  config: {
    hostname: "Campaign", mode: "coop", difficulty: "normal", max_players: 8,
    start_map: "c1m1_hotel", region: "255", public_status: false, auto_restart: true,
    extra_args: ["-secure"],
  },
} as InstanceRecord;

const scopedOperator = {
  id: "user-a", username: "operator", display_name: "Operator", role: "operator", totp_enabled: true, created_at: "2026-01-01T00:00:00Z",
  role_bindings: [{ role: "operator", scope_type: "instance", instance_id: "instance-a" }],
} as User;

describe("实例生命周期权限与状态", () => {
  it("按资源绑定而不是最高显示角色授权", () => {
    expect(canAccessInstance(scopedOperator, instance, "operate")).toBe(true);
    expect(canAccessInstance(scopedOperator, { id: "instance-b", node_id: "node-a" }, "operate")).toBe(false);
    expect(canAccessInstance(scopedOperator, instance, "manage")).toBe(false);
  });

  it("节点权限不被单实例绑定扩大", () => {
    expect(canAccessNode(scopedOperator, "node-a", "operate")).toBe(false);
    expect(canAccessNode({ ...scopedOperator, role_bindings: [{ role: "admin", scope_type: "node", node_id: "node-a" }] }, "node-a", "manage")).toBe(true);
  });

  it("稳定状态允许维护，过渡状态全部阻止写操作", () => {
    expect(lifecycleActionAllowed("update", "running", { operate: true, manage: false })).toBe(true);
    expect(lifecycleActionAllowed("reinstall", "stopped", { operate: true, manage: false })).toBe(false);
    expect(lifecycleActionAllowed("reinstall", "stopped", { operate: true, manage: true })).toBe(true);
    expect(lifecycleActionAllowed("update", "updating", { operate: true, manage: true })).toBe(false);
    expect(lifecycleActionAllowed("start", "running", { operate: true, manage: true })).toBe(false);
  });

  it("外部实例只能解除管理，不能隔离文件", () => {
    expect(lifecycleActionAllowed("detach", "external", { operate: true, manage: true })).toBe(true);
    expect(lifecycleActionAllowed("quarantine", "external", { operate: true, manage: true })).toBe(false);
  });

  it("预览与 Agent 固定参数顺序一致且不含秘密", () => {
    const args = buildStartupArgumentPreview(instance);
    expect(args).toEqual([
      "-console", "-game", "left4dead2", "-port", "27015", "+tv_port", "27020",
      "+map", "c1m1_hotel", "+mp_gamemode", "coop", "+maxplayers", "8",
      "+exec", "l4d2panel.cfg", "-secure",
    ]);
    expect(args.join(" ")).not.toMatch(/password|secret/i);
  });

  it("历史额外参数中的密码名和值都不会被预览", () => {
    const legacy = { ...instance, config: { ...instance.config, extra_args: ["-secure", "+sv_password", "hunter2", "+rcon_password=secret-value", "sv_password quoted-secret", "  +sv_password=spaced-secret", "+sv_lan", "0"] } };
    const args = buildStartupArgumentPreview(legacy);
    expect(args.join(" ")).not.toContain("hunter2");
    expect(args.join(" ")).not.toContain("secret-value");
    expect(args.join(" ")).not.toContain("quoted-secret");
    expect(args.join(" ")).not.toContain("spaced-secret");
    expect(args).toContain("+sv_password [已隐藏且不会传递]");
    expect(args).toContain("+rcon_password [已隐藏且不会传递]");
    expect(args).toContain("+sv_lan");
  });

  it("仅运行中的维护动作带固定倒计时参数", () => {
    expect(maintenancePayload("running")).toEqual({ maintenance: true, warning_seconds: [600, 300, 60] });
    expect(maintenancePayload("stopped")).toBeUndefined();
  });
});
