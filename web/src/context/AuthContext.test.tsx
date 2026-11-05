import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AuthProvider, useAuth } from "./AuthContext";
import { api, expireSession, onSessionExpired, setCsrfToken } from "../lib/api";
import type { User } from "../lib/types";

afterEach(() => { vi.unstubAllGlobals(); setCsrfToken(); });

function SessionStatus() {
  const auth = useAuth();
  return <span>{auth.authenticated ? auth.user!.display_name : "已退出"}</span>;
}

it("首次未登录的 401 不取消仍在读取的初始化状态", async () => {
  let finish!: (value: { required: boolean; key_configured: boolean }) => void;
  vi.spyOn(api, "me").mockResolvedValue({ user: null as unknown as User });
  vi.spyOn(api, "bootstrapStatus").mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  function BootstrapStatus() { const auth = useAuth(); return <span>{auth.loading ? "读取中" : auth.bootstrapRequired ? "需要初始化" : "已初始化"}</span>; }
  render(<QueryClientProvider client={client}><AuthProvider><BootstrapStatus /></AuthProvider></QueryClientProvider>);
  act(() => expireSession());
  await act(async () => finish({ required: true, key_configured: false }));
  expect(await screen.findByText("需要初始化")).toBeInTheDocument();
});

it("会话失效清理资源缓存，迟到请求不能恢复旧账号数据", async () => {
  vi.spyOn(api, "me").mockResolvedValue({ user: { id: "old", display_name: "旧账号" } as User });
  vi.spyOn(api, "bootstrapStatus").mockResolvedValue({ required: false, key_configured: false });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><AuthProvider><SessionStatus /></AuthProvider></QueryClientProvider>);
  expect(await screen.findByText("旧账号")).toBeInTheDocument();
  client.setQueryData(["instances"], { items: [{ name: "private" }] });
  let finish!: (value: string[]) => void;
  const pending = client.fetchQuery({ queryKey: ["nodes"], queryFn: () => new Promise<string[]>((resolve) => { finish = resolve; }) }).catch(() => {});
  sessionStorage.setItem("l4d2-event-cursor", "45");
  act(() => expireSession());
  expect(await screen.findByText("已退出")).toBeInTheDocument();
  finish(["old private node"]);
  await pending;
  await waitFor(() => expect(client.getQueryData(["nodes"])).toBeUndefined());
  expect(client.getQueryData(["instances"])).toBeUndefined();
  expect(sessionStorage.getItem("l4d2-event-cursor")).toBeNull();
});

it("普通 401 通知失效；403 和登录失败不退出会话", async () => {
  const listener = vi.fn();
  const unsubscribe = onSessionExpired(listener);
  const fetch = vi.fn(); vi.stubGlobal("fetch", fetch);
  const response = (status: number) => new Response(JSON.stringify({ error: { message: "denied" } }), { status });
  try {
    fetch.mockResolvedValueOnce(response(403));
    await expect(api.nodes()).rejects.toMatchObject({ status: 403 });
    fetch.mockResolvedValueOnce(response(401));
    await expect(api.login({ username: "x", password: "wrong" })).rejects.toMatchObject({ status: 401 });
    expect(listener).not.toHaveBeenCalled();
    fetch.mockResolvedValueOnce(response(401));
    await expect(api.nodes()).rejects.toMatchObject({ status: 401 });
    expect(listener).toHaveBeenCalledTimes(1);
  } finally { unsubscribe(); }
});

it("旧请求的迟到 401 不会退出新登录会话", async () => {
  const listener = vi.fn();const unsubscribe = onSessionExpired(listener);
  let finish!: (value: Response) => void;
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>((resolve) => { finish = resolve; })));
  try {
    setCsrfToken("old");
    const pending = api.nodes();
    setCsrfToken("new-session");
    finish(new Response("{}", { status: 401 }));
    await expect(pending).rejects.toMatchObject({ status: 401 });
    expect(listener).not.toHaveBeenCalled();
  } finally { unsubscribe(); }
});

it("旧会话成功响应在正文迟到时也不能覆盖新 CSRF 或用户", async () => {
  let finish!: (value: string) => void;
  const text = new Promise<string>((resolve) => { finish = resolve; });
  const fetch = vi.fn().mockResolvedValueOnce({ status: 200, ok: true, text: () => text });
  vi.stubGlobal("fetch", fetch);
  setCsrfToken("old");
  const pending = api.me();
  const rejected = expect(pending).rejects.toMatchObject({ code: "stale_session" });
  await Promise.resolve();
  setCsrfToken("new");
  finish(JSON.stringify({ user: { id: "old-user" }, csrf_token: "old" }));
  await rejected;
  fetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
  await api.markNotificationRead("notification");
  expect((fetch.mock.calls[1][1].headers as Headers).get("X-CSRF-Token")).toBe("new");
});

it("旧注销成功的迟到 finally 不清除新会话令牌", async () => {
  let finish!: (value: Response) => void;
  const fetch = vi.fn().mockImplementationOnce(() => new Promise<Response>((resolve) => { finish = resolve; }));
  vi.stubGlobal("fetch", fetch);
  setCsrfToken("old");
  const pending = api.logout();
  const rejected = expect(pending).rejects.toMatchObject({ code: "stale_session" });
  setCsrfToken("new"); finish(new Response(null, { status: 204 }));
  await rejected;
  fetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
  await api.markNotificationRead("notification");
  expect((fetch.mock.calls[1][1].headers as Headers).get("X-CSRF-Token")).toBe("new");
});

it("开始登录即隔离旧请求，旧 me 的 401 不破坏正在建立的新会话", async () => {
  let oldReply!: (value: Response) => void;
  let loginReply!: (value: Response) => void;
  const fetch = vi.fn().mockImplementationOnce(() => new Promise<Response>((r) => { oldReply = r; }))
    .mockImplementationOnce(() => new Promise<Response>((r) => { loginReply = r; }));
  vi.stubGlobal("fetch", fetch);
  const expired = vi.fn(); const unsubscribe = onSessionExpired(expired);
  try {
    const old = api.me(); const rejected = expect(old).rejects.toMatchObject({ status: 401 });
    const login = api.login({ username: "new", password: "password" });
    oldReply(new Response("{}", { status: 401 })); await rejected;
    expect(expired).not.toHaveBeenCalled();
    loginReply(new Response(JSON.stringify({ user: { id: "new" }, csrf_token: "new-token" })));
    expect((await login).user.id).toBe("new");
  } finally { unsubscribe(); }
});
