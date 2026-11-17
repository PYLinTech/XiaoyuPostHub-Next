#!/usr/bin/env node
// XiaoyuPostHub-Next Node.js SDK 示例。
// 站点地址默认从 ../config.json 读取，可在 Client 的 baseUrl 参数覆盖；
// API Key 只来自参数或 XIAOYU_API_KEY。
// 版本查询是公开接口，不需要 API Key；其余请求都需要 Key。
// 所有路径参数都相对于 Key 的工作区，"/" 就是工作区根目录。
// 只做存储集成时直接使用 upload/download，无需创建分享或直链。
// 覆盖文件树、上传任务、下载/预览、分享与取件码、直链、归档和公告等用户侧操作。

import {
  createDecipheriv, createHash, constants,
  generateKeyPairSync, privateDecrypt, randomUUID,
} from "node:crypto";
import { createReadStream, createWriteStream, readFileSync } from "node:fs";
import { mkdtemp, open, rename, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { Readable, Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
import { fileURLToPath } from "node:url";

const CONFIG_PATH = fileURLToPath(new URL("../config.json", import.meta.url));
const UPLOAD_WORKERS = 3;
const PART_WORKERS = 4;

export class APIError extends Error {
  constructor(status, message, detail = "", retryAfter = undefined) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.detail = detail;
    this.retryAfter = retryAfter;
  }
}

function retryAfterOf(res) {
  const raw = res.headers.get("Retry-After");
  if (!raw) return undefined;
  const seconds = Number.parseInt(raw, 10);
  return Number.isFinite(seconds) && seconds > 0 ? seconds : undefined;
}

async function doFetch(url, init) {
  try {
    return await fetch(url, init);
  } catch (err) {
    throw new APIError(0, "网络连接失败，请检查网络后重试", String(err));
  }
}

async function responseError(res, fallback = "请求失败") {
  const text = await res.text();
  let message = fallback;
  let detail = text.slice(0, 300);
  try {
    const parsed = JSON.parse(text);
    message = parsed.error?.message || message;
    detail = parsed.error?.detail || detail;
  } catch { /* 非 JSON 错误页沿用调用方的状态码文案。 */ }
  return new APIError(res.status, message, detail, retryAfterOf(res));
}

function loadBaseUrl() {
  const cfg = JSON.parse(readFileSync(CONFIG_PATH, "utf8"));
  return String(cfg.baseUrl).replace(/\/+$/, "");
}

/** 只允许 http(s)：交付计划里的地址来自服务端，配置文件也可能被改坏。 */
function requireHttpUrl(raw) {
  const parsed = new URL(raw);
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new Error(`不支持的地址协议：${parsed.protocol}`);
  }
  return raw;
}

/** 先写同目录临时文件，验证通过后再替换目标，失败不会毁掉已有文件。 */
function tempSibling(dest) {
  return path.join(path.dirname(dest), `.${path.basename(dest)}.xph-${randomUUID()}`);
}

async function readEnvelope(res) {
  const text = await res.text();
  let parsed = {};
  try { parsed = text ? JSON.parse(text) : {}; } catch { /* 保留原文 */ }
  if (!res.ok || parsed.error) {
    const err = parsed.error || {};
    throw new APIError(
      res.status,
      err.message || `HTTP ${res.status}`,
      err.detail || text.slice(0, 300),
      retryAfterOf(res),
    );
  }
  return parsed.data ?? {};
}

/** 查询站点公开的示例 SDK 版本；不需要 API Key。 */
export async function sdkVersion(baseUrl = "") {
  const base = (baseUrl || loadBaseUrl()).replace(/\/+$/, "");
  const res = await doFetch(requireHttpUrl(base + "/api/sdk/version"), { method: "GET" });
  const data = await readEnvelope(res);
  return String(data.version || "");
}

export class Client {
  constructor(apiKey = "", baseUrl = "") {
    this.apiKey = apiKey || process.env.XIAOYU_API_KEY || "";
    if (!this.apiKey) throw new Error("缺少 API Key：设置 XIAOYU_API_KEY 或传入 apiKey");
    this.baseUrl = (baseUrl || loadBaseUrl()).replace(/\/+$/, "");
    requireHttpUrl(this.baseUrl);
  }

