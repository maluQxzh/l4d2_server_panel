import { dropFile } from "./file-drop";
import { expect, test, type Page, type Route } from "@playwright/test";
import { createHash } from "node:crypto";

test.beforeEach(async ({ page }) => {
  await page.routeWebSocket("**/api/v1/events**", () => undefined);
  await page.route("**/api/v1/auth/bootstrap", (route) => route.fulfill({
    status: 200, contentType: "application/json", body: JSON.stringify({ required: false }),
  }));
});

const owner = {
  id: "user-1", username: "admin", display_name: "系统管理员", role: "owner",
  totp_enabled: true, account_state: "active", created_at: "2026-07-31T00:00:00Z",
};

const node = {
  id: "node-1", name: "香港-01", status: "online", os: "linux", arch: "amd64",
  agent_version: "1.0.0", address: "203.0.113.10", public_address: "game.example.com", instance_root: "/srv/l4d2panel/instances",
  cpu_percent: 12, memory_used: 1_000_000, memory_total: 8_000_000,
  disk_used: 10_000_000, disk_total: 100_000_000, network_rx: 5_000_000, network_tx: 2_000_000, created_at: "2026-07-30T00:00:00Z",
};

const environment = {
  id: "environment-1", node_id: node.id, node_name: node.name, node_status: "online", agent_supported: true,
  system_dependencies: { status: "ready" }, steamcmd_source: "unconfigured", steamcmd_status: "unconfigured",
  created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
};

const gameInstallation = {
  id: "installation-1", environment_id: environment.id, node_id: node.id,
  path: "/srv/l4d2/server-a", app_id: 222860, status: "ready", build_id: "123456",
  created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
};

const instance = {
  id: "instance-1", node_id: node.id, node_public_address: node.public_address, name: "专家战役 #1", status: "running",
  path: "/srv/l4d2panel/instances/expert", host: "game.example.com", port: 27015,
  tv_port: 27020, pid: 4242, map: "c1m1_hotel", mode: "coop", players: 1,
  max_players: 4, build_id: "123456", update_available: false, version: 3,
  rcon_configured: true,
  config: { hostname: "专家战役 #1", mode: "coop", difficulty: "normal", max_players: 4,
    start_map: "c1m1_hotel", region: "255", public_status: false, auto_restart: true,
    extra_args: [], cvars: {} },
  started_at: "2026-07-31T00:00:00Z", created_at: "2026-07-30T00:00:00Z",
  updated_at: "2026-07-31T00:00:00Z",
};

async function authenticated(page: Page, user = owner) {
  await page.route("**/api/v1/auth/me", (route) => route.fulfill({
    status: 200, contentType: "application/json",
    body: JSON.stringify({ user, csrf_token: "csrf-test" }),
  }));
  await page.route("**/api/v1/notifications", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/nodes/*/quarantine", (route) => json(route, { items: [] }));
}

function json(route: Route, body: unknown, status = 200) {
  return route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
}

test("扫描结果作废后刷新仍提示重新扫描，扫描中仍可安装", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
  let busy = false;
  await page.route("**/api/v1/instances/instance-1/packages/scan", (route) => {
    if (route.request().method() === "POST") { busy = true; return json(route, { job_id: "scan-new" }, 202); }
    return json(route, { scan: { job_id: busy ? "scan-new" : "scan-old", status: busy ? "running" : "succeeded", busy, progress: busy ? 50 : 100, outcome: busy ? "pending" : "stale", registered_count: 0 } });
  });
  await page.goto("/instances/instance-1?tab=mods");
  await expect(page.getByRole("status").filter({ hasText: "结果已作废" })).toBeVisible();
  await page.reload();
  await expect(page.getByRole("status").filter({ hasText: "结果已作废" })).toBeVisible();
  await page.getByRole("button", { name: "扫描已安装" }).click();
  await expect(page.getByRole("button", { name: "扫描已安装" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "安装模组", exact: true })).toBeEnabled();
});

