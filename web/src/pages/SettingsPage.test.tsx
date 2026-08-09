import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { User } from "../lib/types";

const authState = vi.hoisted(() => ({
  user: { role: "viewer", role_bindings: [] } as Record<string, unknown>,
  logout: vi.fn(),
}));

vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

import { SettingsPage } from "./SettingsPage";

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><SettingsPage /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

function mockOwnerSettings(target: User) {
  authState.user = { id: "owner-1", username: "owner", display_name: "所有者", role: "owner", role_bindings: undefined };
  vi.spyOn(api, "users").mockResolvedValue({ items: [target] });
  vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
  vi.spyOn(api, "instances").mockResolvedValue({ items: [] });
  vi.spyOn(api, "systemUpdates").mockResolvedValue({ supported: false, current_version: "dev", history: [], reason: "test" });
}

describe("设置页全局审计权限", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    localStorage.clear();
  });

  it("没有 audit.read_global 时不查询或显示全局审计", async () => {
    authState.user = { role: "viewer", role_bindings: [] };
    const audit = vi.spyOn(api, "audit").mockResolvedValue({ items: [] });
    renderPage();
    expect(screen.queryByRole("heading", { name: "全局审计" })).not.toBeInTheDocument();
    await waitFor(() => expect(audit).not.toHaveBeenCalled());
  });

  it("仅凭独立全局审计权限即可读取脱敏事件", async () => {
    authState.user = {
      role: "viewer",
      role_bindings: [{ role: "viewer", scope_type: "global", permissions: ["audit.read_global"] }],
    };
    const audit = vi.spyOn(api, "audit").mockResolvedValue({ items: [{
      id: "audit-1",
      actor_name: "审计员",
      action: "permissions.updated",
      resource_type: "user",
      resource_id: "user-123456789",
      remote_ip: "192.0.2.10",
      metadata: { changed: 2 },
      created_at: "2026-08-09T00:00:00Z",
    }] });
    renderPage();
    expect(await screen.findByRole("heading", { name: "全局审计" })).toBeInTheDocument();
    expect(await screen.findByText("审计员 · permissions.updated")).toBeInTheDocument();
    expect(screen.getByText('{"changed":2}')).toBeInTheDocument();
    expect(audit).toHaveBeenCalledWith();
  });

  it("全局审计默认只显示最近五条并可展开或收起", async () => {
    authState.user = {
      role: "viewer",
      role_bindings: [{ role: "viewer", scope_type: "global", permissions: ["audit.read_global"] }],
    };
    vi.spyOn(api, "audit").mockResolvedValue({
      items: Array.from({ length: 7 }, (_, index) => ({
        id: `audit-${index + 1}`,
        actor_name: `审计员 ${index + 1}`,
        action: `action.${index + 1}`,
        resource_type: "instance",
        created_at: `2026-08-09T00:0${index}:00Z`,
      })),
    });
    renderPage();

    expect(await screen.findByText("审计员 1 · action.1")).toBeInTheDocument();
    expect(screen.getByText("审计员 5 · action.5")).toBeInTheDocument();
    expect(screen.queryByText("审计员 6 · action.6")).not.toBeInTheDocument();
    expect(screen.getByText("共 7 条审计记录")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "展开其余 2 条" }));
    expect(screen.getByText("审计员 7 · action.7")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "收起，仅显示最近 5 条" }));
    expect(screen.queryByText("审计员 6 · action.6")).not.toBeInTheDocument();
  });

  it("保留服务端已有零绑定，且不会在编辑时回退角色预设", async () => {
    const target: User = { id: "user-empty", username: "empty", display_name: "零权限用户", role: "admin", totp_enabled: false, account_state: "active", role_bindings: [], created_at: "2026-08-09T00:00:00Z" };
    mockOwnerSettings(target);
    const update = vi.spyOn(api, "updateUser").mockResolvedValue({ user: target });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "管理" }));
    expect(await screen.findByText(/当前没有权限绑定/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "新的显示名" } });
    fireEvent.click(screen.getByRole("button", { name: "保存更改" }));
    await waitFor(() => expect(update).toHaveBeenCalledWith("user-empty", { display_name: "新的显示名" }));
  });

  it("阻止把已有非空绑定删除为后端不接受的空集合", async () => {
    const target: User = { id: "user-bound", username: "bound", display_name: "有权限用户", role: "viewer", totp_enabled: false, account_state: "active", role_bindings: [{ role: "viewer", scope_type: "global", permissions: ["nodes.read"] }], created_at: "2026-08-09T00:00:00Z" };
    mockOwnerSettings(target);
    const update = vi.spyOn(api, "updateUser").mockResolvedValue({ user: target });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "管理" }));
    fireEvent.click(await screen.findByRole("button", { name: "删除权限绑定" }));
    expect(screen.getByText(/至少保留一条权限绑定/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "仍然无效" } });
    expect(screen.getByRole("button", { name: "保存更改" })).toBeDisabled();
    expect(update).not.toHaveBeenCalled();
  });

  it("添加账号时可在单实例范围选择全部地图权限", async () => {
    const target: User = { id: "existing-user", username: "existing", display_name: "已有用户", role: "viewer", totp_enabled: false, account_state: "active", role_bindings: [{ role: "viewer", scope_type: "global", permissions: ["nodes.read"] }], created_at: "2026-08-09T00:00:00Z" };
    mockOwnerSettings(target);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "添加用户" }));
    fireEvent.change(screen.getByLabelText("范围"), { target: { value: "instance" } });

    for (const label of ["查看地图库", "导入地图", "分配或取消地图", "校验地图", "删除地图"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(screen.getByText("单实例范围只能删除当前账号导入的地图")).toBeInTheDocument();
  });

  it("零权限账号仍可查看并撤销已有 API Token", async () => {
    authState.user = { id: "empty-user", username: "empty", display_name: "零权限", role: "viewer", role_bindings: [] };
    vi.spyOn(api, "apiTokens").mockResolvedValue({ items: [{
      id: "token-1",
      user_id: "empty-user",
      name: "旧自动化令牌",
      scopes: ["operate"],
      expires_at: "2026-12-31T00:00:00Z",
      created_at: "2026-08-01T00:00:00Z",
    }] });
    const revoke = vi.spyOn(api, "revokeApiToken").mockResolvedValue(undefined);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    renderPage();

    fireEvent.click(screen.getByRole("button", { name: /API Token/ }));
    expect(await screen.findByText("旧自动化令牌")).toBeInTheDocument();
    expect(screen.getByText(/已有令牌仍可在下方撤销/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "新建令牌" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "撤销 旧自动化令牌" }));
    await waitFor(() => expect(revoke).toHaveBeenCalled());
    expect(revoke.mock.calls[0][0]).toBe("token-1");
  });

  it("在最近升级中显示失败阶段和脱敏诊断", async () => {
    authState.user = { id: "owner-1", username: "owner", display_name: "所有者", role: "owner", role_bindings: undefined };
    vi.spyOn(api, "users").mockResolvedValue({ items: [] });
    vi.spyOn(api, "audit").mockResolvedValue({ items: [] });
    vi.spyOn(api, "systemUpdates").mockResolvedValue({
      supported: true,
      current_version: "0.1.0",
      deployment: "docker-compose",
      package_format: "tar.gz",
      history: [{
        job_id: "5c288597-90e6-445b-a46a-ce725736811e",
        status: "failed",
        phase: "rolling_back",
        current_version: "0.1.0",
        target_version: "0.1.0",
        rollback: "succeeded",
        error: "replacement exit_code=1; replacement_log=configuration rejected",
        created_at: "2026-08-09T00:00:00Z",
        updated_at: "2026-08-09T00:01:00Z",
        completed_at: "2026-08-09T00:01:00Z",
      }],
    });
    renderPage();

    expect(await screen.findByText(/失败诊断：replacement exit_code=1/)).toBeInTheDocument();
    expect(screen.getByText("回滚：成功")).toBeInTheDocument();
  });
});
