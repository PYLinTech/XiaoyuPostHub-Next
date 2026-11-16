#!/usr/bin/env node
// 后端中转交付示例。
// 本示例后端用 SDK 下载文件，密文计划在本地解密，再把字节转发给浏览器；API Key 不出后端。
// 示例使用临时文件承载解密结果，生产环境可按需改为缓存、对象存储或流式实现。

import http from "node:http";
import { createReadStream } from "node:fs";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { pipeline } from "node:stream/promises";
import { fileURLToPath } from "node:url";
import { Client } from "../xiaoyuposthub-next-sdk-v3.mjs";

const port = Number(process.env.PORT || 8081);
const pagePath = fileURLToPath(new URL("../../web/relay-delivery.html", import.meta.url));
const client = new Client();

function sendJSON(res, status, value) {
  const body = JSON.stringify(value);
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": Buffer.byteLength(body),
    "Cache-Control": "no-store",
  });
  res.end(body);
}

async function readBody(req) {
  const chunks = [];
  let total = 0;
  for await (const chunk of req) {
    total += chunk.length;
    if (total > 1 << 20) throw new Error("请求体过大");
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString("utf8");
}

function contentDisposition(name) {
  const fallback = String(name).replace(/["\\\r\n]/g, "_").replace(/[^\x20-\x7e]/g, "_");
  return `attachment; filename="${fallback}"; filename*=UTF-8''${encodeURIComponent(name)}`;
}

const server = http.createServer(async (req, res) => {
  try {
    if (req.method === "GET" && req.url === "/") {
      const html = await readFile(pagePath);
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(html);
      return;
    }
    if (req.method !== "POST" || req.url !== "/api/delivery/relay") {
      sendJSON(res, 404, { error: "not found" });
      return;
    }

    let body;
    try {
      body = JSON.parse((await readBody(req)) || "{}");
    } catch {
      sendJSON(res, 400, { error: "请求体不是有效 JSON" });
      return;
    }
    const filePath = String(body.path || "").trim();
    if (!filePath) {
      sendJSON(res, 400, { error: "path 不能为空" });
      return;
    }

    const tempDir = await mkdtemp(path.join(tmpdir(), "xph-relay-"));
    const tempFile = path.join(tempDir, "payload.bin");
    try {
      let plan;
      try {
        plan = await client.download(filePath, tempFile);
      } catch (err) {
        sendJSON(res, 502, { error: err.message || "下载失败" });
        return;
      }
      const info = await stat(tempFile);
      res.writeHead(200, {
        "Content-Type": plan.mimeType || "application/octet-stream",
        "Content-Length": String(info.size),
        "Content-Disposition": contentDisposition(plan.fileName || path.basename(filePath)),
        "Cache-Control": "private, no-store",
      });
      await pipeline(createReadStream(tempFile), res);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  } catch (err) {
    console.error("中转交付示例失败:", err.message);
    if (!res.headersSent) sendJSON(res, 502, { error: err.message || "请求失败" });
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`中转交付示例已启动：http://127.0.0.1:${port}（文件字节经本服务转发）`);
});