test("预下载安装队列离开页面后恢复并可停止", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
  let prepared = false;
  let consumed = false;
  let stopped = false;
  const keys: string[] = [];
  await page.route("**/api/v1/instances/instance-1/prepared-packages", (route) => {
    if (route.request().method() === "POST") {
      expect(route.request().postDataJSON()).toMatchObject({ kind: "vpk", source: "https://example.com/campaign.vpk" });
      prepared = true; return json(route, { job_id: "prepared-one" }, 202);
    }
    return json(route, { supported: true, items: prepared ? [{ id: "prepared-one", name: "campaign.vpk", kind: "vpk", status: "succeeded", progress: 100, ready: !consumed || stopped, queue_id: consumed && !stopped ? "batch-one" : undefined, busy: false }] : [], queues: consumed ? [{ id: "batch-one", status: stopped ? "canceled" : "queued", progress: 0, message: stopped ? "已停止后续安装" : "" }] : [] });
  });
  await page.route("**/api/v1/instances/instance-1/prepared-install-queues", (route) => {
    keys.push(route.request().headers()["idempotency-key"]);
    expect(route.request().postDataJSON()).toEqual({ prepared_ids: ["prepared-one"], acknowledge_native_risk: false });
    consumed = true; return json(route, { job_id: "batch-one" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/prepared-install-queues/batch-one/cancel", (route) => { stopped = true; return json(route, {}); });
  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("button", { name: "安装模组", exact: true }).click();
  await page.getByRole("dialog").getByRole("combobox").first().selectOption("vpk_url");
  await page.getByRole("dialog").getByRole("textbox").fill("https://example.com/campaign.vpk");
  await page.getByRole("button", { name: "预下载，稍后安装" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole("button", { name: "依次安装就绪包（1）" })).toBeEnabled();
  page.on("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "依次安装就绪包（1）" }).click();
  await expect(page.getByRole("status").filter({ hasText: "等待实例空闲" })).toBeVisible();
  await page.goto("/jobs");
  await page.goto("/instances/instance-1?tab=mods");
  await expect(page.getByRole("status").filter({ hasText: "等待实例空闲" })).toBeVisible();
  await page.getByRole("button", { name: "停止后续安装" }).click();
  await expect(page.getByRole("status").filter({ hasText: "已停止后续安装" })).toBeVisible();
  expect(keys).toHaveLength(1);
  expect(keys[0]).toBeTruthy();
});

test("QQ 多文件压缩包可预下载并在刷新后加入持久安装队列", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
  let prepared = false;
  let queued = false;
  await page.route("**/api/v1/instances/instance-1/prepared-packages", (route) => {
    if (route.request().method() === "POST") {
      expect(route.request().postDataJSON()).toEqual({ kind: "qq_vpk_bundle", source: "https://qfile.qq.com/q/BundleCASE", source_transport: "qq_flash" });
      prepared = true;
      return json(route, { job_id: "bundle" }, 202);
    }
    return json(route, { supported: true, qq_flash_supported: true, vpk_bundles_supported: true, items: prepared ? [{ id: "bundle", name: "QQ 闪传 VPK 文件包", kind: "qq_vpk_bundle", status: "succeeded", progress: 100, ready: !queued, file_count: 3, queue_id: queued ? "batch" : undefined }] : [], queues: queued ? [{ id: "batch", status: "queued", progress: 0, message: "等待实例空闲" }] : [] });
  });
  await page.route("**/api/v1/instances/instance-1/prepared-install-queues", (route) => {
    expect(route.request().postDataJSON()).toMatchObject({ prepared_ids: ["bundle"] });
    queued = true;
    return json(route, { job_id: "batch" }, 202);
  });
  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("button", { name: "安装模组", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("combobox").first().selectOption("qq_flash");
  await expect(dialog.getByText("多文件和压缩包可先预下载", { exact: true })).toBeVisible();
  await dialog.locator("textarea").fill("https://qfile.qq.com/q/BundleCASE");
  await dialog.getByRole("button", { name: "预下载，稍后安装" }).click();
  await expect(dialog).toHaveCount(0);
  await page.reload();
  await expect(page.getByText("QQ 闪传 VPK 文件包（3 个 VPK） · 可安装", { exact: true })).toBeVisible();
  page.on("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "安装 QQ 闪传 VPK 文件包", exact: true }).click();
  await expect(page.getByRole("button", { name: "停止后续安装" })).toBeVisible();
  await page.reload();
  await expect(page.getByText("QQ 闪传 VPK 文件包（3 个 VPK） · 等待安装", { exact: true })).toBeVisible();
});

test("Workshop 合集可预下载并在刷新后加入持久安装队列", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
  let prepared = false;
  let queued = false;
  await page.route("**/api/v1/instances/instance-1/prepared-packages", (route) => {
    if (route.request().method() === "POST") {
      expect(route.request().postDataJSON()).toEqual({ kind: "workshop", source: "123" });
      prepared = true;
      return json(route, { job_id: "bundle" }, 202);
    }
    return json(route, { supported: true, qq_flash_supported: true, workshop_supported: true, items: prepared ? [{ id: "bundle", name: "Workshop 文件包", kind: "workshop", status: "succeeded", progress: 100, ready: !queued, file_count: 3, queue_id: queued ? "batch" : undefined }] : [], queues: queued ? [{ id: "batch", status: "queued", progress: 0, message: "等待实例空闲" }] : [] });
  });
  await page.route("**/api/v1/instances/instance-1/prepared-install-queues", (route) => {
    expect(route.request().postDataJSON()).toMatchObject({ prepared_ids: ["bundle"] });
    queued = true;
    return json(route, { job_id: "batch" }, 202);
  });
  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("button", { name: "安装模组", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("combobox").first().selectOption("workshop");
  await expect(dialog.getByText("Workshop 可先预下载", { exact: true })).toBeVisible();
  await dialog.getByLabel("项目或合集 ID").fill("123");
  await dialog.getByRole("button", { name: "预下载，稍后安装" }).click();
  await expect(dialog).toHaveCount(0);
  await page.reload();
  await expect(page.getByText("Workshop 文件包（3 个 VPK） · 可安装", { exact: true })).toBeVisible();
  page.on("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "安装 Workshop 文件包", exact: true }).click();
  await expect(page.getByRole("button", { name: "停止后续安装" })).toBeVisible();
  await page.reload();
  await expect(page.getByText("Workshop 文件包（3 个 VPK） · 等待安装", { exact: true })).toBeVisible();
});

for (const kind of ["vpk", "smx", "native"] as const) {
  test(`QQ 闪传 ${kind} 单文件使用预下载且不触发原安装`, async ({ page }) => {
    await authenticated(page);
    await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
    await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
    let submitted = false;
    await page.route("**/api/v1/instances/instance-1/prepared-packages", (route) => {
      if (route.request().method() === "POST") {
        expect(route.request().postDataJSON()).toEqual({ kind, source: "https://qfile.qq.com/q/SecretCASE", source_transport: "qq_flash" });
        submitted = true;
        return json(route, { job_id: "qq-prepared" }, 202);
      }
      return json(route, { supported: true, qq_flash_supported: true, items: [] });
    });
    await page.route("**/api/v1/instances/instance-1/actions", () => { throw new Error("preparation must not submit installation"); });
    await page.goto(`/instances/instance-1?tab=${kind === "vpk" ? "mods" : "plugins"}`);
    await page.getByRole("button", { name: kind === "vpk" ? "安装模组" : "安装插件", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("combobox").first().selectOption(kind === "vpk" ? "qq_flash" : kind);
    if (kind !== "vpk") {
      await dialog.getByRole("combobox").nth(1).selectOption("qq_flash");
      await expect(dialog.getByRole("button", { name: "提交安装", exact: true })).toBeDisabled();
    }
    await dialog.locator("textarea").fill("https://qfile.qq.com/q/SecretCASE");
    await expect(dialog.getByText("单文件可先预下载", { exact: true })).toBeVisible();
    await dialog.getByRole("button", { name: "预下载，稍后安装" }).click();
    await expect(dialog).toHaveCount(0);
    expect(submitted).toBe(true);
  });
}

test("旧 Agent 隐藏 QQ 预下载但保留 HTTPS 预下载", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/prepared-packages", (route) => json(route, { supported: true, qq_flash_supported: false, items: [] }));
  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("button", { name: "安装模组", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("combobox").first().selectOption("vpk_url");
  await expect(dialog.getByRole("button", { name: "预下载，稍后安装" })).toBeVisible();
  await dialog.getByRole("combobox").first().selectOption("qq_flash");
  await expect(dialog.getByRole("button", { name: "预下载，稍后安装" })).toHaveCount(0);
  await expect(dialog.getByRole("button", { name: "导入 VPK", exact: true })).toBeVisible();
});

test("批量包操作等待实例占用解除并复用原幂等键", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [{
    id: "package-1", instance_id: instance.id, name: "等待测试.vpk", kind: "vpk", enabled: true,
    path: "left4dead2/addons/test.vpk", sha256: "a".repeat(64), installed_at: "2026-08-01T00:00:00Z",
  }] }));
  const keys: string[] = [];
  let release = false;
  await page.route("**/api/v1/instances/instance-1/actions", (route) => {
    expect(route.request().postDataJSON()).toMatchObject({ type: "toggle_package", payload: { package_id: "package-1", enabled: false } });
    keys.push(route.request().headers()["idempotency-key"]);
    if (!release) return json(route, { error: { code: "conflict", reason: "instance_busy", blocking_job_id: "blocking", message: "实例被任务 blocking 占用" } }, 409);
    return json(route, { job_id: "batch-job" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/package-operations/batch-job", (route) => json(route, { status: "succeeded", busy: false }));
  page.on("dialog", (dialog) => dialog.accept());
  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("checkbox", { name: "选择 等待测试.vpk" }).check();
  await page.getByRole("button", { name: "批量停用" }).click();
  await expect(page.getByRole("status").filter({ hasText: "等待实例空闲" })).toBeVisible();
  release = true;
  await expect(page.getByText("等待测试.vpk：停用成功")).toBeVisible();
  expect(keys.length).toBeGreaterThanOrEqual(2);
  expect(keys[0]).toBeTruthy();
  expect(new Set(keys).size).toBe(1);
});

test("拖拽模组、插件压缩包和实例文件复用现有上传流程", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/files?**", (route) => json(route, { items: [] }));
  let install: Record<string, unknown> | undefined;
  let inspection: Record<string, unknown> | undefined;
  let content: Record<string, unknown> | undefined;
  await page.route("**/api/v1/instances/instance-1/actions", (route) => {
    install = route.request().postDataJSON().payload;
    return json(route, { job_id: "install" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/plugin-archives/inspections", (route) => {
    inspection = route.request().postDataJSON();
    return json(route, { error: { message: "测试包无插件内容", code: "invalid_archive" } }, 400);
  });
  await page.route("**/api/v1/instances/instance-1/files/content?**", (route) => {
    content = route.request().postDataJSON();
    expect(new URL(route.request().url()).searchParams.get("path")).toBe("left4dead2/cfg/drag.cfg");
    return json(route, { job_id: "file" }, 202);
  });
  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("button", { name: "安装模组", exact: true }).click();
  let dialog = page.getByRole("dialog");
  await dialog.getByLabel("类型").selectOption("vpk_url");
  await dropFile(page, dialog.getByTestId("file-dropzone"), { name: "拖拽模组.vpk", mimeType: "", buffer: Buffer.from("vpk-content") });
  await expect(dialog.getByText("已选择：拖拽模组.vpk")).toBeVisible();
  await expect.poll(() => install).toBeUndefined(); // Selection does not submit.
  await dialog.getByRole("button", { name: "提交安装" }).click();
  await expect.poll(() => install).toMatchObject({ name: "拖拽模组.vpk", content_base64: Buffer.from("vpk-content").toString("base64") });

  await page.getByRole("button", { name: "插件", exact: true }).click();
  await page.getByRole("button", { name: "安装插件", exact: true }).click();
  dialog = page.getByRole("dialog");
  await dropFile(page, dialog.getByTestId("file-dropzone"), { name: "plugins.7z", mimeType: "", buffer: Buffer.from("plugin-content") });
  await expect(dialog.getByText("已选择：plugins.7z")).toBeVisible();
  await dialog.getByRole("button", { name: "开始识别" }).click();
  await expect.poll(() => inspection).toMatchObject({ name: "plugins.7z", content_base64: Buffer.from("plugin-content").toString("base64") });
  await dialog.getByRole("button", { name: "取消", exact: true }).click();

  await page.goto("/instances/instance-1?tab=files");
  await page.getByRole("button", { name: "cfg", exact: true }).click();
  await page.getByRole("button", { name: "上传或新建", exact: true }).click();
  dialog = page.getByRole("dialog");
  await dropFile(page, dialog.getByTestId("file-dropzone"), { name: "drag.cfg", mimeType: "text/plain", buffer: Buffer.from("hostname test") });
  await expect(dialog.getByLabel("文件内容")).toHaveValue("hostname test");
  await expect(dialog.getByLabel("实例内相对路径")).toHaveValue("left4dead2/cfg/drag.cfg");
  await page.screenshot({ path: test.info().outputPath("file-dropzone.png"), fullPage: true });
  await dialog.getByRole("button", { name: "提交写入" }).click();
  await expect.poll(() => content).toMatchObject({ content: "hostname test" });
});

test("模组多选批量开关、搜索选择、重命名与批量删除", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, status: "stopped" } }));
  await page.route("**/api/v1/instances/instance-1/metrics**", (route) => json(route, { items: [] }));
  let packages = ["alpha", "beta"].map((name) => ({ id: name, name, kind: "vpk", enabled: true, path: `left4dead2/addons/${name}.vpk`, sha256: "a".repeat(64), installed_at: "2026-09-01T00:00:00Z" }));
  const toggled: string[] = [];
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: packages }));
  await page.route("**/api/v1/instances/instance-1/packages/*/versions", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/actions", (route) => {
    const body = route.request().postDataJSON();
    toggled.push(body.payload.package_id);
    packages = packages.map((item) => item.id === body.payload.package_id ? { ...item, enabled: body.payload.enabled } : item);
    return json(route, { job_id: "toggle-job" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/package-operations/*", (route) => json(route, { status: "succeeded", busy: false }));
  await page.route("**/api/v1/instances/instance-1/packages/*/rename", (route) => {
    const body = route.request().postDataJSON();
    packages = packages.map((item) => item.id === "alpha" ? { ...item, name: body.name, path: `left4dead2/addons/${body.filename}` } : item);
    return json(route, { job_id: "rename-job" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/packages/*", (route) => {
    if (route.request().method() !== "DELETE") return route.fallback();
    packages = packages.filter((item) => !route.request().url().endsWith(`/${item.id}`));
    return json(route, { job_id: "delete-job" }, 202);
  });
  page.on("dialog", (dialog) => dialog.accept());
  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("checkbox", { name: "全选当前结果" }).check();
  await page.getByRole("button", { name: "批量停用", exact: true }).click();
  await expect(page.getByText(/批量操作结束：成功 2 项/)).toBeVisible();
  expect(toggled.sort()).toEqual(["alpha", "beta"]);
  await page.getByLabel("按名称搜索模组").fill("alpha");
  await page.getByRole("checkbox", { name: "全选当前结果" }).check();
  await expect(page.getByText("已选 1 项")).toBeVisible();
  await page.getByLabel("按名称搜索模组").fill("");
  await expect(page.getByText("已选 0 项")).toBeVisible();
  await page.getByRole("button", { name: "重命名 alpha", exact: true }).click();
  await page.getByLabel("显示名称", { exact: true }).fill("自定义模组");
  await page.getByLabel("文件名", { exact: true }).fill("custom.vpk");
  await page.getByRole("button", { name: "保存名称" }).click();
  await expect(page.getByRole("button", { name: "查看 自定义模组 详情" })).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("package-multi-select.png"), fullPage: true });
  await page.getByRole("checkbox", { name: "全选当前结果" }).check();
  await page.getByRole("button", { name: "批量删除" }).click();
  await expect(page.getByText("还没有安装模组")).toBeVisible();
  expect(packages).toHaveLength(0);
});

test("创建一次性节点注册令牌", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [] }));
  let enrollmentBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/nodes/enrollment-tokens", async (route) => {
    enrollmentBody = route.request().postDataJSON() as Record<string, unknown>;
    await json(route, { token: "enroll_once_123", expires_at: "2026-07-31T01:00:00Z" }, 201);
  });

  await page.goto("/nodes");
  await page.getByRole("button", { name: "接入第一个节点" }).click();
  await page.getByLabel("节点名称").fill("香港-02");
  await page.getByRole("button", { name: "生成注册令牌" }).click();
  await expect(page.getByText("enroll_once_123")).toBeVisible();
  expect(enrollmentBody).toMatchObject({ label: "香港-02", expires_in_minutes: 30 });
});

test("运行环境可配置自定义 SteamCMD 并手动登记游戏目录", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/environments", (route) => json(route, { items: [environment] }));
  await page.route("**/api/v1/environments/environment-1/game-installations", async (route) => {
    if (route.request().method() === "GET") return json(route, { items: [] });
    gameBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { game_installation: gameInstallation, job_id: "job-import" }, 202);
  });
  let steamBody: Record<string, unknown> | undefined;
  let gameBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/environments/environment-1/steamcmd/actions", async (route) => {
    steamBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { job_id: "job-steamcmd" }, 202);
  });

  await page.goto("/environments");
  await expect(page.getByRole("link", { name: "运行环境" })).toBeVisible();
  await expect(page.getByText(/不会扫描磁盘/)).toBeVisible();
  await page.getByRole("button", { name: "配置 SteamCMD" }).click();
  await page.getByLabel("自定义绝对路径").fill("/opt/steamcmd/steamcmd.sh");
  await page.getByRole("button", { name: "校验并启用" }).click();
  await expect.poll(() => steamBody).toEqual({ action: "configure", path: "/opt/steamcmd/steamcmd.sh" });

  await page.getByRole("button", { name: "添加游戏安装" }).click();
  await page.getByLabel("节点绝对路径").fill("/srv/l4d2/server-b");
  await page.getByRole("button", { name: "提交任务" }).click();
  await expect.poll(() => gameBody).toEqual({ path: "/srv/l4d2/server-b", operation: "import" });
});

test("删除游戏安装时选择仅删记录或永久删除全部文件", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/environments", (route) => json(route, { items: [environment] }));
  await page.route("**/api/v1/environments/environment-1/game-installations", (route) => json(route, { items: [gameInstallation] }));
  const deletionBodies: Array<Record<string, unknown>> = [];
  await page.route("**/api/v1/environments/environment-1/game-installations/installation-1", async (route) => {
    deletionBodies.push(route.request().postDataJSON() as Record<string, unknown>);
    return json(route, { job_id: `job-delete-${deletionBodies.length}` }, 202);
  });

  await page.goto("/environments");
  await expect(page.getByRole("button", { name: "校验并修复", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "更新", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "删除", exact: true }).click();
  const deletionDialog = page.getByRole("dialog", { name: "删除游戏安装" });
  await expect(deletionDialog.getByText("仅删除记录", { exact: true })).toBeVisible();
  await expect(deletionDialog.locator("strong").filter({ hasText: "删除记录和全部文件" })).toBeVisible();
  await expect(deletionDialog.getByText(gameInstallation.path, { exact: true })).toBeVisible();
  await deletionDialog.getByRole("button", { name: "删除记录", exact: true }).click();
  await expect.poll(() => deletionBodies).toEqual([{ mode: "record" }]);

  await page.getByRole("button", { name: "删除", exact: true }).click();
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("dialog", { name: "删除游戏安装" }).getByRole("button", { name: "删除记录和全部文件", exact: true }).click();
  await expect.poll(() => deletionBodies).toEqual([{ mode: "record" }, { mode: "files" }]);
});

test("轮换节点凭据后一次性显示明文和重启提示", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [node] }));
  await page.route("**/api/v1/nodes/node-1/credential/rotate", (route) => json(route, { credential: "rotated-node-secret-once" }));
  page.on("dialog", (dialog) => dialog.accept());

  await page.goto("/nodes");
  await page.getByRole("button", { name: "管理 香港-01" }).click();
  await page.getByRole("button", { name: "轮换节点凭据" }).click();

  await expect(page.getByText("rotated-node-secret-once", { exact: true })).toBeVisible();
  await expect(page.getByText(/更新 agent\.json 的 credential 字段并重启 Agent/)).toBeVisible();
});

