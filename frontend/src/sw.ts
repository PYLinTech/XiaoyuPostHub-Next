/// <reference lib="webworker" />
//
// 解密用的 Service Worker。
//
// 为什么需要它：直链交付下密文在远端、密钥在页面里，而 <video> / <audio> /
// <img> / PDF 阅读器只会发出普通的 HTTP 请求——它们不会替我们解密。SW 在这里
// 扮演"本源虚拟明文文件"：浏览器按 Range 请求它，它换算成密文区间、取回、解密、
// 再按 206 返回。
//
// 这样做的收益是出流量直连存储，不经服务端；代价是必须自己把 Range 语义
// 实现对——包括上游会拒绝的后缀式区间，播放器索要尾部 moov 时会用到它。
//
// 会话（含内容密钥）只存在内存里，页面通过 MessageChannel 注入，密钥永不进入
// URL：一旦进 URL 就会落进 CDN 日志与浏览器历史。

import {
  base64ToBytes,
  blockCipherLen,
  blockCipherOffset,
  decryptCipherRange,
  chunkRange,
  importContentKey,
  parseXphHeader,
  HEADER_SIZE,
  type XphHeader,
} from "@/crypto/xph";

declare const self: ServiceWorkerGlobalScope;

/** 会话前缀。与页面约定的路径，必须保持一致。 */
const STREAM_PREFIX = "/__xph/";

interface StreamSession {
  cipherUrl: string;
  cipherParts?: Array<{ url: string; offset: number; size: number }>;
  key: CryptoKey;
  header: XphHeader;
  mimeType: string;
  fileName: string;
  createdAt: number;
}

const sessions = new Map<string, StreamSession>();

/** 会话上限。播放器可能为同一个文件反复建会话，不设上限等于内存泄漏。 */
const MAX_SESSIONS = 8;
/** 单次取密的块数。取太小会让请求数暴涨并快速耗尽票据次数。 */
const BATCH_BLOCKS = 8;

interface RegisterMessage {
  type: "xph:register";
  cipherUrl: string;
  cipherParts?: Array<{ url: string; offset: number; size: number }>;
  dek: string;
  mimeType: string;
  fileName: string;
}

interface RevokeMessage {
  type: "xph:revoke";
  id: string;
}

type IncomingMessage = RegisterMessage | RevokeMessage;

self.addEventListener("install", (event: ExtendableEvent) => {
  // 新版本立即接管，否则用户要刷新两次才能用上修复。
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event: ExtendableEvent) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("message", (event: ExtendableMessageEvent) => {
  const data = event.data as IncomingMessage | undefined;
  const port = event.ports[0];
  if (!data) {
    return;
  }
  if (data.type === "xph:register") {
    registerSession(data)
      .then((id) => port?.postMessage({ ok: true, id }))
      .catch((err: unknown) => port?.postMessage({ ok: false, error: String(err) }));
    return;
  }
  if (data.type === "xph:revoke") {
    sessions.delete(data.id);
    port?.postMessage({ ok: true });
  }
});

async function registerSession(message: RegisterMessage): Promise<string> {
  const headerBytes = await fetchSessionCipherBytes(message, 0, HEADER_SIZE - 1);
  const header = parseXphHeader(headerBytes);
  const key = await importContentKey(base64ToBytes(message.dek));

  if (sessions.size >= MAX_SESSIONS) {
    // 淘汰最早建立的会话：同一页面同时播放多个文件的概率极低，
    // 而"永远不淘汰"会让长时间使用的页面把密钥越攒越多。
    const oldest = [...sessions.entries()].sort((a, b) => a[1].createdAt - b[1].createdAt)[0];
    if (oldest) {
      sessions.delete(oldest[0]);
    }
  }

  const id = randomId();
  sessions.set(id, {
    cipherUrl: message.cipherUrl,
    cipherParts: message.cipherParts,
    key,
    header,
    mimeType: message.mimeType || "application/octet-stream",
    fileName: message.fileName,
    createdAt: Date.now(),
  });
  return id;
}

self.addEventListener("fetch", (event: FetchEvent) => {
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || !url.pathname.startsWith(STREAM_PREFIX)) {
    return;
  }
  const id = url.pathname.slice(STREAM_PREFIX.length);
  const session = sessions.get(id);
  if (!session) {
    event.respondWith(new Response("会话不存在或已过期\n", { status: 404 }));
    return;
  }
  event.respondWith(handleStream(event.request, session));
});

