// 后端请求得到地址后，前端直接去拿的浏览器实现。
//
// 流程与站内前端一致：你自己的后端用 API Key 请求交付计划（clientPublicKey
// 由本模块现场生成），计划里包含 123 地址、分卷清单、校验值和用本次临时公钥
// 封装的密钥信封，但没有 API Key。浏览器直接向 123 取密文，在本地完成 XPH
// 分块 AES-GCM 解密与 SHA-256 校验，再交给浏览器保存。
//
// 站点与你的后端都不经过文件字节；API Key 也不要放进这个页面。

const MAGIC = "XPHCRPT1";
const HEADER_SIZE = 64;
const AAD_SIZE = 40;
const TAG_SIZE = 16;
const NONCE_SIZE = 12;
const KEY_SIZE = 32;
const MIN_BLOCK_LOG2 = 9;
const MAX_BLOCK_LOG2 = 26;
const DEFAULT_STREAM_THRESHOLD = 256 * 1024 * 1024;
const DEFAULT_BATCH_BLOCKS = 8;
const DEFAULT_CONCURRENCY = 6;

export class DeliveryError extends Error {
  constructor(message) {
    super(message);
    this.name = "DeliveryError";
  }
}

/** 需要 HTTPS（或 localhost）与 WebCrypto；非安全上下文下无法在浏览器本地解密。 */
export function encryptionSupported() {
  return typeof crypto !== "undefined" && !!crypto.subtle && globalThis.isSecureContext;
}

/**
 * 生成一次交付专用的临时 RSA-OAEP 密钥对。
 * 私钥只留在内存里，用完即弃；每次交付都新建。
 */
export async function createClientKeyPair() {
  if (!encryptionSupported()) {
    throw new DeliveryError("当前环境不支持浏览器本地解密（需要 HTTPS 与 WebCrypto）");
  }
  const pair = await crypto.subtle.generateKey(
    {
      name: "RSA-OAEP",
      modulusLength: 2048,
      publicExponent: new Uint8Array([0x01, 0x00, 0x01]),
      hash: "SHA-256",
    },
    false,
    ["encrypt", "decrypt"],
  );
  const spki = await crypto.subtle.exportKey("spki", pair.publicKey);
  return {
    /** base64 SPKI DER，提交给你自己的后端并转发给站点的 clientPublicKey。 */
    publicKey: bytesToBase64(new Uint8Array(spki)),
    /** 解开交付计划里的 keyEnvelope，得到 32 字节内容密钥。 */
    async unwrap(envelopeBase64) {
      let raw;
      try {
        raw = await crypto.subtle.decrypt(
          { name: "RSA-OAEP" },
          pair.privateKey,
          base64ToBytes(envelopeBase64),
        );
      } catch (err) {
        throw new DeliveryError(`解开内容密钥失败：${String(err)}`);
      }
      const dek = new Uint8Array(raw);
      if (dek.byteLength !== KEY_SIZE) {
        throw new DeliveryError(`内容密钥长度应为 ${KEY_SIZE}，实得 ${dek.byteLength}`);
      }
      return dek;
    },
  };
}

/**
 * 向你自己的后端请求交付计划。后端应调用站点 /api/fs/download，
 * 把浏览器公钥放进 clientPublicKey，并代传观测到的访客真实 IP（clientIp，
 * 仅 API Key 生效），再把计划原样返回（其中没有 API Key）。
 */
export async function requestDeliveryPlan(endpoint, path, pair, { signal } = {}) {
  const res = await fetch(endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path, clientPublicKey: pair.publicKey }),
    signal,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new DeliveryError(data.error || `申请交付计划失败：HTTP ${res.status}`);
  }
  return data;
}

/**
 * 回执交付票据。密文通道按预扣额度结算；明文通道由服务端结算，
 * 带 Range 的中转流不结算，票据到期退款。
 */