test("删除节点后清理卡片并说明主机文件不会删除", async ({ page }) => {
  await authenticated(page);
  let items = [node];
  await page.route("**/api/v1/nodes", (route) => json(route, { items }));
  await page.route("**/api/v1/nodes/node-1", async (route) => {
    if (route.request().method() !== "DELETE") return route.fallback();
    items = [];
    return route.fulfill({ status: 204 });
  });
  page.on("dialog", (dialog) => dialog.accept());

  await page.goto("/nodes");
  await page.getByRole("button", { name: "管理 香港-01" }).click();
  await expect(page.getByText(/不会移除 Agent 主机上的游戏文件/)).toBeVisible();
  await page.getByRole("button", { name: "删除节点" }).click();

  await expect(page.getByText("还没有节点")).toBeVisible();
  await expect(page.getByText("节点及中心端关联数据已删除")).toBeVisible();
});

test("节点可维护公开状态页连接地址", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [node] }));
  let updateBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/nodes/node-1", async (route) => {
    updateBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { node: { ...node, public_address: updateBody.public_address } });
  });

  await page.goto("/nodes");
  await page.getByRole("button", { name: "管理 香港-01" }).click();
  await page.getByLabel("公开连接地址").fill("l4d2.example.net");
  await page.getByRole("button", { name: "保存公开地址" }).click();

  await expect.poll(() => updateBody).toMatchObject({ name: "香港-01", public_address: "l4d2.example.net" });
  await expect(page.getByText("l4d2.example.net", { exact: true })).toBeVisible();
});

test("隔离区到期后仍需一次性确认和完整名称才能彻底清理", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [node] }));
  const expiredEntry = {
    id: "quarantine-expired", node_id: node.id, source_job_id: "job-delete-1", original_instance_id: "old-instance-1",
    instance_name: "旧战役服", root_index: 0, entry_name: "old-instance-1-20260720T000000Z",
    quarantined_at: "2026-07-20T00:00:00Z", retention_expires_at: "2026-07-27T00:00:00Z", created_at: "2026-07-20T00:00:00Z",
  };
  const retainedEntry = {
    ...expiredEntry, id: "quarantine-retained", instance_name: "新隔离服", entry_name: "new-instance-20260801T000000Z",
    quarantined_at: "2026-08-01T00:00:00Z", retention_expires_at: "2099-08-08T00:00:00Z", created_at: "2026-08-01T00:00:00Z",
  };
  const confirmationToken = "one-time-confirmation-must-not-render";
  let confirmationRequested = false;
  let purgeBody: Record<string, unknown> | undefined;
  let purgeIdempotencyKey = "";
  await page.route("**/api/v1/nodes/node-1/quarantine**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/purge-confirmations")) {
      confirmationRequested = true;
      return json(route, { confirmation_token: confirmationToken, expires_at: "2099-08-01T00:05:00Z" }, 201);
    }
    if (path.endsWith("/purge")) {
      purgeBody = route.request().postDataJSON() as Record<string, unknown>;
      purgeIdempotencyKey = route.request().headers()["idempotency-key"] || "";
      return json(route, { job_id: "job-purge-quarantine" }, 202);
    }
    return json(route, { items: [expiredEntry, retainedEntry] });
  });

  await page.goto("/nodes");
  await page.getByRole("button", { name: "管理 香港-01" }).click();
  const retainedRow = page.locator(".quarantine-row").filter({ hasText: retainedEntry.entry_name });
  await expect(retainedRow.getByRole("button", { name: "彻底清理" })).toBeDisabled();
  const expiredRow = page.locator(".quarantine-row").filter({ hasText: expiredEntry.entry_name });
  await expiredRow.getByRole("button", { name: "彻底清理" }).click();
  const dialog = page.getByRole("dialog", { name: "彻底清理隔离文件" });
  await dialog.getByRole("button", { name: "生成一次性确认" }).click();
  await expect.poll(() => confirmationRequested).toBeTruthy();
  await expect(page.getByText(confirmationToken, { exact: true })).toHaveCount(0);
  await dialog.getByLabel("隔离项名称确认").fill("错误名称");
  await expect(dialog.getByRole("button", { name: "确认彻底清理" })).toBeDisabled();
  await dialog.getByLabel("隔离项名称确认").fill(expiredEntry.entry_name);
  await dialog.getByRole("button", { name: "确认彻底清理" }).click();

  await expect.poll(() => purgeBody).toEqual({ confirmation_token: confirmationToken });
  expect(purgeIdempotencyKey).toContain(`purge-quarantine:${expiredEntry.id}:`);
  await expect(dialog).toHaveCount(0);
});

test("外部实例接管只在本次重启发送现有 RCON 密码", async ({ page }) => {
  await authenticated(page);
  const external = { ...instance, status: "external" };
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: external }));
  let actionBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/instances/instance-1/actions", async (route) => {
    actionBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { job_id: "job-takeover" }, 202);
  });

  await page.goto("/instances/instance-1");
  await page.getByRole("button", { name: "受控重启并接管" }).click();
  await page.getByLabel("现有 RCON 密码").fill("existing-rcon-secret");
  await page.getByRole("button", { name: "确认重启并接管" }).click();

  await expect.poll(() => actionBody).toEqual({ type: "restart", payload: { existing_rcon_password: "existing-rcon-secret" } });
  await expect(page.getByRole("dialog", { name: "受控重启并接管" })).toHaveCount(0);
});

test("运行中实例有玩家时警告并允许立即重启", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance }));
  let actionBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/instances/instance-1/actions", async (route) => {
    actionBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { job_id: "job-maintenance-restart" }, 202);
  });

  await page.goto("/instances/instance-1");
  await page.getByRole("button", { name: "重启", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "服务器内仍有玩家" });
  await expect(dialog.getByText(/当前有 1 名玩家在线/)).toBeVisible();
  await expect(dialog.getByText(/不会发送游戏内广播/)).toBeVisible();
  await dialog.getByRole("button", { name: "仍然重启" }).click();
  await expect.poll(() => actionBody).toEqual({ type: "restart" });
});

test("创建实例从运行节点绑定可用安装并提交难度、RCON 和 Tickrate", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/environments", (route) => json(route, { items: [environment] }));
  await page.route("**/api/v1/environments/environment-1/game-installations", (route) => json(route, { items: [gameInstallation] }));
  await page.route("**/api/v1/instances", async (route) => {
    if (route.request().method() === "GET") return json(route, { items: [] });
    const body = route.request().postDataJSON() as Record<string, unknown>;
    expect(body).toMatchObject({ node_id: node.id, game_installation_id: gameInstallation.id, name: "专家战役 #1", rcon_password: "panel-rcon",
      config: { mode: "mutation4", difficulty: "impossible", start_map: "c14m2_lighthouse", extra_args: ["-tickrate", "60"] } });
    expect(body.config).not.toHaveProperty("region");
    expect(body).not.toHaveProperty("operation");
    expect(body).not.toHaveProperty("path");
    expect(body).not.toHaveProperty("root_index");
    return json(route, { instance, job_id: "job-import" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance }));
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));

  await page.goto("/instances");
  await page.getByRole("button", { name: "创建第一个实例" }).click();
  await expect(page.getByText("选择已验证安装", { exact: true })).toHaveCount(0);
  await expect(page.getByText("手动填写游戏路径", { exact: true })).toHaveCount(0);
  await page.getByLabel("实例名称").fill("专家战役 #1");
  await page.getByRole("combobox", { name: "运行节点", exact: true }).selectOption(environment.id);
  await page.getByRole("combobox", { name: /^可用游戏安装/ }).selectOption(gameInstallation.id);
  await page.getByLabel("游戏模式").selectOption("mutation4");
  await page.getByLabel("难度").selectOption("impossible");
  await page.getByLabel("RCON 密码（可留空）").fill("panel-rcon");
  await expect(page.getByLabel("初始地图").locator("option")).toHaveCount(57);
  await page.getByLabel("初始地图").selectOption("c14m2_lighthouse");
  await page.getByRole("switch", { name: "启用 Tickrate" }).click();
  await page.getByLabel("Tickrate 数值").fill("60");
  await page.getByRole("dialog", { name: "创建游戏实例" }).getByRole("button", { name: "创建实例" }).click();
  await expect(page).toHaveURL(/\/instances\/instance-1/);
});

test("TOTP 挑战可切换为一次性恢复码登录", async ({ page }) => {
  let authenticatedSession = false;
  const loginBodies: Array<Record<string, unknown>> = [];
  await page.route("**/api/v1/auth/me", (route) => json(route, authenticatedSession
    ? { user: owner, csrf_token: "csrf-recovery" }
    : { error: { code: "unauthorized", message: "authentication required" } }, authenticatedSession ? 200 : 401));
  await page.route("**/api/v1/auth/login", async (route) => {
    const body = route.request().postDataJSON() as Record<string, unknown>;
    loginBodies.push(body);
    if (!body.recovery_code) return json(route, { error: { code: "totp_required", message: "需要双重验证码" } }, 401);
    authenticatedSession = true;
    return json(route, { user: owner, csrf_token: "csrf-recovery" });
  });
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/audit**", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/notifications", (route) => json(route, { items: [] }));

  await page.goto("/login");
  await page.getByLabel("账号").fill("admin");
  await page.locator('input[autocomplete="current-password"]').fill("correct-horse-battery-staple");
  await page.getByRole("button", { name: /安全登录/ }).click();
  await expect(page.getByLabel("双重验证码")).toBeVisible();
  await page.getByRole("button", { name: "无法使用认证器？改用恢复码" }).click();
  await page.getByLabel("一次性恢复码").fill("abcd-efgh-jklm-npqr");
  await page.getByRole("button", { name: /安全登录/ }).click();

  await expect(page).toHaveURL(/\/$/);
  expect(loginBodies).toHaveLength(2);
  expect(loginBodies[0]).not.toHaveProperty("recovery_code");
  expect(loginBodies[1]).toMatchObject({ recovery_code: "ABCD-EFGH-JKLM-NPQR" });
  expect(loginBodies[1]).not.toHaveProperty("totp_code");
});

