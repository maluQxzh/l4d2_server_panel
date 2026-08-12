import { describe, expect, it } from "vitest";
import type { RoleBinding } from "./types";
import { hasInstancePermission, hasJobPermission, hasMapNodePermission, hasRootSecurityPermission, normalizePermissionsAtScope, permissionAllowedAtScope, permissionCatalog, rolePresetPermissions, tokenScopesForGlobal, tokenScopesForInstance, tokenScopesForNode, tokenScopesForUser } from "./account-permissions";

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
  it("操作员预设移除高影响权限并保留观察者读取权限", () => {
    const operator = rolePresetPermissions("operator");
    const admin = rolePresetPermissions("admin");
    const excluded = [
      "players.kick", "players.ban", "players.unban", "players.broadcast", "rcon.execute",
      "files.read", "files.write", "files.delete",
      "backups.download", "backups.create", "backups.restore", "schedules.manage",
    ] as const;
    for (const permission of excluded) {
      expect(operator).not.toContain(permission);
      expect(admin).toContain(permission);
    }
    for (const permission of ["files.list", "backups.read", "schedules.read", "logs.read"] as const) {
      expect(rolePresetPermissions("viewer")).toContain(permission);
      expect(operator).toContain(permission);
    }
    for (const permission of ["maps.read", "maps.import", "maps.assign", "maps.verify", "maps.delete"] as const) {
      expect(operator).toContain(permission);
    }
    expect(operator).toContain("players.change_map");
  });

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

  it("按任务实际节点和实例范围判定取消权限", () => {
    const user = {
      role: "operator" as const,
      role_bindings: [{
        role: "operator" as const,
        scope_type: "instance" as const,
        instance_id: "instance-a",
        permissions: ["instances.read", "jobs.read", "jobs.cancel"],
      }] as RoleBinding[],
    };
    expect(hasJobPermission(user, { node_id: "node-a", instance_id: "instance-a" }, "jobs.cancel")).toBe(true);
    expect(hasJobPermission(user, { node_id: "node-a", instance_id: "instance-b" }, "jobs.cancel")).toBe(false);
    expect(hasJobPermission(user, { node_id: "node-a", scope_instance_id: "instance-a" }, "jobs.cancel")).toBe(true);
    expect(hasJobPermission(user, { node_id: "node-a", scope_instance_id: "instance-b" }, "jobs.cancel")).toBe(false);
    expect(hasJobPermission(user, { node_id: "node-a" }, "jobs.cancel")).toBe(false);
  });

  it("权限目录不再暴露已经迁移或没有入口的权限", () => {
    const ids = permissionCatalog.map((item) => item.id);
    expect(ids).not.toContain("instances.update");
    expect(ids).not.toContain("instances.validate");
    expect(ids).not.toContain("files.upload");
  });

  it("明确的空绑定表示没有权限，不回退到显示角色预设", () => {
    const user = { role: "admin" as const, role_bindings: [] as RoleBinding[] };
    expect(hasInstancePermission(user, { id: "instance-a", node_id: "node-a" }, "instances.read")).toBe(false);
    expect(hasRootSecurityPermission({ role: "owner", role_bindings: [] })).toBe(false);
    expect(tokenScopesForUser(user)).toEqual([]);
    const emptyPermissionBinding = {
      role: "admin" as const,
      role_bindings: [{ role: "admin" as const, scope_type: "global" as const, permissions: [] }],
    };
    expect(hasRootSecurityPermission(emptyPermissionBinding)).toBe(false);
    expect(tokenScopesForUser(emptyPermissionBinding)).toEqual([]);
  });

  it("API Token 只委派所选资源上实际拥有的权限层级", () => {
    const user = {
      role: "operator" as const,
      role_bindings: [{
        role: "operator" as const,
        scope_type: "node" as const,
        node_id: "node-a",
        permissions: ["nodes.read", "instances.read", "instances.start"],
      }] as RoleBinding[],
    };
    expect(tokenScopesForGlobal(user)).toEqual([]);
    expect(tokenScopesForNode(user, "node-a")).toEqual(["read", "operate"]);
    expect(tokenScopesForNode(user, "node-b")).toEqual([]);
    expect(tokenScopesForInstance(user, { id: "instance-a", node_id: "node-a" })).toEqual(["read", "operate"]);
    expect(tokenScopesForInstance(user, { id: "instance-b", node_id: "node-b" })).toEqual([]);
  });

  it("权限范围会补齐后端要求的节点和实例基础读取依赖", () => {
    expect(normalizePermissionsAtScope(["nodes.discover"], "node")).toEqual(["nodes.discover", "nodes.read"]);
    expect(normalizePermissionsAtScope(["instances.start"], "instance")).toEqual(["instances.read", "instances.start"]);
    expect(normalizePermissionsAtScope(["maps.assign"], "node")).toEqual(["instances.read", "maps.assign", "nodes.read"]);
    expect(normalizePermissionsAtScope(["notifications.manage"], "global")).toEqual(["notifications.manage"]);
    expect(normalizePermissionsAtScope([], "instance")).toEqual([]);
  });

  it("允许把共享地图权限下放到单个实例", () => {
    const mapPermissions = ["maps.read", "maps.import", "maps.assign", "maps.verify", "maps.delete"] as const;
    for (const permission of mapPermissions) expect(permissionAllowedAtScope(permission, "instance")).toBe(true);
    expect(normalizePermissionsAtScope([...mapPermissions], "instance")).toEqual([
      "instances.read", "maps.assign", "maps.delete", "maps.import", "maps.read", "maps.verify",
    ]);

    const user = {
      role: "admin" as const,
      role_bindings: [{
        role: "admin" as const,
        scope_type: "instance" as const,
        instance_id: "instance-a",
        permissions: ["instances.read", ...mapPermissions],
      }] as RoleBinding[],
    };
    expect(hasMapNodePermission(user, [{ id: "instance-a", node_id: "node-a" }], "node-a", "maps.verify")).toBe(true);
    expect(hasMapNodePermission(user, [{ id: "instance-a", node_id: "node-a" }], "node-b", "maps.verify")).toBe(false);
  });
});
