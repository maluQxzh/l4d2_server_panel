export const backupUploadChunkSize = 1 << 20;
export const maxBackupUploadSize = 20 * 1024 * 1024 * 1024;

const sha256Constants = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
];

function rotateRight(value: number, count: number) {
  return (value >>> count) | (value << (32 - count));
}

export class IncrementalSHA256 {
  private state = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
  private buffer = new Uint8Array(64);
  private bufferLength = 0;
  private bytesHashed = 0;
  private finished = false;

  update(input: Uint8Array) {
    if (this.finished) throw new Error("SHA-256 digest has already been finalized");
    this.bytesHashed += input.length;
    let offset = 0;
    if (this.bufferLength) {
      const take = Math.min(64 - this.bufferLength, input.length);
      this.buffer.set(input.subarray(0, take), this.bufferLength);
      this.bufferLength += take;
      offset = take;
      if (this.bufferLength === 64) {
        this.processBlock(this.buffer);
        this.bufferLength = 0;
      }
    }
    while (offset + 64 <= input.length) {
      this.processBlock(input.subarray(offset, offset + 64));
      offset += 64;
    }
    if (offset < input.length) {
      this.buffer.set(input.subarray(offset), 0);
      this.bufferLength = input.length - offset;
    }
    return this;
  }

  digestHex() {
    if (this.finished) throw new Error("SHA-256 digest has already been finalized");
    const messageBytes = this.bytesHashed;
    const paddingLength = this.bufferLength < 56 ? 56 - this.bufferLength : 120 - this.bufferLength;
    const padding = new Uint8Array(paddingLength + 8);
    padding[0] = 0x80;
    const bitLength = messageBytes * 8;
    const high = Math.floor(bitLength / 0x100000000);
    const low = bitLength >>> 0;
    const view = new DataView(padding.buffer);
    view.setUint32(padding.length - 8, high, false);
    view.setUint32(padding.length - 4, low, false);
    this.update(padding);
    this.finished = true;
    return Array.from(this.state, (value) => value.toString(16).padStart(8, "0")).join("");
  }

  private processBlock(block: Uint8Array) {
    const words = new Uint32Array(64);
    const view = new DataView(block.buffer, block.byteOffset, 64);
    for (let index = 0; index < 16; index += 1) words[index] = view.getUint32(index * 4, false);
    for (let index = 16; index < 64; index += 1) {
      const x = words[index - 15];
      const y = words[index - 2];
      const sigma0 = rotateRight(x, 7) ^ rotateRight(x, 18) ^ (x >>> 3);
      const sigma1 = rotateRight(y, 17) ^ rotateRight(y, 19) ^ (y >>> 10);
      words[index] = (words[index - 16] + sigma0 + words[index - 7] + sigma1) >>> 0;
    }
    let [a, b, c, d, e, f, g, h] = this.state;
    for (let index = 0; index < 64; index += 1) {
      const sum1 = rotateRight(e, 6) ^ rotateRight(e, 11) ^ rotateRight(e, 25);
      const choice = (e & f) ^ (~e & g);
      const temp1 = (h + sum1 + choice + sha256Constants[index] + words[index]) >>> 0;
      const sum0 = rotateRight(a, 2) ^ rotateRight(a, 13) ^ rotateRight(a, 22);
      const majority = (a & b) ^ (a & c) ^ (b & c);
      const temp2 = (sum0 + majority) >>> 0;
      h = g; g = f; f = e; e = (d + temp1) >>> 0; d = c; c = b; b = a; a = (temp1 + temp2) >>> 0;
    }
    this.state[0] = (this.state[0] + a) >>> 0;
    this.state[1] = (this.state[1] + b) >>> 0;
    this.state[2] = (this.state[2] + c) >>> 0;
    this.state[3] = (this.state[3] + d) >>> 0;
    this.state[4] = (this.state[4] + e) >>> 0;
    this.state[5] = (this.state[5] + f) >>> 0;
    this.state[6] = (this.state[6] + g) >>> 0;
    this.state[7] = (this.state[7] + h) >>> 0;
  }
}

export function sha256Hex(bytes: Uint8Array) {
  return new IncrementalSHA256().update(bytes).digestHex();
}

export async function hashBlobSHA256(blob: Blob, onProgress?: (processed: number, total: number) => void) {
  const hasher = new IncrementalSHA256();
  const readSize = 4 << 20;
  for (let offset = 0; offset < blob.size; offset += readSize) {
    const bytes = new Uint8Array(await readBlobSlice(blob.slice(offset, Math.min(blob.size, offset + readSize))));
    hasher.update(bytes);
    onProgress?.(Math.min(blob.size, offset + bytes.length), blob.size);
  }
  return hasher.digestHex();
}

export function readBlobSlice(blob: Blob): Promise<ArrayBuffer> {
  if (typeof blob.arrayBuffer === "function") return blob.arrayBuffer();
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error || new Error("无法读取备份分片"));
    reader.onload = () => resolve(reader.result as ArrayBuffer);
    reader.readAsArrayBuffer(blob);
  });
}

export function bytesToBase64(bytes: Uint8Array) {
  let binary = "";
  const encodingChunk = 0x8000;
  for (let offset = 0; offset < bytes.length; offset += encodingChunk) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + encodingChunk));
  }
  return btoa(binary);
}

export function validateBackupUploadFile(name: string, size: number) {
  if (!name || new TextEncoder().encode(name).length > 180 || !name.toLowerCase().endsWith(".zip") || /[\0\r\n/\\]/.test(name)) {
    return "请选择文件名安全且不超过 180 个字符的 .zip 备份";
  }
  if (!Number.isSafeInteger(size) || size < 1 || size > maxBackupUploadSize) {
    return "备份文件大小必须在 1 字节至 20 GiB 之间";
  }
  return "";
}