test("启用 TOTP 后只显示一次恢复码", async ({ page }) => {
  const userWithoutTotp = { ...owner, totp_enabled: false };
  await authenticated(page, userWithoutTotp);
  await page.route("**/api/v1/users", (route) => json(route, { items: [userWithoutTotp] }));
  await page.route("**/api/v1/auth/totp/setup", (route) => json(route, {
    secret: "JBSWY3DPEHPK3PXP", otpauth_url: "otpauth://totp/L4D2:admin?secret=JBSWY3DPEHPK3PXP",
  }));
  let confirmBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/auth/totp/confirm", async (route) => {
    confirmBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { enabled: true, recovery_codes: ["AAAA-BBBB-CCCC-DDDD", "EEEE-FFFF-GGGG-HHHH"] });
  });

  await page.goto("/settings");
  await page.getByRole("button", { name: /TOTP 双重验证/ }).click();
  await page.getByRole("button", { name: "生成认证器密钥" }).click();
  await expect(page.getByText("JBSWY3DPEHPK3PXP", { exact: true })).toBeVisible();
  await page.getByLabel("6 位动态验证码").fill("123456");
  await page.getByRole("button", { name: "验证并启用" }).click();

  await expect(page.getByText("AAAA-BBBB-CCCC-DDDD")).toBeVisible();
  await expect(page.getByText("EEEE-FFFF-GGGG-HHHH")).toBeVisible();
  await expect(page.getByText(/唯一一次明文显示/)).toBeVisible();
  expect(confirmBody).toEqual({ code: "123456" });
});

test("API Token 限定到实例并只显示一次明文", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/users", (route) => json(route, { items: [owner] }));
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [node] }));
  await page.route("**/api/v1/instances", (route) => json(route, { items: [instance] }));
  let tokenBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/api-tokens", async (route) => {
    if (route.request().method() === "GET") return json(route, { items: [] });
    tokenBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, {
      api_token: { id: "token-1", user_id: owner.id, name: "维护脚本", scopes: ["operate"], instance_id: instance.id, created_at: "2026-08-01T00:00:00Z" },
      token: "l4d2_once_plaintext_token",
    }, 201);
  });

  await page.goto("/settings");
  await page.getByRole("button", { name: /API Token/ }).click();
  await page.getByRole("button", { name: "新建令牌" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("名称").fill("维护脚本");
  await dialog.getByLabel("权限范围").selectOption("operate");
  await dialog.getByLabel("资源范围").selectOption("instance");
  await dialog.getByText("实例", { exact: true }).locator("..").locator("select").selectOption(instance.id);
  await dialog.getByRole("button", { name: "创建", exact: true }).click();

  await expect(page.getByText("l4d2_once_plaintext_token")).toBeVisible();
  await expect(page.getByText(/只显示这一次/)).toBeVisible();
  expect(tokenBody).toMatchObject({ name: "维护脚本", scopes: ["operate"], instance_id: instance.id });
  expect(tokenBody?.expires_at).toEqual(expect.any(String));
});

test("删除停用团队账号需要确认并刷新列表", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/audit**", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/system/updates", (route) => json(route, { supported: false, history: [] }));
  const target = { ...owner, id: "delete-user", username: "delete-me", display_name: "待删除用户", role: "viewer", disabled: true, account_state: "pending_activation", role_bindings: [] };
  let deleted = false;
  await page.route("**/api/v1/users", (route) => json(route, { items: deleted ? [owner] : [owner, target] }));
  await page.route("**/api/v1/users/delete-user", async (route) => {
    expect(route.request().method()).toBe("DELETE");
    expect(route.request().headers()["x-csrf-token"]).toBe("csrf-test");
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    deleted = true;
    await route.fulfill({ status: 204 });
  });
  await page.goto("/settings");
  await page.locator(".user-row").filter({ hasText: "待删除用户" }).getByRole("button", { name: "管理" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("button", { name: "删除账号" })).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("user-delete-entry.png"), fullPage: true });
  await dialog.getByRole("button", { name: "删除账号" }).click();
  await expect(dialog.getByRole("button", { name: "确认永久删除" })).toBeDisabled();
  await dialog.getByRole("button", { name: "返回" }).click();
  expect(deleted).toBe(false);
  await dialog.getByRole("button", { name: "删除账号" }).click();
  await dialog.getByLabel("输入用户名以确认删除").fill(target.username);
  await page.screenshot({ path: test.info().outputPath("user-delete-confirm.png"), fullPage: true });
  await dialog.getByRole("button", { name: "确认永久删除" }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.locator(".user-row").filter({ hasText: "待删除用户" })).toHaveCount(0);
  expect(deleted).toBe(true);
  await page.locator(".user-row").getByRole("button", { name: "管理" }).click();
  await expect(dialog.getByRole("button", { name: "删除账号" })).toBeDisabled();
});

test("创建团队用户并绑定单个节点角色", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [node] }));
  await page.route("**/api/v1/instances", (route) => json(route, { items: [instance] }));
  let createBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/users", async (route) => {
    if (route.request().method() === "GET") return json(route, { items: [owner] });
    createBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { user: {
      id: "user-operator", username: "operator", display_name: "值班操作员", role: "operator",
      totp_enabled: false, account_state: "pending_activation", created_at: "2026-08-01T00:00:00Z", role_bindings: [{ role: "operator", scope_type: "node", node_id: node.id }],
    }, one_time_password: "l4d2p_once_0123456789abcdefghijklmnopqrstuv", expires_at: "2026-08-09T00:00:00Z" }, 201);
  });

  await page.goto("/settings");
  await page.getByRole("button", { name: "添加用户" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("用户名").fill("operator");
  await dialog.getByLabel("显示名称").fill("值班操作员");
  await dialog.getByLabel("角色").selectOption("operator");
  await dialog.getByRole("combobox", { name: "范围" }).selectOption("node");
  await dialog.getByText("节点", { exact: true }).locator("..").locator("select").selectOption(node.id);
  await dialog.getByRole("button", { name: "创建账号并生成密码" }).click();

  await expect(dialog.getByText("l4d2p_once_0123456789abcdefghijklmnopqrstuv")).toBeVisible();

  await expect.poll(() => createBody).toMatchObject({
    username: "operator", display_name: "值班操作员",
    role_bindings: [{ role: "operator", scope_type: "node", node_id: node.id }],
  });
  expect(createBody).not.toHaveProperty("password");
});

test("站内通知可标记已读且失败投递可手动重试", async ({ page }) => {
  await authenticated(page);
  let readNotice = false;
  let retriedDelivery = false;
  await page.route("**/api/v1/notifications**", (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/notice-1/read")) {
      readNotice = true;
      return route.fulfill({ status: 204 });
    }
    return json(route, { items: [{
      id: "notice-1", level: "critical", title: "节点已离线", message: "香港-01 已超过心跳阈值", read: false,
      created_at: "2026-08-01T00:00:00Z",
    }] });
  });
  await page.route("**/api/v1/notification-deliveries**", (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/delivery-1/retry")) {
      retriedDelivery = true;
      return route.fulfill({ status: 204 });
    }
    return json(route, { items: [{
      id: "delivery-1", notification_id: "notice-1", channel_id: "channel-1", channel_name: "运维 Webhook",
      status: "failed", attempt_count: 6, next_attempt_at: "2026-08-01T00:10:00Z", last_error: "HTTP 503",
      updated_at: "2026-08-01T00:00:00Z",
    }] });
  });

  await page.goto("/notifications");
  await expect(page.getByText("节点已离线")).toBeVisible();
  await page.getByRole("button", { name: "标为已读" }).click();
  await expect.poll(() => readNotice).toBeTruthy();
  await page.getByRole("button", { name: "重试" }).click();
  await expect.poll(() => retriedDelivery).toBeTruthy();
});

test("已停止实例卡片保留重启入口但只能启动", async ({ page }) => {
  await authenticated(page);
  const stopped = { ...instance, status: "stopped", pid: 0, started_at: null };
  await page.route("**/api/v1/instances", (route) => json(route, { items: [stopped] }));
  await page.goto("/instances");
  const card = page.locator(".instance-card").filter({ hasText: stopped.name });
  await expect(card.getByRole("button", { name: "启动" })).toBeVisible();
  await expect(card.getByRole("button", { name: "重启" })).toBeDisabled();
  await expect(card.getByRole("button", { name: "重启" })).toHaveAttribute("title", "服务器启动后才能重启");
});

test("空 RCON 的运行实例保持绿色状态并显示独立警告", async ({ page }) => {
  await authenticated(page);
  const runningWithoutRCON = { ...instance, status: "running", rcon_configured: false };
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: runningWithoutRCON }));
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));

  await page.goto("/instances/instance-1");
  await expect(page.locator(".instance-hero .status--running")).toContainText("运行中");
  await expect(page.getByText("RCON 密码为空", { exact: true })).toBeVisible();
});

test("RCON 玩家查询失败时显示节点返回的诊断", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance }));
  await page.route("**/api/v1/instances/instance-1/players", (route) => json(route, {
    items: [], status: "failed", message: "RCON 查询失败：RCON authentication failed",
  }));
  await page.route("**/api/v1/instances/instance-1/bans", (route) => json(route, { items: [] }));

  await page.goto("/instances/instance-1?tab=players");
  await expect(page.getByRole("alert")).toContainText("RCON 查询失败：RCON authentication failed");
});

