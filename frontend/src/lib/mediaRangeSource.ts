import { ALL_FORMATS, CustomSource, Input } from "mediabunny";
import { blockCipherLen, blockCipherOffset, decryptBlock, HEADER_SIZE, importContentKey, parseXphHeader, XphFormatError } from "../crypto/xph";
import type { XphHeader } from "../crypto/xph";
import { cipherPartRanges } from "../delivery/cipherParts";
import type { CipherPart } from "../delivery/cipherParts";

export interface MediaCipherSource { url: string; parts?: CipherPart[]; key: Uint8Array; plainSize: number; cipherSize: number }
import { FULL_PREVIEW_LIMIT } from "./mediaPreviewLimits";
export { FULL_PREVIEW_LIMIT } from "./mediaPreviewLimits";
const CACHE_BYTES = 32 * 1024 * 1024;
/**
 * 单次预取的目标字节数，取 1 MiB 对齐默认加密块。
 *
 * 对齐后 batchCount 恒为 1（1 MiB 块 ÷ 1 MiB 目标），一个 HTTP Range 请求
 * 恰好对应一个完整加密块。这样"首批可交付字节"不再等整批：读到一个块就认证、
 * 就交付，无需等齐同一批里的其他块——慢网下这是首帧延迟的直接下限。
 *
 * 代价是请求数变为字节数的 4 倍，所以并发从 4 提到 6（见 MAX_CONCURRENT）
 * 来补偿吞吐；两者叠加后首帧更快，且在途总量反而比"4 MiB 批 × 4 路"更可控。
 */
const BATCH_BYTES = 1024 * 1024;
/**
 * 同时在途的批次数上限。
 *
 * BATCH_BYTES 已与加密块对齐（batchCount 恒为 1），所以一个批次就是一次请求、
 * 一块数据，并发数直接等于同时下载的块数。
 *
 * 取 8：浏览器对同源 HTTP/1.1 的连接数上限是 6，多出来的 2 路在多数浏览器里
 * 仍能生效（HTTP/2 下无此限制，且部分浏览器对同源连接有更高余量），而它们
 * 能让"预取深度 > 并发数"时多几块先在途。若部署在严格 6 连接的链路上，
 * 超出的部分只会在浏览器内部排队，不会变慢也不会报错。
 *
 * 在途上限 = 并发 × PREFETCH_BLOCKS。取 8 × 4 = 32 块 ≈ 32 MiB，与 CACHE_BYTES
 * 同量级，缓存装得下这些块，seek 时命中率高，不必回头重下。
 */
const MAX_CONCURRENT = 8;
/**
 * 跨批预取的窗口深度（以**块**为单位，不是批）。
 *
 * 这个深度必须在消费追上之前把后续请求放出去，否则每消耗完一块就把一段完整
 * 往返暴露给播放器，首帧卡顿正是这么来的。旧写法用 batchCount 兼表窗口深度，
 * 一旦 BATCH_BYTES 调到与加密块同大小、batchCount 退化成 1，窗口就塌成"只预取
 * 下一块"——而那一块要么已在同批 pending 里空转，要么仍要等消费推进才发起，正是
 * 要避免的形态。所以窗口深度改为直接按块计，与批次大小彻底解耦：批怎么调，
 * 深度都是 PREFETCH_BLOCKS。
 *
 * 取 8 块（8 MiB 预取视野）：与 MAX_CONCURRENT 同深，两者相乘即"最多同时有
 * 多少 MiB 在途"的上限，实际由两者中较小者决定在途量，深窗口只保证"请求发得
 * 出去、不至于被消费卡住发起时机"。再深只会让快网白下数据、挤占 seek 与后续
 * 分片的带宽；真正的缓冲深度由播放器侧的 ahead（秒）控制，那一层会在数据跟不上
 * 时主动收缩。
 */
const PREFETCH_BLOCKS = 8;
export class MediaSourceError extends Error {
  constructor(message: string, public readonly code: "auth" | "range" | "network" | "size") { super(message); this.name = "MediaSourceError"; }
}

type Block = { promise: Promise<Uint8Array>; resolve(value: Uint8Array): void; reject(error: unknown): void };
/** 一个会话唯一的取数、认证与缓存层。seek 不销毁它，也不把取消传播给共享索引。 */
export class MediaRangeSource {
  private localFile?: File;
  private temporary?: { directory: FileSystemDirectoryHandle; name: string };
  private diskTask?: Promise<void>;
  private header!: XphHeader;
  private key!: CryptoKey;
  private signal!: AbortSignal;
  private cache = new Map<number, Uint8Array>();
  private cachedBytes = 0;
  private pending = new Map<number, Block>();
  private rangeUnsupported = false;
  private active = 0;
  private queue: Array<() => void> = [];
  constructor(private descriptor: MediaCipherSource) {}
  get size() { return this.header.plainSize; }

