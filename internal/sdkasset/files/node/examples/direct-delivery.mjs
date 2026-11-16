#!/usr/bin/env node
// 后端请求得到地址后，前端直接去拿的示例。
// 浏览器只向本示例后端要一份交付计划，再拿计划里的 123 地址直接取密文，
// 并在浏览器本地解密；API Key 只保存在本进程，不会返回给浏览器。

import http from "node:http";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { Client } from "../xiaoyuposthub-next-sdk-v4.mjs";

const port = Number(process.env.PORT || 8080);
const pagePath = fileURLToPath(new URL("../../web/direct-delivery.html", import.meta.url));
const modulePath = fileURLToPath(new URL("../../web/browser-delivery.mjs", import.meta.url));
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
    if (req.method === "GET" && req.url === "/browser-delivery.mjs") {
      const module = await readFile(modulePath, "utf8");
      res.writeHead(200, {
        "Content-Type": "text/javascript; charset=utf-8",
        "Cache-Control": "no-store",
      });
      res.end(module);
      return;
    }
    if (req.method !== "POST" || (req.url !== "/api/delivery/plan" && req.url !== "/api/delivery/settle")) {
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

    if (req.url === "/api/delivery/settle") {
      const ticketId = String(body.ticketId || "").trim();
      if (!ticketId) {
        sendJSON(res, 400, { error: "ticketId 不能为空" });
        return;
      }
      await client.settle(ticketId, Boolean(body.completed));
      sendJSON(res, 200, { ok: true });
      return;
    }

    const filePath = String(body.path || "").trim();
    const clientPublicKey = String(body.clientPublicKey || "").trim();
    if (!filePath) {
      sendJSON(res, 400, { error: "path 不能为空" });
      return;
    }
    if (!clientPublicKey) {
      sendJSON(res, 400, { error: "clientPublicKey 不能为空" });
      return;
    }
    const plan = await client.prepareDownload(filePath, clientPublicKey);
    if (plan.mode !== "direct" || plan.contentForm !== "ciphertext") {
      // 密文票据由客户端回执；proxy_decrypt 由服务端在开流时自行结算，回执会被拒绝。
      if (plan.contentForm === "ciphertext") {
        await client.settle(plan.ticketId, false).catch(() => {});
      }
      sendJSON(res, 409, {
        error: "本次交付被降级为站点中转，浏览器无法直接向 123 取密文；请确认站点开启「优先直链交付」且 123 直链空间可用",
      });
      return;
    }
    sendJSON(res, 200, plan);
  } catch (err) {
    console.error("方式 2 示例失败:", err.message);
    if (!res.headersSent) sendJSON(res, 502, { error: err.message || "请求失败" });
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`方式 2 示例（后端请求得到地址后，前端直接去拿）已启动：http://127.0.0.1:${port}`);
});