test("实例控制台、玩家、配置、模组、插件、备份与计划任务", async ({ page }) => {
  test.setTimeout(60_000);
  await authenticated(page);
  const configWrites: Record<string, unknown>[] = [];
  let currentInstance = instance;
  await page.route("**/api/v1/instances/instance-1", async (route) => {
    if (route.request().method() === "PATCH") {
      const body = route.request().postDataJSON() as Record<string, unknown>;
      configWrites.push(body);
      const rconConfigured = body.rcon_password === "" ? false : body.rcon_password ? true : currentInstance.rcon_configured;
      currentInstance = { ...currentInstance, version: currentInstance.version + 1, rcon_configured: rconConfigured };
      return json(route, { instance: currentInstance, job_id: "job-config" });
    }
    return json(route, { instance: currentInstance });
  });
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/audit**", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/players", (route) => json(route, { items: [{
    name: "Zoey", steam_id: "STEAM_1:1:12345", ping: 42, connected_at: "2026-07-31T00:10:00Z",
  }] }));
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [{
    id: "package-1", name: "campaign.vpk", kind: "vpk", version: "v2", enabled: true,
    sha256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    path: "left4dead2/addons/campaign.vpk", installed_at: "2026-07-31T01:00:00Z",
  }, {
    id: "package-2", name: "admin-tools.smx", kind: "plugin", version: "1.0.0", enabled: true,
    sha256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    path: "left4dead2/addons/sourcemod/plugins/admin-tools.smx", installed_at: "2026-07-31T01:05:00Z",
  }, {
    id: "package-smart", name: "Autoscar", kind: "smart", enabled: true, file_count: 2,
    sha256: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", installed_at: "2026-07-31T01:10:00Z",
  }, {
    id: "package-sourcemod", name: "SourceMod", kind: "sourcemod", version: "1.12", enabled: true, file_count: 120,
    sha256: "1111111111111111111111111111111111111111111111111111111111111111", installed_at: "2026-07-31T01:15:00Z",
  }, {
    id: "package-metamod", name: "Metamod:Source", kind: "metamod", version: "1.12", enabled: true, file_count: 8,
    sha256: "2222222222222222222222222222222222222222222222222222222222222222", installed_at: "2026-07-31T01:20:00Z",
  }] }));
	let smartFileWrite: Record<string, unknown> | undefined;
	await page.route("**/api/v1/instances/instance-1/packages/package-smart", (route) => json(route, {
		package: { id: "package-smart", name: "Autoscar", kind: "smart", enabled: true, file_count: 2 },
		files: [{ package_id: "package-smart", instance_id: "instance-1", path: "left4dead2/cfg/sourcemod/autoscar.cfg", sha256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", size: 12, entrypoint: false, editable: true },
			{ package_id: "package-smart", instance_id: "instance-1", path: "left4dead2/addons/sourcemod/plugins/autoscar.smx", sha256: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", size: 20, entrypoint: true, editable: false }],
	}));
	await page.route("**/api/v1/instances/instance-1/packages/package-smart/files/content**", async (route) => {
		if (route.request().method() === "PUT") {
			smartFileWrite = route.request().postDataJSON() as Record<string, unknown>;
			return json(route, { job_id: "job-smart-file-write" }, 202);
		}
		return json(route, { path: "left4dead2/cfg/sourcemod/autoscar.cfg", content: "enabled = 1", size: 11, sha256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" });
	});
  await page.route("**/api/v1/instances/instance-1/packages/package-1/versions", (route) => json(route, { items: [{
    id: "package-version-1", package_id: "package-1", instance_id: "instance-1", version: "v2",
    sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", size: 1024,
    target_path: "left4dead2/addons/campaign.vpk", created_at: "2026-07-30T01:00:00Z",
  }] }));
  await page.route("**/api/v1/instances/instance-1/packages/package-2/versions", (route) => json(route, { items: [] }));
  let packageScan = false;
  await page.route("**/api/v1/instances/instance-1/packages/scan", (route) => {
    if (route.request().method() === "GET") return json(route, { scan: null });
    packageScan = route.request().method() === "POST" && route.request().postData() === null && Boolean(route.request().headers()["idempotency-key"]);
    return json(route, { job_id: "job-package-scan" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [] }));
  let pluginBackupLayout = "";
  await page.route("**/api/v1/instances/instance-1/plugin-backups", (route) => {
    pluginBackupLayout = (route.request().postDataJSON() as { layout: string }).layout;
    return json(route, { job_id: "job-plugin-backup" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/schedules", (route) => json(route, { items: [{
    id: "schedule-1", name: "每周维护", cron: "0 4 * * 1", action: "restart", enabled: true,
    next_run_at: "2026-08-03T04:00:00Z",
  }] }));

  let fileWrite: Record<string, unknown> | undefined;
  await page.route("**/api/v1/instances/instance-1/files**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/files/content")) {
      if (route.request().method() === "GET") return json(route, {
        path: "left4dead2/cfg/server.cfg", content: "hostname before\n", size: 16, sha256: "sha-before",
      });
      if (route.request().method() === "PUT") {
        fileWrite = route.request().postDataJSON() as Record<string, unknown>;
        return json(route, { job_id: "job-file-write" }, 202);
      }
    }
    return json(route, { items: [{
      name: "server.cfg", path: "left4dead2/cfg/server.cfg", type: "file", size: 16,
      modified_at: "2026-07-31T00:00:00Z",
    }] });
  });

  let logSearchBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/instances/instance-1/logs/search", async (route) => {
    logSearchBody = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { job_id: "job-log-search" }, 202);
  });
  await page.route("**/api/v1/instances/instance-1/logs/search/job-log-search", (route) => json(route, {
    items: [{ at: "2026-07-31T00:15:00Z", stream: "stderr", message: "ERROR workshop item unavailable" }],
    returned: 1, matched: 1, truncated: false,
  }));

  let packageRollback = false;
  await page.route("**/api/v1/instances/instance-1/packages/package-1/versions/package-version-1/rollback", (route) => {
    packageRollback = true;
    return json(route, { job_id: "job-package-rollback" }, 202);
  });
  let packageDelete = false;
  await page.route("**/api/v1/instances/instance-1/packages/package-1", (route) => {
    packageDelete = route.request().method() === "DELETE";
    return json(route, { job_id: "job-package-delete" }, 202);
  });
  const scheduleWrites: Array<{ method: string; body?: Record<string, unknown> }> = [];
  await page.route("**/api/v1/instances/instance-1/schedules/schedule-1", async (route) => {
    const method = route.request().method();
    scheduleWrites.push({ method, body: method === "PATCH" ? route.request().postDataJSON() as Record<string, unknown> : undefined });
    if (method === "DELETE") return route.fulfill({ status: 204 });
    return json(route, { schedule: { id: "schedule-1", enabled: false } });
  });

  const actions: Array<{ type: string; payload?: Record<string, unknown> }> = [];
  await page.route("**/api/v1/instances/instance-1/actions", async (route) => {
    actions.push(route.request().postDataJSON() as { type: string; payload?: Record<string, unknown> });
    await json(route, { job_id: `job-${actions.length}` }, 202);
  });

  await page.goto("/instances/instance-1?tab=console");
  await page.getByLabel("RCON 命令").fill("status");
  await page.getByRole("button", { name: "发送" }).click();
  await expect.poll(() => actions.some((item) => item.type === "rcon")).toBeTruthy();

  await page.getByLabel("关键词").fill("ERROR");
  await page.getByRole("button", { name: "检索" }).click();
  await expect(page.getByText("ERROR workshop item unavailable")).toBeVisible();
  expect(logSearchBody).toMatchObject({ keyword: "ERROR", limit: 500 });

  await page.getByRole("button", { name: "玩家" }).click();
  await expect(page.getByText("STEAM_1:1:12345")).toBeVisible();
  await page.getByRole("button", { name: "踢出" }).click();
  await expect.poll(() => actions.some((item) => item.type === "kick_player")).toBeTruthy();

  await page.getByRole("button", { name: "配置" }).click();
  await expect(page.getByText("配置版本与差异")).toHaveCount(0);
  await page.getByLabel("服务器名称").fill("专家战役 · 已更新");
  await page.getByRole("button", { name: "保存配置" }).click();
  await expect.poll(() => configWrites.length).toBe(1);
  expect(configWrites[0]).not.toHaveProperty("rcon_password");
  expect(configWrites[0].config).not.toHaveProperty("server_password");
  expect(configWrites[0].config).not.toHaveProperty("region");
  expect(configWrites[0].config).not.toHaveProperty("maintenance_cron");
  await expect(page.getByText("配置已保存", { exact: true })).toBeVisible();
  await page.getByLabel("RCON 密码").fill("new-rcon-secret");
  await page.getByRole("button", { name: "保存配置" }).click();
  await expect.poll(() => configWrites.length).toBe(2);
  expect(configWrites[1]).toMatchObject({ rcon_password: "new-rcon-secret" });
  expect(configWrites[1].config).not.toHaveProperty("server_password");
  await expect(page.getByLabel("RCON 密码")).toHaveAttribute("placeholder", "已保存（输入新密码可替换）");
  await page.getByRole("button", { name: "清除 RCON 密码" }).click();
  await page.getByRole("button", { name: "保存配置" }).click();
  await expect.poll(() => configWrites.length).toBe(3);
  expect(configWrites[2]).toHaveProperty("rcon_password", "");
  await expect(page.getByLabel("RCON 密码")).toHaveAttribute("placeholder", "尚未设置");

  await page.getByRole("button", { name: "模组" }).click();
  await page.getByRole("button", { name: "扫描已安装" }).click();
  await expect.poll(() => packageScan).toBeTruthy();
  await expect(page.getByText(/扫描任务已提交/)).toBeVisible();
  await expect(page.getByText(/当前实例未停止，不能停用已启用的模组/)).toBeVisible();
  await expect(page.getByRole("button", { name: "停用 campaign.vpk" })).toBeDisabled();
  await page.getByLabel("按名称搜索模组").fill("campaign");
  await expect(page.getByLabel("模组排序")).toHaveValue("installed_desc");
  await expect(page.getByLabel("campaign.vpk 可回退版本")).toBeVisible();
  await expect(page.getByText("admin-tools.smx", { exact: true })).toHaveCount(0);
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "回退", exact: true }).click();
  await expect.poll(() => packageRollback).toBeTruthy();
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "删除 campaign.vpk" }).click();
  await expect.poll(() => packageDelete).toBeTruthy();
  await page.getByRole("button", { name: "安装模组" }).click();
  await expect(page.getByLabel("类型").locator("option")).toHaveText([
    "Workshop 项目 / 合集 ID", "VPK 文件", "QQ闪传（玩家上传 VPK）",
  ]);
  await page.getByLabel("项目或合集 ID").fill("123456789");
  await page.getByRole("button", { name: "提交安装" }).click();
  await expect.poll(() => actions.some((item) => item.type === "install_package")).toBeTruthy();

  await page.getByRole("button", { name: "插件", exact: true }).click();
  await expect(page.getByText("admin-tools.smx", { exact: true })).toBeVisible();
  await expect(page.getByText("campaign.vpk", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^查看 .* 详情$/ }).first()).toHaveAttribute("aria-label", "查看 Metamod:Source 详情");
  await expect(page.getByRole("button", { name: /^查看 .* 详情$/ }).nth(1)).toHaveAttribute("aria-label", "查看 SourceMod 详情");
  await expect(page.getByRole("button", { name: "删除 Metamod:Source" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "删除 SourceMod" })).toBeDisabled();
  await page.getByRole("button", { name: "查看 Autoscar 详情" }).click();
  await expect(page.getByText("left4dead2/cfg/sourcemod/autoscar.cfg")).toBeVisible();
  await expect(page.getByText("left4dead2/addons/sourcemod/plugins/autoscar.smx")).toBeVisible();
  await page.getByRole("button", { name: "编辑", exact: true }).click();
  await page.getByLabel("文件内容").fill("enabled = 0");
  await page.getByRole("button", { name: "提交写入" }).click();
  await expect.poll(() => smartFileWrite).toMatchObject({ content: "enabled = 0", expected_sha256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" });
  await page.getByRole("button", { name: "安装插件" }).click();
  await expect(page.getByLabel("类型").locator("option")).toHaveText([
    "智能识别", "单文件 SMX 插件", "单文件原生扩展", "官方 SourceMod 稳定版", "官方 Metamod:Source 稳定版",
    "第三方 SourceMod 整包", "第三方 Metamod 整包",
  ]);
  await page.getByLabel("类型").selectOption("metamod");
  await page.getByRole("button", { name: "提交安装" }).click();
  await expect.poll(() => actions.some((item) => item.type === "install_package" && item.payload?.kind === "metamod")).toBeTruthy();

  await page.getByRole("button", { name: "安装插件" }).click();
  for (const pluginKind of ["smart", "sourcemod", "metamod", "sourcemod_url", "metamod_url"]) {
    await page.getByLabel("类型").selectOption(pluginKind);
    await expect(page.getByLabel("传输方式").locator('option[value="qq_flash"]')).toHaveText("QQ 闪传");
  }
  await page.getByLabel("类型").selectOption("metamod_url");
  await page.getByLabel("传输方式").selectOption("qq_flash");
  await page.getByLabel("QQ 闪传分享链接").fill("QQ 分享：https://qfile.qq.com/q/PluginPackage1");
  await page.getByRole("dialog").getByRole("checkbox", { name: /我了解第三方原生代码风险/ }).check();
  await page.getByRole("button", { name: "提交安装" }).click();
  await expect.poll(() => actions.some((item) => item.type === "install_package" && item.payload?.kind === "metamod_url" && item.payload?.source_transport === "qq_flash")).toBeTruthy();

  await page.getByRole("button", { name: "文件", exact: true }).click();
  await expect(page.getByRole("button", { name: "cfg", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "addons", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "logs", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "server.cfg", exact: true }).first().click();
  await page.getByLabel("文件内容").fill("hostname after\n");
  await page.getByRole("button", { name: "提交写入" }).click();
  await expect.poll(() => fileWrite).toMatchObject({ content: "hostname after\n", expected_sha256: "sha-before" });

  await page.getByRole("button", { name: "备份" }).click();
  await page.getByRole("button", { name: "立即备份" }).click();
  await expect.poll(() => actions.some((item) => item.type === "backup")).toBeTruthy();
  await page.getByRole("button", { name: "备份插件" }).click();
  await page.getByRole("radio", { name: /不保留原路径/ }).click();
  await page.getByRole("button", { name: "开始备份" }).click();
  await expect.poll(() => pluginBackupLayout).toBe("compact");

  await page.getByRole("button", { name: "计划任务" }).click();
  await page.getByRole("button", { name: "停用 每周维护" }).click();
  await expect.poll(() => scheduleWrites.some((item) => item.method === "PATCH" && item.body?.enabled === false)).toBeTruthy();
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "删除 每周维护" }).click();
  await expect.poll(() => scheduleWrites.some((item) => item.method === "DELETE")).toBeTruthy();
  await page.getByRole("button", { name: "新增计划" }).first().click();
  await page.getByLabel("计划名称").fill("每周维护");
  await page.getByLabel("执行动作").selectOption("restart");
  await page.getByRole("button", { name: "创建计划" }).click();
  await expect.poll(() => actions.some((item) => item.type === "create_schedule")).toBeTruthy();
});

