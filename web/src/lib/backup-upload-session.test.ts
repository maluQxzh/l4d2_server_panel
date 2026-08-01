import { beforeEach, describe, expect, it } from "vitest";
import {
  backupUploadSessionExpired,
  backupUploadSessionStorageKey,
  clearBackupUploadSession,
  loadBackupUploadSession,
  sameBackupUploadFile,
  saveBackupUploadSession,
  type PersistedBackupUploadSession,
} from "./backup-upload-session";
import { maxBackupUploadSize } from "./backup-upload";

const session = (): PersistedBackupUploadSession => ({
  version: 1,
  instanceId: "instance-1",
  createKey: "backup-create:stable-key",
  uploadId: "upload-1",
  name: "portable.zip",
  size: 4096,
  sha256: "a".repeat(64),
  platform: "linux/amd64",
  expiresAt: "2099-08-02T00:00:00Z",
  pendingChunk: {
    offset: 0,
    length: 4096,
    sha256: "b".repeat(64),
    idempotencyKey: "backup-chunk:stable-key",
  },
  pendingAbort: { idempotencyKey: "backup-abort:stable-key" },
  updatedAt: "2026-08-01T00:00:00Z",
});

describe("跨节点备份上传会话", () => {
  beforeEach(() => localStorage.clear());

  it("按实例持久化创建键、会话标识、文件身份与未确认分块键", () => {
    saveBackupUploadSession(session());

    expect(loadBackupUploadSession("instance-1")).toEqual(session());
    expect(JSON.parse(localStorage.getItem(backupUploadSessionStorageKey("instance-1")) || "null")).toMatchObject({
      createKey: "backup-create:stable-key",
      uploadId: "upload-1",
      name: "portable.zip",
      size: 4096,
      sha256: "a".repeat(64),
      platform: "linux/amd64",
      expiresAt: "2099-08-02T00:00:00Z",
      pendingChunk: { idempotencyKey: "backup-chunk:stable-key" },
      pendingAbort: { idempotencyKey: "backup-abort:stable-key" },
    });
  });

  it("隔离不同实例并可显式清理", () => {
    saveBackupUploadSession(session());
    expect(loadBackupUploadSession("instance-2")).toBeNull();
    clearBackupUploadSession("instance-1");
    expect(loadBackupUploadSession("instance-1")).toBeNull();
  });

  it("拒绝损坏、串实例或危险结构的存储值", () => {
    localStorage.setItem(backupUploadSessionStorageKey("instance-1"), "{");
    expect(loadBackupUploadSession("instance-1")).toBeNull();

    localStorage.setItem(backupUploadSessionStorageKey("instance-1"), JSON.stringify({ ...session(), instanceId: "instance-2" }));
    expect(loadBackupUploadSession("instance-1")).toBeNull();

    localStorage.setItem(backupUploadSessionStorageKey("instance-1"), JSON.stringify({ ...session(), sha256: "not-a-digest" }));
    expect(loadBackupUploadSession("instance-1")).toBeNull();

    localStorage.setItem(backupUploadSessionStorageKey("instance-1"), JSON.stringify({ ...session(), size: maxBackupUploadSize + 1 }));
    expect(loadBackupUploadSession("instance-1")).toBeNull();

    localStorage.setItem(backupUploadSessionStorageKey("instance-1"), JSON.stringify({ ...session(), createKey: "x".repeat(201) }));
    expect(loadBackupUploadSession("instance-1")).toBeNull();
  });

  it("用名称、大小、摘要和平台共同识别重选文件并判断过期", () => {
    const value = session();
    expect(sameBackupUploadFile(value, { name: value.name, size: value.size, sha256: value.sha256, platform: value.platform })).toBe(true);
    expect(sameBackupUploadFile(value, { name: value.name, size: value.size, sha256: value.sha256, platform: "windows/amd64" })).toBe(false);
    expect(backupUploadSessionExpired(value, Date.parse("2099-08-01T00:00:00Z"))).toBe(false);
    expect(backupUploadSessionExpired(value, Date.parse("2099-08-03T00:00:00Z"))).toBe(true);
  });
});