  async request(method, endpoint, { json, body, headers = {} } = {}) {
    const finalHeaders = { Authorization: `Bearer ${this.apiKey}`, ...headers };
    let payload;
    if (json !== undefined) {
      finalHeaders["Content-Type"] = "application/json";
      payload = JSON.stringify(json);
    } else if (body !== undefined) {
      finalHeaders["Content-Type"] = "application/octet-stream";
      payload = body;
    }
    const res = await doFetch(this.baseUrl + endpoint, { method, headers: finalHeaders, body: payload });
    return readEnvelope(res);
  }

  _resolveUrl(raw) {
    return requireHttpUrl(/^https?:\/\//i.test(raw) ? raw : this.baseUrl + raw);
  }

  _siteUrl(raw) {
    // 认证请求只能发往站内相对路径，避免把 API Key 交给计划里的外部地址。
    if (typeof raw !== "string" || !raw.startsWith("/")) {
      throw new APIError(0, "交付计划的中转地址不正确");
    }
    return requireHttpUrl(this.baseUrl + raw);
  }

  async _saveStream(url, dest, auth = false) {
    // 分片地址可能是跨域的预签名地址：绝对地址直接用，且绝不附带 Authorization
    // ——把本站令牌发给 CDN 既没用，也等于主动泄露凭据。
    const res = await doFetch(this._resolveUrl(url), {
      headers: auth ? { Authorization: `Bearer ${this.apiKey}` } : {},
    });
    if (!res.ok) throw await responseError(res, `下载失败：HTTP ${res.status}`);
    if (!res.body) throw new APIError(0, "响应没有内容");
    await pipeline(Readable.fromWeb(res.body), createWriteStream(dest));
  }

  async _savePart(url, dest, offset, size) {
    const res = await doFetch(this._resolveUrl(url));
    if (!res.ok) throw await responseError(res, `下载失败：HTTP ${res.status}`);
    if (!res.body) throw new APIError(0, "响应没有内容");
    let received = 0;
    const meter = new Transform({
      transform(chunk, _encoding, callback) {
        received += chunk.length;
        callback(null, chunk);
      },
    });
    await pipeline(
      Readable.fromWeb(res.body), meter,
      createWriteStream(dest, { flags: "r+", start: offset }),
    );
    if (received !== size) {
      throw new APIError(0, `分卷长度不一致：偏移 ${offset} 期望 ${size} 字节，实得 ${received} 字节`);
    }
  }

  async _downloadParts(parts, dest) {
    const last = parts[parts.length - 1];
    const total = Number(last.offset) + Number(last.size);
    const fh = await open(dest, "w");
    try {
      await fh.truncate(total);
    } finally {
      await fh.close();
    }
    let cursor = 0;
    let failure = null;
    const workers = Array.from({ length: Math.min(PART_WORKERS, parts.length) }, async () => {
      while (!failure) {
        const position = cursor++;
        if (position >= parts.length) return;
        const part = parts[position];
        try {
          await this._savePart(part.url, dest, Number(part.offset), Number(part.size));
        } catch (err) {
          failure = failure || err;
        }
      }
    });
    await Promise.all(workers);
    if (failure) throw failure;
  }

  // ---------------------------------------------------------------- 版本
  sdkVersion() {
    return sdkVersion(this.baseUrl);
  }

  // ---------------------------------------------------------------- 文件树
  profile() { return this.request("GET", "/api/auth/profile"); }
  list(dir = "/") { return this.request("GET", `/api/fs/list?path=${encodeURIComponent(dir)}`); }
  stat(filePath) { return this.request("GET", `/api/fs/stat?path=${encodeURIComponent(filePath)}`); }
  stats(dir = "/") { return this.request("GET", `/api/fs/stats?path=${encodeURIComponent(dir)}`); }
  mkdir(parent, name) { return this.request("POST", "/api/fs/mkdir", { json: { parentPath: parent, name } }); }
  rename(filePath, newName) { return this.request("POST", "/api/fs/rename", { json: { path: filePath, newName } }); }
  move(filePath, destParent) { return this.request("POST", "/api/fs/move", { json: { path: filePath, destParent } }); }
  remove(filePath) { return this.request("POST", "/api/fs/delete", { json: { path: filePath } }); }

  // ---------------------------------------------------------------- 上传
  async upload(localPath, parent = "/", name = "", conflictAction = "rename", pollMs = 1000) {
    const info = await stat(localPath);
    const digest = createHash("sha256");
    for await (const chunk of createReadStream(localPath)) digest.update(chunk);
    const init = await this.request("POST", "/api/upload/init", {
      json: {
        checksum: digest.digest("hex"),
        sizePlain: info.size,
        parentPath: parent,
        name: name || path.basename(localPath),
        conflictAction,
      },
    });
    if (init.dedup) return init;
    const session = encodeURIComponent(init.sessionId);
    const chunkSize = Number(init.chunkSize);
    const received = new Set(init.received || []);
    const pending = [];
    for (let index = 0; index < init.chunkTotal; index++) {
      if (!received.has(index)) pending.push(index);
    }
    let cursor = 0;
    let failure = null;
    const workers = Array.from({ length: Math.min(UPLOAD_WORKERS, pending.length) }, async () => {
      let fh = null;
      try {
        fh = await open(localPath, "r");
        while (!failure) {
          const position = cursor++;
          if (position >= pending.length) return;
          const index = pending[position];
          const length = Math.min(chunkSize, info.size - index * chunkSize);
          if (length <= 0) throw new APIError(0, "读取本地文件失败：文件在上传过程中被截断", "");
          const buf = Buffer.alloc(length);
          const { bytesRead } = await fh.read(buf, 0, length, index * chunkSize);
          if (bytesRead !== length) {
            throw new APIError(0, "读取本地文件失败",
              `偏移 ${index * chunkSize} 期望 ${length} 字节，实得 ${bytesRead} 字节`);
          }
          await this.request("PUT", `/api/upload/${session}/chunk/${index}`, { body: buf });
        }
      } catch (err) {
        failure = failure || err;
      } finally {
        if (fh) await fh.close();
      }
    });
    await Promise.all(workers);
    if (failure) throw failure;
    await this.request("POST", `/api/upload/${session}/complete`);
    const deadline = Date.now() + 300_000;
    while (Date.now() < deadline) {
      const status = await this.request("GET", `/api/upload/${session}`);
      if (status.state === "done") return status;
      if (status.state === "error") throw new APIError(0, "上传失败", status.error || "");
      await new Promise((resolve) => setTimeout(resolve, pollMs));
    }
    throw new APIError(0, "上传收尾超时", session);
  }

  uploadTasks() { return this.request("GET", "/api/upload/tasks"); }
  uploadStatus(session) { return this.request("GET", `/api/upload/${encodeURIComponent(session)}`); }
  cancelUpload(session) { return this.request("POST", `/api/upload/${encodeURIComponent(session)}/cancel`); }
  resolveUpload(session, checksum) {
    return this.request("POST", `/api/upload/${encodeURIComponent(session)}/resolve`, {
      json: { checksum },
    });
  }

  // ---------------------------------------------------------------- 下载
  /**
   * 请求交付计划：只协商通道与密钥，不下载任何字节。
   * clientPublicKey 是 base64 SPKI DER；密文计划会附带用该公钥封装的内容密钥。
   * clientIp 可选，代传终端用户真实地址（仅 API Key 身份生效）：「后端申请、
   * 前端直取」时票据按它的网段绑定，123 回源鉴权才放行；不传则按后端 IP 绑定。
   */
  prepareDownload(filePath, clientPublicKey, preview = false, clientIp = "") {
    const json = { path: filePath, clientPublicKey };
    if (typeof clientIp === "string" && clientIp.trim()) json.clientIp = clientIp.trim();
    return this.request("POST", preview ? "/api/fs/preview" : "/api/fs/download", { json });
  }

  /**
   * 回执交付票据；密文通道按预扣额度结算，completed 只表示是否完整接收。
   * 明文通道由服务端结算（带 Range 的中转流不结算），客户端回执会被拒绝。
   */
  settle(ticketId, completed = true) {
    if (!ticketId) return Promise.resolve();
    return this.request("POST", "/api/fs/settle", { json: { ticketId, completed } });
  }

  // 本方法由后端自己取字节，票据按后端自身网段绑定，不接受代传访客 IP：
  // 若计划落到 mode=direct，取数方就是后端，绑到访客网段会让回源鉴权 403。
  // 需要「后端申请、前端直取」时改用 prepareDownload 并代传 clientIp。
  async download(filePath, dest, preview = false) {
    const { privateKey, publicKey } = generateKeyPairSync("rsa", {
      modulusLength: 2048,
      publicKeyEncoding: { type: "spki", format: "der" },
      privateKeyEncoding: { type: "pkcs8", format: "pem" },
    });
    const plan = await this.prepareDownload(
      filePath,
      Buffer.from(publicKey).toString("base64"),
      preview,
    );
    if (plan.contentForm === "plaintext") {
      if (!plan.streamUrl) throw new APIError(0, "交付计划缺少可下载地址");
      await this._downloadPlain(plan, dest);
      return plan;
    }
    if (!plan.encryption) throw new APIError(0, "交付计划缺少解密材料");
    const tempDir = await mkdtemp(path.join(tmpdir(), "xph-"));
    const cipherPath = path.join(tempDir, "cipher.bin");
    try {
      if (plan.mode === "direct" && plan.parts?.length) {
        const parts = this._orderedParts(plan.parts, Number(plan.encryption.cipherSize));
        await this._downloadParts(parts, cipherPath);
      } else if (plan.mode === "direct" && plan.url) {
        await this._saveStream(plan.url, cipherPath);
      } else if (plan.streamUrl) {
        await this._saveStream(this._siteUrl(plan.streamUrl), cipherPath, true);
      } else {
        throw new APIError(0, "交付计划缺少可下载地址");
      }
      await this._decryptFile(cipherPath, dest, plan, privateKey);
    } finally {
      // 失败时不回执：密文票据一旦回执即按预扣全额结算，不回执则到期退款。
      await rm(tempDir, { recursive: true, force: true });
    }
    if (plan.ticketId) {
      try {
        await this.settle(plan.ticketId, true);
      } catch { /* 结算失败不影响已落盘文件 */ }
    }
    return plan;
  }

  /** 分卷必须按偏移排序、连续覆盖整份密文，缺口或重叠都说明计划不可信。 */
  _orderedParts(parts, expectedSize) {
    if (!Array.isArray(parts) || parts.length === 0) {
      throw new APIError(0, "分卷清单不正确");
    }
    for (const part of parts) {
      if (!part || typeof part !== "object" || Array.isArray(part)
        || typeof part.url !== "string" || !part.url) {
        throw new APIError(0, "分卷清单不正确");
      }
      const offset = Number(part.offset);
      const size = Number(part.size);
      if (!Number.isSafeInteger(offset) || offset < 0
        || !Number.isSafeInteger(size) || size <= 0 || !Number.isSafeInteger(offset + size)) {
        throw new APIError(0, "分卷清单不正确");
      }
    }
    const ordered = [...parts].sort((a, b) => Number(a.offset) - Number(b.offset));
    let cursor = 0;
    for (const part of ordered) {
      const offset = Number(part.offset);
      const size = Number(part.size);
      if (offset !== cursor) {
        throw new APIError(0, "分卷清单存在缺口或重叠");
      }
      cursor += size;
    }
    if (Number.isFinite(expectedSize) && expectedSize > 0 && cursor !== expectedSize) {
      throw new APIError(0, "分卷清单无法覆盖完整密文");
    }
    return ordered;
  }

  /** 明文计划由服务端产出，仍要核对长度与摘要，截断的流不能当成功落盘。 */
  async _downloadPlain(plan, dest) {
    const res = await doFetch(this._siteUrl(plan.streamUrl), {
      headers: { Authorization: `Bearer ${this.apiKey}` },
    });
    if (!res.ok) throw await responseError(res, `接收失败：HTTP ${res.status}`);
    const hash = createHash("sha256");
    let done = 0;
    const meter = new Transform({
      transform(chunk, _encoding, callback) {
        hash.update(chunk);
        done += chunk.length;
        callback(null, chunk);
      },
    });
    const tempPath = tempSibling(dest);
    try {
      if (!res.body) throw new APIError(0, "响应没有内容");
      await pipeline(Readable.fromWeb(res.body), meter, createWriteStream(tempPath));
      if (plan.plainSize && done !== Number(plan.plainSize)) {
        throw new APIError(0, `内容长度不一致：期望 ${plan.plainSize} 字节，实得 ${done} 字节`);
      }
      const digest = hash.digest("hex");
      if (plan.checksum && digest !== plan.checksum) {
        throw new APIError(0, `内容校验失败：期望 ${plan.checksum}，实得 ${digest}`);
      }
      await rename(tempPath, dest);
    } catch (err) {
      await rm(tempPath, { force: true });
      throw err;
    }
  }

  async _decryptFile(cipherPath, dest, plan, privateKey) {
    const meta = plan.encryption;
    if (!meta?.keyEnvelope) throw new APIError(0, "交付计划缺少解密材料");
    const src = await open(cipherPath, "r");
    const hash = createHash("sha256");
    const tempPath = tempSibling(dest);
    let out = null;
    try {
      const header = Buffer.alloc(64);
      const head = await src.read(header, 0, 64, 0);
      if (head.bytesRead !== 64 || header.subarray(0, 8).toString() !== "XPHCRPT1") {
        throw new APIError(0, "密文文件头无效");
      }
      if (header[8] !== 1) {
        throw new APIError(0, "密文文件头无效", `不支持的加密算法标识 ${header[8]}`);
      }
      const blockLog2 = header[9];
      if (blockLog2 < 9 || blockLog2 > 26) {
        throw new APIError(0, "密文文件头无效", `非法的块大小指数 ${blockLog2}`);
      }
      const plainSizeBig = header.readBigUInt64LE(28);
      if (plainSizeBig > 1n << 62n) {
        throw new APIError(0, "密文文件头无效", "非法的明文长度");
      }
      const plainSize = Number(plainSizeBig);
      if (!Number.isSafeInteger(plainSize)) {
        throw new APIError(0, "密文文件头无效", "明文长度超出可安全处理范围");
      }
      const noncePrefix = header.readUInt32LE(36);
      const blockSize = 2 ** blockLog2;
      const blockCount = plainSize === 0 ? 0 : Math.ceil(plainSize / blockSize);
      const cipherSize = 64 + plainSize + blockCount * 16;

      const problems = [];
      if (Number(meta.chunkLog2) !== blockLog2) {
        problems.push(`块大小指数 头=${blockLog2} 记录=${meta.chunkLog2}`);
      }
      if (Number(meta.plainSize) !== plainSize) {
        problems.push(`明文长度 头=${plainSize} 记录=${meta.plainSize}`);
      }
      if (Number(meta.cipherSize) !== cipherSize) {
        problems.push(`密文长度 头=${cipherSize} 记录=${meta.cipherSize}`);
      }
      if ((Number(meta.noncePrefix) >>> 0) !== noncePrefix) {
        problems.push(`nonce 前缀 头=${noncePrefix} 记录=${meta.noncePrefix}`);
      }
      if (problems.length > 0) {
        throw new APIError(0, "对象与记录不一致，已中止", problems.join("；"));
      }

      const info = await stat(cipherPath);
      if (info.size !== cipherSize) {
        throw new APIError(0, "密文长度与文件头不一致", `头=${cipherSize} 实际=${info.size}`);
      }

      const dek = privateDecrypt(
        { key: privateKey, padding: constants.RSA_PKCS1_OAEP_PADDING, oaepHash: "sha256" },
        Buffer.from(meta.keyEnvelope, "base64"),
      );
      const aad = header.subarray(0, 40);
      const prefix = Buffer.alloc(4);
      prefix.writeUInt32BE(noncePrefix, 0);
      out = await open(tempPath, "w");
      let position = 64;
      let remaining = plainSize;
      let index = 0;
      while (remaining > 0) {
        const take = Math.min(blockSize, remaining);
        const encrypted = Buffer.alloc(take + 16);
        const { bytesRead } = await src.read(encrypted, 0, encrypted.length, position);
        if (bytesRead !== encrypted.length) throw new APIError(0, "密文被截断");
        const counter = Buffer.alloc(8);
        counter.writeBigUInt64BE(BigInt(index));
        const decipher = createDecipheriv("aes-256-gcm", dek, Buffer.concat([prefix, counter]));
        decipher.setAAD(aad);
        decipher.setAuthTag(encrypted.subarray(take));
        const plain = Buffer.concat([decipher.update(encrypted.subarray(0, take)), decipher.final()]);
        await out.write(plain);
        hash.update(plain);
        remaining -= take;
        position += encrypted.length;
        index += 1;
      }
      await out.close();
      out = null;

      const expectedChecksum = plan.checksum || meta.checksum;
      if (expectedChecksum && hash.digest("hex") !== expectedChecksum) {
        throw new APIError(0, `内容校验失败：期望 ${expectedChecksum}，实得与明文不一致`);
      }
      await rename(tempPath, dest);
    } catch (err) {
      if (out) {
        try { await out.close(); } catch { /* 关闭失败不覆盖原始错误 */ }
        out = null;
      }
      await rm(tempPath, { force: true });
      throw err;
    } finally {
      await src.close();
      if (out) await out.close();
    }
  }

  // ---------------------------------------------------------------- 分享
  async createShare(filePath, {
    kind = "file", accessMode = "public", password = "",
    allowDownload = true, allowPreview = true, allowSubpath = true,
    showSharerName = true, customMarkdown = "",
    expiresAt = 0, maxDeliveries = 0,
  } = {}) {
    const data = await this.request("POST", "/api/share", {
      json: {
        path: filePath, kind, accessMode, password,
        allowDownload, allowPreview, allowSubpath, showSharerName,
        customMarkdown, expiresAt, maxDeliveries,
      },
    });
    return data.share;
  }

  listShares(limit = 100, offset = 0) {
    return this.request("GET", `/api/share?limit=${encodeURIComponent(limit)}&offset=${encodeURIComponent(offset)}`);
  }
  updateShare(shareId, updates = {}) {
    return this.request("PATCH", `/api/share/${encodeURIComponent(shareId)}`, { json: updates })
      .then((data) => data.share);
  }
  deleteShare(shareId) { return this.request("DELETE", `/api/share/${encodeURIComponent(shareId)}`); }
  listShareAccesses(shareId, limit = 100, offset = 0, snapshotId = undefined) {
    const query = new URLSearchParams({
      limit: String(limit),
      offset: String(offset),
    });
    if (snapshotId !== undefined && snapshotId !== null) query.set("snapshotId", String(snapshotId));
    return this.request(
      "GET",
      `/api/share/${encodeURIComponent(shareId)}/accesses?${query.toString()}`,
    );
  }
  createPickup(shareId, maxUses = 1) {
    return this.request("POST", `/api/share/${encodeURIComponent(shareId)}/pickup`, {
      json: { maxUses },
    }).then((data) => data.pickup);
  }
  listPickups(shareId) {
    return this.request("GET", `/api/share/${encodeURIComponent(shareId)}/pickup`);
  }
  deletePickup(code) {
    return this.request("DELETE", `/api/pickup/${encodeURIComponent(code)}`);
  }
  shareUrl(shareId) { return `${this.baseUrl}/s/${encodeURIComponent(shareId)}`; }

  // ---------------------------------------------------------------- 直链
  async createDirectLink(filePath, expiresAt = 0, maxHits = 0) {
    const data = await this.request("POST", "/api/direct-link", { json: { path: filePath, expiresAt, maxHits } });
    return data.link;
  }

  listDirectLinks(limit = 100, offset = 0) {
    return this.request("GET", `/api/direct-link?limit=${encodeURIComponent(limit)}&offset=${encodeURIComponent(offset)}`);
  }
  updateDirectLink(linkId, updates = {}) {
    return this.request("PATCH", `/api/direct-link/${encodeURIComponent(linkId)}`, { json: updates })
      .then((data) => data.link);
  }
  deleteDirectLink(linkId) {
    return this.request("DELETE", `/api/direct-link/${encodeURIComponent(linkId)}`);
  }
  async directLinkTarget(linkId) {
    const data = await this.request("GET", `/api/direct-link/${encodeURIComponent(linkId)}/target`);
    return data.path;
  }
  listNodeDirectLinks(checksums) {
    const query = new URLSearchParams({ checksums: (checksums || []).join(",") });
    return this.request("GET", `/api/direct-link/nodes?${query.toString()}`);
  }
  directLinkUrl(linkId) { return `${this.baseUrl}/d/${encodeURIComponent(linkId)}`; }

  // ---------------------------------------------------------------- 归档
  listArchive(limit = 100, offset = 0) {
    return this.request("GET", `/api/archive?limit=${encodeURIComponent(limit)}&offset=${encodeURIComponent(offset)}`);
  }
  restoreArchive(batchId) {
    return this.request("POST", `/api/archive/${encodeURIComponent(batchId)}/restore`);
  }
  clearArchive(batchId) {
    return this.request("POST", `/api/archive/${encodeURIComponent(batchId)}/clear`);
  }
  clearAllArchive() { return this.request("POST", "/api/archive/clear-all"); }

  // ---------------------------------------------------------------- 公告
  listAnnouncements(kinds = []) {
    const list = Array.isArray(kinds) ? kinds : (kinds ? [kinds] : []);
    const query = new URLSearchParams();
    if (list.length > 0) query.set("kinds", list.join(","));
    const suffix = query.toString() ? `?${query.toString()}` : "";
    return this.request("GET", `/api/announcements${suffix}`);
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [local, parent = "/"] = process.argv.slice(2);
  if (local === "--version") {
    console.log(await sdkVersion());
  } else if (local) {
    const client = new Client();
    const result = await client.upload(local, parent);
    console.log(JSON.stringify(result, null, 2));
  } else {
    console.error("用法: node xiaoyuposthub-next-sdk-v5.mjs --version");
    console.error("      node xiaoyuposthub-next-sdk-v5.mjs <本地文件> [目标目录]");
    process.exit(2);
  }
}