  async open(signal: AbortSignal): Promise<void> {
    this.signal = signal;
    signal.throwIfAborted();
    this.key = await importContentKey(this.descriptor.key);
    this.descriptor.key.fill(0);
    const raw = new Uint8Array(HEADER_SIZE);
    for (let attempt = 0; ; attempt++) {
      try {
        let offset = 0;
        for await (const bytes of this.cipher(0, HEADER_SIZE)) { raw.set(bytes, offset); offset += bytes.length; }
        break;
      } catch (error) {
        signal.throwIfAborted();
        if (attempt >= 2 || !(error instanceof MediaSourceError && error.code === "network")) throw error;
        await this.delay(150 * 2 ** attempt);
      }
    }
    this.header = parseXphHeader(raw);
    if (this.header.plainSize !== this.descriptor.plainSize || this.header.cipherSize !== this.descriptor.cipherSize) {
      throw new XphFormatError("媒体文件头与交付计划不一致");
    }
    signal.throwIfAborted();
    signal.addEventListener("abort", () => { this.cache.clear(); this.cachedBytes = 0; }, { once: true });
  }

  input(signal: AbortSignal, readBudget = Infinity): Input {
    const touched = new Set<number>(); let count = 0;
    return new Input({ formats: ALL_FORMATS, source: new CustomSource({
      getSize: () => this.size,
      read: (start, end) => {
        signal.throwIfAborted();
        if (Number.isFinite(readBudget)) {
          for (let i = Math.floor(start / this.header.blockSize); i * this.header.blockSize < end; i++) {
            if (!touched.has(i)) { touched.add(i); count += blockCipherLen(this.header, i); }
          }
          if (count > readBudget) throw new Error("媒体元数据探测达到读取上限");
        }
        return this.stream(start, end, signal, !Number.isFinite(readBudget));
      },
      // 小缓存仅供解析器回看；认证块缓存与网络并发只在本类管理。
      maxCacheSize: 1024 * 1024, prefetchProfile: "none", handleUnhandledError: () => {},
    }) });
  }

  /**
   * `lookahead` 表示消费方会持续往后读（解复用播放），可以安全地把下一批也排在途；
   * 一次性有明确终点的读取（`read()`、元数据探测）不排——那些块本来就不会被消费，
   * 提前取回只是白花一次请求。
   */
  private stream(start: number, end: number, signal: AbortSignal, prefetch = true, lookahead = true) {
    let position = start, cancelled = false;
    return new ReadableStream<Uint8Array>({
      pull: async output => {
        try {
          signal.throwIfAborted();
          if (position >= end) { output.close(); return; }
          const index = Math.floor(position / this.header.blockSize);
          const current = this.block(index, prefetch);
          // 预取窗口按块推进，必须跨过批次边界：消费消耗完当前块时，下一块的请求
          // 早已在途，才能把网络往返藏在消费背后。深度取 PREFETCH_BLOCKS，与
          // 批次大小解耦——见该常量注释。
          if (prefetch && lookahead) {
            for (let i = index + 1; i <= index + PREFETCH_BLOCKS; i++) {
              if (i >= this.header.blockCount) break;
              void this.block(i).catch(() => {});
            }
          }
          const bytes = await current;
          signal.throwIfAborted();
          if (cancelled) return;
          const base = index * this.header.blockSize;
          const next = Math.min(end, base + bytes.length);
          output.enqueue(bytes.subarray(position - base, next - base)); position = next;
          if (position >= end) output.close();
        } catch (error) { if (!cancelled) output.error(error); }
      }, cancel: () => { cancelled = true; },
    }, { highWaterMark: 0 });
  }

