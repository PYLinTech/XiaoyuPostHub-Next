#!/usr/bin/env node
// 后端接入 + 前端直连交付示例。
// 浏览器只向本示例后端要一条直链，文件字节由浏览器直接向 XiaoyuPostHub 请求。
// API Key 只保存在本进程，不会返回给浏览器。

import http from "node:http";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { Client } from "../xiaoyuposthub-next-sdk-v3.mjs";

const port = Number(process.env.PORT || 8080);
const pagePath = fileURLToPath(new URL("../../web/direct-delivery.html", import.meta.url));
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

const server = http.createServer(async (req, res) => {
  try {
    if (req.method === "GET" && req.url === "/") {
      const html = await readFile(pagePath);
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(html);
      return;
    }
    if (req.method !== "POST" || req.url !== "/api/delivery/direct") {
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
    const expiresAt = Number(body.expiresAt) || Math.floor(Date.now() / 1000) + 3600;
    const maxHits = Number(body.maxHits) || 0;
    const link = await client.createDirectLink(filePath, expiresAt, maxHits);
    sendJSON(res, 200, {
      url: client.directLinkUrl(link.id),
      fileName: link.sourceName,
      size: link.size,
      expiresAt: link.expiresAt,
    });
  } catch (err) {
    console.error("直连交付示例失败:", err.message);
    if (!res.headersSent) sendJSON(res, 502, { error: err.message || "请求失败" });
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`直连交付示例已启动：http://127.0.0.1:${port}（浏览器会直接向站点取文件）`);
});