export async function reportSettlement(endpoint, plan, completed, { signal } = {}) {
  if (!endpoint || !plan?.ticketId || plan.mode === "proxy_decrypt") {
    return true;
  }
  try {
    const res = await fetch(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ticketId: plan.ticketId, completed }),
      signal,
    });
    return res.ok;
  } catch {
    return false;
  }
}

/**
 * 接收一次密文直链交付：取文件头 → 解密钥 → 分块解密 → 校验 → 保存。
 *
 * 只接受 mode=direct 且 contentForm=ciphertext 的计划。站点未启用「优先直链
 * 交付」或上游直链签发失败时会降级为中转，此时应由方式 1 的示例处理，本模块
 * 明确报错而不是退回站点中转。
 */
export async function receiveDelivery(plan, pair, options = {}) {
  const report = options.onProgress ?? (() => {});
  const signal = options.signal;
  const meta = plan?.encryption;
  if (plan?.mode !== "direct") {
    throw new DeliveryError(
      `本次交付计划为 ${plan?.mode ?? "未知"}，不是前端直接向 123 取密文所需的计划。` +
        "请确认站点已打开「优先直链交付」，且 123 直链空间与直链流量可用。",
    );
  }
  if (plan.contentForm !== "ciphertext") {
    throw new DeliveryError("交付计划是明文，不需要浏览器本地解密；请改用方式 1 的后端中转示例。");
  }
  if (!meta) {
    throw new DeliveryError("交付计划声明为密文，但没有提供解密元数据。");
  }
  if (!plan.checksum) {
    throw new DeliveryError("交付计划缺少明文摘要，无法校验完整性。");
  }

  report({ phase: "fetching", bytesDone: 0, bytesTotal: meta.plainSize, message: "正在读取文件头" });
  const headerBytes = await fetchCipherRange(plan, 0, HEADER_SIZE - 1, signal);
  const header = parseXphHeader(headerBytes);
  assertHeaderMatchesMeta(header, meta);

  const dek = await pair.unwrap(meta.keyEnvelope);
  const key = await crypto.subtle.importKey("raw", dek, { name: "AES-GCM" }, false, [
    "decrypt",
  ]);

  const sink =
    meta.plainSize >= (options.streamToDiskAbove ?? DEFAULT_STREAM_THRESHOLD)
      ? await (options.openDiskSink ?? tryOpenDiskSink)(plan.fileName)
      : null;
  const parts = [];
  const hasher = new Sha256();
  let done = 0;

  try {
    await decryptAll(
      key,
      header,
      (start, endExclusive, rangeSignal) =>
        fetchCipherRange(plan, start, endExclusive - 1, rangeSignal),
      async (plain) => {
        hasher.update(plain);
        if (sink) {
          await sink.write(plain);
        } else {
          parts.push(plain);
        }
        done += plain.byteLength;
        report({
          phase: "decrypting",
          bytesDone: done,
          bytesTotal: header.plainSize,
          message: "正在本地解密",
        });
      },
      {
        concurrency: options.concurrency ?? DEFAULT_CONCURRENCY,
        batchBlocks: options.batchBlocks ?? DEFAULT_BATCH_BLOCKS,
        signal,
      },
    );
    signal?.throwIfAborted();

    report({
      phase: "verifying",
      bytesDone: done,
      bytesTotal: header.plainSize,
      message: "正在校验完整性",
    });
    const digest = hasher.hex();
    if (digest !== String(plan.checksum).toLowerCase()) {
      throw new DeliveryError(`内容校验失败：期望 ${plan.checksum}，实得 ${digest}`);
    }

    signal?.throwIfAborted();
    report({
      phase: "delivering",
      bytesDone: done,
      bytesTotal: header.plainSize,
      message: "正在保存",
    });
    if (sink) {
      await sink.close();
      return {
        fileName: plan.fileName,
        mimeType: plan.mimeType,
        bytes: done,
        checksum: digest,
        blob: null,
        savedAs: sink.name,
      };
    }
    return {
      fileName: plan.fileName,
      mimeType: plan.mimeType,
      bytes: done,
      checksum: digest,
      blob: new Blob(parts, { type: plan.mimeType || "application/octet-stream" }),
      savedAs: null,
    };
  } catch (err) {
    await sink?.abort().catch(() => {});
    throw err;
  }
}