test("安装模组可选择 QQ闪传批量导入", async ({ page }) => {
  await authenticated(page);
  let qqFlashImport: Record<string, unknown> | undefined;
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance }));
  await page.route("**/api/v1/jobs**", (route) => {
    const background = new URL(route.request().url()).searchParams.get("background");
    if (background === "only") return json(route, { items: [{
      id: "job-a2s-probe", node_id: node.id, instance_id: instance.id, type: "a2s.info",
      status: "succeeded", progress: 100, message: "operation completed", created_at: "2026-08-04T00:01:00Z",
    }], background_summary: { targets: 1, active: 0, failed: 0, latest_at: "2026-08-04T00:01:00Z" } });
    const pluginInstall = {
      id: "job-plugin-install", node_id: node.id, instance_id: instance.id, type: "install_mod", display_type: "install_plugin",
      status: "failed", progress: 10, message: "download official package from trusted sources failed", created_at: "2026-08-03T23:59:00Z",
    };
    return json(route, { items: qqFlashImport ? [{
      id: "job-qq-flash-import", node_id: node.id, instance_id: instance.id, type: "qq_flash_import",
      status: "running", progress: 42, message: "downloading QQ flash files", created_at: "2026-08-04T00:00:00Z",
    }, pluginInstall] : [pluginInstall], background_summary: { targets: 1, active: 0, failed: 0, latest_at: "2026-08-04T00:01:00Z" } });
  });
  await page.route("**/api/v1/instances/instance-1/packages", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/files**", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/packages/qq-flash", async (route) => {
    qqFlashImport = route.request().postDataJSON() as Record<string, unknown>;
    return json(route, { job_id: "job-qq-flash-import" }, 202);
  });

  await page.goto("/instances/instance-1?tab=mods");
  await page.getByRole("button", { name: "安装模组", exact: true }).click();
  await page.getByLabel("类型").selectOption("qq_flash");
  await expect(page.getByLabel("固定目标目录")).toHaveValue("left4dead2/addons");
  await expect(page.getByLabel("固定目标目录")).toHaveAttribute("readonly", "");
  await page.getByLabel("QQ闪传分享链接").fill("链接：https://qfile.qq.com/q/Example123");
  await expect(page.getByText(/仅接收经过校验的 VPK/)).toBeVisible();
  await expect(page.getByText(/ZIP、7Z、RAR/)).toBeVisible();
  await page.getByRole("button", { name: "导入 VPK" }).click();

  await expect.poll(() => qqFlashImport).toEqual({
    share_url: "链接：https://qfile.qq.com/q/Example123",
  });

  await page.goto("/jobs");
  await expect(page.getByText("QQ闪传 VPK 导入", { exact: true })).toBeVisible();
  await expect(page.getByText("安装插件", { exact: true })).toBeVisible();
  await expect(page.getByText("安装模组", { exact: true })).toHaveCount(0);
  await expect(page.getByText("42%", { exact: true })).toBeVisible();
  await expect(page.getByText("查询服务器状态", { exact: true })).toHaveCount(0);
  await page.getByLabel("任务分类").selectOption("background");
  await expect(page.getByText("查询服务器状态", { exact: true })).toBeVisible();

  await page.goto("/instances/instance-1?tab=files");
  await page.getByRole("button", { name: "上传或新建" }).click();
  const fileDialog = page.getByRole("dialog", { name: "上传或新建文件" });
  await expect(fileDialog.getByLabel("实例内相对路径")).toBeVisible();
  await expect(fileDialog.getByLabel("从本机选择文件")).toBeVisible();
  await expect(fileDialog.getByText("QQ闪传", { exact: true })).toHaveCount(0);
});

test("地图管理支持导入、警告、运行中热更新分配与两阶段删除", async ({ page }) => {
  await authenticated(page);
  const stopped = { ...instance, id: "instance-2", name: "停止实例", status: "stopped", port: 27016, pid: 0, started_at: null };
  const assignedMap = {
    id: "11111111-1111-1111-1111-111111111111", node_id: node.id, name: "字典警告地图", health: "healthy",
    aggregate_sha256: "a".repeat(64), total_size: 1024, source_type: "qq_flash", source_name: "campaign.zip",
    removed_files: 3, removed_bytes: 4096, artifacts: [{ id: "artifact-1", ordinal: 0, sha256: "b".repeat(64), size: 1024 }],
    bsps: [{ virtual_path: "maps/custom_map.bsp", start_name: "custom_map", dictionary_present: false }],
    warnings: [{ code: "missing_stringtable_dictionary", message: "BSP 不包含 stringtable_dictionary.dct", virtual_path: "maps/custom_map.bsp" }],
    replicas: [{ filesystem_id: "linux:1", health: "healthy" }],
    assignments: [{ map_id: "11111111-1111-1111-1111-111111111111", instance_id: instance.id, restart_required: false, created_at: "2026-08-05T00:00:00Z", updated_at: "2026-08-05T00:00:00Z" }],
    created_at: "2026-08-05T00:00:00Z", updated_at: "2026-08-05T00:00:00Z",
  };
  const deletableMap = { ...assignedMap, id: "22222222-2222-2222-2222-222222222222", name: "可删除地图", aggregate_sha256: "c".repeat(64), assignments: [], warnings: [] };
  await page.route("**/api/v1/nodes", (route) => json(route, { items: [node] }));
  await page.route("**/api/v1/instances", (route) => json(route, { items: [instance, stopped] }));
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));
  const writes: Array<{ method: string; path: string; body?: unknown }> = [];
  await page.route("**/api/v1/maps**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (request.method() === "GET" && path === "/api/v1/maps") return json(route, { items: [assignedMap, deletableMap] });
    writes.push({ method: request.method(), path, body: request.postDataJSON() });
    if (path.endsWith("/delete-confirmations")) return json(route, { confirmation_token: "mdc_once", expires_at: "2026-08-05T00:05:00Z" }, 201);
    return json(route, { job_id: `job-${writes.length}` }, 202);
  });
  await page.route("**/api/v1/nodes/node-1/maps/imports", async (route) => {
    writes.push({ method: route.request().method(), path: new URL(route.request().url()).pathname, body: route.request().postDataJSON() });
    return json(route, { job_id: "job-import-map" }, 202);
  });
  await page.goto("/maps");
  await expect(page.getByRole("heading", { name: "地图管理" })).toBeVisible();
  await expect(page.getByText("地图占用空间")).toBeVisible();
  await expect(page.getByText("处理后容量")).toHaveCount(0);
  const assignedCard = page.locator(".map-card").filter({ hasText: "字典警告地图" });
  await expect(assignedCard.getByText("处理后 VPK 大小：")).toBeVisible();
  await expect(assignedCard.getByText("BSP 不包含 stringtable_dictionary.dct")).toHaveCount(0);
  await assignedCard.getByRole("button", { name: "展开详情" }).click();
  await expect(page.getByText("BSP 不包含 stringtable_dictionary.dct")).toBeVisible();
  await expect(assignedCard.getByText("已生效")).toBeVisible();

  await assignedCard.getByRole("button", { name: "分配实例" }).click();
  const runningAssigned = page.getByRole("dialog", { name: /分配地图/ }).getByRole("checkbox").first();
  await expect(runningAssigned).toBeDisabled();
  await page.getByRole("dialog", { name: /分配地图/ }).getByRole("button", { name: "取消" }).click();
  await assignedCard.getByRole("button", { name: "校验" }).click();
  await expect.poll(() => writes.some((item) => item.path.endsWith(`/${assignedMap.id}/verify`))).toBeTruthy();

  await page.getByRole("button", { name: "QQ 闪传导入" }).click();
  await page.getByLabel("目标节点").selectOption(node.id);
  await page.getByLabel("QQ 闪传链接").fill("https://qfile.qq.com/q/Example123");
  await page.getByRole("button", { name: "开始导入" }).click();
  await expect.poll(() => writes.some((item) => item.path.endsWith("/nodes/node-1/maps/imports") && (item.body as { share_url?: string })?.share_url?.includes("Example123"))).toBeTruthy();

  page.once("dialog", (dialog) => dialog.accept());
  const deletableCard = page.locator(".map-card").filter({ hasText: "可删除地图" });
  await deletableCard.getByRole("button", { name: "永久删除" }).click();
  await expect.poll(() => writes.some((item) => item.path.endsWith(`/${deletableMap.id}/delete-confirmations`))).toBeTruthy();
  await expect.poll(() => writes.some((item) => item.method === "DELETE" && item.path.endsWith(`/maps/${deletableMap.id}`) && (item.body as { confirmation_token?: string })?.confirmation_token === "mdc_once")).toBeTruthy();
});

test("缺少节点公开地址时阻止公开实例", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: { ...instance, node_public_address: "" } }));

  await page.goto("/instances/instance-1?tab=config");
  await expect(page.getByText(/需要可供玩家访问的节点地址/)).toBeVisible();
  const publicSwitch = page.getByRole("switch", { name: "公开状态页" });
  await publicSwitch.click();
  await expect(publicSwitch).toHaveAttribute("aria-checked", "false");
  await expect(page.getByText("请先在节点页设置公开连接地址")).toBeVisible();
});

test("实例运行中禁用备份恢复并显示停服提示", async ({ page }) => {
  await authenticated(page);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance }));
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [{
    id: "backup-1", instance_id: instance.id, name: "daily-20260801.zip", kind: "daily",
    platform: "linux", size: 4096, checksum: "a".repeat(64), created_at: "2026-08-01T04:00:00Z",
  }] }));

  await page.goto("/instances/instance-1?tab=backups");
  await expect(page.getByText(/必须先停止服务器/)).toBeVisible();
  await expect(page.getByRole("button", { name: "恢复", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "恢复", exact: true })).toHaveAttribute("title", "请先停止实例再恢复");
  await expect(page.getByRole("button", { name: "上传并恢复" })).toBeDisabled();
});

