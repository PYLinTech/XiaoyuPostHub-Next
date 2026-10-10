import { cipherPartRanges, readCipherParts } from "@/delivery/cipherParts";
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
  decryptBlock,
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
  controller: AbortController;
  plainBlocks: Map<number, Uint8Array>;
  plainBytes: number;
  pendingBlocks: Map<number, BlockRead>;
}

interface BlockRead {
  controller: AbortController;
  readers: number;
  indices: number[];
  blocks: Map<number, Promise<Uint8Array>>;
  settled: boolean;
}

const sessions = new Map<string, StreamSession>();
const pendingSessions = new Map<string, AbortController>();

/** 会话上限。播放器可能为同一个文件反复建会话，不设上限等于内存泄漏。 */
const MAX_SESSIONS = 8;
/** 单次取密的块数。取太小会让请求数暴涨并快速耗尽票据次数。 */
const BATCH_BLOCKS = 8;
const BATCH_BYTES = 4 * 1024 * 1024;
const PLAIN_CACHE_BYTES = 32 * 1024 * 1024;

interface RegisterMessage {
  type: "xph:register";
  id?: string;
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
    event.waitUntil(registerSession(data)
      .then((id) => port?.postMessage({ ok: true, id }))
      .catch((err: unknown) => port?.postMessage({ ok: false, error: String(err) })));
    return;
  }
  if (data.type === "xph:revoke") {
    pendingSessions.get(data.id)?.abort();
    sessions.get(data.id)?.controller.abort();
    sessions.get(data.id)?.plainBlocks.clear();
    sessions.delete(data.id);
    port?.postMessage({ ok: true });
  }
});

async function registerSession(message: RegisterMessage): Promise<string> {
  const id = message.id || randomId();
  const controller = new AbortController();
  pendingSessions.set(id, controller);
  try {
    const headerBytes = await fetchSessionCipherBytes(message, 0, HEADER_SIZE - 1, controller.signal);
    const header = parseXphHeader(headerBytes);
    const key = await importContentKey(base64ToBytes(message.dek));

    controller.signal.throwIfAborted();
    if (sessions.size >= MAX_SESSIONS) {
      // 淘汰最早建立的会话：同一页面同时播放多个文件的概率极低，
      // 而"永远不淘汰"会让长时间使用的页面把密钥越攒越多。
      const oldest = [...sessions.entries()].sort((a, b) => a[1].createdAt - b[1].createdAt)[0];
      if (oldest) {
        oldest[1].controller.abort();
        sessions.delete(oldest[0]);
      }
    }

    sessions.set(id, {
      cipherUrl: message.cipherUrl,
      cipherParts: message.cipherParts,
      key,
      header,
      mimeType: message.mimeType || "application/octet-stream",
      fileName: message.fileName,
      createdAt: Date.now(),
      controller,
      plainBlocks: new Map(), plainBytes: 0, pendingBlocks: new Map(),
    });
    return id;
  } finally { pendingSessions.delete(id); }
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
    return respondStream(session, parsed.start, parsed.length, true, request.signal);
  }

  // 无 Range 时必须给出完整内容（流式），而不是静默截断成一个块：
  // 截断会让播放器把残缺数据当成完整文件，用户看到的是一个能播但损坏的媒体。
  return respondStream(session, 0, total, false, request.signal);
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

/** 即使播放器请求 bytes=0-，也只按背压读取有限批次，不等待整段下载。 */
function respondStream(
  session: StreamSession, start: number, length: number, partial: boolean, requestSignal?: AbortSignal,
): Response {
  const { header } = session;
  const end = start + length;
  const batchBlocks = Math.max(1, Math.min(BATCH_BLOCKS, Math.floor(BATCH_BYTES / header.blockSize)));
  const controller = new AbortController();
  const signal = requestSignal ? AbortSignal.any([requestSignal, session.controller.signal, controller.signal])
    : AbortSignal.any([session.controller.signal, controller.signal]);
  let position = start;
  let cancelled = false;
  let reads: Promise<Uint8Array>[] = [];
  const stream = new ReadableStream<Uint8Array>({
    async pull(output) {
      try {
        signal.throwIfAborted();
        if (position >= end) { output.close(); return; }
        const first = Math.floor(position / header.blockSize);
        if (!reads.length) {
          const last = Math.min(first + batchBlocks - 1, Math.ceil(end / header.blockSize) - 1);
          reads = readPlainBlocks(session, first, last, signal);
          // 后续认证块仍在下载时，首块已经可以交付。
          for (const read of reads) void read.catch(() => {});
        }
        const block = await reads.shift()!;
        const next = Math.min(end, (first + 1) * header.blockSize);
        const plain = block.subarray(position - first * header.blockSize, next - first * header.blockSize);
        signal.throwIfAborted();
        position = next;
        if (cancelled) return;
        output.enqueue(plain);
        if (position >= end) output.close();
      } catch (error) {
        if (!cancelled) output.error(error);
      }
    },
    cancel() { cancelled = true; controller.abort(); },
  }, { highWaterMark: 0 });
  const headers = new Headers(baseHeaders(session, header.plainSize));
  headers.set("Content-Length", String(length));
  if (partial) headers.set("Content-Range", `bytes ${start}-${end - 1}/${header.plainSize}`);
  return new Response(stream, { status: partial ? 206 : 200, headers });
}

