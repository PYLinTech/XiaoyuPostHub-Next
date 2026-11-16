#!/usr/bin/env node
// XiaoyuPostHub-Next Node.js SDK 示例。
// 站点地址只来自 ../config.json；API Key 只来自参数或 XIAOYU_API_KEY。
// 所有路径参数都相对于 Key 的工作区，"/" 就是工作区根目录。

import {
  createDecipheriv, createHash, constants,
  generateKeyPairSync, privateDecrypt,
} from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { mkdtemp, open, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { fileURLToPath } from "node:url";

const CONFIG_PATH = fileURLToPath(new URL("../config.json", import.meta.url));

export class APIError extends Error {
  constructor(status, message, detail = "") {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.detail = detail;
  }
}

async function loadBaseUrl() {
  const cfg = JSON.parse(await readFile(CONFIG_PATH, "utf8"));
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

export class Client {
  constructor(apiKey = "", baseUrl = "") {
    this.apiKey = apiKey || process.env.XIAOYU_API_KEY || "";
    if (!this.apiKey) throw new Error("缺少 API Key：设置 XIAOYU_API_KEY 或传入 apiKey");
    this.baseUrl = (baseUrl || "").replace(/\/+$/, "");
    this.ready = (this.baseUrl ? Promise.resolve() : loadBaseUrl().then((u) => { this.baseUrl = u; }))
      .then(() => { requireHttpUrl(this.baseUrl); });
  }

  async request(method, endpoint, { json, body, headers = {} } = {}) {
    await this.ready;
    const finalHeaders = { Authorization: `Bearer ${this.apiKey}`, ...headers };
    let payload;
    if (json !== undefined) {
      finalHeaders["Content-Type"] = "application/json";
      payload = JSON.stringify(json);
    } else if (body !== undefined) {
      finalHeaders["Content-Type"] = "application/octet-stream";
      payload = body;
    }
    const res = await fetch(this.baseUrl + endpoint, { method, headers: finalHeaders, body: payload });
    const text = await res.text();
    let parsed = {};
    try { parsed = text ? JSON.parse(text) : {}; } catch { /* 保留原文 */ }
    if (!res.ok || parsed.error) {
      const err = parsed.error || {};
      throw new APIError(res.status, err.message || `HTTP ${res.status}`, err.detail || text.slice(0, 300));
    }
    return parsed.data ?? {};
  }

  async _saveStream(url, dest, auth = false, append = false) {
    await this.ready;
    // 分片地址可能是跨域的预签名地址：绝对地址直接用，且绝不附带 Authorization
    // ——把本站令牌发给 CDN 既没用，也等于主动泄露凭据。
    const target = requireHttpUrl(/^https?:\/\//i.test(url) ? url : this.baseUrl + url);
    const res = await fetch(target, {
      headers: auth ? { Authorization: `Bearer ${this.apiKey}` } : {},
    });
    if (!res.ok) {
      const text = await res.text();
      let message = `下载失败：HTTP ${res.status}`;
      try {
        const parsed = JSON.parse(text);
        message = parsed.error?.message || message;
      } catch { /* 非 JSON 错误页沿用状态码文案。 */ }
      throw new APIError(res.status, message, text.slice(0, 300));
    }
    await pipeline(Readable.fromWeb(res.body), createWriteStream(dest, { flags: append ? "a" : "w" }));
  }

  // ---------------------------------------------------------------- 版本
  async sdkVersion() {
    return String((await this.request("GET", "/api/sdk/version")).version || "");
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
    const session = init.sessionId;
    const chunkSize = Number(init.chunkSize);
    const received = new Set(init.received || []);
    const fh = await open(localPath, "r");
    try {
      for (let index = 0; index < init.chunkTotal; index++) {
        if (received.has(index)) continue;
        const length = Math.min(chunkSize, info.size - index * chunkSize);
        const buf = Buffer.alloc(length);
        await fh.read(buf, 0, length, index * chunkSize);
        await this.request("PUT", `/api/upload/${session}/chunk/${index}`, { body: buf });
      }
    } finally {
      await fh.close();
    }
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

  cancelUpload(session) { return this.request("POST", `/api/upload/${session}/cancel`); }

  // ---------------------------------------------------------------- 下载
  async download(filePath, dest, preview = false) {
    const { privateKey, publicKey } = generateKeyPairSync("rsa", {
      modulusLength: 2048,
      publicKeyEncoding: { type: "spki", format: "der" },
      privateKeyEncoding: { type: "pkcs8", format: "pem" },
    });
    const plan = await this.request("POST", preview ? "/api/fs/preview" : "/api/fs/download", {
      json: { path: filePath, clientPublicKey: Buffer.from(publicKey).toString("base64") },
    });
    if (plan.contentForm === "plaintext") {
      if (!plan.streamUrl) throw new APIError(0, "交付计划缺少可下载地址");
      await this._saveStream(this.baseUrl + plan.streamUrl, dest, true);
      return plan;
    }
    const tempDir = await mkdtemp(path.join(tmpdir(), "xph-"));
    const cipherPath = path.join(tempDir, "cipher.bin");
    try {
      if (plan.mode === "direct" && plan.parts?.length) {
        for (const part of plan.parts) await this._saveStream(part.url, cipherPath, false, true);
      } else if (plan.mode === "direct" && plan.url) {
        await this._saveStream(plan.url, cipherPath);
      } else if (plan.streamUrl) {
        await this._saveStream(this.baseUrl + plan.streamUrl, cipherPath, true, true);
      } else {
        throw new APIError(0, "交付计划缺少可下载地址");
      }
      await this._decryptFile(cipherPath, dest, plan.encryption, privateKey);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
    if (plan.ticketId) {
      try {
        await this.request("POST", "/api/fs/settle", { json: { ticketId: plan.ticketId, completed: true } });
      } catch { /* 结算失败不影响已落盘文件 */ }
    }
    return plan;
  }

  async _decryptFile(cipherPath, dest, meta, privateKey) {
    const src = await open(cipherPath, "r");
    const out = await open(dest, "w");
    const hash = createHash("sha256");
    let index = 0;
    let remaining = Number(meta.plainSize);
    try {
      const header = Buffer.alloc(64);
      const head = await src.read(header, 0, 64, 0);
      if (head.bytesRead !== 64 || header.subarray(0, 8).toString() !== "XPHCRPT1") {
        throw new APIError(0, "密文文件头无效");
      }
      const blockSize = 2 ** header[9];
      const dek = privateDecrypt(
        { key: privateKey, padding: constants.RSA_PKCS1_OAEP_PADDING, oaepHash: "sha256" },
        Buffer.from(meta.keyEnvelope, "base64"),
      );
      const aad = header.subarray(0, 40);
      const prefix = Buffer.alloc(4);
      prefix.writeUInt32BE(Number(meta.noncePrefix) >>> 0, 0);
      let position = 64;
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
    } finally {
      await src.close();
      await out.close();
    }
    if (meta.checksum && hash.digest("hex") !== meta.checksum) {
      await rm(dest, { force: true });
      throw new APIError(0, "下载内容校验失败", "SHA-256 与交付计划不一致");
    }
  }

  // ---------------------------------------------------------------- 分享
  async createShare(filePath, {
    kind = "file", accessMode = "public", password = "", expiresAt = 0,
    allowDownload = true, allowPreview = true,
  } = {}) {
    const data = await this.request("POST", "/api/share", {
      json: {
        path: filePath, kind, accessMode, password, expiresAt, allowDownload, allowPreview,
      },
    });
    return data.share;
  }

  listShares(limit = 100, offset = 0) { return this.request("GET", `/api/share?limit=${limit}&offset=${offset}`); }
  deleteShare(shareId) { return this.request("DELETE", `/api/share/${shareId}`); }
  shareUrl(shareId) { return `${this.baseUrl}/s/${shareId}`; }

  // ---------------------------------------------------------------- 直链
  async createDirectLink(filePath, expiresAt = 0, maxHits = 0) {
    const data = await this.request("POST", "/api/direct-link", { json: { path: filePath, expiresAt, maxHits } });
    return data.link;
  }

  listDirectLinks(limit = 100, offset = 0) {
    return this.request("GET", `/api/direct-link?limit=${limit}&offset=${offset}`);
  }
  deleteDirectLink(linkId) { return this.request("DELETE", `/api/direct-link/${linkId}`); }
  directLinkUrl(linkId) { return `${this.baseUrl}/d/${linkId}`; }
}

if (process.argv[1] && process.argv[1].endsWith("xiaoyu.mjs")) {
  const [local, parent = "/"] = process.argv.slice(2);
  if (!local) {
    console.error("用法: node xiaoyu.mjs <本地文件> [目标目录]");
    process.exit(2);
  }
  const client = new Client();
  const result = await client.upload(local, parent);
  console.log(JSON.stringify(result, null, 2));
}
