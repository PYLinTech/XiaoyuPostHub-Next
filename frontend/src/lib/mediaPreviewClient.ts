import { supportsMediaSourceType } from "./mediaSourceSupport";
import type { MediaInfo } from "./mediaRemux";
import type { MediaCipherSource } from "./mediaRangeSource";
import type { PlaybackDirective, ReadPolicy } from "./mediaPreviewProtocol";

export function createMediaPreviewClient(source: MediaCipherSource, lifetime: AbortSignal) {
  lifetime.throwIfAborted();
  const worker = new Worker(new URL("./mediaPreview.worker.ts", import.meta.url), { type: "module" });
  let sequence = 0, generation = 0, failure: Error | null = null, closing = false;
  let shutdown: ReturnType<typeof setTimeout> | undefined;
  let closeResolve: (() => void) | undefined;
  let receiver: ((track: number, bytes: Uint8Array<ArrayBuffer>, mime: string, generation: number, keyframes: number[], mediaSeconds: number) => Promise<void>) | undefined;
  let ended: ((end: number, generation: number) => void) | undefined;
  let failed: ((error: Error) => void) | undefined;
  let progress: ((ratio: number, stage: string) => void) | undefined;
  /** 已认证交付字节的回调，交给缓冲控制器测吞吐。 */
  let delivered: ((bytes: number, at: number) => void) | undefined;
  const pending = new Map<number, { resolve(value: any): void; reject(error: unknown): void }>();
  const deserialize = (error: { name: string; message: string; code?: string; transient?: boolean }) =>
    Object.assign(new Error(error.message), { name: error.name, code: error.code, transient: error.transient });
  const fail = (error: Error) => {
    if (failure) return;
    failure = error; lifetime.removeEventListener("abort", stop);
    // 让 Worker 中止网络并删除密文临时文件；异常线程仍有终止超时保护。
    try { worker.postMessage({ kind: "close" }); shutdown = setTimeout(() => worker.terminate(), 5000); }
    catch { worker.terminate(); }
    for (const entry of pending.values()) entry.reject(error); pending.clear();
    if (!lifetime.aborted) failed?.(error);
  };
  const stop = () => fail(lifetime.reason || new DOMException("已取消", "AbortError"));
  lifetime.addEventListener("abort", stop, { once: true });
  worker.onerror = event => fail(new Error(event.message || "媒体线程异常"));
  worker.onmessageerror = () => fail(new Error("媒体线程响应无法解析"));
  worker.onmessage = ({ data }) => {
    if (data.kind === "closed") { clearTimeout(shutdown); worker.terminate(); closeResolve?.(); return; }
    if (failure || closing) return;
    if (data.kind === "checkMime") { worker.postMessage({ kind: "ack", chunkId: data.chunkId, supported: supportsMediaSourceType(data.mime) }); return; }
    if (data.kind === "progress") { progress?.(data.ratio, data.stage); return; }
    if (data.kind === "delivered") { delivered?.(data.bytes, data.at); return; }
    if (data.kind === "chunk") {
      const acknowledge = (error?: unknown) => { if (!failure) worker.postMessage({ kind: "ack", chunkId: data.chunkId, error: error ? { name: error instanceof Error ? error.name : "Error", message: error instanceof Error ? error.message : String(error) } : undefined }); };
      if (data.generation !== generation || !receiver) { acknowledge("已取消"); return; }
      void receiver(data.track, data.bytes, data.mime, data.generation, data.keyframes || [], data.mediaSeconds || 0).then(() => acknowledge(), acknowledge); return;
    }
    if (data.kind === "failure") { if (data.generation === generation) failed?.(deserialize(data.error)); return; }
    if (data.kind === "ended") { if (data.generation === generation) ended?.(data.end, generation); return; }
    if (data.kind === "result") {
      const entry = pending.get(data.id); if (!entry) return; pending.delete(data.id);
      if (data.error) entry.reject(deserialize(data.error)); else entry.resolve(data.value);
    }
  };
  function request<T>(value: object): Promise<T> {
    lifetime.throwIfAborted(); if (failure) return Promise.reject(failure);
    const id = ++sequence;
    return new Promise((resolve, reject) => {
      pending.set(id, { resolve, reject });
      try { worker.postMessage({ ...value, id }); } catch (error) { pending.delete(id); reject(error); }
    });
  }
  return {
    async open() { const opened = request({ kind: "open", source }); source.key.fill(0); await opened; },
    inspect: () => request<MediaInfo>({ kind: "inspect" }),
    duration: (origin: number) => request<number | null>({ kind: "duration", origin }),
    complete(mime: string, onProgress: (ratio: number, stage: string) => void, allowDisk: boolean) { progress = onProgress; return request<Blob | { cached: true }>({ kind: "complete", mime, allowDisk }); },
    listen(chunk: NonNullable<typeof receiver>, end: NonNullable<typeof ended>, error: NonNullable<typeof failed>) { receiver = chunk; ended = end; failed = error; },
    stop() { generation++; worker.postMessage({ kind: "stop" }); return generation; },
    /** 重新授权换源时关闭旧 Worker，释放它持有的 demux 索引与 OPFS 临时文件。 */
    close() {
      if (closing) return Promise.resolve();
      closing = true;
      lifetime.removeEventListener("abort", stop);
      const reason = new DOMException("媒体会话已关闭", "AbortError");
      for (const entry of pending.values()) entry.reject(reason);
      pending.clear();
      return new Promise<void>(resolve => {
        closeResolve = resolve;
        try {
          worker.postMessage({ kind: "close" });
          shutdown = setTimeout(() => { worker.terminate(); closeResolve?.(); }, 5000);
        } catch { worker.terminate(); resolve(); }
      });
    },
    /**
     * 开始（或 seek 后重新开始）一次顺序消费。
     *
     * `directive` 一次带齐代际、缓冲目标、暂停与预测开关——worker 拿到的是
     * 自洽的一组参数，而不是可能互相矛盾的多次更新。
     */
    start(time: number, directive: PlaybackDirective) {
      if (!failure) worker.postMessage({ kind: "start", time, directive });
    },
    /** 只更新缓冲目标与播放状态，不换代际。播放器每秒调用一次。 */
    directive(directive: PlaybackDirective) {
      if (!failure) worker.postMessage({ kind: "directive", directive });
    },
    /**
     * 注册交付回调，供缓冲控制器测真实吞吐。
     *
     * 必须在 retune 之前设置：吞吐是健康度 R 的分子，没有它策略只能靠中性先验。
     */
    onDelivered(handler: (bytes: number, at: number) => void) { delivered = handler; },
    /**
     * 把缓冲控制器算出的策略落成调度参数。
     *
     * 这是三层缓冲的最后一环：控制器算"该留多少秒、要不要预测"，这里转成
     * 并发数与预取视野送进 worker 线程里的调度器。
     */
    updatePolicy(policy: ReadPolicy) {
      if (!failure) worker.postMessage({ kind: "policy", policy });
    },
  };
}
export type MediaPreviewClient = ReturnType<typeof createMediaPreviewClient>;
