import { describe, expect, it } from "vitest";
import type { RoleBinding } from "./types";
import { hasInstancePermission, hasRootSecurityPermission, tokenScopesForUser } from "./account-permissions";

describe("账号根安全权限", () => {
  it("只允许所有者进入用户管理", () => {
    expect(hasRootSecurityPermission({ role: "owner" })).toBe(true);
    expect(hasRootSecurityPermission({ role: "admin" })).toBe(false);
    expect(hasRootSecurityPermission({ role: "operator" })).toBe(false);
    expect(hasRootSecurityPermission({ role: "viewer" })).toBe(false);
    expect(hasRootSecurityPermission(null)).toBe(false);
  });
});

describe("细粒度权限", () => {
  it("以显式权限为准而不继承角色预设", () => {
    const user = {
      role: "admin" as const,
      role_bindings: [{
        role: "admin" as const,
        scope_type: "instance" as const,
        instance_id: "instance-a",
        permissions: ["instances.read", "backups.create"],
      }] as RoleBinding[],
    };
    const instance = { id: "instance-a", node_id: "node-a" };
    expect(hasInstancePermission(user, instance, "backups.create")).toBe(true);
    expect(hasInstancePermission(user, instance, "rcon.execute")).toBe(false);
    expect(tokenScopesForUser(user)).toEqual(["read", "operate"]);
  });
});
