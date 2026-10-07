// 增量 SHA-256。
//
// 为什么不用 crypto.subtle.digest：它只能对一整块缓冲区求哈希，而我们下载的
// 明文是按块累积的碎片。要么把全部碎片拼成一块连续内存（大文件上等于把内存
// 占用翻倍），要么就得自己维护流式状态。这里选择后者。

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

export class Sha256 {
  private readonly state = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  private readonly buffer = new Uint8Array(64);
  private bufferLen = 0;
  private totalLen = 0;
  private readonly w = new Uint32Array(64);
  private finished = false;

  update(data: Uint8Array): this {
    if (this.finished) {
      throw new Error("SHA-256 状态已终结，不能再追加数据");
    }
    this.totalLen += data.byteLength;
    let offset = 0;

    if (this.bufferLen > 0) {
      const need = 64 - this.bufferLen;
      const take = Math.min(need, data.byteLength);
      this.buffer.set(data.subarray(0, take), this.bufferLen);
      this.bufferLen += take;
      offset = take;
      if (this.bufferLen === 64) {
        this.compress(this.buffer, 0);
        this.bufferLen = 0;
      }
    }

    while (data.byteLength - offset >= 64) {
      this.compress(data, offset);
      offset += 64;
    }

    if (offset < data.byteLength) {
      this.buffer.set(data.subarray(offset), 0);
      this.bufferLen = data.byteLength - offset;
    }
    return this;
  }

  digest(): Uint8Array {
    if (!this.finished) {
      this.finish();
    }
    const out = new Uint8Array(32);
    for (let i = 0; i < 8; i++) {
      out[i * 4] = (this.state[i] >>> 24) & 0xff;
      out[i * 4 + 1] = (this.state[i] >>> 16) & 0xff;
      out[i * 4 + 2] = (this.state[i] >>> 8) & 0xff;
      out[i * 4 + 3] = this.state[i] & 0xff;
    }
    return out;
  }

  hex(): string {
    return bytesToHex(this.digest());
  }

  private finish(): void {
    // 填充长度必须让「已有字节 + 填充」正好凑成 64 的整数倍，且至少留出
    // 1 字节的 0x80 与 8 字节长度。用固定长度（64/128）是错的：那样在
    // bufferLen 落在 56..63 时会算错边界，表现为"有时候对、有时候错"。
    const bitsHigh = Math.floor(this.totalLen / 0x20000000);
    const bitsLow = (this.totalLen * 8) >>> 0;

    const padTotal = this.bufferLen < 56 ? 64 - this.bufferLen : 128 - this.bufferLen;
    const pad = new Uint8Array(padTotal);
    pad[0] = 0x80;
    const tail = padTotal - 8;
    pad[tail] = (bitsHigh >>> 24) & 0xff;
    pad[tail + 1] = (bitsHigh >>> 16) & 0xff;
    pad[tail + 2] = (bitsHigh >>> 8) & 0xff;
    pad[tail + 3] = bitsHigh & 0xff;
    pad[tail + 4] = (bitsLow >>> 24) & 0xff;
    pad[tail + 5] = (bitsLow >>> 16) & 0xff;
    pad[tail + 6] = (bitsLow >>> 8) & 0xff;
    pad[tail + 7] = bitsLow & 0xff;

    if (this.bufferLen === 0) {
      // 缓冲区正好空着：填充自身就是完整的一块。
      this.compress(pad, 0);
    } else {
      // 末块 = 缓冲区里的残留字节 ‖ 填充的前半部分。
      const need = 64 - this.bufferLen;
      const block = new Uint8Array(64);
      block.set(this.buffer.subarray(0, this.bufferLen), 0);
      block.set(pad.subarray(0, need), this.bufferLen);
      this.compress(block, 0);
      if (padTotal > need) {
        // 残留字节在 56..63 之间时填充会跨到第二块，剩余部分恰好 64 字节。
        this.compress(pad, need);
      }
    }
    this.finished = true;
  }

  private compress(chunk: Uint8Array, offset: number): void {
    const w = this.w;
    for (let i = 0; i < 16; i++) {
      const j = offset + i * 4;
      w[i] = ((chunk[j] << 24) | (chunk[j + 1] << 16) | (chunk[j + 2] << 8) | chunk[j + 3]) >>> 0;
    }
    for (let i = 16; i < 64; i++) {
      const a = w[i - 15];
      const b = w[i - 2];
      const s0 = ((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3);
      const s1 = ((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }

    let [a, b, c, d, e, f, g, h] = this.state;

    for (let i = 0; i < 64; i++) {
      const s1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const ch = (e & f) ^ (~e & g);
      const t1 = (h + s1 + ch + K[i] + w[i]) >>> 0;
      const s0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (s0 + maj) >>> 0;

      h = g;
      g = f;
      f = e;
      e = (d + t1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) >>> 0;
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

export function bytesToHex(bytes: Uint8Array): string {
  let out = "";
  for (let i = 0; i < bytes.byteLength; i++) {
    out += bytes[i].toString(16).padStart(2, "0");
  }
  return out;
}
