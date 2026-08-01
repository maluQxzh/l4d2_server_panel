import { describe, expect, it } from "vitest";
import { hashBlobSHA256, IncrementalSHA256, sha256Hex, validateBackupUploadFile } from "./backup-upload";

const encoder = new TextEncoder();

describe("备份流式上传工具", () => {
  it("增量 SHA-256 与标准测试向量一致", () => {
    const hasher = new IncrementalSHA256();
    hasher.update(encoder.encode("a"));
    hasher.update(encoder.encode("bc"));
    expect(hasher.digestHex()).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    expect(sha256Hex(new Uint8Array())).toBe("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  });

  it("按 Blob 分片计算摘要并报告进度", async () => {
    const progress: number[] = [];
    const blob = new Blob([encoder.encode("streamed backup")]);
    await expect(hashBlobSHA256(blob, (processed) => progress.push(processed))).resolves.toBe("5e52a39a7a01469e76569f029f119fc2bd93b01c546341c6aec186f7a9092539");
    expect(progress.at(-1)).toBe(blob.size);
  });

  it("只接受安全 ZIP 名称和有界大小", () => {
    expect(validateBackupUploadFile("daily.zip", 1024)).toBe("");
    expect(validateBackupUploadFile("../daily.zip", 1024)).toMatch(/文件名安全/);
    expect(validateBackupUploadFile("daily.tar", 1024)).toMatch(/\.zip/);
    expect(validateBackupUploadFile("empty.zip", 0)).toMatch(/大小/);
    expect(validateBackupUploadFile(`${"备".repeat(60)}.zip`, 1024)).toMatch(/180/);
  });
});
