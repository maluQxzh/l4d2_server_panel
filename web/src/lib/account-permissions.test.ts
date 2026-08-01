import { describe, expect, it } from "vitest";
import { hasRootSecurityPermission } from "./account-permissions";

describe("账号根安全权限", () => {
  it("只允许所有者进入用户管理", () => {
    expect(hasRootSecurityPermission({ role: "owner" })).toBe(true);
    expect(hasRootSecurityPermission({ role: "admin" })).toBe(false);
    expect(hasRootSecurityPermission({ role: "operator" })).toBe(false);
    expect(hasRootSecurityPermission({ role: "viewer" })).toBe(false);
    expect(hasRootSecurityPermission(null)).toBe(false);
  });
});