async function handleStream(request: Request, session: StreamSession): Promise<Response> {
  const total = session.header.plainSize;
  const rangeHeader = request.headers.get("Range");

  if (request.method === "HEAD") {
    return new Response(null, {
      status: 200,
      headers: baseHeaders(session, total),
    });
  }

  if (rangeHeader) {
    let parsed: ParsedRange;
    try {
      parsed = parseRange(rangeHeader, total);
    } catch (err) {
      return new Response(String(err), {
        status: 416,
        headers: { "Content-Range": `bytes */${total}` },
      });
    }
    return respondRange(session, parsed.start, parsed.length);
  }

  // 无 Range 时必须给出完整内容（流式），而不是静默截断成一个块：
  // 截断会让播放器把残缺数据当成完整文件，用户看到的是一个能播但损坏的媒体。
  return respondFull(session);
}

function baseHeaders(session: StreamSession, total: number): HeadersInit {
  return {
    "Content-Type": session.mimeType,
    "Accept-Ranges": "bytes",
    "Content-Length": String(total),
    // 明文内容绝不能被任何中间层缓存：同一来源下可能对应不同的密钥与身份。
    "Cache-Control": "no-store",
    "Content-Disposition": `inline; filename*=UTF-8''${encodeURIComponent(session.fileName)}`,
    // 这里是 new Response() 造出来的响应，不会经过服务端的中间件，
    // 所以服务端的 nosniff / X-Frame-Options 得在这里补一遍。缺了它，
    // Content-Type 由文件扩展名推断而来（.html → text/html）加上 inline
    // 处置，被 iframe 或直接导航打开时会在本站源内执行脚本。
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
  };
}

async function respondRange(
  session: StreamSession,
  start: number,
  length: number,
): Promise<Response> {
  const end = start + length; // 不含
  const { first, last, start: cipherStart, end: cipherEnd } = chunkRange(session.header, start, length);
  if (last < first) {
    return new Response(null, { status: 204 });
  }

  const cipher = await fetchSessionCipherBytes(session, cipherStart, cipherEnd - 1);
  const plain = await decryptCipherRange(
    session.key,
    session.header,
    cipher,
    { first, last, start: cipherStart, end: cipherEnd },
    start,
    length,
  );

  const headers: Record<string, string> = {
    "Content-Type": session.mimeType,
    "Content-Range": `bytes ${start}-${end - 1}/${session.header.plainSize}`,
    "Content-Length": String(plain.byteLength),
    "Accept-Ranges": "bytes",
    "Cache-Control": "no-store",
    // 同 baseHeaders：这条路径同样是 new Response()，安全头要自带。
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
  };
  return new Response(plain as unknown as BodyInit, { status: 206, headers });
}

async function respondFull(session: StreamSession): Promise<Response> {
  const total = session.header.plainSize;
  const header = session.header;
  const key = session.key;
  const blockCount = header.blockCount;

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      try {
        for (let first = 0; first < blockCount; first += BATCH_BLOCKS) {
          const last = Math.min(first + BATCH_BLOCKS, blockCount) - 1;
          const cipherStart = blockCipherOffset(header, first);
          const cipherEnd = blockCipherOffset(header, last) + blockCipherLen(header, last);
          const cipher = await fetchSessionCipherBytes(session, cipherStart, cipherEnd - 1);
          const from = first * header.blockSize;
          const plain = await decryptCipherRange(
            key,
            header,
            cipher,
            { first, last, start: cipherStart, end: cipherEnd },
            from,
            Math.min(total, (last + 1) * header.blockSize) - from,
          );
          controller.enqueue(plain);
        }
        controller.close();
      } catch (err) {
        controller.error(err);
      }
    },
  });

  return new Response(stream, { status: 200, headers: baseHeaders(session, total) });
}