  async read(start: number, end: number, signal: AbortSignal): Promise<Uint8Array> {
    if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end > this.size || end <= start) throw new Error("媒体读取区间无效");
    // 有限区间：终点明确，不排跨批预取——区间之后的块不会被这次读取消费。
    const reader = this.stream(start, end, signal, true, false).getReader();
    const result = new Uint8Array(end - start); let offset = 0;
    try { for (;;) { const { value, done } = await reader.read(); if (done) break; result.set(value, offset); offset += value.length; } }
    finally { await reader.cancel(); }
    return result;  }

  /**
   * 读取文件末尾 `length` 字节的明文。
   *
   * 分片 MP4（fMP4）按设计把 duration 留在 mvhd 里写 0（媒体可以无限追加），
   * 真实时长只存在于每个 moof 的 tfdt。从头读永远拿不到总时长，所以时长探测
   * 必须从尾部反向进行。这也是唯一需要"倒着读"的场景，因此单独开一个入口，
   * 不去改动既有正向读取的语义。
   */
  async tail(length: number, signal: AbortSignal): Promise<Uint8Array> {
    const start = Math.max(0, this.size - length);
    return this.read(start, this.size, signal);
  }

  /**
   * 一次 Range 请求覆盖多少个加密块。
   *
   * 按 BATCH_BYTES 目标折算，并封顶 8 块——封顶只为在加密块异常小（格式下限
   * 512 B）时不至于把整份文件一次性预取进内存。当前默认 1 MiB 块配 1 MiB
   * 目标下恒为 1，即一个请求对应一个完整加密块。
   *
   * 预取**窗口深度**不由这里决定：那是 PREFETCH_BLOCKS 的职责，两者已解耦，
   * 批次调小才不会连带把窗口压塌。
   */
  private batchCount(prefetch: boolean): number {
    return prefetch ? Math.max(1, Math.min(8, Math.floor(BATCH_BYTES / this.header.blockSize))) : 1;
  }

  private block(index: number, prefetch = true): Promise<Uint8Array> {
    this.signal.throwIfAborted();
    const cached = this.cache.get(index);
    if (cached) { this.cache.delete(index); this.cache.set(index, cached); return Promise.resolve(cached); }
    if (!this.pending.has(index)) {
      const indices: number[] = [];
      const count = this.batchCount(prefetch);
      for (let i = index; i < Math.min(this.header.blockCount, index + count); i++) {
        if (this.pending.has(i) || this.cache.has(i)) break;
        let resolve!: Block["resolve"], reject!: Block["reject"];
        const promise = new Promise<Uint8Array>((yes, no) => { resolve = yes; reject = no; });
        void promise.catch(() => {}); // 预取块可能没有消费者。
        this.pending.set(i, { promise, resolve, reject }); indices.push(i);
      }
      void this.fetchBlocks(indices);
    }
    return this.pending.get(index)!.promise;
  }

  private async fetchBlocks(indices: number[]) {
    const entries = indices.map(i => this.pending.get(i)!);
    try {
      await this.acquire();
      try {
        // 网络断开只重试尚未认证的后缀，已经交付的块不再请求。
        let next = 0;
        for (let attempt = 0; next < indices.length; attempt++) {
          try {
            let block = new Uint8Array(blockCipherLen(this.header, indices[next])), used = 0;
            let finalPlain: Uint8Array | undefined;
            const last = indices[indices.length - 1];
            for await (const chunk of this.cipher(blockCipherOffset(this.header, indices[next]), blockCipherOffset(this.header, last) + blockCipherLen(this.header, last))) {
              let offset = 0;
              while (offset < chunk.length) {
                const take = Math.min(block.length - used, chunk.length - offset);
                block.set(chunk.subarray(offset, offset + take), used); used += take; offset += take;
                if (used !== block.length) continue;
                const index = indices[next];
                const plain = await decryptBlock(this.key, this.header, index, block);
                this.signal.throwIfAborted();
                if (next === indices.length - 1) finalPlain = plain;
                else { this.remember(index, plain); entries[next].resolve(plain); }
                next++; used = 0;
                if (next < indices.length) block = new Uint8Array(blockCipherLen(this.header, indices[next]));
              }
            }
            if (finalPlain) { this.remember(last, finalPlain); entries[entries.length - 1].resolve(finalPlain); }
          } catch (error) {
            this.signal.throwIfAborted();
            if (next === indices.length) next--; // 最后一块须等精确响应结束后才交付。
            if (attempt >= 2 || !(error instanceof TypeError || error instanceof MediaSourceError && error.code === "network")) throw error;
            await this.delay(150 * 2 ** attempt);
          }
        }
      } finally { this.active--; this.queue.shift()?.(); }
    } catch (error) { for (const entry of entries) entry.reject(error); }
    finally { for (const index of indices) this.pending.delete(index); }
  }

  private remember(index: number, plain: Uint8Array) {
    const existing = this.cache.get(index);
    if (existing) this.cachedBytes -= existing.length;
    this.cache.delete(index); this.cache.set(index, plain); this.cachedBytes += plain.length;
    while (this.cachedBytes > CACHE_BYTES && this.cache.size > 1) {
      const oldest = this.cache.keys().next().value!;
      this.cachedBytes -= this.cache.get(oldest)!.length; this.cache.delete(oldest);
    }
  }

  /**
   * 取一个请求槽位。没有槽位就排队，等前面的批次完成再补位。
   *
   * 并发只决定同时在途多少个请求，不影响预取窗口深度——后者由
   * PREFETCH_BLOCKS 固定，两者已解耦。调大并发能在慢网上摊平单个请求的
   * 等待（往返延迟被流水线藏起来），代价是多占连接。
   */
  private async acquire() {
    this.signal.throwIfAborted();
    if (this.active < MAX_CONCURRENT) { this.active++; return; }
    await new Promise<void>((resolve, reject) => {
      const ready = () => { this.signal.removeEventListener("abort", abort); this.active++; resolve(); };
      const abort = () => { const i = this.queue.indexOf(ready); if (i >= 0) this.queue.splice(i, 1); reject(this.signal.reason); };
      this.queue.push(ready); this.signal.addEventListener("abort", abort, { once: true });
    });
  }
  private delay(ms: number) {
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => { this.signal.removeEventListener("abort", abort); resolve(); }, ms);
      const abort = () => { clearTimeout(timer); reject(this.signal.reason); };
      this.signal.addEventListener("abort", abort, { once: true });
    });
  }

  private async *cipher(start: number, end: number): AsyncGenerator<Uint8Array> {
    if (this.localFile) {
      const reader = this.localFile.slice(start, end).stream().getReader();
      try { for (;;) { this.signal.throwIfAborted(); const { value, done } = await reader.read(); if (done) break; yield value; } }
      finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
      return;
    }
    const parts = this.descriptor.parts?.length ? this.descriptor.parts : [{ url: this.descriptor.url, offset: 0, size: this.descriptor.cipherSize }];
    for (const range of cipherPartRanges(parts, start, end - 1)) {
      const timeout = new AbortController();
      const signal = AbortSignal.any([this.signal, timeout.signal]);
      // 限制无数据等待时间，而非整个请求时长；大文件完整缓存可能持续数分钟。
      const timed = async <T>(operation: () => Promise<T>): Promise<T> => {
        const timer = setTimeout(() => timeout.abort(), 30_000);
        try { return await operation(); } finally { clearTimeout(timer); }
      };
      let response: Response;
      try { response = await timed(() => fetch(range.url, { headers: { Range: `bytes=${range.start}-${range.end}` }, signal, cache: "no-store" })); }
      catch (error) { this.signal.throwIfAborted(); if (signal.aborted || error instanceof TypeError) throw new MediaSourceError(signal.aborted ? "媒体请求超时" : "媒体网络连接中断", "network"); throw error; }
      const expected = range.end - range.start + 1;
      const contentRange = response.headers.get("Content-Range");
      const match = contentRange?.match(/^bytes (\d+)-(\d+)\/(\d+)$/);
      const part = parts.find(part => part.url === range.url && start + range.offset >= part.offset && start + range.offset < part.offset + part.size)!;
      if (response.status === 401 || response.status === 403) { await response.body?.cancel(); throw new MediaSourceError("预览票据已失效，请重新打开文件", "auth"); }
      if (response.status >= 500 || response.status === 429) { await response.body?.cancel(); throw new MediaSourceError("媒体服务暂时不可用", "network"); }
      if (!(response.status === 206 || response.status === 200 && range.start === 0 && expected === part.size)
        || contentRange && (!match || +match[1] !== range.start || +match[2] !== range.end || +match[3] !== part.size)) {
        await response.body?.cancel(); this.rangeUnsupported = true; throw new MediaSourceError("媒体源未返回正确的分片区间", "range");
      }
      const reader = response.body?.getReader();
      if (!reader) throw new MediaSourceError("媒体响应为空", "network");
      let received = 0;
      try {
        for (;;) {
          const { value, done } = await timed(() => reader.read()); if (done) break;
          received += value.length;
          if (received > expected) throw new MediaSourceError("媒体分片长度不匹配", "range");
          yield value;
        }
        if (received !== expected) throw new MediaSourceError("媒体分片被截断", "network");
      } catch (error) { this.signal.throwIfAborted(); if (signal.aborted || error instanceof TypeError) throw new MediaSourceError(signal.aborted ? "媒体请求超时" : "媒体网络连接中断", "network"); throw error; }
      finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
    }
  }

  /** 文件只保存密文；关闭会话时删除，超出磁盘配额时不会转为无限内存下载。 */
  private async cacheCompleteCipher(progress: (bytes: number) => void): Promise<void> {
    if (!globalThis.navigator?.storage?.getDirectory) throw new MediaSourceError("此浏览器不支持大文件临时缓存，请下载后播放", "size");
    const directory = await navigator.storage.getDirectory();
    this.signal.throwIfAborted();
    const name = `xph-media-${crypto.randomUUID()}`;
    this.temporary = { directory, name };
    const file = await directory.getFileHandle(name, { create: true });
    this.signal.throwIfAborted();
    const writer = await file.createWritable();
    let completed = false;
    try {
      let received = 0;
      for await (const bytes of this.cipher(0, this.descriptor.cipherSize)) {
        this.signal.throwIfAborted(); await writer.write(bytes as Uint8Array<ArrayBuffer>);
        received += bytes.length;
        progress(Math.min(this.descriptor.plainSize, Math.floor(received / this.descriptor.cipherSize * this.descriptor.plainSize)));
      }
      this.signal.throwIfAborted(); await writer.close(); completed = true;
      this.localFile = await file.getFile();
      this.signal.throwIfAborted();
      this.header = parseXphHeader(new Uint8Array(await this.localFile.slice(0, HEADER_SIZE).arrayBuffer()));
      if (this.header.plainSize !== this.descriptor.plainSize || this.header.cipherSize !== this.descriptor.cipherSize) throw new XphFormatError("媒体文件头与交付计划不一致");
      // 先校验全部认证块，后续播放才可以完全依赖本地密文。
      for (let i = 0; i < this.header.blockCount; i++) {
        this.signal.throwIfAborted();
        const offset = blockCipherOffset(this.header, i);
        await decryptBlock(this.key, this.header, i, new Uint8Array(await this.localFile.slice(offset, offset + blockCipherLen(this.header, i)).arrayBuffer()));
      }
    } finally { if (!completed) await writer.abort().catch(() => {}); }
  }

  async dispose(): Promise<void> {
    await this.diskTask?.catch(() => {});
    this.localFile = undefined; this.cache.clear(); this.cachedBytes = 0;
    const temporary = this.temporary; this.temporary = undefined;
    if (temporary) await temporary.directory.removeEntry(temporary.name).catch(() => {});
  }

  /** 小文件使用完整 Blob；大文件仅密文落盘，再经同一解密接口本地播放。 */
  async complete(mime: string, progress: (bytes: number) => void, allowDisk = true): Promise<Blob | { cached: true }> {
    if (this.descriptor.plainSize > FULL_PREVIEW_LIMIT) {
      if (!allowDisk) throw new MediaSourceError("此浏览器无法流式播放大文件，请下载后播放", "size");
      this.diskTask = this.cacheCompleteCipher(progress);
      await this.diskTask;
      return { cached: true };
    }
    const chunks: BlobPart[] = [];
    if (this.rangeUnsupported) {
      // 上游不支持 Range 时，只允许显式完整读取；仍逐块认证，不把密文整体放进内存。
      let buffer = new Uint8Array(HEADER_SIZE), used = 0, index = -1;
      for await (const chunk of this.cipher(0, this.descriptor.cipherSize)) {
        let offset = 0;
        while (offset < chunk.length) {
          const take = Math.min(buffer.length - used, chunk.length - offset);
          buffer.set(chunk.subarray(offset, offset + take), used); offset += take; used += take;
          if (used !== buffer.length) continue;
          if (index < 0) {
            this.header = parseXphHeader(buffer);
            if (this.header.plainSize !== this.descriptor.plainSize || this.header.cipherSize !== this.descriptor.cipherSize) throw new XphFormatError("媒体文件头与交付计划不一致");
          } else {
            const bytes = await decryptBlock(this.key, this.header, index, buffer);
            chunks.push(new Blob([bytes as Uint8Array<ArrayBuffer>])); progress(Math.min(this.size, (index + 1) * this.header.blockSize));
          }
          index++; used = 0;
          if (index < this.header.blockCount) buffer = new Uint8Array(blockCipherLen(this.header, index));
        }
      }
      this.signal.throwIfAborted();
      if (index !== this.header.blockCount || used) throw new XphFormatError("完整媒体被截断");
      return new Blob(chunks, { type: mime });
    }
    for (let i = 0; i < this.header.blockCount; i++) {
      const current = this.block(i);
      if (i + 1 < this.header.blockCount) void this.block(i + 1).catch(() => {});
      const bytes = await current; this.signal.throwIfAborted();
      chunks.push(new Blob([bytes as Uint8Array<ArrayBuffer>]));
      progress(Math.min(this.size, (i + 1) * this.header.blockSize));
    }
    return new Blob(chunks, { type: mime });
  }
}
