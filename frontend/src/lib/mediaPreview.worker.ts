import { MediaRangeSource } from "./mediaRangeSource";
import { MediaDemuxSession, setMediaSupportCheck } from "./mediaRemux";
import type { MediaInfo } from "./mediaRemux";

declare const self: DedicatedWorkerGlobalScope;
const lifetime = new AbortController();
let source: MediaRangeSource, session: MediaDemuxSession, info: MediaInfo;
let playback = new AbortController(), generation = 0, horizon = 30, chunkId = 0;
const waiters = new Set<() => void>();
const acknowledgements = new Map<number, { resolve(value?: any): void; reject(error: unknown): void; clean(): void }>();
function acknowledgement(message: object, signal: AbortSignal, bytes?: Uint8Array<ArrayBuffer>) {
  signal.throwIfAborted();
  return new Promise<any>((resolve, reject) => {
    const id = ++chunkId;
    const abort = () => { acknowledgements.delete(id); reject(signal.reason); };
    acknowledgements.set(id, { resolve, reject, clean: () => signal.removeEventListener("abort", abort) });
    signal.addEventListener("abort", abort, { once: true });
    self.postMessage({ ...message, chunkId: id }, bytes ? [bytes.buffer] : []);
  });
}
setMediaSupportCheck(mime => acknowledgement({ checkMime: mime }, lifetime.signal));
function wake() { for (const resolve of waiters) resolve(); waiters.clear(); }
function stop() { playback.abort(); wake(); }
function serializeError(error: unknown) {
  return { name: error instanceof Error ? error.name : "Error", message: error instanceof Error ? error.message : String(error),
    code: error && typeof error === "object" && "code" in error ? error.code : undefined };
}
async function permit(time: number, signal: AbortSignal) {
  while (time >= horizon) {
    signal.throwIfAborted();
    await new Promise<void>(resolve => waiters.add(resolve));
  }
  signal.throwIfAborted();
}
self.onmessage = ({ data }) => {
  if (data.kind === "close") {
    stop(); lifetime.abort();
    void source?.dispose().finally(() => self.postMessage({ closed: true }));
    if (!source) self.postMessage({ closed: true });
    return;
  }
  if (data.kind === "ack") {
    const entry = acknowledgements.get(data.chunkId); acknowledgements.delete(data.chunkId); entry?.clean();
    if (data.error) entry?.reject(Object.assign(new Error(data.error.message), { name: data.error.name })); else entry?.resolve(data.supported);
    return;
  }
  if (data.kind === "stop") { stop(); return; }
  if (data.kind === "demand") { horizon = data.horizon; wake(); return; }
  if (data.kind === "start") {
    stop(); playback = new AbortController(); generation = data.generation; horizon = data.horizon;
    const signal = playback.signal, token = generation;
    void Promise.all(info.tracks.map((spec, track) => session.stream(spec, data.time, info.origin, signal,
      (bytes, mime, keyframes) => acknowledgement({ generation: token, track, bytes, mime, keyframes }, signal, bytes),
      time => permit(time, signal)))).then(ends => {
        if (!signal.aborted) self.postMessage({ generation: token, ended: Math.max(...ends) });
      }).catch(error => {
        if (!signal.aborted) { stop(); self.postMessage({ generation: token, failure: serializeError(error) }); }
      });
    return;
  }
  void (async () => {
    try {
      let value;
      if (data.kind === "open") {
        source = new MediaRangeSource(data.source); await source.open(lifetime.signal);
        session = new MediaDemuxSession(source, lifetime.signal);
        value = true;
      } else if (data.kind === "inspect") { info = await session.inspect(); value = info; }
      else if (data.kind === "duration") value = await session.duration(data.origin);
      else if (data.kind === "complete") {
        stop(); let reported = 0;
        value = await source.complete(data.mime, bytes => {
          const now = performance.now();
          if (now - reported >= 100) { reported = now; self.postMessage({ progress: bytes }); }
        }, data.allowDisk);
        lifetime.signal.throwIfAborted();
        if (!(value instanceof Blob)) { session?.dispose(); session = new MediaDemuxSession(source, lifetime.signal); }
      }
      self.postMessage({ id: data.id, value });
    } catch (error) { self.postMessage({ id: data.id, error: serializeError(error) }); }
  })();
};