/**
 * 按块序并发取密文并解密。
 *
 * 与站内前端同一条窗口策略：在途批次最多 concurrency 个，已解密但尚未轮到
 * 交付的批次按序暂存；回调永远按块序执行，避免并发完成顺序把文件拼错。
 */
async function decryptAll(key, header, readRange, onBlock, options = {}) {
  if (header.blockCount === 0) {
    return;
  }
  const concurrency = Math.max(1, options.concurrency ?? DEFAULT_CONCURRENCY);
  const batchBlocks = Math.max(1, options.batchBlocks ?? DEFAULT_BATCH_BLOCKS);
  const batchCount = Math.ceil(header.blockCount / batchBlocks);
  const workController = new AbortController();
  const abortWork = () => workController.abort();
  if (options.signal?.aborted) {
    abortWork();
  } else {
    options.signal?.addEventListener("abort", abortWork, { once: true });
  }
  const signal = workController.signal;

  const pending = new Map();
  let deliverIndex = 0;
  let deliverError = null;
  const windowWaiters = new Set();
  const wakeWindowWaiters = () => {
    for (const wake of [...windowWaiters]) {
      wake();
    }
  };
  const waitForWindow = async (batchIndex) => {
    while (batchIndex >= deliverIndex + concurrency) {
      if (deliverError) {
        return false;
      }
      if (signal.aborted) {
        throw abortError();
      }
      await new Promise((resolve, reject) => {
        const cleanup = () => {
          windowWaiters.delete(wake);
          signal.removeEventListener("abort", abort);
        };
        const wake = () => {
          cleanup();
          resolve();
        };
        const abort = () => {
          cleanup();
          reject(abortError());
        };
        windowWaiters.add(wake);
        signal.addEventListener("abort", abort, { once: true });
        if (signal.aborted) {
          abort();
        }
      });
    }
    return !deliverError;
  };

  // 串成一条 promise 链，保证同一时刻只有一处调用 onBlock，且严格按块序。
  let delivery = Promise.resolve();
  const scheduleDelivery = () => {
    delivery = delivery.then(async () => {
      for (;;) {
        const blocks = pending.get(deliverIndex);
        if (!blocks) {
          return;
        }
        pending.delete(deliverIndex);
        const base = deliverIndex * batchBlocks;
        for (let i = 0; i < blocks.length; i++) {
          if (signal.aborted) {
            throw abortError();
          }
          await onBlock(blocks[i], base + i);
        }
        deliverIndex += 1;
        wakeWindowWaiters();
      }
    });
    // 交付侧失败（写盘被拒、Blob 分配失败）要立刻让在途 worker 停手。
    void delivery.catch((err) => {
      deliverError ??= err;
      workController.abort();
      wakeWindowWaiters();
    });
  };

  let nextBatch = 0;
  const worker = async () => {
    for (;;) {
      if (signal.aborted) {
        throw abortError();
      }
      if (deliverError) {
        return;
      }
      const batchIndex = nextBatch++;
      if (batchIndex >= batchCount) {
        return;
      }
      if (!(await waitForWindow(batchIndex))) {
        return;
      }
      const first = batchIndex * batchBlocks;
      const last = Math.min(first + batchBlocks, header.blockCount) - 1;
      const start = blockCipherOffset(header, first);
      const end = blockCipherOffset(header, last) + blockCipherLen(header, last);
      const cipher = await readRange(start, end, signal);

      const blocks = [];
      let cursor = 0;
      for (let index = first; index <= last; index++) {
        const length = blockCipherLen(header, index);
        blocks.push(await decryptBlock(key, header, index, cipher.subarray(cursor, cursor + length)));
        cursor += length;
      }
      if (signal.aborted) {
        throw abortError();
      }
      pending.set(batchIndex, blocks);
      scheduleDelivery();
    }
  };

  const workers = Array.from({ length: Math.min(concurrency, batchCount) }, worker);
  try {
    await Promise.all(workers);
    await delivery;
  } catch (err) {
    workController.abort();
    deliverError ??= err;
    await Promise.allSettled(workers);
    await delivery.catch(() => {});
    pending.clear();
    wakeWindowWaiters();
    throw deliverError;
  } finally {
    options.signal?.removeEventListener("abort", abortWork);
  }
}

