import { IncrementalSHA256, readBlobSlice } from "./backup-upload";
import type { TransferChunk } from "./types";

export const maxBufferedBackupDownload = 512 * 1024 * 1024;

export interface BackupDownloadWriter {
  write(data: Blob): Promise<void>;
  close(): Promise<void>;
  abort?(reason?: unknown): Promise<void>;
}

interface ReceiveBackupOptions {
  expectedSize: number;
  expectedSHA256?: string;
  fetchChunk: (offset: number) => Promise<TransferChunk>;
  writer?: BackupDownloadWriter;
  onProgress?: (percent: number) => void;
}

export async function receiveBackupDownload(options: ReceiveBackupOptions): Promise<Blob | null> {
  const { expectedSize, expectedSHA256 = "", fetchChunk, writer, onProgress } = options;
  if (!Number.isSafeInteger(expectedSize) || expectedSize < 1) throw new Error("备份下载大小无效");
  const parts: BlobPart[] | null = writer ? null : [];
  const hasher = new IncrementalSHA256();
  let offset = 0;
  try {
    for (;;) {
      const chunk = await fetchChunk(offset);
      if (chunk.offset !== offset || chunk.nextOffset <= offset || chunk.nextOffset > expectedSize
        || chunk.nextOffset - chunk.offset !== chunk.blob.size || chunk.totalSize !== expectedSize
        || chunk.eof !== (chunk.nextOffset === expectedSize)) {
        throw new Error("备份下载分块顺序或大小与清单不一致");
      }
      hasher.update(new Uint8Array(await readBlobSlice(chunk.blob)));
      if (writer) await writer.write(chunk.blob);
      else parts!.push(chunk.blob);
      offset = chunk.nextOffset;
      onProgress?.(Math.round((offset / expectedSize) * 100));
      if (chunk.eof) break;
    }
    if (offset !== expectedSize) throw new Error("备份下载大小与清单不一致");
    const digest = hasher.digestHex();
    if (/^[a-f0-9]{64}$/i.test(expectedSHA256) && digest !== expectedSHA256.toLowerCase()) {
      throw new Error("备份下载 SHA-256 与清单不一致");
    }
    if (writer) {
      await writer.close();
      return null;
    }
    return new Blob(parts!, { type: "application/zip" });
  } catch (error) {
    if (writer?.abort) {
      try {
        await writer.abort(error);
      } catch {
        // Preserve the original transfer failure.
      }
    }
    throw error;
  }
}
