import { MediaRangeSource } from "./mediaRangeSource";
import { MediaDemuxSession, setMediaSupportCheck } from "./mediaRemux";
import type { MediaInfo } from "./mediaRemux";
import { shouldWaitForOutput } from "./mediaPreviewProtocol";
import type { PlaybackDirective } from "./mediaPreviewProtocol";

declare const self: DedicatedWorkerGlobalScope;
const lifetime = new AbortController();
let source: MediaRangeSource, session: MediaDemuxSession, info: MediaInfo;
let playback = new AbortController(), generation = 0, chunkId = 0;
/**
 * 播放指令快照。worker 不自己猜播放状态——它看不到媒体元素，也不知道用户是不是
 * 暂停了，所以由主线程每次变化时推过来。分开存放是为了让 permit 在 await 之后
 * 读到的是最新值，而不是 start 那一刻的旧值。
 */
let directive: PlaybackDirective = { generation: 0, currentTime: 0, horizon: 2, predict: false };
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
setMediaSupportCheck(mime => acknowledgement({ kind: "checkMime", mime }, lifetime.signal));
function wake() { for (const resolve of waiters) resolve(); waiters.clear(); }
function stop() { playback.abort(); wake(); }
function serializeError(error: unknown) {
  return { name: error instanceof Error ? error.name : "Error", message: error instanceof Error ? error.message : String(error),
    code: error && typeof error === "object" && "code" in error ? error.code : undefined,
    transient: error && typeof error === "object" && "transient" in error ? error.transient : undefined };
}
/**
 * 产出闸门：数据已经排到 horizon 之外就停下，等主线程把需求推近。
 *
 * 暂停和连续 seek 仍受缓冲上限约束，只缩小前向窗口；指令更新会唤醒闸门，
 * 因而暂停时最多补齐目标缓冲，恢复播放或 seek 后可立即重算。
 */
async function permit(time: number, signal: AbortSignal) {
  while (shouldWaitForOutput(time, directive)) {
    signal.throwIfAborted();
    // 暂停时也要停在缓冲目标处，不能让解复用器一路处理完整文件。
    await new Promise<void>(resolve => waiters.add(resolve));
  }
  signal.throwIfAborted();
}
/**
 * 上报一次有效交付。
 *
 * 只统计认证成功的明文字节，重试与被取消的请求不算——否则测出来的吞吐会虚高，
 * 缓冲策略会误判成网络很好而铺得太开。
 */
let lastDeliveredAt = performance.now();
let pendingDeliveredBytes = 0;
function reportDelivered(bytes: number) {
  pendingDeliveredBytes += bytes;
  const now = performance.now();
  // 节流到每 250ms 一次：更细的粒度对吞吐估计没有帮助，只会刷爆主线程消息队列。
  if (now - lastDeliveredAt < 250) return;
  lastDeliveredAt = now;
  self.postMessage({ kind: "delivered", bytes: pendingDeliveredBytes, at: now });
  pendingDeliveredBytes = 0;
}
self.onmessage = ({ data }) => {
  if (data.kind === "close") {
    stop(); lifetime.abort();
    session?.dispose();
    void source?.dispose().finally(() => self.postMessage({ kind: "closed", closed: true }));
    if (!source) self.postMessage({ kind: "closed", closed: true });
    return;
  }
  if (data.kind === "ack") {
    const entry = acknowledgements.get(data.chunkId); acknowledgements.delete(data.chunkId); entry?.clean();
    if (data.error) entry?.reject(Object.assign(new Error(data.error.message), { name: data.error.name })); else entry?.resolve(data.supported);
    return;
  }
  if (data.kind === "stop") { stop(); return; }
  if (data.kind === "directive") { directive = data.directive; wake(); return; }
  if (data.kind === "policy") { source?.updatePolicy(data.policy); return; }
  if (data.kind === "start") {
    stop(); playback = new AbortController(); directive = data.directive; generation = data.directive.generation;
    const signal = playback.signal, token = generation;
    void Promise.all(info.tracks.map((spec, track) => session.stream(spec, data.time, info.origin, signal,
      (bytes, mime, keyframes, mediaSeconds) => acknowledgement({ kind: "chunk", generation: token, track, bytes, mime, keyframes, mediaSeconds }, signal, bytes),
      time => permit(time, signal)))).then(ends => {
        if (!signal.aborted) self.postMessage({ kind: "ended", generation: token, end: Math.max(...ends) });
      }).catch(error => {
        if (!signal.aborted) { stop(); self.postMessage({ kind: "failure", generation: token, error: serializeError(error) }); }
      });
    return;
  }
  void (async () => {
    try {
      let value;
      if (data.kind === "open") {
        source = new MediaRangeSource(data.source, reportDelivered); await source.open(lifetime.signal);
        session = new MediaDemuxSession(source, lifetime.signal);
        value = true;
      } else if (data.kind === "inspect") { info = await session.inspect(); value = info; }
      else if (data.kind === "duration") value = await session.duration(data.origin);
      else if (data.kind === "complete") {
        stop(); let reported = 0; let lastStage = "";
        value = await source.complete(data.mime, (stage, ratio) => {
          // 阶段切换必须立刻上报：用户需要知道"下载完了、现在开始校验"，
          // 而不是把两个阶段混在一个数字里。阶段内再按 100ms 节流。
          const now = performance.now();
          if (stage === lastStage && now - reported < 100) return;
          reported = now; lastStage = stage;
          self.postMessage({ kind: "progress", ratio, stage });
        }, data.allowDisk);
        lifetime.signal.throwIfAborted();
        if (!(value instanceof Blob)) { session?.dispose(); session = new MediaDemuxSession(source, lifetime.signal); }
      }
      self.postMessage({ kind: "result", id: data.id, value });
    } catch (error) { self.postMessage({ kind: "result", id: data.id, error: serializeError(error) }); }
  })();
};