/** 把内存中的结果交给浏览器下载；流式落盘时无需再保存。 */
export function saveDelivery(result) {
  if (result.savedAs || !result.blob) {
    return;
  }
  const url = URL.createObjectURL(result.blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = result.fileName || "download.bin";
  anchor.rel = "noopener";
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 60000);
}

// ---------------------------------------------------------------- XPH 解析

export function parseXphHeader(raw) {
  if (raw.byteLength < HEADER_SIZE) {
    throw new DeliveryError(`文件头被截断：需要 ${HEADER_SIZE} 字节，实得 ${raw.byteLength}`);
  }
  if (latin1(raw.subarray(0, MAGIC.length)) !== MAGIC) {
    throw new DeliveryError("密文魔数不匹配，对象可能已损坏");
  }
  const algo = raw[8];
  if (algo !== 1) {
    throw new DeliveryError(`不支持的加密算法标识 ${algo}`);
  }
  const blockLog2 = raw[9];
  if (blockLog2 < MIN_BLOCK_LOG2 || blockLog2 > MAX_BLOCK_LOG2) {
    throw new DeliveryError(`非法的块大小指数 ${blockLog2}`);
  }
  const view = new DataView(raw.buffer, raw.byteOffset, HEADER_SIZE);
  const plainSizeBig = view.getBigUint64(28, true);
  if (plainSizeBig > 1n << 62n) {
    throw new DeliveryError("非法的明文长度");
  }
  const plainSize = Number(plainSizeBig);
  if (!Number.isSafeInteger(plainSize)) {
    throw new DeliveryError("明文长度超出浏览器可安全表示的范围");
  }
  const noncePrefix = view.getUint32(36, true);
  const blockSize = 2 ** blockLog2;
  const blockCount = plainSize <= 0 ? 0 : Math.ceil(plainSize / blockSize);
  return {
    raw: raw.subarray(0, HEADER_SIZE),
    aad: raw.subarray(0, AAD_SIZE),
    blockLog2,
    blockSize,
    plainSize,
    noncePrefix,
    blockCount,
    cipherSize: HEADER_SIZE + plainSize + blockCount * TAG_SIZE,
  };
}

function assertHeaderMatchesMeta(header, meta) {
  const problems = [];
  if (header.blockLog2 !== meta.chunkLog2) {
    problems.push(`块大小指数 头=${header.blockLog2} 记录=${meta.chunkLog2}`);
  }
  if (header.plainSize !== meta.plainSize) {
    problems.push(`明文长度 头=${header.plainSize} 记录=${meta.plainSize}`);
  }
  if (header.cipherSize !== meta.cipherSize) {
    problems.push(`密文长度 头=${header.cipherSize} 记录=${meta.cipherSize}`);
  }
  if (header.noncePrefix !== meta.noncePrefix) {
    problems.push(`nonce 前缀 头=${header.noncePrefix} 记录=${meta.noncePrefix}`);
  }
  if (problems.length > 0) {
    throw new DeliveryError(`对象与记录不一致，已中止（${problems.join("；")}）`);
  }
}

function blockCipherOffset(header, index) {
  return HEADER_SIZE + index * (header.blockSize + TAG_SIZE);
}

function blockPayloadLen(header, index) {
  if (index < 0 || index >= header.blockCount) {
    return 0;
  }
  if (index < header.blockCount - 1) {
    return header.blockSize;
  }
  return header.plainSize - (header.blockCount - 1) * header.blockSize;
}

