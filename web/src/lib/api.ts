import type {
  AuditEvent,
  APITokenRecord,
  BackupRecord,
  InstanceConfig,
  InstanceRecord,
  Job,
  ListResponse,
  NodeRecord,
  NotificationRecord,
  PackageRecord,
  PackageFileRecord,
  PackageVersionRecord,
  Player,
  PlayerBan,
  PublicServer,
  ScheduleRecord,
  User,
  ManagedFileContent,
  ManagedFileList,
  NotificationChannelRecord,
  NotificationDeliveryRecord,
  MetricPoint,
  LogSearchEntry,
  TransferChunk,
  JobStatus,
  JobListResponse,
  RoleBinding,
  Role,
  NodeDiscoveryResult,
  QuarantineEntry,
  BackupUploadRecord,
  SystemUpdateOverview,
  SystemUpdateRecord,
  RuntimeEnvironmentRecord,
  GameInstallationRecord,
  MapRecord,
  SmartPluginPlan,
} from "./types";

const baseURL = (import.meta.env.VITE_API_BASE || "/api/v1").replace(/\/$/, "");
let csrfToken = "";
let bootstrapCsrfToken = "";

export interface AuthSession {
  user: User;
  csrf_token?: string;
  password_change_required?: boolean;
}

export interface UserActivationResult {
  user?: User;
  one_time_password: string;
  expires_at: string;
}

export class ApiError extends Error {
  status: number;
  code: string;