/** 小明文 Range 共享完整认证块；取消单个读者不会打断其他轨道的同块读取。 */
function readPlainBlocks(session: StreamSession, first: number, last: number, signal: AbortSignal): Promise<Uint8Array>[] {
  signal.throwIfAborted();
  const reads: Promise<Uint8Array>[] = [];
  for (let index = first; index <= last; index++) {
    const cached = session.plainBlocks.get(index);
    if (cached) {
      session.plainBlocks.delete(index);
      session.plainBlocks.set(index, cached);
      reads.push(Promise.resolve(cached));
      continue;
    }
    let entry = session.pendingBlocks.get(index);
    if (!entry) {
      const indices = [index];
      for (let next = index + 1; next <= last && !session.plainBlocks.has(next) && !session.pendingBlocks.has(next); next++) indices.push(next);
      const controller = new AbortController();
      const combined = AbortSignal.any([session.controller.signal, controller.signal]);
      const begin = blockCipherOffset(session.header, index);
      const final = indices.at(-1)!;
      const end = blockCipherOffset(session.header, final) + blockCipherLen(session.header, final);
      const deferred = new Map<number, { resolve(bytes: Uint8Array): void; reject(error: unknown): void }>();
      const blocks = new Map<number, Promise<Uint8Array>>();
      for (const block of indices) {
        blocks.set(block, new Promise((resolve, reject) => deferred.set(block, { resolve, reject })));
      }
      entry = { controller, indices, readers: 0, blocks, settled: false };
      const pending = entry;
      for (const block of indices) session.pendingBlocks.set(block, pending);
      void (async () => {
        let blockIndex = index;
        let filled = 0;
        let cipherBlock = new Uint8Array(blockCipherLen(session.header, blockIndex));
        const publish = (block: number, plain: Uint8Array) => {
          const previous = session.plainBlocks.get(block);
          if (previous) session.plainBytes -= previous.byteLength;
          session.plainBlocks.delete(block);
          session.plainBlocks.set(block, plain);
          session.plainBytes += plain.byteLength;
          while (session.plainBytes > PLAIN_CACHE_BYTES) {
            const oldest = session.plainBlocks.keys().next().value!;
            session.plainBytes -= session.plainBlocks.get(oldest)!.byteLength;
            session.plainBlocks.delete(oldest);
          }
          deferred.get(block)!.resolve(plain);
        };
        let tail: Uint8Array | undefined;
        try {
          for await (const bytes of streamSessionCipherBytes(session, begin, end - 1, combined)) {
            let offset = 0;
            while (offset < bytes.byteLength) {
              combined.throwIfAborted();
              const count = Math.min(bytes.byteLength - offset, cipherBlock.byteLength - filled);
              cipherBlock.set(bytes.subarray(offset, offset + count), filled);
              filled += count; offset += count;
              if (filled === cipherBlock.byteLength) {
                const plain = await decryptBlock(session.key, session.header, blockIndex, cipherBlock);
                combined.throwIfAborted();
                if (blockIndex === final) tail = plain; else publish(blockIndex, plain);
                blockIndex++; filled = 0;
                if (blockIndex <= final) cipherBlock = new Uint8Array(blockCipherLen(session.header, blockIndex));
              }
            }
          }
          combined.throwIfAborted();
          if (blockIndex !== final + 1 || filled || !tail) throw new Error("密文认证块被截断");
          pending.settled = true;
          publish(final, tail);
        } catch (error) {
          pending.settled = true;
          for (const value of deferred.values()) value.reject(error);
        } finally {
          for (const block of indices) if (session.pendingBlocks.get(block) === pending) session.pendingBlocks.delete(block);
        }
      })();
    }
    const pending = entry;
    const block = index;
    pending.readers++;
    reads.push(new Promise<Uint8Array>((resolve, reject) => {
      let finished = false;
      const finish = () => {
        if (finished) return false;
        finished = true;
        signal.removeEventListener("abort", abort);
        if (--pending.readers === 0 && !pending.settled) {
          pending.controller.abort();
          for (const i of pending.indices) if (session.pendingBlocks.get(i) === pending) session.pendingBlocks.delete(i);
        }
        return true;
      };
      const abort = () => { if (finish()) reject(signal.reason); };
      signal.addEventListener("abort", abort, { once: true });
      pending.blocks.get(block)!.then(bytes => { if (finish()) resolve(bytes); }, error => { if (finish()) reject(error); });
    }));
  }
  return reads;
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
  if (total <= 0) throw new Error("空文件没有可读取区间");
  const value = header.trim();
  if (!value.startsWith("bytes=")) {
    throw new Error("不支持的区间单位");
  }
  const spec = value.slice("bytes=".length);
  if (!/^\d*-\d*$/.test(spec)) throw new Error("区间格式不正确");
  const dash = spec.indexOf("-");
  const startRaw = spec.slice(0, dash).trim();
  const endRaw = spec.slice(dash + 1).trim();

  if (startRaw === "") {
    const suffix = Number.parseInt(endRaw, 10);
    if (!Number.isSafeInteger(suffix) || suffix <= 0) {
      throw new Error("后缀式区间不正确");
    }
    const start = Math.max(0, total - suffix);
    return { start, length: total - start };
  }

  const start = Number.parseInt(startRaw, 10);
  if (!Number.isSafeInteger(start) || start < 0 || start >= total) {
    throw new Error("区间起点越界");
  }
  if (endRaw === "") {
    return { start, length: total - start };
  }
  const end = Number.parseInt(endRaw, 10);
  if (!Number.isSafeInteger(end) || end < start) {
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
async function fetchCipherBytes(url: string, start: number, endInclusive: number, signal?: AbortSignal): Promise<Uint8Array> {
  const response = await fetch(url, {
    headers: { Range: `bytes=${start}-${endInclusive}` },
    cache: "no-store",
    signal,
  });
  if (response.status !== 206 && !(response.status === 200 && start === 0
    && response.headers.get("Content-Length") === String(endInclusive + 1))) {
    await response.body?.cancel();
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
  signal?: AbortSignal,
): Promise<Uint8Array> {
  if (!source.cipherParts?.length) return fetchCipherBytes(source.cipherUrl, start, endInclusive, signal);
  return readCipherParts(source.cipherParts, start, endInclusive, fetchCipherBytes, signal);
}

function randomId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** 按物理卷顺序读取有界密文区间；不先拼出整批密文。 */
async function* streamSessionCipherBytes(
  source: { cipherUrl: string; cipherParts?: Array<{ url: string; offset: number; size: number }> },
  start: number, end: number, signal: AbortSignal,
): AsyncGenerator<Uint8Array> {
  const ranges = source.cipherParts?.length ? cipherPartRanges(source.cipherParts, start, end)
    : [{ url: source.cipherUrl, start, end }];
  for (const range of ranges) {
    signal.throwIfAborted();
    const response = await fetch(range.url, { headers: { Range: `bytes=${range.start}-${range.end}` }, signal, cache: "no-store" });
    const expected = range.end - range.start + 1;
    const contentRange = response.headers.get("Content-Range");
    const parsed = contentRange?.match(/^bytes (\d+)-(\d+)\/(\d+)$/);
    if ((response.status !== 206 && !(response.status === 200 && range.start === 0 && response.headers.get("Content-Length") === String(expected)))
      || (contentRange && (!parsed || Number(parsed[1]) !== range.start || Number(parsed[2]) !== range.end))) {
      await response.body?.cancel();
      throw new Error(`取密文失败：HTTP ${response.status} 或区间不匹配`);
    }
    if (!response.body) throw new Error("密文响应为空");
    const reader = response.body.getReader();
    let received = 0;
    const abort = () => { void reader.cancel(signal.reason).catch(() => {}); };
    signal.addEventListener("abort", abort, { once: true });
    try {
      while (true) {
        signal.throwIfAborted();
        const { done, value } = await reader.read();
        if (done) break;
        received += value.byteLength;
        if (received > expected) throw new Error("密文长度不符：响应超出区间");
        yield value;
      }
      signal.throwIfAborted();
      if (received !== expected) throw new Error(`密文长度不符：期望 ${expected}，实得 ${received}`);
    } finally { signal.removeEventListener("abort", abort); await reader.cancel().catch(() => {}); reader.releaseLock(); }
  }
}
