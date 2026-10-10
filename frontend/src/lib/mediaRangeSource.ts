import { ALL_FORMATS, CustomSource, Input } from "mediabunny";

const PAGE_BYTES = 256 * 1024;
const CACHE_BYTES = 32 * 1024 * 1024;

/** 只读取虚拟明文文件；物理分卷映射与 AES 块认证由 SW 负责。 */
export class MediaRangeSource {
  private size = 0;
  private cache = new Map<number, Uint8Array<ArrayBuffer>>();
  private cachedBytes = 0;
  private active = 0;
  private queue: Array<() => void> = [];
  private pending = new Map<number, { signal: AbortSignal; promise: Promise<Uint8Array<ArrayBuffer>> }>();

  constructor(private url: string) {}

  async open(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted();
    const response = await fetch(this.url, { method: "HEAD", signal, cache: "no-store" });
    if (!response.ok) throw new Error("无法读取媒体文件信息");
    this.size = Number(response.headers.get("Content-Length"));
    if (!Number.isSafeInteger(this.size) || this.size <= 0) throw new Error("媒体文件长度无效");
  }

  input(signal: AbortSignal, readBudget = Infinity): Input {
    let readBytes = 0;
    const pages = new Set<number>();
    return new Input({
      formats: ALL_FORMATS,
      source: new CustomSource({
        getSize: () => this.size,
        read: (start, end) => {
          // 按真实 Range 页计费，而非解析器请求的几个头字节。
          // 裸流逐帧扫描时，否则会用很小的逻辑读取量下载整份音视频。
          if (Number.isFinite(readBudget)) {
            for (let page = Math.floor(start / PAGE_BYTES) * PAGE_BYTES; page < end; page += PAGE_BYTES) {
              if (!pages.has(page)) {
                pages.add(page);
                readBytes += Math.min(PAGE_BYTES, this.size - page);
              }
            }
            if (readBytes > readBudget) throw new Error("媒体元数据探测达到读取上限");
          }
          return this.read(start, end, signal);
        },
        maxCacheSize: 2 * 1024 * 1024,
        // 调度器控制时间窗口，避免解复用器自行提前下载远处内容。
        prefetchProfile: "none",
        handleUnhandledError: () => {},
      }),
    });
  }

  async read(start: number, end: number, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
    signal.throwIfAborted();
    if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end > this.size || end <= start) {
      throw new Error("媒体读取区间无效");
    }
    const result = new Uint8Array(end - start);
    for (let position = start; position < end;) {
      signal.throwIfAborted();
      const page = Math.floor(position / PAGE_BYTES) * PAGE_BYTES;
      let bytes = this.cache.get(page);
      if (!bytes) {
        let entry = this.pending.get(page);
        if (!entry || entry.signal !== signal) {
          const promise = this.fetchPage(page, signal);
          entry = { signal, promise };
          this.pending.set(page, entry);
          void promise.finally(() => {
            if (this.pending.get(page)?.promise === promise) this.pending.delete(page);
          }).catch(() => {});
        }
        bytes = await entry.promise;
        signal.throwIfAborted();
        // 其他并发窗口可能已经填充了相同页。
        if (!this.cache.has(page)) {
          this.cache.set(page, bytes);
          this.cachedBytes += bytes.byteLength;
        }
      }
      this.cache.delete(page);
      this.cache.set(page, bytes);
      while (this.cachedBytes > CACHE_BYTES) {
        const oldest = this.cache.keys().next().value!;
        this.cachedBytes -= this.cache.get(oldest)!.byteLength;
        this.cache.delete(oldest);
      }
      const count = Math.min(end - position, bytes.byteLength - (position - page));
      result.set(bytes.subarray(position - page, position - page + count), position - start);
      position += count;
    }
    return result;
  }

  private async fetchPage(page: number, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
    signal.throwIfAborted();
    if (this.active >= 4) {
      await new Promise<void>((resolve, reject) => {
        const ready = () => { signal.removeEventListener("abort", cancel); this.active++; resolve(); };
        const cancel = () => {
          const index = this.queue.indexOf(ready);
          if (index >= 0) this.queue.splice(index, 1);
          reject(signal.reason);
        };
        this.queue.push(ready);
        signal.addEventListener("abort", cancel, { once: true });
      });
    } else this.active++;
    try {
      signal.throwIfAborted();
      const last = Math.min(this.size, page + PAGE_BYTES) - 1;
      const response = await fetch(this.url, { headers: { Range: `bytes=${page}-${last}` }, signal, cache: "no-store" });
      const range = response.headers.get("Content-Range")?.match(/^bytes (\d+)-(\d+)\/(\d+)$/);
      if (response.status !== 206 || !range || Number(range[1]) !== page
        || Number(range[2]) !== last || Number(range[3]) !== this.size) {
        await response.body?.cancel();
        throw new Error("媒体源未返回正确的分片区间");
      }
      const bytes = new Uint8Array(await response.arrayBuffer());
      signal.throwIfAborted();
      if (bytes.byteLength !== last - page + 1) throw new Error("媒体分片长度不匹配");
      return bytes;
    } finally { this.active--; this.queue.shift()?.(); }
  }

  clear(): void { this.cache.clear(); this.cachedBytes = 0; this.pending.clear(); }
}