function blockCipherLen(header, index) {
  return blockPayloadLen(header, index) + TAG_SIZE;
}

async function decryptBlock(key, header, index, cipherBlock) {
  const expected = blockCipherLen(header, index);
  if (cipherBlock.byteLength !== expected) {
    throw new DeliveryError(`第 ${index} 块密文长度应为 ${expected}，实得 ${cipherBlock.byteLength}`);
  }
  const nonce = new Uint8Array(NONCE_SIZE);
  const view = new DataView(nonce.buffer);
  view.setUint32(0, header.noncePrefix, false);
  view.setBigUint64(4, BigInt(index), false);
  try {
    const plain = await crypto.subtle.decrypt(
      {
        name: "AES-GCM",
        iv: nonce,
        additionalData: header.aad,
        tagLength: TAG_SIZE * 8,
      },
      key,
      cipherBlock,
    );
    return new Uint8Array(plain);
  } catch {
    throw new DeliveryError(`第 ${index} 块解密认证失败（对象与记录不匹配，或内容被篡改）`);
  }
}

// ---------------------------------------------------------------- 取密文

/** 按逻辑密文区间取字节；多卷直链会自动跨卷拼接。 */
async function fetchCipherRange(plan, start, endInclusive, signal) {
  if (plan.mode === "direct" && Array.isArray(plan.parts) && plan.parts.length > 0) {
    return fetchPartRanges(plan.parts, start, endInclusive, signal);
  }
  if (!plan.url) {
    throw new DeliveryError("交付计划声明为直链，但没有提供直链地址");
  }
  return fetchUrlRange(plan.url, start, endInclusive, signal);
}

async function fetchUrlRange(url, start, endInclusive, signal) {
  const response = await fetch(url, {
    headers: { Range: `bytes=${start}-${endInclusive}` },
    signal,
  });
  if (!response.ok && response.status !== 206) {
    // 取密文失败时，上游（123，或其转述的本端回源鉴权结果）可能给出 JSON 错误体
    // （如 code 1051 / message）。只抛 HTTP 状态会把「票据过期 / IP 不符 /
    // 缺 remote_addr」混为一谈，集成方无从诊断；这里尽量把码与说明带出来，
    // 非 JSON 时退回首段文本，读体失败也不影响抛出状态码。
    let detail = "";
    try {
      const text = await response.text();
      try {
        const body = JSON.parse(text);
        const code = body?.code ?? body?.error?.code;
        const message = body?.message ?? body?.error?.message ?? body?.error;
        if (code !== undefined || message) {
          detail = [code !== undefined ? `code ${code}` : "", typeof message === "string" ? message : ""]
            .filter(Boolean).join("：");
        }
      } catch {
        if (text) detail = text.slice(0, 200);
      }
    } catch {
      /* 读体失败不影响抛出状态码 */
    }
    throw new DeliveryError(`取密文失败：HTTP ${response.status}${detail ? `（${detail}）` : ""}`);
  }
  const buffer = await response.arrayBuffer();
  const expected = endInclusive - start + 1;
  if (buffer.byteLength !== expected) {
    throw new DeliveryError(`密文长度不符：期望 ${expected}，实际 ${buffer.byteLength}`);
  }
  return new Uint8Array(buffer);
}

