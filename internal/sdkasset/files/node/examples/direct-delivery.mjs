#!/usr/bin/env node
// 后端请求得到地址后，前端直接去拿的示例。
// 浏览器只向本示例后端要一份交付计划，再拿计划里的 123 地址直接取密文，
// 并在浏览器本地解密；API Key 只保存在本进程，不会返回给浏览器。
//
// 注意「后端 IP ≠ 访客 IP」：站点看到的是本后端出口 IP，真正去 123 取字节的
// 是浏览器用户。票据默认按申请方网段绑定，不代传访客 IP 时真实部署直连必然
// 403。本示例把观测到的访客地址经 clientIp 传给站点。后端若在反向代理后，
// 用 TRUSTED_PROXIES（逗号分隔的 IP 或 CIDR，默认空）声明代理网段，再从
// X-Forwarded-For 右侧跳过受信代理取真实访客；站点侧「可信代理」只作用于
// 站点自身的入站请求，不会替本后端判断转发头。

import http from "node:http";
import net from "node:net";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { Client } from "../xiaoyuposthub-next-sdk-v5.mjs";

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

function normalizeIP(raw) {
  let value = String(raw || "").trim();
  if (!value) return "";
  if (value.startsWith("[")) {
    const end = value.indexOf("]");
    if (end > 0) value = value.slice(1, end);
  } else if (value.split(":").length === 2 && value.includes(".")) {
    value = value.split(":", 1)[0];
  }
  if (value.toLowerCase().startsWith("::ffff:") && net.isIPv4(value.slice(7))) {
    value = value.slice(7);
  }
  return net.isIP(value) ? value : "";
}

const trustedProxies = (() => {
  const list = new net.BlockList();
  for (const item of String(process.env.TRUSTED_PROXIES || "").split(",")) {
    const value = item.trim();
    if (!value) continue;
    const [addrPart, prefixPart] = value.split("/");
    const addr = normalizeIP(addrPart);
    if (!addr) continue;
    const family = net.isIPv6(addr) ? "ipv6" : "ipv4";
    const maxBits = family === "ipv6" ? 128 : 32;
    if (prefixPart === undefined) {
      list.addAddress(addr, family);
      continue;
    }
    const bits = Number(prefixPart);
    if (Number.isInteger(bits) && bits >= 0 && bits <= maxBits) {
      list.addSubnet(addr, bits, family);
    }
  }
  return list;
})();

function isTrustedProxy(addr) {
  if (!addr) return false;
  return trustedProxies.check(addr, net.isIPv6(addr) ? "ipv6" : "ipv4");
}

// 只有 TCP 对端是受信代理时才读取 X-Forwarded-For，并从右往左跳过受信代理。
// 转发链里出现无法解析的条目时回退到 TCP 对端，不继续相信更左侧的值。
function visitorIP(req) {
  const peer = normalizeIP(req.socket.remoteAddress);
  if (!peer) return "";
  if (!isTrustedProxy(peer)) return peer;
  const forwarded = req.headers["x-forwarded-for"];
  const raw = Array.isArray(forwarded) ? forwarded.join(",") : String(forwarded || "");
  const hops = raw.split(",");
  let leftmost = "";
  for (let i = hops.length - 1; i >= 0; i -= 1) {
    const addr = normalizeIP(hops[i]);
    if (!addr) return peer;
    leftmost = addr;
    if (!isTrustedProxy(addr)) return addr;
  }
  return leftmost || peer;
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
    // 代传终端用户真实地址，票据按访客网段绑定，123 回源鉴权才放行。
    const plan = await client.prepareDownload(filePath, clientPublicKey, false, visitorIP(req));
    if (plan.mode !== "direct" || plan.contentForm !== "ciphertext") {
      // 不回执：密文票据一旦回执即按预扣全额结算，不回执则到期退款；
      // proxy_decrypt 由服务端结算，回执也会被拒绝。
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
