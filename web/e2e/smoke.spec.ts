import { expect, test, type Page } from "@playwright/test";

test.beforeEach(async ({ page }) => {
  await json(page, "**/api/v1/**", { error: { code: "not_implemented", message: "测试未配置此接口" } }, 501);
  await json(page, "**/api/v1/auth/me", { error: { code: "unauthorized" } }, 401);
  await page.routeWebSocket("**/api/v1/events**", () => undefined);
  await page.route("**/api/v1/auth/bootstrap", (route) => route.fulfill({
    status: 200, contentType: "application/json", body: JSON.stringify({ required: false, key_configured: false }),
  }));
});

const owner = {
  id: "00000000-0000-4000-8000-000000000001",
  username: "admin",
  display_name: "系统管理员",
  role: "owner",
  totp_enabled: true,
  created_at: "2026-07-31T00:00:00Z",
};

async function json(page: Page, pattern: string | RegExp, body: unknown, status = 200) {
  await page.route(pattern, (route) => route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  }));
}

test("首次启动通过初始化密钥创建首个所有者", async ({ page }) => {
  await page.unroute("**/api/v1/auth/bootstrap");
  let authenticated = false;
  let initialized = false;
  await page.route("**/api/v1/auth/bootstrap", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ required: !initialized, key_configured: !initialized, csrf_token: initialized ? undefined : "bootstrap-csrf" }) });
      return;
    }
    expect(route.request().headers()["x-bootstrap-csrf-token"]).toBe("bootstrap-csrf");
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    expect(route.request().postDataJSON()).toEqual({ setup_key: "l4d2p_setup_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG" });
    initialized = true;
    authenticated = true;
    await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ user: { ...owner, totp_enabled: false }, csrf_token: "session-csrf" }) });
  });
  await page.route("**/api/v1/auth/me", (route) => route.fulfill({
    status: authenticated ? 200 : 401,
    contentType: "application/json",
    body: JSON.stringify(authenticated ? { user: { ...owner, totp_enabled: false }, csrf_token: "session-csrf" } : { error: { code: "unauthorized", message: "authentication required" } }),
  }));
  await json(page, "**/api/v1/nodes", { items: [] });
  await json(page, "**/api/v1/instances", { items: [] });
  await json(page, "**/api/v1/metrics**", { items: [], resolution: "1m" });
  await json(page, /\/api\/v1\/jobs(?:\?.*)?$/, { items: [] });
  await json(page, "**/api/v1/audit**", { items: [] });
  await json(page, "**/api/v1/notifications", { items: [] });
  await json(page, "**/api/v1/users", { items: [{ ...owner, totp_enabled: false, bootstrap_owner: true, role_bindings: [{ id: "binding-owner", user_id: owner.id, role: "owner", scope_type: "global" }] }] });

  await page.goto("/");
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole("heading", { name: "输入初始化密钥", exact: true })).toBeVisible();
  await page.getByLabel("初始化密钥", { exact: true }).fill("l4d2p_setup_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG");
  await page.getByRole("button", { name: /验证并进入设置/ }).click();
  await expect(page).toHaveURL(/\/settings$/);
  await expect(page.getByRole("heading", { name: /用户与设置/ })).toBeVisible();
  await expect(page.getByRole("alert")).toContainText("初始化密钥也是 admin 的当前密码");
});

test("尚未签发初始化密钥时显示本机命令并禁用提交", async ({ page }) => {
  await page.unroute("**/api/v1/auth/bootstrap");
  await json(page, "**/api/v1/auth/bootstrap", { required: true, key_configured: false, csrf_token: "bootstrap-csrf" });
  await json(page, "**/api/v1/auth/me", { error: { code: "unauthorized", message: "authentication required" } }, 401);

  await page.goto("/setup");
  await expect(page.getByText("docker compose exec -T app setup-key issue")).toBeVisible();
  await expect(page.getByRole("button", { name: /验证并进入设置/ })).toBeDisabled();
});

test("错误初始化密钥保持在首启页并显示稳定错误", async ({ page }) => {
  await page.unroute("**/api/v1/auth/bootstrap");
  await page.route("**/api/v1/auth/bootstrap", (route) => route.request().method() === "GET"
    ? route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ required: true, key_configured: true, csrf_token: "bootstrap-csrf" }) })
    : route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify({ error: { code: "invalid_setup_key", message: "setup key is invalid" } }) }));
  await json(page, "**/api/v1/auth/me", { error: { code: "unauthorized", message: "authentication required" } }, 401);

  await page.goto("/setup");
  await page.getByLabel("初始化密钥", { exact: true }).fill("l4d2p_setup_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG");
  await page.getByRole("button", { name: /验证并进入设置/ }).click();
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole("alert")).toContainText("初始化密钥无效");
});

test("初始化完成后 setup 自动跳转登录页", async ({ page }) => {
  await json(page, "**/api/v1/auth/me", { error: { code: "unauthorized", message: "authentication required" } }, 401);
  await page.goto("/setup");
  await expect(page).toHaveURL(/\/login$/);
});

test("公开状态页只展示脱敏服务器信息", async ({ page }) => {
  await json(page, "**/api/v1/public/servers", {
    items: [{
      id: "server-1", name: "香港战役服", status: "running", address: "game.example.com:27015",
      map: "c1m1_hotel", mode: "coop", players: 3, max_players: 8,
    }],
  });
  await page.goto("/status");
  await expect(page.getByRole("heading", { name: /服务器状态/ })).toBeVisible();
  await expect(page.getByText("香港战役服")).toBeVisible();
  await expect(page.getByText("game.example.com:27015")).toBeVisible();
  await expect(page.getByText(/RCON|Steam ID|实例根目录/)).toHaveCount(0);
});

