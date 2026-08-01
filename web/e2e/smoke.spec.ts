import { expect, test, type Page } from "@playwright/test";

test.beforeEach(async ({ page }) => {
  await page.routeWebSocket("**/api/v1/events**", () => undefined);
});

const owner = {
  id: "00000000-0000-4000-8000-000000000001",
  username: "admin",
  display_name: "系统管理员",
  role: "owner",
  totp_enabled: true,
  created_at: "2026-07-31T00:00:00Z",
};

async function json(page: Page, pattern: string, body: unknown, status = 200) {
  await page.route(pattern, (route) => route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  }));
}

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
  await json(page, "**/api/v1/jobs", { items: [] });
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
});
