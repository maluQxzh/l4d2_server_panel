import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, setCsrfToken } from "./api";

describe("API 客户端", () => {
  beforeEach(() => setCsrfToken("csrf-test"));
  afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  it("写请求携带 CSRF 和幂等键", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ job_id: "job-1" }), { status: 202, headers: { "Content-Type": "application/json" } }));
    await api.instanceAction("instance-1", "restart");
    const [, init] = fetchMock.mock.calls[0];
    const headers = new Headers(init?.headers);
    expect(headers.get("X-CSRF-Token")).toBe("csrf-test");
    expect(headers.get("Idempotency-Key")).toBeTruthy();
    expect(init?.credentials).toBe("include");
  });

  it("运行环境操作使用资源路径和固定结构化参数", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "steam-job" }), { status: 202, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ game_installation: { id: "game-1" }, job_id: "game-job" }), { status: 202, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "validate-job" }), { status: 202, headers: { "Content-Type": "application/json" } }));

    await api.steamCMDAction("environment/one", "configure", "/opt/steamcmd/steamcmd.sh");
    await api.createGameInstallation("environment/one", { path: "D:\\servers\\one", operation: "import" });
    await api.gameInstallationAction("environment/one", "game/one", "import", { path: "D:\\servers\\moved" });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/v1/environments/environment%2Fone/steamcmd/actions",
      "/api/v1/environments/environment%2Fone/game-installations",
      "/api/v1/environments/environment%2Fone/game-installations/game%2Fone/actions",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ action: "configure", path: "/opt/steamcmd/steamcmd.sh" });
    expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({ path: "D:\\servers\\one", operation: "import" });
    expect(JSON.parse(String(fetchMock.mock.calls[2][1]?.body))).toEqual({ action: "import", path: "D:\\servers\\moved" });
    for (const [, init] of fetchMock.mock.calls) {
      expect(new Headers(init?.headers).get("Idempotency-Key")).toBeTruthy();
    }
  });

  it("首启所有者使用独立的初始化 CSRF 令牌", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ required: true, key_configured: true, csrf_token: "bootstrap-csrf" }), { status: 200, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({
        user: { id: "owner-1", username: "admin", display_name: "系统管理员", role: "owner", totp_enabled: false, created_at: "2026-08-01T00:00:00Z" },
        csrf_token: "session-csrf",
      }), { status: 201, headers: { "Content-Type": "application/json" } }));

    await api.bootstrapStatus();
    await api.bootstrapOwner({ setup_key: "l4d2p_setup_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG" });

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/bootstrap");
    expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/auth/bootstrap");
    const headers = new Headers(fetchMock.mock.calls[1][1]?.headers);
    expect(headers.get("X-Bootstrap-CSRF-Token")).toBe("bootstrap-csrf");
    expect(headers.get("Idempotency-Key")).toBeTruthy();
    expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({ setup_key: "l4d2p_setup_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG" });
  });

  it("解析统一错误结构", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ error: { code: "forbidden", message: "权限不足" } }), { status: 403, headers: { "Content-Type": "application/json" } }));
    await expect(api.nodes()).rejects.toMatchObject({ status: 403, code: "forbidden", message: "权限不足" } satisfies Partial<ApiError>);
  });

  it("按实例查询任务用于显示 RCON 刷新诊断", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ items: [] }), { status: 200, headers: { "Content-Type": "application/json" } }));
    await api.instanceJobs("instance/one");
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/jobs?instance_id=instance%2Fone");
  });

  it("恢复码登录不会同时提交 TOTP", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
      user: { id: "user-1", username: "admin", display_name: "管理员", role: "owner", totp_enabled: true, created_at: "2026-08-01T00:00:00Z" },
      csrf_token: "rotated-csrf",
    }), { status: 200, headers: { "Content-Type": "application/json" } }));

    await api.login({ username: "admin", password: "correct-horse-battery-staple", recovery_code: "ABCD-EFGH-JKLM-NPQR" });

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/auth/login");
    expect(JSON.parse(String(init?.body))).toEqual({
      username: "admin",
      password: "correct-horse-battery-staple",
      recovery_code: "ABCD-EFGH-JKLM-NPQR",
    });
    expect(JSON.parse(String(init?.body))).not.toHaveProperty("totp_code");
  });

  it("首次改密和重新签发使用受限激活端点", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({
        user: { id: "user-1", username: "viewer", display_name: "Viewer", role: "viewer", totp_enabled: false, disabled: false, account_state: "active", created_at: "2026-08-08T00:00:00Z" },
        csrf_token: "activated-csrf",
        password_change_required: false,
      }), { status: 200, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ one_time_password: "l4d2p_once_example", expires_at: "2026-08-09T00:00:00Z" }), { status: 201, headers: { "Content-Type": "application/json" } }));

    await api.completeActivation("new-user-password-123");
    await api.reissueUserActivation("user/one");

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/activate");
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ new_password: "new-user-password-123" });
    expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/users/user%2Fone/activation");
    for (const [, init] of fetchMock.mock.calls) {
      expect(init?.method).toBe("POST");
      expect(new Headers(init?.headers).get("X-CSRF-Token")).toBeTruthy();
      expect(new Headers(init?.headers).get("Idempotency-Key")).toBeTruthy();
    }
  });

  it("文件写入携带版本摘要并正确编码相对路径", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ job_id: "job-file" }), { status: 202, headers: { "Content-Type": "application/json" } }));

    await api.writeFile("实例/一", "left4dead2/cfg/server test.cfg", "hostname test", "abc123");

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/instances/%E5%AE%9E%E4%BE%8B%2F%E4%B8%80/files/content?path=left4dead2%2Fcfg%2Fserver%20test.cfg");
    expect(JSON.parse(String(init?.body))).toEqual({ content: "hostname test", expected_sha256: "abc123" });
  });

  it("QQ 闪传导入提交分享文本到固定模组端点", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ job_id: "job-qq" }), { status: 202, headers: { "Content-Type": "application/json" } }));

    await api.importQQFlashFile("实例/一", "链接：https://qfile.qq.com/q/Example123");

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/instances/%E5%AE%9E%E4%BE%8B%2F%E4%B8%80/packages/qq-flash");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ share_url: "链接：https://qfile.qq.com/q/Example123" });
    expect(new Headers(init?.headers).get("Idempotency-Key")).toBeTruthy();
  });

  it("计划启停与删除使用资源化端点", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ schedule: { id: "schedule-1", enabled: false } }), { status: 200, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));

    await api.setScheduleEnabled("instance-1", "schedule-1", false);
    await api.deleteSchedule("instance-1", "schedule-1");

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/instances/instance-1/schedules/schedule-1");
    expect(fetchMock.mock.calls[0][1]?.method).toBe("PATCH");
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ enabled: false });
    expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/instances/instance-1/schedules/schedule-1");
    expect(fetchMock.mock.calls[1][1]?.method).toBe("DELETE");
  });

  it("查询模组版本并通过版本范围端点提交回退", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [] }), { status: 200, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "job-package-rollback" }), { status: 202, headers: { "Content-Type": "application/json" } }));

    await api.packageVersions("instance/one", "package one");
    await api.rollbackPackageVersion("instance/one", "package one", "version one");

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/instances/instance%2Fone/packages/package%20one/versions");
    expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/instances/instance%2Fone/packages/package%20one/versions/version%20one/rollback");
    expect(fetchMock.mock.calls[1][1]?.method).toBe("POST");
    expect(new Headers(fetchMock.mock.calls[1][1]?.headers).get("Idempotency-Key")).toBeTruthy();
  });

  it("通知已读与投递重试均为幂等写请求", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));

    await api.markNotificationRead("notice-1");
    await api.retryNotificationDelivery("delivery-1");

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/v1/notifications/notice-1/read",
      "/api/v1/notification-deliveries/delivery-1/retry",
    ]);
    for (const [, init] of fetchMock.mock.calls) {
      const headers = new Headers(init?.headers);
      expect(headers.get("Idempotency-Key")).toBeTruthy();
      expect(headers.get("X-CSRF-Token")).toBe("csrf-test");
    }
  });

  it("节点发现任务通过资源端点轮询并返回候选项", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "job-discovery" }), { status: 202, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ job: {
        id: "job-discovery", node_id: "node-1", type: "discover_instances", status: "succeeded", progress: 100,
        result: { candidates: [{ root_index: 0, path: "server", name: "server", executable: "srcds.exe", evidence: [] }], scanned_directories: 12, skipped_links: 1, truncated: false },
        created_at: "2026-08-01T00:00:00Z",
      } }), { status: 200, headers: { "Content-Type": "application/json" } }));

    const result = await api.discoverNodeInstances("node/1");

    expect(result.candidates[0].path).toBe("server");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/v1/nodes/node%2F1/discoveries",
      "/api/v1/nodes/node%2F1/discoveries/job-discovery",
    ]);
  });

  it("轮换节点凭据不会由客户端隐藏返回值", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ credential: "node-secret-once" }), { status: 200, headers: { "Content-Type": "application/json" } }));
    await expect(api.rotateNodeCredential("node-1")).resolves.toEqual({ credential: "node-secret-once" });
  });

  it("隔离区二次确认使用资源端点且最终请求携带稳定幂等键", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [] }), { status: 200, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ confirmation_token: "confirm-once", expires_at: "2026-08-01T01:05:00Z" }), { status: 201, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "job-purge" }), { status: 202, headers: { "Content-Type": "application/json" } }));

    await api.quarantineEntries("node/one");
    await api.createQuarantinePurgeConfirmation("node/one", "entry one");
    await api.purgeQuarantineEntry("node/one", "entry one", "confirm-once", "stable-purge-key");

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/v1/nodes/node%2Fone/quarantine",
      "/api/v1/nodes/node%2Fone/quarantine/entry%20one/purge-confirmations",
      "/api/v1/nodes/node%2Fone/quarantine/entry%20one/purge",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[2][1]?.body))).toEqual({ confirmation_token: "confirm-once" });
    expect(new Headers(fetchMock.mock.calls[2][1]?.headers).get("Idempotency-Key")).toBe("stable-purge-key");
  });

  it("跨节点备份上传保留服务端包装结构并逐块使用调用方幂等键", async () => {
    const upload = {
      id: "upload-1", instance_id: "instance-1", name: "daily.zip", platform: "linux/amd64", size: 3,
      sha256: "a".repeat(64), received_size: 0, status: "uploading", expires_at: "2026-08-02T00:00:00Z",
      created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
    };
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ upload, idempotent_replay: false }), { status: 201, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ upload: { ...upload, received_size: 3, status: "ready" } }), { status: 200, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "job-chunk", status: "queued" }), { status: 202, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "job-restore", status: "queued" }), { status: 202, headers: { "Content-Type": "application/json" } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ job_id: "job-abort", status: "queued" }), { status: 202, headers: { "Content-Type": "application/json" } }));

    await api.createBackupUpload("instance-1", { name: "daily.zip", platform: "linux/amd64", size: 3, sha256: "a".repeat(64) }, "create-key");
    await expect(api.backupUpload("instance-1", "upload-1")).resolves.toMatchObject({ status: "ready", received_size: 3 });
    await api.uploadBackupChunk("instance-1", "upload-1", { offset: 0, chunk_sha256: "b".repeat(64), content_base64: "AQID" }, "chunk-key");
    await api.completeBackupUpload("instance-1", "upload-1", "complete-key");
    await api.abortBackupUpload("instance-1", "upload-1", "abort-key");

    expect(new Headers(fetchMock.mock.calls[0][1]?.headers).get("Idempotency-Key")).toBe("create-key");
    expect(new Headers(fetchMock.mock.calls[2][1]?.headers).get("Idempotency-Key")).toBe("chunk-key");
    expect(new Headers(fetchMock.mock.calls[3][1]?.headers).get("Idempotency-Key")).toBe("complete-key");
    expect(new Headers(fetchMock.mock.calls[4][1]?.headers).get("Idempotency-Key")).toBe("abort-key");
    expect(fetchMock.mock.calls[4][1]?.method).toBe("DELETE");
    expect(fetchMock.mock.calls[4][1]?.body).toBeUndefined();
    expect(JSON.parse(String(fetchMock.mock.calls[2][1]?.body))).toEqual({ offset: 0, chunk_sha256: "b".repeat(64), content_base64: "AQID" });
    expect(fetchMock.mock.calls[3][1]?.body).toBeUndefined();
  });

  it("中心端升级使用 multipart、CSRF、幂等键并报告上传进度", async () => {
    const headers = new Map<string, string>();
    let sentFileName = "";
    class MockXHR {
      upload: { onprogress: ((event: ProgressEvent) => void) | null } = { onprogress: null };
      onerror: (() => void) | null = null;
      onload: (() => void) | null = null;
      status = 202;
      responseText = JSON.stringify({ job_id: "update-1", status: "queued", phase: "validating", target_version: "1.2.0" });
      withCredentials = false;
      open(method: string, url: string) { expect(method).toBe("POST"); expect(url).toBe("/api/v1/system/updates"); }
      setRequestHeader(name: string, value: string) { headers.set(name, value); }
      send(body: FormData) {
        const part = body.get("package");
        sentFileName = part instanceof File ? part.name : "";
        this.upload.onprogress?.({ lengthComputable: true, loaded: 3, total: 4 } as ProgressEvent);
        this.onload?.();
      }
    }
    vi.stubGlobal("XMLHttpRequest", MockXHR);
    const progress = vi.fn();
    const file = new File(["zip"], "center.zip", { type: "application/zip" });

    await expect(api.uploadSystemUpdate(file, progress)).resolves.toMatchObject({ job_id: "update-1", target_version: "1.2.0" });

    expect(headers.get("X-CSRF-Token")).toBe("csrf-test");
    expect(headers.get("Idempotency-Key")).toBeTruthy();
    expect(progress).toHaveBeenCalledWith(75);
    expect(sentFileName).toBe("center.zip");
  });
});
