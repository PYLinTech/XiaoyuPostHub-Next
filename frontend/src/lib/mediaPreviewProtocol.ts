import type { MediaInfo } from "./mediaRemux";
import type { MediaCipherSource } from "./mediaRangeSource";

/**
 * 主线程与媒体 Worker 之间的通信协议。
 *
 * 单独成文件而不是各写一套：协议字段一旦两边不同名，报错会出现在运行时的
 * 静默分支上（`if (data.progress !== undefined)` 读到 undefined），那种 bug 极难
 * 定位。集中定义后，改字段时编译期就会指出来。
 *
 * 方向标注：
 *   ToWorker — 主线程发给 Worker 的请求
 *   FromWorker — Worker 发回主线程的消息
 */

/** 块请求的三档优先级，与 MediaReadScheduler 共用同一套语义。 */
export type RequestPriority = "critical" | "playback" | "prefetch";

/** 播放参数快照。worker 据此协调调度，不自己猜播放状态。 */
export interface PlaybackDirective {
  /** 播放代际。每次 start/seek 递增，旧代际的产出立即作废。 */
  generation: number;
  /** 当前播放头（秒），用于把缓冲上限解释为相对时间。 */
  currentTime: number;
  /** 前向缓冲目标（秒）：worker 的产出闸门 permit 按它放行。 */
  horizon: number;
  /** 连续播放时是否允许预测性读取（连续 seek 时关掉）。 */
  predict: boolean;
}

/** 由当前播放头与目标缓冲计算 Worker 应停止产出的时间点。 */
export function productionHorizon(directive: PlaybackDirective): number {
  const ahead = Math.max(0, directive.horizon - directive.currentTime);
  return directive.currentTime + (directive.predict ? ahead : Math.min(ahead, 2));
}

/** 暂停与 seek 都遵守有限窗口；时间统一使用媒体时间轴上的秒数。 */
export function shouldWaitForOutput(time: number, directive: PlaybackDirective): boolean {
  return time >= productionHorizon(directive);
}

export interface ReadPolicy {
  maxConcurrent: number;
  prefetchBlocks: number;
  cacheBytes: number;
}

export type ToWorker =
  | { kind: "open"; source: MediaCipherSource }
  | { kind: "inspect" }
  | { kind: "duration"; origin: number }
  | { kind: "complete"; mime: string; allowDisk: boolean }
  /** 开始（或 seek 后重新开始）一次顺序消费。 */
  | { kind: "start"; time: number; directive: PlaybackDirective }
  /** 只更新缓冲目标与播放状态，不换代际——播放器每秒都会发。 */
  | { kind: "directive"; directive: PlaybackDirective }
  /** 把缓冲控制器的结论落成调度策略。 */
  | { kind: "policy"; policy: ReadPolicy }
  | { kind: "stop" }
  | { kind: "close" }
  | { kind: "ack"; chunkId: number; error?: { name: string; message: string } };

export type FromWorker =
  /** 请求结果：成功带 value，失败带 error。两者互斥，用一个可选字段表达。 */
  | { kind: "result"; id: number; value?: unknown; error?: { name: string; message: string; code?: string; transient?: boolean } }
  | { kind: "chunk"; chunkId: number; generation: number; track: number; bytes: Uint8Array<ArrayBuffer>; mime: string; keyframes: number[]; mediaSeconds: number }
  | { kind: "checkMime"; chunkId: number; mime: string }
  | { kind: "progress"; ratio: number; stage: string }
  /** 已认证交付的明文字节数。缓冲控制器靠它测真实吞吐，据此算健康度 R。 */
  | { kind: "delivered"; bytes: number; at: number }
  | { kind: "ended"; generation: number; end: number }
  | { kind: "failure"; generation: number; error: { name: string; message: string; code?: string; transient?: boolean } }
  | { kind: "closed"; closed: true };

/** 轨道信息在两条链路上都要用，单独导出免得重复 import。 */
export type { MediaInfo };
