import { maxBackupUploadSize } from "./backup-upload";

export type PersistedBackupPlatform = "windows/amd64" | "linux/amd64";

export interface PersistedBackupChunk {
  offset: number;
  length: number;
  sha256: string;
  idempotencyKey: string;
  jobId?: string;
}

export interface PersistedBackupComplete {
  idempotencyKey: string;
  jobId?: string;
}

export interface PersistedBackupAbort {
  idempotencyKey: string;
  jobId?: string;
}

export interface PersistedBackupUploadSession {
  version: 1;
  instanceId: string;
  createKey: string;
  uploadId?: string;
  name: string;
  size: number;
  sha256: string;
  platform: PersistedBackupPlatform;
  expiresAt?: string;
  pendingChunk?: PersistedBackupChunk;
  pendingComplete?: PersistedBackupComplete;
  pendingAbort?: PersistedBackupAbort;
  restoreJobId?: string;
  updatedAt: string;
}

const storagePrefix = "l4d2panel.backup-upload.v1";
const digestPattern = /^[a-f0-9]{64}$/;

export function backupUploadSessionStorageKey(instanceId: string) {
  return `${storagePrefix}:${instanceId}`;
}

export function newBackupUploadKey(prefix: string) {
  const random = typeof crypto?.randomUUID === "function"
    ? crypto.randomUUID()
    : `${Date.now()}-${Math.random().toString(16).slice(2)}`;
  return `${prefix}:${random}`;
}

export function sameBackupUploadFile(
  session: PersistedBackupUploadSession,
  file: { name: string; size: number; sha256: string; platform: PersistedBackupPlatform },
) {
  return session.name === file.name
    && session.size === file.size
    && session.sha256 === file.sha256
    && session.platform === file.platform;
}

export function backupUploadSessionExpired(session: PersistedBackupUploadSession, now = Date.now()) {
  if (!session.expiresAt) return false;
  const expires = Date.parse(session.expiresAt);
  return !Number.isFinite(expires) || expires <= now;
}

export function loadBackupUploadSession(instanceId: string, storage: Storage = window.localStorage) {
  let raw: string | null;
  try {
    raw = storage.getItem(backupUploadSessionStorageKey(instanceId));
  } catch {
    return null;
  }
  if (!raw) return null;
  try {
    const value = JSON.parse(raw) as unknown;
    return validSession(value, instanceId) ? value : null;
  } catch {
    return null;
  }
}

export function saveBackupUploadSession(session: PersistedBackupUploadSession, storage: Storage = window.localStorage) {
  if (!validSession(session, session.instanceId)) throw new Error("Invalid persisted backup upload session");
  try {
    storage.setItem(backupUploadSessionStorageKey(session.instanceId), JSON.stringify(session));
  } catch {
    // Uploads remain safe and idempotent within the current page even if browser storage is unavailable.
  }
  return session;
}

export function clearBackupUploadSession(instanceId: string, storage: Storage = window.localStorage) {
  try {
    storage.removeItem(backupUploadSessionStorageKey(instanceId));
  } catch {
    // Storage can be disabled by browser policy; there is nothing else to clear.
  }
}

function validSession(value: unknown, instanceId: string): value is PersistedBackupUploadSession {
  if (!isObject(value)) return false;
  if (value.version !== 1 || value.instanceId !== instanceId || !validText(value.createKey, 200)) return false;
  if (!validText(value.name, 180) || !Number.isSafeInteger(value.size)
    || (value.size as number) < 1 || (value.size as number) > maxBackupUploadSize) return false;
  if (typeof value.sha256 !== "string" || !digestPattern.test(value.sha256)) return false;
  if (value.platform !== "windows/amd64" && value.platform !== "linux/amd64") return false;
  if (!validText(value.updatedAt, 80) || !validOptionalText(value.uploadId, 200)) return false;
  if (!validOptionalText(value.expiresAt, 80) || !validOptionalText(value.restoreJobId, 200)) return false;
  if (value.pendingChunk !== undefined && !validChunk(value.pendingChunk)) return false;
  if (value.pendingComplete !== undefined && !validComplete(value.pendingComplete)) return false;
  if (value.pendingAbort !== undefined && !validComplete(value.pendingAbort)) return false;
  return true;
}

function validChunk(value: unknown) {
  if (!isObject(value)) return false;
  return Number.isSafeInteger(value.offset) && (value.offset as number) >= 0
    && Number.isSafeInteger(value.length) && (value.length as number) > 0 && (value.length as number) <= (1 << 20)
    && typeof value.sha256 === "string" && digestPattern.test(value.sha256)
    && validText(value.idempotencyKey, 200)
    && validOptionalText(value.jobId, 200);
}

function validComplete(value: unknown) {
  return isObject(value) && validText(value.idempotencyKey, 200) && validOptionalText(value.jobId, 200);
}

function validOptionalText(value: unknown, maxLength: number) {
  return value === undefined || validText(value, maxLength);
}

function validText(value: unknown, maxLength: number) {
  return typeof value === "string" && value.length > 0 && value.length <= maxLength;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