test("停止实例可串行分块上传跨节点备份并排队恢复", async ({ page }) => {
  await authenticated(page);
  const stopped = { ...instance, status: "stopped", pid: undefined, started_at: undefined };
  const archive = Buffer.alloc((1 << 20) + 17, 0x5a);
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: stopped }));
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [] }));
  let uploadBody: Record<string, unknown> | undefined;
  let receivedSize = 0;
  let uploadStatus = "uploading";
  const chunkOffsets: number[] = [];
  let pendingChunk: { offset: number; length: number; jobID: string; statusPolled: boolean } | undefined;
  const idempotencyKeys: string[] = [];
  let restoreQueued = false;
  let genericJobDetailReads = 0;
  const uploadRecord = () => ({
    id: "upload-cross-node", instance_id: instance.id, name: "cross-node.zip", platform: "windows/amd64",
    size: archive.length, sha256: String(uploadBody?.sha256 || ""), received_size: receivedSize, status: uploadStatus,
    expires_at: "2099-08-02T00:00:00Z", created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  });
  await page.route("**/api/v1/instances/instance-1/backup-uploads**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    if (path.endsWith("/chunks") && method === "POST") {
      const body = route.request().postDataJSON() as { offset: number; chunk_sha256: string; content_base64: string };
      const length = Buffer.from(body.content_base64, "base64").length;
      expect(body.offset).toBe(receivedSize);
      expect(body.chunk_sha256).toMatch(/^[a-f0-9]{64}$/);
      expect(length).toBeLessThanOrEqual(1 << 20);
      chunkOffsets.push(body.offset);
      const jobID = `job-upload-chunk-${chunkOffsets.length}`;
      pendingChunk = { offset: body.offset, length, jobID, statusPolled: false };
      idempotencyKeys.push(route.request().headers()["idempotency-key"] || "");
      return json(route, { job_id: jobID, status: "queued", idempotent_replay: false }, 202);
    }
    if (path.includes("/chunks/") && method === "GET") {
      expect(pendingChunk).toBeDefined();
      expect(path.endsWith(`/${pendingChunk?.jobID}`)).toBeTruthy();
      pendingChunk!.statusPolled = true;
      return json(route, { job_id: pendingChunk!.jobID, status: "succeeded" });
    }
    if (path.endsWith("/complete") && method === "POST") {
      expect(uploadStatus).toBe("ready");
      expect(route.request().postData()).toBeNull();
      restoreQueued = true;
      idempotencyKeys.push(route.request().headers()["idempotency-key"] || "");
      return json(route, { job_id: "job-cross-node-restore", status: "queued", idempotent_replay: false }, 202);
    }
    if (method === "GET") {
      if (pendingChunk?.statusPolled && receivedSize === pendingChunk.offset) {
        receivedSize += pendingChunk.length;
        pendingChunk = undefined;
        if (receivedSize === archive.length) uploadStatus = "ready";
      }
      return json(route, { upload: uploadRecord() });
    }
    uploadBody = route.request().postDataJSON() as Record<string, unknown>;
    idempotencyKeys.push(route.request().headers()["idempotency-key"] || "");
    return json(route, { upload: uploadRecord(), idempotent_replay: false }, 201);
  });
  await page.route("**/api/v1/jobs**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/jobs") return json(route, { items: [] });
    if (path === "/api/v1/jobs/terminal-conflicts") return json(route, { items: [] });
    genericJobDetailReads += 1;
    return json(route, { error: { code: "forbidden", message: "jobs.read is not granted" } }, 403);
  });

  await page.goto("/instances/instance-1?tab=backups");
  await page.getByRole("button", { name: "上传并恢复" }).click();
  const dialog = page.getByRole("dialog", { name: "上传备份并恢复" });
  await dropFile(page, dialog.getByTestId("file-dropzone"), { name: "cross-node.zip", mimeType: "application/zip", buffer: archive });
  await dialog.getByLabel("备份来源平台").selectOption("windows/amd64");
  await dialog.getByRole("button", { name: "开始上传并恢复" }).click();

  await expect(page).toHaveURL(/\/jobs$/);
  expect(uploadBody).toMatchObject({ name: "cross-node.zip", platform: "windows/amd64", size: archive.length });
  expect(uploadBody?.sha256).toMatch(/^[a-f0-9]{64}$/);
  expect(chunkOffsets).toEqual([0, 1 << 20]);
  expect(restoreQueued).toBeTruthy();
  expect(new Set(idempotencyKeys).size).toBe(idempotencyKeys.length);
  expect(idempotencyKeys.every(Boolean)).toBeTruthy();
  expect(genericJobDetailReads).toBe(0);
});

test("备份分块失败后清除旧幂等键并可独立重试", async ({ page }) => {
  await authenticated(page);
  const stopped = { ...instance, status: "stopped", pid: undefined, started_at: undefined };
  const archive = Buffer.from("retry failed backup upload chunk");
  const digest = createHash("sha256").update(archive).digest("hex");
  const uploadID = "upload-retry-failed-chunk";
  let receivedSize = 0;
  let uploadStatus = "uploading";
  let chunkCalls = 0;
  let secondChunkStatusPolled = false;
  let genericJobDetailReads = 0;
  const chunkKeys: string[] = [];
  const polledChunkJobs: string[] = [];
  const uploadRecord = () => ({
    id: uploadID, instance_id: instance.id, name: "retry-chunk.zip", platform: "linux/amd64",
    size: archive.length, sha256: digest, received_size: receivedSize, status: uploadStatus,
    expires_at: "2099-08-02T00:00:00Z", created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  });
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: stopped }));
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/backup-uploads**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    if (path.endsWith("/chunks") && method === "POST") {
      chunkCalls += 1;
      chunkKeys.push(route.request().headers()["idempotency-key"] || "");
      return json(route, { job_id: `failed-chunk-job-${chunkCalls}`, status: "queued", idempotent_replay: false }, 202);
    }
    if (path.includes("/chunks/") && method === "GET") {
      const jobID = path.split("/").at(-1) || "";
      polledChunkJobs.push(jobID);
      if (jobID === "failed-chunk-job-1") return json(route, { job_id: jobID, status: "failed" });
      secondChunkStatusPolled = true;
      return json(route, { job_id: jobID, status: "succeeded" });
    }
    if (path.endsWith("/complete") && method === "POST") {
      expect(uploadStatus).toBe("ready");
      return json(route, { job_id: "retry-chunk-restore", status: "queued", idempotent_replay: false }, 202);
    }
    if (method === "GET") {
      if (secondChunkStatusPolled && receivedSize === 0) {
        receivedSize = archive.length;
        uploadStatus = "ready";
      }
      return json(route, { upload: uploadRecord() });
    }
    return json(route, { upload: uploadRecord(), idempotent_replay: false }, 201);
  });
  await page.route("**/api/v1/jobs**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/jobs") return json(route, { items: [] });
    if (path === "/api/v1/jobs/terminal-conflicts") return json(route, { items: [] });
    genericJobDetailReads += 1;
    return json(route, { error: { code: "forbidden", message: "jobs.read is not granted" } }, 403);
  });

  await page.goto("/instances/instance-1?tab=backups");
  await page.getByRole("button", { name: "上传并恢复" }).click();
  const dialog = page.getByRole("dialog", { name: "上传备份并恢复" });
  await dialog.getByLabel("ZIP 备份文件").setInputFiles({ name: "retry-chunk.zip", mimeType: "application/zip", buffer: archive });
  await dialog.getByLabel("备份来源平台").selectOption("linux/amd64");
  await dialog.getByRole("button", { name: "开始上传并恢复" }).click();
  await expect(dialog.getByRole("alert")).toContainText("可以重试当前分片");
  expect(chunkCalls).toBe(1);

  await dialog.getByRole("button", { name: "开始上传并恢复" }).click();
  await expect(page).toHaveURL(/\/jobs$/);
  expect(chunkCalls).toBe(2);
  expect(chunkKeys).toHaveLength(2);
  expect(chunkKeys.every(Boolean)).toBeTruthy();
  expect(chunkKeys[1]).not.toBe(chunkKeys[0]);
  expect(polledChunkJobs).toEqual(["failed-chunk-job-1", "failed-chunk-job-2"]);
  expect(genericJobDetailReads).toBe(0);
});

test("可放弃持久化上传会话并排队节点暂存清理", async ({ page }) => {
  await authenticated(page);
  const stopped = { ...instance, status: "stopped", pid: undefined, started_at: undefined };
  const uploadID = "upload-to-abandon";
  const digest = "a".repeat(64);
  await page.addInitScript((stored) => {
    localStorage.setItem(`l4d2panel.backup-upload.v1:${stored.instanceId}`, JSON.stringify(stored));
  }, {
    version: 1,
    instanceId: instance.id,
    createKey: "backup-upload:created",
    uploadId: uploadID,
    name: "damaged.zip",
    size: 4096,
    sha256: digest,
    platform: "linux/amd64",
    expiresAt: "2099-08-02T00:00:00Z",
    updatedAt: "2026-08-01T00:00:00Z",
  });
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: stopped }));
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [] }));
  let deleteCalls = 0;
  let abortKey = "";
  await page.route("**/api/v1/instances/instance-1/backup-uploads/**", async (route) => {
    const request = route.request();
    if (request.method() === "DELETE") {
      deleteCalls += 1;
      abortKey = request.headers()["idempotency-key"] || "";
      expect(request.postData()).toBeNull();
      return json(route, { job_id: "job-upload-abort", status: "queued", idempotent_replay: false }, 202);
    }
    return json(route, { upload: {
      id: uploadID, instance_id: instance.id, name: "damaged.zip", platform: "linux/amd64", size: 4096,
      sha256: digest, received_size: 1024, status: "uploading", expires_at: "2099-08-02T00:00:00Z",
      created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
    } });
  });

  await page.goto("/instances/instance-1?tab=backups");
  await page.getByRole("button", { name: "上传并恢复" }).click();
  const dialog = page.getByRole("dialog", { name: "上传备份并恢复" });
  await expect(dialog.getByText(/有可恢复的上传会话/)).toBeVisible();
  page.once("dialog", (confirmation) => confirmation.accept());
  await dialog.getByRole("button", { name: "放弃会话" }).click();

  await expect(page.getByText("上传会话已放弃，节点暂存清理任务已排队")).toBeVisible();
  await expect(dialog.getByText(/有可恢复的上传会话/)).toHaveCount(0);
  expect(deleteCalls).toBe(1);
  expect(abortKey).toBeTruthy();
  expect(await page.evaluate(() => localStorage.getItem("l4d2panel.backup-upload.v1:instance-1"))).toBeNull();
});

test("尚未创建服务端上传会话时放弃操作只清理浏览器记录", async ({ page }) => {
  await authenticated(page);
  const stopped = { ...instance, status: "stopped", pid: undefined, started_at: undefined };
  await page.addInitScript((stored) => {
    localStorage.setItem(`l4d2panel.backup-upload.v1:${stored.instanceId}`, JSON.stringify(stored));
  }, {
    version: 1,
    instanceId: instance.id,
    createKey: "backup-upload:not-created",
    name: "local-only.zip",
    size: 1024,
    sha256: "b".repeat(64),
    platform: "windows/amd64",
    updatedAt: "2026-08-01T00:00:00Z",
  });
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: stopped }));
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [] }));
  let uploadMutations = 0;
  await page.route("**/api/v1/instances/instance-1/backup-uploads**", (route) => {
    if (route.request().method() !== "GET") uploadMutations += 1;
    return json(route, { error: { code: "unexpected", message: "unexpected upload request" } }, 500);
  });

  await page.goto("/instances/instance-1?tab=backups");
  await page.getByRole("button", { name: "上传并恢复" }).click();
  const dialog = page.getByRole("dialog", { name: "上传备份并恢复" });
  page.once("dialog", (confirmation) => confirmation.accept());
  await dialog.getByRole("button", { name: "放弃会话" }).click();

  await expect(page.getByText("本地上传记录已清除")).toBeVisible();
  expect(uploadMutations).toBe(0);
  expect(await page.evaluate(() => localStorage.getItem("l4d2panel.backup-upload.v1:instance-1"))).toBeNull();
});

