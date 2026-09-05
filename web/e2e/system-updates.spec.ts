import { dropFile } from "./file-drop";
import { expect, test, type Page, type Route } from "@playwright/test";

const owner = {
  id: "owner-1",
  username: "admin",
  display_name: "系统管理员",
  role: "owner",
  totp_enabled: true,
  created_at: "2026-08-01T00:00:00Z",
};

function json(route: Route, body: unknown, status = 200) {
  return route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
}

async function authenticate(page: Page, user = owner) {
  await page.routeWebSocket("**/api/v1/events**", () => undefined);
  await page.route("**/api/v1/auth/bootstrap", (route) => json(route, { required: false }));
  await page.route("**/api/v1/auth/me", (route) => json(route, { user, csrf_token: "csrf-update" }));
  await page.route("**/api/v1/notifications", (route) => json(route, { items: [] }));
  await page.route("**/api/v1/users", (route) => json(route, { items: [user] }));
}

test("所有者上传便携 ZIP 后可跨短暂断线等待升级成功", async ({ page }) => {
  await authenticate(page);
  let uploadSeen = false;
  let statusReads = 0;
  let settingsLoads = 0;
  page.on("request", (request) => {
    if (request.isNavigationRequest() && new URL(request.url()).pathname === "/settings") settingsLoads += 1;
  });

  await page.route("**/api/v1/system/updates", async (route) => {
    if (route.request().method() === "POST") {
      uploadSeen = true;
      expect(route.request().headers()["idempotency-key"]).toBeTruthy();
      expect(route.request().headers()["x-csrf-token"]).toBe("csrf-update");
      expect(route.request().postDataBuffer()?.toString()).toContain("upgrade.zip");
      return json(route, { job_id: "update-1", status: "queued", phase: "validating", target_version: "1.2.0" }, 202);
    }
    return json(route, { supported: true, current_version: "1.1.0", history: [] });
  });
  await page.route("**/api/v1/system/updates/update-1", async (route) => {
    statusReads += 1;
    if (statusReads === 1) return route.abort("connectionrefused");
    if (statusReads === 2) return json(route, {
      job_id: "update-1", status: "running", phase: "health_check", current_version: "1.1.0",
      target_version: "1.2.0", rollback: "not_required", created_at: "2026-08-02T00:00:00Z", updated_at: "2026-08-02T00:00:01Z",
    });
    return json(route, {
      job_id: "update-1", status: "succeeded", phase: "health_check", current_version: "1.1.0",
      target_version: "1.2.0", rollback: "not_required", created_at: "2026-08-02T00:00:00Z",
      updated_at: "2026-08-02T00:00:02Z", completed_at: "2026-08-02T00:00:02Z",
    });
  });
  page.on("dialog", (dialog) => dialog.accept());

  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "中心端升级" })).toBeVisible();
  await dropFile(page, page.getByTestId("file-dropzone"), { name: "upgrade.zip", mimeType: "application/zip", buffer: Buffer.from("trusted-test-package") });
  await page.getByRole("button", { name: "一键升级" }).click();

  await expect.poll(() => uploadSeen).toBeTruthy();
  await expect(page.getByText("中心端已升级到 1.2.0，页面即将刷新")).toBeVisible({ timeout: 15_000 });
  await expect.poll(() => settingsLoads, { timeout: 15_000 }).toBeGreaterThanOrEqual(2);
  await expect(page).toHaveURL(/\/settings$/);
  expect(statusReads).toBeGreaterThanOrEqual(3);
});

test("非便携部署和非所有者不显示中心端升级入口", async ({ page }) => {
  await authenticate(page);
  await page.route("**/api/v1/system/updates", (route) => json(route, {
    supported: false, current_version: "dev", history: [], reason: "不支持此部署方式",
  }));
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "中心端升级" })).toHaveCount(0);

  const operatorPage = await page.context().newPage();
  await authenticate(operatorPage, { ...owner, id: "operator-1", username: "operator", role: "operator" });
  await operatorPage.goto("/settings");
  await expect(operatorPage.getByRole("heading", { name: "中心端升级" })).toHaveCount(0);
});

test("升级失败历史明确显示自动回滚结果", async ({ page }) => {
  await authenticate(page);
  await page.route("**/api/v1/system/updates", (route) => json(route, {
    supported: true,
    current_version: "1.1.0",
    history: [{
      job_id: "update-failed", status: "failed", phase: "rolling_back", current_version: "1.1.0",
      target_version: "1.2.0", rollback: "succeeded", error: "健康检查失败",
      created_at: "2026-08-02T00:00:00Z", updated_at: "2026-08-02T00:01:00Z", completed_at: "2026-08-02T00:01:00Z",
    }],
  }));

  await page.goto("/settings");
  await expect(page.getByText("1.1.0 → 1.2.0")).toBeVisible();
  await expect(page.getByText("回滚：成功")).toBeVisible();
});

test("Docker 开发构建允许上传其他开发或正式镜像", async ({ page }) => {
  await authenticate(page);
  await page.route("**/api/v1/system/updates", (route) => json(route, {
    supported: true,
    current_version: "dev",
    deployment: "docker-compose",
    package_format: "tar.gz",
    max_package_bytes: 1073741824,
    history: [],
  }));

  await page.goto("/settings");
  await expect(page.getByText("开发版本可替换为其他开发构建或正式版本；最大 1 GiB")).toBeVisible();
  await expect(page.getByLabel("升级包")).toHaveAttribute("accept", ".tar.gz,application/gzip,application/x-gzip");
  await dropFile(page, page.getByTestId("file-dropzone"), { name: "wrong.zip", mimeType: "application/zip", buffer: Buffer.from("archive") });
  await expect(page.getByRole("alert")).toContainText("不支持此文件类型");
  await expect(page.getByRole("button", { name: "一键升级" })).toBeDisabled();
  await dropFile(page, page.getByTestId("file-dropzone"), { name: "control.tar.gz", mimeType: "", buffer: Buffer.from("archive") });
  await expect(page.getByText("已选择：control.tar.gz")).toBeVisible();
  await expect(page.getByRole("button", { name: "一键升级" })).toBeEnabled();
});

test("Windows 开发构建允许上传其他开发或正式 ZIP", async ({ page }) => {
  await authenticate(page);
  await page.route("**/api/v1/system/updates", (route) => json(route, {
    supported: true,
    current_version: "dev",
    deployment: "windows-portable",
    package_format: "zip",
    max_package_bytes: 536870912,
    history: [],
  }));

  await page.goto("/settings");
  await expect(page.getByText("开发版本可替换为其他开发构建或正式版本；最大 512 MiB")).toBeVisible();
  await expect(page.getByLabel("升级包")).toHaveAttribute("accept", ".zip,application/zip");
});
