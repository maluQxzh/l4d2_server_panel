import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { describe, expect, it, vi } from "vitest";

const auth = vi.hoisted(() => ({
  user: { id: "user-1", username: "new-user", display_name: "新用户", role: "viewer", totp_enabled: false, disabled: false, account_state: "pending_activation", created_at: "2026-08-08T00:00:00Z" },
  loading: false,
  authenticated: true,
  activationRequired: true,
  completeActivation: vi.fn(),
  logout: vi.fn(),
}));

vi.mock("../context/AuthContext", () => ({ useAuth: () => auth }));

import { ActivationPage } from "./ActivationPage";

describe("首次账号激活", () => {
  it("要求两次密码一致并在成功后进入控制台", async () => {
    auth.completeActivation.mockResolvedValue({ user: { ...auth.user, account_state: "active" }, csrf_token: "new-csrf" });
    render(<MemoryRouter initialEntries={["/activate"]}><Routes>
      <Route path="/activate" element={<ActivationPage />} />
      <Route path="/" element={<div>控制台已开放</div>} />
    </Routes></MemoryRouter>);

    fireEvent.change(screen.getByLabelText("新密码"), { target: { value: "a-secure-password-123" } });
    fireEvent.change(screen.getByLabelText("确认新密码"), { target: { value: "different-password-456" } });
    fireEvent.click(screen.getByRole("button", { name: /保存密码并进入/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("两次输入的密码不一致");
    expect(auth.completeActivation).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText("确认新密码"), { target: { value: "a-secure-password-123" } });
    fireEvent.click(screen.getByRole("button", { name: /保存密码并进入/ }));
    await waitFor(() => expect(auth.completeActivation).toHaveBeenCalledWith("a-secure-password-123"));
    expect(await screen.findByText("控制台已开放")).toBeInTheDocument();
  });
});