/** 把跨卷的连续密文区间拆成每卷的物理区间，并发取回后按逻辑偏移拼装。 */
async function fetchPartRanges(parts, start, endInclusive, signal) {
  signal?.throwIfAborted();
  const end = endInclusive + 1;
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end <= start) {
    throw new DeliveryError("密文区间不正确");
  }
  const ordered = [...parts].sort((a, b) => a.offset - b.offset);
  let cursor = start;
  const ranges = [];
  for (const part of ordered) {
    if (
      !part.url ||
      !Number.isSafeInteger(part.offset) ||
      part.offset < 0 ||
      !Number.isSafeInteger(part.size) ||
      part.size <= 0 ||
      !Number.isSafeInteger(part.offset + part.size)
    ) {
      throw new DeliveryError("分卷清单不正确");
    }
    if (part.offset >= end || part.offset + part.size <= start) {
      continue;
    }
    const from = Math.max(start, part.offset);
    const to = Math.min(end, part.offset + part.size);
    if (from !== cursor) {
      throw new DeliveryError("分卷清单存在缺口或重叠");
    }
    ranges.push({
      url: part.url,
      start: from - part.offset,
      end: to - part.offset - 1,
      offset: from - start,
    });
    cursor = to;
  }
  if (cursor !== end) {
    throw new DeliveryError("分卷清单无法覆盖所请求的密文区间");
  }

  const controller = new AbortController();
  const combined = signal ? AbortSignal.any([signal, controller.signal]) : controller.signal;
  const output = new Uint8Array(end - start);
  let next = 0;
  const workers = Array.from({ length: Math.min(4, ranges.length) }, async () => {
    while (next < ranges.length) {
      combined.throwIfAborted();
      const range = ranges[next++];
      const bytes = await fetchUrlRange(range.url, range.start, range.end, combined);
      combined.throwIfAborted();
      if (bytes.byteLength !== range.end - range.start + 1) {
        throw new DeliveryError("分卷响应长度与请求区间不一致");
      }
      output.set(bytes, range.offset);
    }
  });
  try {
    await Promise.all(workers);
  } catch (err) {
    controller.abort();
    await Promise.allSettled(workers);
    throw err;
  }
  return output;
}

// ---------------------------------------------------------------- 保存

async function tryOpenDiskSink(fileName) {
  const picker = globalThis.showSaveFilePicker;
  if (typeof picker !== "function") {
    return null;
  }
  try {
    const handle = await picker({ suggestedName: fileName });
    const writable = await handle.createWritable();
    let closed = false;
    return {
      name: handle.name,
      async write(chunk) {
        await writable.write(chunk);
      },
      async close() {
        if (!closed) {
          closed = true;
          await writable.close();
        }
      },
      async abort() {
        if (!closed) {
          closed = true;
          await writable.abort();
        }
      },
    };
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") {
      throw err;
    }
    return null;
  }
}

// ---------------------------------------------------------------- 工具

function abortError() {
  return new DOMException("已取消", "AbortError");
}

function base64ToBytes(base64) {
  const binary = atob(base64.trim());
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

function bytesToBase64(bytes) {
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.byteLength; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

function latin1(bytes) {
  let out = "";
  for (let i = 0; i < bytes.byteLength; i++) {
    out += String.fromCharCode(bytes[i]);
  }
  return out;
}

// 增量 SHA-256：crypto.subtle.digest 只能对整块缓冲区求哈希，大文件在流式
// 解密时无法把全部明文留在内存里。
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

class Sha256 {
  constructor() {
    this.state = new Uint32Array([
      0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
      0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
    ]);
    this.buffer = new Uint8Array(64);
    this.bufferLen = 0;
    this.totalLen = 0;
    this.w = new Uint32Array(64);
    this.finished = false;
  }

  update(data) {
    this.totalLen += data.byteLength;
    let offset = 0;
    if (this.bufferLen > 0) {
      const take = Math.min(64 - this.bufferLen, data.byteLength);
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

  hex() {
    if (!this.finished) {
      this.finish();
    }
    let out = "";
    for (let i = 0; i < 8; i++) {
      out += this.state[i].toString(16).padStart(8, "0");
    }
    return out;
  }

  finish() {
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
      this.compress(pad, 0);
    } else {
      const need = 64 - this.bufferLen;
      const block = new Uint8Array(64);
      block.set(this.buffer.subarray(0, this.bufferLen), 0);
      block.set(pad.subarray(0, need), this.bufferLen);
      this.compress(block, 0);
      if (padTotal > need) {
        this.compress(pad, need);
      }
    }
    this.finished = true;
  }

  compress(chunk, offset) {
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