  constructor(message: string, status: number, code = "request_failed") {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

export function setCsrfToken(token?: string) {
  csrfToken = token || "";
}

function newIdempotencyKey() {
  return typeof crypto?.randomUUID === "function"
    ? crypto.randomUUID()
    : `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method || "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body && !(init.body instanceof FormData)) headers.set("Content-Type", "application/json");
  if (!(["GET", "HEAD"].includes(method))) {
    if (csrfToken) headers.set("X-CSRF-Token", csrfToken);
    if (!headers.has("Idempotency-Key")) headers.set("Idempotency-Key", newIdempotencyKey());
  }

  const response = await fetch(`${baseURL}${path}`, {
    ...init,
    headers,
    credentials: "include",
  });
  if (response.status === 204) return undefined as T;

  const text = await response.text();
  let body: Record<string, unknown> = {};
  if (text) {
    try {
      body = JSON.parse(text) as Record<string, unknown>;
    } catch {
      if (!response.ok) throw new ApiError(`请求失败（${response.status}）`, response.status, "invalid_response");
      throw new ApiError("服务器返回了无法识别的数据", response.status, "invalid_response");
    }
  }
  if (!response.ok) {
    const error = body.error as { message?: string; code?: string } | string | undefined;
    const message = typeof error === "string" ? error : error?.message;
    throw new ApiError(message || `请求失败（${response.status}）`, response.status, typeof error === "object" ? error?.code : undefined);
  }
  return body as T;
}

function list<T>(path: string) {
  return request<ListResponse<T>>(path).then((data) => ({ ...data, items: data.items || [] }));
}

const transferChunkSize = 1 << 20;

function delay(milliseconds: number) {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

async function responseError(response: Response) {
  let body: { error?: { message?: string; code?: string } | string } = {};
  try { body = await response.json() as typeof body; } catch { /* A proxy can replace an error body. */ }
  const message = typeof body.error === "string" ? body.error : body.error?.message;
  const code = typeof body.error === "object" ? body.error?.code : undefined;
  return new ApiError(message || `请求失败（${response.status}）`, response.status, code);
}

async function pollTransferChunk(path: string): Promise<TransferChunk | null> {
  const response = await fetch(`${baseURL}${path}`, { credentials: "include", headers: { Accept: "application/octet-stream, application/json" } });
  if (response.status === 202) return null;
  if (response.status !== 206) throw await responseError(response);
  const blob = await response.blob();
  const offset = Number(response.headers.get("Content-Range")?.match(/^bytes (\d+)-/)?.[1] || 0);
  const nextOffset = Number(response.headers.get("X-Next-Offset"));
  const eof = response.headers.get("X-Transfer-EOF") === "true";
  const rangeTotal = response.headers.get("Content-Range")?.match(/\/(\d+)$/)?.[1];
  const totalSize = Number(rangeTotal ?? (eof ? nextOffset : Number.NaN));
  const fileSha256 = response.headers.get("X-File-SHA256") || "";
  const chunkSha256 = response.headers.get("X-Chunk-SHA256") || "";
  if (![offset, nextOffset, totalSize].every(Number.isFinite) || blob.size > transferChunkSize || nextOffset !== offset + blob.size || (!eof && blob.size === 0)) {
    throw new ApiError("下载分块元数据无效", 502, "invalid_transfer_chunk");
  }
  if (chunkSha256 && globalThis.crypto?.subtle) {
    const digest = Array.from(new Uint8Array(await globalThis.crypto.subtle.digest("SHA-256", await blob.arrayBuffer())), (value) => value.toString(16).padStart(2, "0")).join("");
    if (digest !== chunkSha256.toLowerCase()) throw new ApiError("下载分块校验失败", 502, "transfer_checksum_mismatch");
  }
  return { blob, offset, nextOffset, totalSize, eof, fileSha256, chunkSha256 };
}

async function waitForTransfer(path: string): Promise<TransferChunk> {
  for (let attempt = 0; attempt < 240; attempt += 1) {
    const result = await pollTransferChunk(path);
    if (result) return result;
    await delay(500);
  }
  throw new ApiError("节点传输任务等待超时", 408, "transfer_timeout");
}

export const api = {
  bootstrapStatus: async () => {
    const result = await request<{ required: boolean; key_configured: boolean; csrf_token?: string }>("/auth/bootstrap");
    bootstrapCsrfToken = result.required ? result.csrf_token || "" : "";
    return result;
  },
  bootstrapOwner: async (payload: { setup_key: string }) => {
    const result = await request<{ user: User; csrf_token: string }>("/auth/bootstrap", {
      method: "POST",
      headers: { "X-Bootstrap-CSRF-Token": bootstrapCsrfToken },
      body: JSON.stringify(payload),
    });
    bootstrapCsrfToken = "";
    setCsrfToken(result.csrf_token);
    return result;
  },
  login: async (credentials: { username: string; password: string; totp_code?: string; recovery_code?: string }) => {
    const result = await request<AuthSession>("/auth/login", {
      method: "POST",
      body: JSON.stringify(credentials),
    });
    setCsrfToken(result.csrf_token);
    return result;
  },
  me: async () => {
    const result = await request<AuthSession>("/auth/me");
    setCsrfToken(result.csrf_token);
    return result;
  },
  logout: () => request<void>("/auth/logout", { method: "POST" }).finally(() => setCsrfToken()),
  completeActivation: async (newPassword: string) => {
    const result = await request<AuthSession>("/auth/activate", { method: "POST", body: JSON.stringify({ new_password: newPassword }) });
    setCsrfToken(result.csrf_token);
    return result;
  },
  setupTotp: () => request<{ secret: string; otpauth_url: string }>("/auth/totp/setup", { method: "POST" }),
  confirmTotp: (code: string) => request<{ enabled: boolean; recovery_codes: string[] }>("/auth/totp/confirm", {
    method: "POST",
    body: JSON.stringify({ code }),
  }),
  apiTokens: () => list<APITokenRecord>("/api-tokens"),
  createApiToken: (payload: { name: string; scopes: string[]; node_id?: string; instance_id?: string; expires_at?: string }) =>
    request<{ api_token: APITokenRecord; token: string }>("/api-tokens", { method: "POST", body: JSON.stringify(payload) }),
  revokeApiToken: (id: string) => request<void>(`/api-tokens/${encodeURIComponent(id)}`, { method: "DELETE" }),
  notificationChannels: () => list<NotificationChannelRecord>("/notification-channels"),
  createNotificationChannel: (payload: { name: string; kind: "smtp" | "webhook"; enabled: boolean; config: Record<string, unknown>; secret: string }) =>
    request<{ channel: NotificationChannelRecord }>("/notification-channels", { method: "POST", body: JSON.stringify(payload) }),
  deleteNotificationChannel: (id: string) => request<void>(`/notification-channels/${encodeURIComponent(id)}`, { method: "DELETE" }),
  notificationDeliveries: () => list<NotificationDeliveryRecord>("/notification-deliveries"),
  retryNotificationDelivery: (id: string) => request<void>(`/notification-deliveries/${encodeURIComponent(id)}/retry`, { method: "POST" }),
  metrics: (scope: { nodeId?: string; instanceId?: string; resolution?: "raw" | "1m" | "1h"; from?: string; to?: string }) => {
    const params = new URLSearchParams();
    if (scope.nodeId) params.set("node_id", scope.nodeId);
    if (scope.instanceId) params.set("instance_id", scope.instanceId);
    if (scope.resolution) params.set("resolution", scope.resolution);
    if (scope.from) params.set("from", scope.from);
    if (scope.to) params.set("to", scope.to);
    return request<{ items: MetricPoint[]; resolution: string; from: string; to: string }>(`/metrics?${params}`);
  },

  nodes: () => list<NodeRecord>("/nodes"),
  node: (id: string) => request<{ node: NodeRecord }>(`/nodes/${encodeURIComponent(id)}`).then((r) => r.node),
  createEnrollment: (name: string) => request<{ token: string; expires_at?: string }>("/nodes/enrollment-tokens", {
    method: "POST",
    body: JSON.stringify({ label: name, expires_in_minutes: 30 }),
  }),
  updateNode: (id: string, payload: Partial<NodeRecord>) => request<{ node: NodeRecord }>(`/nodes/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(payload) }).then((result) => result.node),
  rotateNodeCredential: (id: string) => request<{ credential: string }>(`/nodes/${encodeURIComponent(id)}/credential/rotate`, { method: "POST" }),
  discoverNodeInstances: async (id: string): Promise<NodeDiscoveryResult> => {
    const nodeID = encodeURIComponent(id);
    const created = await request<{ job_id: string }>(`/nodes/${nodeID}/discoveries`, {
      method: "POST",
      body: JSON.stringify({ max_depth: 6, max_entries: 5000 }),
    });
    for (let attempt = 0; attempt < 240; attempt += 1) {
      const response = await request<{ job: Job }>(`/nodes/${nodeID}/discoveries/${encodeURIComponent(created.job_id)}`);
      if (response.job.status === "succeeded") {
        const result = response.job.result || {};
        return {
          candidates: Array.isArray(result.candidates) ? result.candidates as NodeDiscoveryResult["candidates"] : [],
          scanned_directories: Number(result.scanned_directories || 0),
          skipped_links: Number(result.skipped_links || 0),
          truncated: Boolean(result.truncated),
        };
      }
      if (["failed", "canceled"].includes(response.job.status)) {
        throw new ApiError(response.job.message || "节点安装扫描失败", 409, "discovery_failed");
      }
      await delay(500);
    }
    throw new ApiError("节点安装扫描等待超时", 408, "discovery_timeout");
  },
  deleteNode: (id: string) => request<void>(`/nodes/${encodeURIComponent(id)}`, { method: "DELETE" }),
  quarantineEntries: (nodeId: string) => list<QuarantineEntry>(`/nodes/${encodeURIComponent(nodeId)}/quarantine`),
  createQuarantinePurgeConfirmation: (nodeId: string, entryId: string) => request<{ confirmation_token: string; expires_at: string }>(
    `/nodes/${encodeURIComponent(nodeId)}/quarantine/${encodeURIComponent(entryId)}/purge-confirmations`,
    { method: "POST" },
  ),
  purgeQuarantineEntry: (nodeId: string, entryId: string, confirmationToken: string, idempotencyKey: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/nodes/${encodeURIComponent(nodeId)}/quarantine/${encodeURIComponent(entryId)}/purge`,
    { method: "POST", headers: { "Idempotency-Key": idempotencyKey }, body: JSON.stringify({ confirmation_token: confirmationToken }) },
  ),

  environments: () => list<RuntimeEnvironmentRecord>("/environments"),
  environment: (id: string) => request<{ environment: RuntimeEnvironmentRecord; game_installations: GameInstallationRecord[] }>(`/environments/${encodeURIComponent(id)}`),
  inspectEnvironment: (id: string) => request<{ job_id: string }>(`/environments/${encodeURIComponent(id)}/inspect`, { method: "POST" }),
  steamCMDAction: (id: string, action: "configure" | "clear" | "install" | "repair", path?: string) => request<{ job_id: string }>(
    `/environments/${encodeURIComponent(id)}/steamcmd/actions`, { method: "POST", body: JSON.stringify({ action, path }) },
  ),
  gameInstallations: (id: string) => list<GameInstallationRecord>(`/environments/${encodeURIComponent(id)}/game-installations`),
  createGameInstallation: (id: string, payload: { path: string; operation: "install" | "import" }) => request<{ game_installation: GameInstallationRecord; job_id: string }>(
    `/environments/${encodeURIComponent(id)}/game-installations`, { method: "POST", body: JSON.stringify(payload) },
  ),
  gameInstallationAction: (environmentId: string, installationId: string, action: "install" | "import" | "validate" | "update", location?: { path: string }) => request<{ job_id: string }>(
    `/environments/${encodeURIComponent(environmentId)}/game-installations/${encodeURIComponent(installationId)}/actions`, { method: "POST", body: JSON.stringify({ action, ...location }) },
  ),
  deleteGameInstallation: (environmentId: string, installationId: string, mode: "record" | "files") => request<{ job_id: string }>(
    `/environments/${encodeURIComponent(environmentId)}/game-installations/${encodeURIComponent(installationId)}`,
    { method: "DELETE", body: JSON.stringify({ mode }) },
  ),
  instances: () => list<InstanceRecord>("/instances"),
  instance: (id: string) => request<{ instance: InstanceRecord }>(`/instances/${encodeURIComponent(id)}`).then((r) => r.instance),
  createInstance: (payload: Record<string, unknown>) => request<{ instance?: InstanceRecord; job_id: string }>("/instances", {
    method: "POST",
    body: JSON.stringify(payload),
  }),
  updateInstance: (id: string, payload: Partial<Omit<InstanceRecord, "config">> & { config?: InstanceConfig; rcon_password?: string }) =>
    request<{ instance: InstanceRecord }>(`/instances/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(payload),
    }),
  deleteInstance: (id: string, mode: "detach" | "quarantine") => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(id)}?mode=${mode}`,
    { method: "DELETE" },
  ),
  instanceAction: (id: string, type: string, payload?: Record<string, unknown>) => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(id)}/actions`,
    { method: "POST", body: JSON.stringify({ type, payload }) },
  ),
  inspectPluginArchive: async (instanceId: string, payload: { name: string; content_base64: string }): Promise<SmartPluginPlan> => {
    const encodedID = encodeURIComponent(instanceId);
    const created = await request<{ job_id: string }>(`/instances/${encodedID}/plugin-archives/inspections`, {
      method: "POST",
      body: JSON.stringify(payload),
    });
    for (let attempt = 0; attempt < 400; attempt += 1) {
      const response = await request<{ status: JobStatus; progress: number; message?: string; error?: string; plan?: SmartPluginPlan }>(
        `/instances/${encodedID}/plugin-archives/inspections/${encodeURIComponent(created.job_id)}`,
      );
      if (response.status === "succeeded" && response.plan) return response.plan;
      if (["failed", "canceled"].includes(response.status)) {
        throw new ApiError(response.error || response.message || "插件压缩包识别失败", 409, "plugin_inspection_failed");
      }
      await delay(750);
    }
    throw new ApiError("插件压缩包识别等待超时", 408, "plugin_inspection_timeout");
  },

  maps: (nodeId?: string) => list<MapRecord>(`/maps${nodeId ? `?node_id=${encodeURIComponent(nodeId)}` : ""}`),
  map: (id: string) => request<{ map: MapRecord }>(`/maps/${encodeURIComponent(id)}`).then((result) => result.map),
  importMap: (nodeId: string, shareUrl: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/nodes/${encodeURIComponent(nodeId)}/maps/imports`, { method: "POST", body: JSON.stringify({ share_url: shareUrl }) },
  ),
  assignMap: (mapId: string, instanceId: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/maps/${encodeURIComponent(mapId)}/assignments/${encodeURIComponent(instanceId)}`, { method: "PUT" },
  ),
  unassignMap: (mapId: string, instanceId: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/maps/${encodeURIComponent(mapId)}/assignments/${encodeURIComponent(instanceId)}`, { method: "DELETE" },
  ),
  verifyMap: (mapId: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/maps/${encodeURIComponent(mapId)}/verify`, { method: "POST" },
  ),
  createMapDeleteConfirmation: (mapId: string) => request<{ confirmation_token: string; expires_at: string }>(
    `/maps/${encodeURIComponent(mapId)}/delete-confirmations`, { method: "POST" },
  ),
  deleteMap: (mapId: string, confirmationToken: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/maps/${encodeURIComponent(mapId)}`, { method: "DELETE", body: JSON.stringify({ confirmation_token: confirmationToken }) },
  ),

  jobs: () => list<Job>("/jobs"),
  jobList: ({ background = "include", backgroundSummary = false, limit = 100 }: {
    background?: "include" | "exclude" | "only";
    backgroundSummary?: boolean;
    limit?: number;
  } = {}) => {
    const query = new URLSearchParams({ background, limit: String(limit) });
    if (backgroundSummary) query.set("background_summary", "true");
    return request<JobListResponse>(`/jobs?${query.toString()}`);
  },
  instanceJobs: (instanceId: string, background: "include" | "exclude" | "only" = "include") => list<Job>(
    `/jobs?instance_id=${encodeURIComponent(instanceId)}${background === "include" ? "" : `&background=${background}`}`,
  ),
  job: (id: string) => request<{ job: Job }>(`/jobs/${encodeURIComponent(id)}`).then((r) => r.job),
  cancelJob: (id: string) => request<void>(`/jobs/${encodeURIComponent(id)}/cancel`, { method: "POST" }),
  audit: (resourceId?: string) => list<AuditEvent>(`/audit${resourceId ? `?resource_id=${encodeURIComponent(resourceId)}` : ""}`),
  publicServers: () => list<PublicServer>("/public/servers"),

  players: (instanceId: string) => list<Player>(`/instances/${encodeURIComponent(instanceId)}/players`),
  bans: (instanceId: string) => list<PlayerBan>(`/instances/${encodeURIComponent(instanceId)}/bans`),
  packages: (instanceId: string) => list<PackageRecord>(`/instances/${encodeURIComponent(instanceId)}/packages`),
  packageDetails: (instanceId: string, packageId: string) => request<{ package: PackageRecord; files: PackageFileRecord[] }>(
    `/instances/${encodeURIComponent(instanceId)}/packages/${encodeURIComponent(packageId)}`,
  ),
  scanInstalledPackages: (instanceId: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/instances/${encodeURIComponent(instanceId)}/packages/scan`,
    { method: "POST" },
  ),
  packageVersions: (instanceId: string, packageId: string) => list<PackageVersionRecord>(
    `/instances/${encodeURIComponent(instanceId)}/packages/${encodeURIComponent(packageId)}/versions`,
  ),
  rollbackPackageVersion: (instanceId: string, packageId: string, versionId: string) => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(instanceId)}/packages/${encodeURIComponent(packageId)}/versions/${encodeURIComponent(versionId)}/rollback`,
    { method: "POST" },
  ),
  deletePackage: (instanceId: string, packageId: string) => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(instanceId)}/packages/${encodeURIComponent(packageId)}`,
    { method: "DELETE" },
  ),
  backups: (instanceId: string) => list<BackupRecord>(`/instances/${encodeURIComponent(instanceId)}/backups`),
  deleteBackup: (instanceId: string, backupId: string, idempotencyKey: string) => request<{ job_id: string; idempotent_replay?: boolean }>(
    `/instances/${encodeURIComponent(instanceId)}/backups/${encodeURIComponent(backupId)}`,
    { method: "DELETE", headers: { "Idempotency-Key": idempotencyKey } },
  ),
  createBackupUpload: (
    instanceId: string,
    payload: { name: string; platform: "windows/amd64" | "linux/amd64"; size: number; sha256: string },
    idempotencyKey: string,
  ) => request<{ upload: BackupUploadRecord; idempotent_replay?: boolean }>(`/instances/${encodeURIComponent(instanceId)}/backup-uploads`, {
    method: "POST", headers: { "Idempotency-Key": idempotencyKey }, body: JSON.stringify(payload),
  }),
  backupUpload: (instanceId: string, uploadId: string) => request<{ upload: BackupUploadRecord }>(
    `/instances/${encodeURIComponent(instanceId)}/backup-uploads/${encodeURIComponent(uploadId)}`,
  ).then((result) => result.upload),
  uploadBackupChunk: (
    instanceId: string,
    uploadId: string,
    payload: { offset: number; chunk_sha256: string; content_base64: string },
    idempotencyKey: string,
  ) => request<{ job_id: string; status: JobStatus; idempotent_replay?: boolean }>(
    `/instances/${encodeURIComponent(instanceId)}/backup-uploads/${encodeURIComponent(uploadId)}/chunks`,
    { method: "POST", headers: { "Idempotency-Key": idempotencyKey }, body: JSON.stringify(payload) },
  ),
  backupUploadChunkStatus: (instanceId: string, uploadId: string, jobId: string) => request<{ job_id: string; status: JobStatus }>(
    `/instances/${encodeURIComponent(instanceId)}/backup-uploads/${encodeURIComponent(uploadId)}/chunks/${encodeURIComponent(jobId)}`,
  ),
  completeBackupUpload: (instanceId: string, uploadId: string, idempotencyKey: string) => request<{ job_id: string; status: JobStatus; idempotent_replay?: boolean }>(
    `/instances/${encodeURIComponent(instanceId)}/backup-uploads/${encodeURIComponent(uploadId)}/complete`,
    { method: "POST", headers: { "Idempotency-Key": idempotencyKey } },
  ),
  abortBackupUpload: (instanceId: string, uploadId: string, idempotencyKey: string) => request<{ job_id: string; status: JobStatus; idempotent_replay?: boolean }>(
    `/instances/${encodeURIComponent(instanceId)}/backup-uploads/${encodeURIComponent(uploadId)}`,
    { method: "DELETE", headers: { "Idempotency-Key": idempotencyKey } },
  ),
  schedules: (instanceId: string) => list<ScheduleRecord>(`/instances/${encodeURIComponent(instanceId)}/schedules`),
  setScheduleEnabled: (instanceId: string, scheduleId: string, enabled: boolean) => request<{ schedule: ScheduleRecord }>(
    `/instances/${encodeURIComponent(instanceId)}/schedules/${encodeURIComponent(scheduleId)}`,
    { method: "PATCH", body: JSON.stringify({ enabled }) },
  ),
  deleteSchedule: (instanceId: string, scheduleId: string) => request<void>(
    `/instances/${encodeURIComponent(instanceId)}/schedules/${encodeURIComponent(scheduleId)}`,
    { method: "DELETE" },
  ),
  searchLogs: async (instanceId: string, payload: { keyword?: string; since?: string; until?: string; limit?: number }) => {
    const created = await request<{ job_id: string }>(`/instances/${encodeURIComponent(instanceId)}/logs/search`, { method: "POST", body: JSON.stringify(payload) });
    for (let attempt = 0; attempt < 240; attempt += 1) {
      const result = await request<{ status?: JobStatus; items?: LogSearchEntry[]; returned?: number; matched?: number; truncated?: boolean }>(
        `/instances/${encodeURIComponent(instanceId)}/logs/search/${encodeURIComponent(created.job_id)}`,
      );
      if (!result.status) return { items: result.items || [], returned: result.returned || 0, matched: result.matched || 0, truncated: Boolean(result.truncated) };
      await delay(500);
    }
    throw new ApiError("日志检索等待超时", 408, "log_search_timeout");
  },
  downloadLogChunk: async (instanceId: string, segment: number, offset: number, expectedSha256 = "") => {
    const created = await request<{ job_id: string }>(`/instances/${encodeURIComponent(instanceId)}/logs/downloads`, {
      method: "POST", body: JSON.stringify({ segment, offset, length: transferChunkSize, expected_sha256: expectedSha256 }),
    });
    return waitForTransfer(`/instances/${encodeURIComponent(instanceId)}/logs/downloads/${encodeURIComponent(created.job_id)}`);
  },
  downloadBackupChunk: async (instanceId: string, backupId: string, offset: number) => {
    const created = await request<{ job_id: string }>(`/instances/${encodeURIComponent(instanceId)}/backups/${encodeURIComponent(backupId)}/downloads`, {
      method: "POST", body: JSON.stringify({ offset, length: transferChunkSize }),
    });
    return waitForTransfer(`/instances/${encodeURIComponent(instanceId)}/backups/${encodeURIComponent(backupId)}/downloads/${encodeURIComponent(created.job_id)}`);
  },
  files: (instanceId: string, path = "") => request<ManagedFileList>(
    `/instances/${encodeURIComponent(instanceId)}/files?path=${encodeURIComponent(path)}`,
  ).then((result) => ({ ...result, items: result.items || [], shortcuts: result.shortcuts || [] })),
  fileContent: (instanceId: string, path: string) => request<ManagedFileContent>(
    `/instances/${encodeURIComponent(instanceId)}/files/content?path=${encodeURIComponent(path)}`,
  ),
  writeFile: (instanceId: string, path: string, content: string, expectedSha256?: string, contentBase64?: string) => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(instanceId)}/files/content?path=${encodeURIComponent(path)}`,
    { method: "PUT", body: JSON.stringify(contentBase64 != null ? { content_base64: contentBase64, expected_sha256: expectedSha256 } : { content, expected_sha256: expectedSha256 }) },
  ),
  writePackageFile: (instanceId: string, packageId: string, path: string, content: string, expectedSha256: string) => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(instanceId)}/packages/${encodeURIComponent(packageId)}/files/content?path=${encodeURIComponent(path)}`,
    { method: "PUT", body: JSON.stringify({ content, expected_sha256: expectedSha256 }) },
  ),
  packageFileContent: (instanceId: string, packageId: string, path: string) => request<ManagedFileContent>(
    `/instances/${encodeURIComponent(instanceId)}/packages/${encodeURIComponent(packageId)}/files/content?path=${encodeURIComponent(path)}`,
  ),
  importQQFlashFile: (instanceId: string, shareUrl: string) => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(instanceId)}/packages/qq-flash`,
    { method: "POST", body: JSON.stringify({ share_url: shareUrl }) },
  ),
  deleteFile: (instanceId: string, path: string, expectedSha256?: string) => request<{ job_id: string }>(
    `/instances/${encodeURIComponent(instanceId)}/files/content?path=${encodeURIComponent(path)}`,
    { method: "DELETE", headers: expectedSha256 ? { "If-Match": `"${expectedSha256}"` } : undefined },
  ),
  notifications: () => list<NotificationRecord>("/notifications"),
  markNotificationRead: (id: string) => request<void>(`/notifications/${encodeURIComponent(id)}/read`, { method: "POST" }),
  users: () => list<User>("/users"),
  createUser: (payload: { username: string; display_name?: string; role_bindings: Array<Pick<RoleBinding, "role" | "permissions" | "scope_type" | "node_id" | "instance_id">> }) =>
    request<UserActivationResult>("/users", { method: "POST", body: JSON.stringify(payload) }),
  reissueUserActivation: (id: string) => request<UserActivationResult>(`/users/${encodeURIComponent(id)}/activation`, { method: "POST" }),
  updateUser: (id: string, payload: { display_name?: string; password?: string; disabled?: boolean; role_bindings?: Array<{ role: Role; permissions?: RoleBinding["permissions"]; scope_type: "global" | "node" | "instance"; node_id?: string; instance_id?: string }> }) =>
    request<{ user: User }>(`/users/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(payload) }),
  userRoleBindings: (id: string) => list<RoleBinding>(`/users/${encodeURIComponent(id)}/role-bindings`),
  systemUpdates: () => request<SystemUpdateOverview>("/system/updates"),
  systemUpdate: (id: string) => request<SystemUpdateRecord>(`/system/updates/${encodeURIComponent(id)}`),
  uploadSystemUpdate: (file: File, onProgress?: (percent: number) => void) => new Promise<{ job_id: string; status: string; phase: string; target_version?: string }>((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `${baseURL}/system/updates`);
    xhr.withCredentials = true;
    xhr.setRequestHeader("Accept", "application/json");
    xhr.setRequestHeader("Idempotency-Key", newIdempotencyKey());
    if (csrfToken) xhr.setRequestHeader("X-CSRF-Token", csrfToken);
    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress?.(Math.min(100, Math.round((event.loaded / event.total) * 100)));
    };
    xhr.onerror = () => reject(new ApiError("升级包上传中断，请确认中心端网络连接", 0, "network_error"));
    xhr.onload = () => {
      let body: Record<string, unknown> = {};
      try { body = xhr.responseText ? JSON.parse(xhr.responseText) as Record<string, unknown> : {}; } catch { /* handled below */ }
      if (xhr.status < 200 || xhr.status >= 300) {
        const error = body.error as { message?: string; code?: string } | undefined;
        reject(new ApiError(error?.message || `请求失败（${xhr.status}）`, xhr.status, error?.code));
        return;
      }
      resolve(body as { job_id: string; status: string; phase: string; target_version?: string });
    };
    const form = new FormData();
    form.append("package", file, file.name);
    xhr.send(form);
  }),
};

export function apiBaseURL() {
  return baseURL;
}