test("管理员登录后进入真实数据仪表盘", async ({ page }) => {
  let authenticated = false;
  await page.route("**/api/v1/auth/me", (route) => route.fulfill({
    status: authenticated ? 200 : 401,
    contentType: "application/json",
    body: JSON.stringify(authenticated
      ? { user: owner, csrf_token: "csrf-test" }
      : { error: { code: "unauthorized", message: "authentication required" } }),
  }));
  await page.route("**/api/v1/auth/login", async (route) => {
    const request = route.request().postDataJSON() as { username?: string; password?: string };
    expect(request.username).toBe("admin");
    expect(request.password).toBe("correct-horse-battery-staple");
    authenticated = true;
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ user: owner, csrf_token: "csrf-test" }) });
  });
  await json(page, "**/api/v1/nodes", { items: [{
    id: "node-1", name: "香港-01", status: "online", os: "linux", arch: "amd64", agent_version: "0.1.0",
    cpu_percent: 18, memory_used: 2147483648, memory_total: 8589934592, disk_used: 10737418240,
    disk_total: 107374182400, network_rx: 5368709120, network_tx: 2147483648,
    last_seen_at: "2026-07-31T00:00:00Z", created_at: "2026-07-30T00:00:00Z",
  }] });
  await json(page, "**/api/v1/instances", { items: [{
    id: "instance-1", node_id: "node-1", name: "待更新战役服", status: "running", host: "0.0.0.0", port: 27015,
    map: "c1m1_hotel", mode: "coop", players: 2, max_players: 8, build_id: "123456", update_available: true,
    available_build: "654321", last_update_checked_at: "2026-07-31T00:00:00Z", version: 1,
    config: { hostname: "待更新战役服", mode: "coop", difficulty: "normal", max_players: 8, start_map: "c1m1_hotel", region: "255", public_status: false, auto_restart: true },
    started_at: "2026-07-31T00:00:00Z", created_at: "2026-07-30T00:00:00Z", updated_at: "2026-07-31T00:00:00Z",
  }] });
  await json(page, "**/api/v1/metrics**", { items: [], resolution: "1m" });
  await json(page, /\/api\/v1\/jobs(?:\?.*)?$/, { items: [] });
  await json(page, "**/api/v1/audit**", { items: [] });
  await json(page, "**/api/v1/notifications", { items: [] });

  await page.goto("/login");
  await page.getByLabel("账号").fill("admin");
  await page.locator('input[autocomplete="current-password"]').fill("correct-horse-battery-staple");
  await page.getByRole("button", { name: /安全登录/ }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole("heading", { name: /作战总览/ })).toBeVisible();
  await expect(page.getByText("香港-01")).toBeVisible();
  await expect(page.getByText("5.0 GB", { exact: true })).toBeVisible();
  await expect(page.getByText("2.0 GB", { exact: true })).toBeVisible();
  await expect(page.getByText("可更新至 654321")).toBeVisible();
  await expect(page.getByText(/1 个实例可更新/)).toBeVisible();
  await expect(page.getByLabel("集群当前在线玩家")).toContainText("2 人");
  await expect(page.getByText("暂无历史玩家数据")).toBeVisible();
});

test("集群当前玩家汇总全部实例，与单实例历史曲线明确区分", async ({ page }, testInfo) => {
  await json(page, "**/api/v1/auth/me", { user: owner, csrf_token: "csrf-test" });
  await json(page, "**/api/v1/nodes", { items: [] });
  await json(page, /\/api\/v1\/jobs(?:\?.*)?$/, { items: [] });
  await json(page, "**/api/v1/notifications", { items: [] });
  await json(page, "**/api/v1/instances", { items: [
    { id: "empty", name: "空服", status: "running", players: 0 },
    { id: "occupied", name: "多人绝境01", status: "running", players: 3 },
    { id: "stopped", name: "已停止服", status: "stopped", players: 8 },
  ].map((item) => ({ ...item, node_id: "node-1", host: "127.0.0.1", port: 27015, max_players: 10 })) });
  await page.route("**/api/v1/metrics**", (route) => {
    expect(new URL(route.request().url()).searchParams.get("instance_id")).toBe("empty");
    return route.fulfill({ contentType: "application/json", body: JSON.stringify({ resolution: "1m", items: [
      { collected_at: "2026-10-01T00:00:00Z", players: 0 },
      { collected_at: "2026-10-01T00:01:00Z", players: 0 },
    ] }) });
  });
  await page.goto("/");
  await expect(page.getByRole("link", { name: /多人绝境01/ })).toContainText("3/10");
  await expect(page.getByLabel("集群当前在线玩家")).toContainText("3 人");
  await expect(page.getByText("空服 · 近 24 小时玩家趋势（单实例）")).toBeVisible();
  await page.locator(".capacity-chart .recharts-wrapper").hover({ position: { x: 50, y: 40 } });
  const tooltip = page.locator(".recharts-tooltip-wrapper");
  await expect(tooltip).toContainText("空服 · 10/01");
  await expect(tooltip).toContainText("历史玩家数");
  await expect(tooltip).toContainText("0");
  await page.locator(".capacity-card").screenshot({ path: testInfo.outputPath("fleet-players.png") });
});
