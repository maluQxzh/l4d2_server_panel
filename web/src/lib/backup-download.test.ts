import { describe, expect, it, vi } from "vitest";
import { receiveBackupDownload, type BackupDownloadWriter } from "./backup-download";
import { readBlobSlice, sha256Hex } from "./backup-upload";
import type { TransferChunk } from "./types";

function chunks(value: Uint8Array, size: number) {
  return async (offset: number): Promise<TransferChunk> => {
    const nextOffset = Math.min(value.length, offset + size);
    const blob = new Blob([value.slice(offset, nextOffset)]);
    return {
      blob, offset, nextOffset, totalSize: value.length, eof: nextOffset === value.length,
      fileSha256: sha256Hex(value), chunkSha256: "",
    };
  };
}

describe("备份流式下载", () => {
  it("小文件回退模式按序聚合并校验完整摘要", async () => {
    const value = new TextEncoder().encode("portable backup download");
    const progress: number[] = [];
    const result = await receiveBackupDownload({
      expectedSize: value.length,
      expectedSHA256: sha256Hex(value),
      fetchChunk: chunks(value, 5),
      onProgress: (percent) => progress.push(percent),
    });
    expect(Array.from(new Uint8Array(await readBlobSlice(result!)))).toEqual(Array.from(value));
    expect(progress.at(-1)).toBe(100);
  });

  it("可写文件句柄逐块落盘而不聚合 Blob", async () => {
    const value = new TextEncoder().encode("stream this backup");
    const written: Uint8Array[] = [];
    const writer: BackupDownloadWriter = {
      write: vi.fn(async (blob: Blob) => { written.push(new Uint8Array(await readBlobSlice(blob))); }),
      close: vi.fn(async () => undefined),
      abort: vi.fn(async () => undefined),
    };
    const result = await receiveBackupDownload({
      expectedSize: value.length,
      expectedSHA256: sha256Hex(value),
      fetchChunk: chunks(value, 4),
      writer,
    });
    expect(result).toBeNull();
    expect(writer.close).toHaveBeenCalledOnce();
    expect(writer.abort).not.toHaveBeenCalled();
    expect(Array.from(new Uint8Array(await readBlobSlice(new Blob(written))))).toEqual(Array.from(value));
  });

  it("分块元数据或完整摘要异常时中止目标文件", async () => {
    const value = new TextEncoder().encode("corrupt backup");
    const writer: BackupDownloadWriter = {
      write: vi.fn(async () => undefined), close: vi.fn(async () => undefined), abort: vi.fn(async () => undefined),
    };
    await expect(receiveBackupDownload({
      expectedSize: value.length,
      expectedSHA256: "f".repeat(64),
      fetchChunk: chunks(value, value.length),
      writer,
    })).rejects.toThrow("SHA-256");
    expect(writer.close).not.toHaveBeenCalled();
    expect(writer.abort).toHaveBeenCalledOnce();
  });
});