test("恢复异步失败后重开页面复用原上传会话且不重传分块", async ({ page }) => {
  await authenticated(page);
  const degraded = { ...instance, status: "degraded", pid: undefined, started_at: undefined };
  const archive = Buffer.from("verified portable backup");
  const uploadID = "upload-resume-after-restore-failure";
  let uploadDigest = "";
  let receivedSize = 0;
  let uploadStatus = "uploading";
  let restoreJobID = "";
  let restoreJobStatus = "queued";
  let createCalls = 0;
  let completeCalls = 0;
  let chunkCalls = 0;
  let chunkQueued = false;
  let createKey = "";
  const completeKeys: string[] = [];
  const requestedUploadIDs: string[] = [];
  const completedUploadIDs: string[] = [];
  const uploadRecord = () => ({
    id: uploadID,
    instance_id: instance.id,
    name: "retry.zip",
    platform: "linux/amd64",
    size: archive.length,
    sha256: uploadDigest,
    received_size: receivedSize,
    status: uploadStatus,
    restore_job_id: restoreJobID || undefined,
    expires_at: "2099-08-02T00:00:00Z",
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-01T00:00:00Z",
  });
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: degraded }));
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/backup-uploads**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    if (path.endsWith("/chunks") && method === "POST") {
      chunkCalls += 1;
      chunkQueued = true;
      expect(path).toContain(`/${uploadID}/chunks`);
      return json(route, { job_id: "job-upload-resume-chunk", status: "queued", idempotent_replay: false }, 202);
    }
    if (path.endsWith("/complete") && method === "POST") {
      completeCalls += 1;
      completedUploadIDs.push(path.split("/").at(-2) || "");
      completeKeys.push(route.request().headers()["idempotency-key"] || "");
      restoreJobID = completeCalls === 1 ? "job-restore-first" : "job-restore-retry";
      restoreJobStatus = "queued";
      uploadStatus = "restoring";
      return json(route, { job_id: restoreJobID, status: "queued", idempotent_replay: false }, 202);
    }
    if (method === "GET") {
      requestedUploadIDs.push(path.split("/").at(-1) || "");
      if (chunkQueued) {
        receivedSize = archive.length;
        uploadStatus = "ready";
        chunkQueued = false;
      }
      return json(route, { upload: uploadRecord() });
    }
    expect(path).toBe("/api/v1/instances/instance-1/backup-uploads");
    createCalls += 1;
    expect(createCalls).toBe(1);
    const body = route.request().postDataJSON() as { sha256: string };
    uploadDigest = body.sha256;
    createKey = route.request().headers()["idempotency-key"] || "";
    return json(route, { upload: uploadRecord(), idempotent_replay: false }, 201);
  });
  await page.route("**/api/v1/jobs**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/jobs") return json(route, { items: [] });
    const jobID = path.split("/").at(-1) || "";
    if (jobID === "job-upload-resume-chunk") {
      receivedSize = archive.length;
      uploadStatus = "ready";
      return json(route, { job: {
        id: jobID, node_id: node.id, instance_id: instance.id, type: "backup.upload.chunk", status: "succeeded",
        progress: 100, created_at: "2026-08-01T00:00:00Z", finished_at: "2026-08-01T00:00:01Z",
      } });
    }
    return json(route, { job: {
      id: jobID, node_id: node.id, instance_id: instance.id, type: "restore", status: restoreJobStatus,
      progress: restoreJobStatus === "failed" ? 100 : 10, message: restoreJobStatus === "failed" ? "restore verification failed" : undefined,
      created_at: "2026-08-01T00:00:00Z", finished_at: restoreJobStatus === "failed" ? "2026-08-01T00:00:02Z" : undefined,
    } });
  });

  await page.goto("/instances/instance-1?tab=backups");
  await expect(page.getByRole("button", { name: "上传并恢复" })).toBeEnabled();
  await page.getByRole("button", { name: "上传并恢复" }).click();
  const dialog = page.getByRole("dialog", { name: "上传备份并恢复" });
  await expect(dialog.getByText(/降级状态下 Agent/)).toBeVisible();
  await dialog.getByLabel("ZIP 备份文件").setInputFiles({ name: "retry.zip", mimeType: "application/zip", buffer: archive });
  await dialog.getByLabel("备份来源平台").selectOption("linux/amd64");
  await dialog.getByRole("button", { name: "开始上传并恢复" }).click();

  await expect(page).toHaveURL(/\/jobs$/);
  expect(createCalls).toBe(1);
  expect(chunkCalls).toBe(1);
  expect(completeCalls).toBe(1);
  expect(createKey).toBeTruthy();
  const storedAfterQueue = await page.evaluate(() => JSON.parse(localStorage.getItem("l4d2panel.backup-upload.v1:instance-1") || "null"));
  expect(storedAfterQueue).toMatchObject({ createKey, uploadId: uploadID, restoreJobId: "job-restore-first" });

  restoreJobStatus = "failed";
  uploadStatus = "failed";
  await page.goto("/instances/instance-1?tab=backups");
  await page.getByRole("button", { name: "上传并恢复" }).click();
  const reopened = page.getByRole("dialog", { name: "上传备份并恢复" });
  await expect(reopened.getByText(/有可恢复的上传会话/)).toBeVisible();
  await reopened.getByLabel("ZIP 备份文件").setInputFiles({ name: "retry.zip", mimeType: "application/zip", buffer: archive });
  await expect(reopened.getByLabel("备份来源平台")).toHaveValue("linux/amd64");
  const chunksBeforeRetry = chunkCalls;
  await reopened.getByRole("button", { name: "开始上传并恢复" }).click();

  await expect(page).toHaveURL(/\/jobs$/);
  expect(createCalls).toBe(1);
  expect(chunkCalls).toBe(chunksBeforeRetry);
  expect(completeCalls).toBe(2);
  expect(requestedUploadIDs.length).toBeGreaterThan(0);
  expect(new Set(requestedUploadIDs)).toEqual(new Set([uploadID]));
  expect(completedUploadIDs).toEqual([uploadID, uploadID]);
  expect(completeKeys).toHaveLength(2);
  expect(completeKeys[0]).toBeTruthy();
  expect(completeKeys[1]).toBeTruthy();
  expect(completeKeys[1]).not.toBe(completeKeys[0]);
  expect(completeKeys.every((key) => key !== createKey)).toBeTruthy();
});

test("恢复响应丢失后先重放原键确认终态再用新键重试", async ({ page }) => {
  await authenticated(page);
  const degraded = { ...instance, status: "degraded", pid: undefined, started_at: undefined };
  const archive = Buffer.from("portable backup with a lost complete response");
  const digest = createHash("sha256").update(archive).digest("hex");
  const uploadID = "upload-lost-complete-response";
  const oldCompleteKey = "backup-complete:lost-response";
  let uploadStatus = "failed";
  let completeCalls = 0;
  const completeKeys: string[] = [];
  let unexpectedWrites = 0;
  const uploadRecord = () => ({
    id: uploadID,
    instance_id: instance.id,
    name: "lost-response.zip",
    platform: "linux/amd64",
    size: archive.length,
    sha256: digest,
    received_size: archive.length,
    status: uploadStatus,
    restore_job_id: completeCalls > 1 ? "job-restore-new" : "job-restore-old",
    expires_at: "2099-08-02T00:00:00Z",
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-01T00:00:00Z",
  });
  await page.addInitScript((stored) => {
    localStorage.setItem(`l4d2panel.backup-upload.v1:${stored.instanceId}`, JSON.stringify(stored));
  }, {
    version: 1,
    instanceId: instance.id,
    createKey: "backup-upload:already-created",
    uploadId: uploadID,
    name: "lost-response.zip",
    size: archive.length,
    sha256: digest,
    platform: "linux/amd64",
    expiresAt: "2099-08-02T00:00:00Z",
    pendingComplete: { idempotencyKey: oldCompleteKey },
    updatedAt: "2026-08-01T00:00:00Z",
  });
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: degraded }));
  await page.route("**/api/v1/instances/instance-1/backups", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/instances/instance-1/backup-uploads**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    if (path.endsWith("/complete") && method === "POST") {
      completeCalls += 1;
      completeKeys.push(route.request().headers()["idempotency-key"] || "");
      if (completeCalls === 1) {
        return json(route, { job_id: "job-restore-old", status: "failed", idempotent_replay: true }, 202);
      }
      uploadStatus = "restoring";
      return json(route, { job_id: "job-restore-new", status: "queued", idempotent_replay: false }, 202);
    }
    if (method === "GET") return json(route, { upload: uploadRecord() });
    unexpectedWrites += 1;
    return json(route, { error: { code: "unexpected_write", message: "unexpected write" } }, 500);
  });

  await page.goto("/instances/instance-1?tab=backups");
  await page.getByRole("button", { name: "上传并恢复" }).click();
  const dialog = page.getByRole("dialog", { name: "上传备份并恢复" });
  await dialog.getByLabel("ZIP 备份文件").setInputFiles({ name: "lost-response.zip", mimeType: "application/zip", buffer: archive });
  await expect(dialog.getByLabel("备份来源平台")).toHaveValue("linux/amd64");
  await dialog.getByRole("button", { name: "开始上传并恢复" }).click();

  await expect(page).toHaveURL(/\/jobs$/);
  expect(unexpectedWrites).toBe(0);
  expect(completeCalls).toBe(2);
  expect(completeKeys[0]).toBe(oldCompleteKey);
  expect(completeKeys[1]).toBeTruthy();
  expect(completeKeys[1]).not.toBe(oldCompleteKey);
});

test("实例详情展示安装绑定并仅解除实例绑定", async ({ page }) => {
  await authenticated(page);
  const stopped = { ...instance, game_installation_id: gameInstallation.id, status: "stopped", pid: undefined, started_at: undefined };
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: stopped }));
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));
  let deleteMode = "";
  await page.route("**/api/v1/instances/instance-1?mode=*", async (route) => {
    deleteMode = new URL(route.request().url()).searchParams.get("mode") || "";
    return json(route, { job_id: "job-delete" }, 202);
  });

  await page.goto("/instances/instance-1");
  const argv = page.getByLabel("启动参数数组");
  await expect(argv).toContainText('"-console"');
  await expect(argv).toContainText('"27015"');
  await expect(argv).not.toContainText('"l4d2panel.cfg"');
  await expect(page.getByText(/不经过 Shell/)).toBeVisible();
  await expect(page.getByText("已绑定独占游戏安装")).toBeVisible();
  await expect(page.getByText(/SteamCMD、更新和校验修复统一在运行环境中执行/)).toBeVisible();
  await expect(page.getByRole("button", { name: "校验", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "更新", exact: true })).toHaveCount(0);
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "删除实例" }).click();
  await expect.poll(() => deleteMode).toBe("detach");
  await expect(page).toHaveURL(/\/jobs$/);
});

test("操作员可启停实例但不能删除实例", async ({ page }) => {
  const operator = {
    ...owner, role: "operator", display_name: "值班操作员",
    role_bindings: [{ role: "operator", scope_type: "instance", instance_id: instance.id }],
  };
  await authenticated(page, operator);
  const stopped = { ...instance, status: "stopped", pid: undefined, started_at: undefined };
  await page.route("**/api/v1/instances/instance-1", (route) => json(route, { instance: stopped }));
  await page.route("**/api/v1/jobs", (route) => json(route, { items: [] }));

  await page.goto("/instances/instance-1");
  await expect(page.getByRole("button", { name: "启动", exact: true })).toBeEnabled();
  await expect(page.getByRole("button", { name: "删除实例" })).toBeDisabled();
});
