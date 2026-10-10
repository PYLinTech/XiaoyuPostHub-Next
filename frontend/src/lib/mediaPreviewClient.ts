import { supportsMediaSourceType } from "./mediaSourceSupport";
import type { MediaInfo } from "./mediaRemux";
import type { MediaCipherSource } from "./mediaRangeSource";

export function createMediaPreviewClient(source: MediaCipherSource, lifetime: AbortSignal) {
  lifetime.throwIfAborted();
  const worker = new Worker(new URL("./mediaPreview.worker.ts", import.meta.url), { type: "module" });
  let sequence = 0, generation = 0, failure: Error | null = null;
  let shutdown: ReturnType<typeof setTimeout> | undefined;
  let receiver: ((track: number, bytes: Uint8Array<ArrayBuffer>, mime: string, generation: number, keyframes: number[]) => Promise<void>) | undefined;
  let ended: ((end: number, generation: number) => void) | undefined;
  let failed: ((error: Error) => void) | undefined;
  let progress: ((bytes: number) => void) | undefined;
  const pending = new Map<number, { resolve(value: any): void; reject(error: unknown): void }>();
  const deserialize = (error: { name: string; message: string; code?: string }) => Object.assign(new Error(error.message), { name: error.name, code: error.code });
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
    if (data.closed) { clearTimeout(shutdown); worker.terminate(); return; }
    if (failure) return;
    if (data.checkMime) { worker.postMessage({ kind: "ack", chunkId: data.chunkId, supported: supportsMediaSourceType(data.checkMime) }); return; }
    if (data.progress !== undefined) { progress?.(data.progress); return; }
    if (data.chunkId) {
      const acknowledge = (error?: unknown) => { if (!failure) worker.postMessage({ kind: "ack", chunkId: data.chunkId, error: error ? { name: error instanceof Error ? error.name : "Error", message: error instanceof Error ? error.message : String(error) } : undefined }); };
      if (data.generation !== generation || !receiver) { acknowledge("已取消"); return; }
      void receiver(data.track, data.bytes, data.mime, data.generation, data.keyframes || []).then(() => acknowledge(), acknowledge); return;
    }
    if (data.failure) { if (data.generation === generation) failed?.(deserialize(data.failure)); return; }
    if (data.ended !== undefined) { if (data.generation === generation) ended?.(data.ended, generation); return; }
    const entry = pending.get(data.id); if (!entry) return; pending.delete(data.id);
    if (data.error) entry.reject(deserialize(data.error)); else entry.resolve(data.value);
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
    complete(mime: string, onProgress: (bytes: number) => void, allowDisk: boolean) { progress = onProgress; return request<Blob | { cached: true }>({ kind: "complete", mime, allowDisk }); },
    listen(chunk: NonNullable<typeof receiver>, end: NonNullable<typeof ended>, error: NonNullable<typeof failed>) { receiver = chunk; ended = end; failed = error; },
    stop() { generation++; worker.postMessage({ kind: "stop" }); return generation; },
    start(time: number, horizon: number) { worker.postMessage({ kind: "start", time, horizon, generation }); },
    demand(horizon: number) { if (!failure) worker.postMessage({ kind: "demand", horizon }); },
  };
}
export type MediaPreviewClient = ReturnType<typeof createMediaPreviewClient>;