interface ParsedRange {
  start: number;
  /** 0 表示"读到末尾"。 */
  length: number;
}

/**
 * 解析 Range 头。
 *
 * 后缀式 `bytes=-N` 必须在这里自己支持：上游在部分 CDN 上会以预检拒绝它，
 * 而 MP4 未做 faststart 时播放器恰恰会去取尾部 moov。
 */
function parseRange(header: string, total: number): ParsedRange {
  const value = header.trim();
  if (!value.startsWith("bytes=")) {
    throw new Error("不支持的区间单位");
  }
  const spec = value.slice("bytes=".length);
  if (spec.includes(",")) {
    throw new Error("不支持多区间请求");
  }
  const dash = spec.indexOf("-");
  if (dash < 0) {
    throw new Error("区间格式不正确");
  }
  const startRaw = spec.slice(0, dash).trim();
  const endRaw = spec.slice(dash + 1).trim();

  if (startRaw === "") {
    const suffix = Number.parseInt(endRaw, 10);
    if (!Number.isFinite(suffix) || suffix <= 0) {
      throw new Error("后缀式区间不正确");
    }
    const start = Math.max(0, total - suffix);
    return { start, length: total - start };
  }

  const start = Number.parseInt(startRaw, 10);
  if (!Number.isFinite(start) || start < 0 || start >= total) {
    throw new Error("区间起点越界");
  }
  if (endRaw === "") {
    return { start, length: total - start };
  }
  const end = Number.parseInt(endRaw, 10);
  if (!Number.isFinite(end) || end < start) {
    throw new Error("区间终点不正确");
  }
  return { start, length: Math.min(end, total - 1) - start + 1 };
}

/**
 * 取一段密文。
 *
 * 只带 Range 一个头：直链是跨域的，任何自定义头都会触发预检并被拒。
 * 同时校验实际收到的字节数——跨域下 Content-Length 未必可读，靠它判断会漏。
 */
async function fetchCipherBytes(url: string, start: number, endInclusive: number): Promise<Uint8Array> {
  const response = await fetch(url, {
    headers: { Range: `bytes=${start}-${endInclusive}` },
    cache: "no-store",
  });
  if (!response.ok && response.status !== 206) {
    throw new Error(`取密文失败：HTTP ${response.status}`);
  }
  const buffer = await response.arrayBuffer();
  const expected = endInclusive - start + 1;
  if (buffer.byteLength !== expected) {
    throw new Error(`密文长度不符：期望 ${expected}，实得 ${buffer.byteLength}`);
  }
  return new Uint8Array(buffer);
}

async function fetchSessionCipherBytes(
  source: { cipherUrl: string; cipherParts?: Array<{ url: string; offset: number; size: number }> },
  start: number,
  endInclusive: number,
): Promise<Uint8Array> {
  const endExclusive = endInclusive + 1;
  if (!source.cipherParts?.length) {
    return fetchCipherBytes(source.cipherUrl, start, endInclusive);
  }
  const hits = source.cipherParts.filter((part) => part.offset < endExclusive && part.offset + part.size > start);
  if (!hits.length || hits[0].offset > start || hits[hits.length - 1].offset + hits[hits.length - 1].size < endExclusive) {
    throw new Error("分卷清单无法覆盖所请求的密文区间");
  }
  const blocks = await Promise.all(hits.map((part) => {
    const from = Math.max(start, part.offset);
    const to = Math.min(endExclusive, part.offset + part.size);
    return fetchCipherBytes(part.url, from - part.offset, to - part.offset - 1);
  }));
  const out = new Uint8Array(endExclusive - start);
  let offset = 0;
  for (const block of blocks) {
    out.set(block, offset);
    offset += block.byteLength;
  }
  if (offset !== out.byteLength) {
    throw new Error("分卷响应长度与逻辑密文区间不一致");
  }
  return out;
}

function randomId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}
