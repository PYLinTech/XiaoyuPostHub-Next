import type { ReadPolicy } from "./mediaPreviewProtocol";

/**
 * 播放缓冲控制器。
 *
 * 职责边界：只算"该缓冲多少秒、预取多深、并发多少"，不碰 MSE、不碰 SourceBuffer、
 * 不碰网络实现。播放器读它的结论去设 worker 的产出闸门，调度器读它去排请求。
 *
 * ## 为什么不能用固定秒数
 *
 * 固定的 30 秒在两种情况下都是错的：快网上白拉几倍于播放所需的字节（浪费流量、
 * 占内存、挤占带宽），慢网上又根本追不上（缓冲照样耗尽）。
 *
 * 真正该问的问题是：**按当前速度，缓冲还能撑多久？** 这需要三个量：
 *
 *   bufferedAhead 播放头之后已经连续缓冲了多少秒
 *   throughput     最近窗口内实际认证并交付的有效字节吞吐
 *   mediaBitrate   媒体本身的消费速率
 *
 * 由此得到健康度 R = throughput / mediaBitrate：R 长期大于 1 说明下载跟得上播放，
 * 小于 1 则缓冲会逐渐耗尽。R 决定往前铺多宽，而不是拍脑袋定秒数。
 *
 * 这些取值都不是实测最优，是把常见工况的起点固定下来，仍需按真实网络继续调。
 */

/** 缓冲健康度的目标区间。低于 low 就该收缩，站稳 high 之上才敢往外铺。 */
const HEALTH = { low: 1.0, high: 1.6 } as const;
/** 目标缓冲的上下限（秒）。任何工况下都不越过这两条线。 */
const TARGET = { min: 2, max: 30 } as const;
/** 最少有效吞吐样本数。太少的观测会被单次抖动带偏，先按中性值处理。 */
const MIN_SAMPLES = 3;
/** 吞吐的指数平滑系数：新观测占多少权重。太大则抖动，太小则跟不上网络变化。 */
const THROUGHPUT_SMOOTHING = 0.35;
/** 尚未测出吞吐时用的保守先验（bit/s）。约等于 3 Mbps，够普通 1080p 用。 */
const ASSUMED_BITRATE = 3_000_000;
/** 观测到的有效吞吐低于这个值就认为网络不可用，按最保守处理。 */
const MIN_PLAUSIBLE_THROUGHPUT = 32_000;

export interface BufferedRange {
  start(index: number): number;
  end(index: number): number;
  readonly length: number;
}

export interface BufferState {
  started: boolean;
  paused: boolean;
  playbackRate: number;
  currentTime: number;
}

export class MediaBufferController {
  private ahead: number = TARGET.min;
  private behind: number = 20;
  private throughput = 0;
  private lastSampleAt?: number;
  private pendingBytes = 0;
  private sampleCount = 0;
  private starvedSince = 0;
  private lastSeekAt = 0;
  private seekBurst = 0;
  private seeking = false;
  /** 每条轨道的运行时平均码率，来自实际输出片段。 */
  private readonly trackBitrates = new Map<number, number>();

  get target() { return this.ahead; }
  get retention() { return this.behind; }
  get predictEnabled() { return !this.seeking && this.ahead >= 3; }

  /** 设置媒体消费速率（bit/s）。传 null 表示元数据没给，改用运行时估计。 */
  setBitrate(bitrates: Array<number | null>) {
    this.trackBitrates.clear();
    bitrates.forEach((value, index) => { if (value && value > 0) this.trackBitrates.set(index, value); });
  }

  /** 用重封装后真实交付的媒体片段更新每轨消费速率，不为估码率额外扫描源文件。 */
  noteMediaRate(track: number, bytes: number, mediaSeconds: number) {
    if (bytes <= 0 || mediaSeconds <= 0) return;
    const measured = bytes * 8 / mediaSeconds;
    if (!Number.isFinite(measured) || measured <= 0) return;
    const previous = this.trackBitrates.get(track);
    this.trackBitrates.set(track, previous ? previous * 0.65 + measured * 0.35 : measured);
  }

  /** 记录一次 seek。短时间内多次即视为乱拖。 */
  noteSeek(now: number) { this.lastSeekAt = now; this.seekBurst++; }

  /**
   * 记录一次有效交付（块已解密并进入缓存）。
   *
   * 只统计"认证后交付"的字节：重试、被取消的请求、被抢占的预取都不算，
   * 否则测出来的吞吐会虚高，缓冲策略会误判成网络很好。
   */
  noteDelivered(bytes: number, now: number) {
    if (bytes <= 0) return;
    if (this.lastSampleAt === undefined) this.lastSampleAt = now;
    this.pendingBytes += bytes;
    const elapsed = now - this.lastSampleAt;
    if (elapsed < 250) return; // 太快，样本间隔太短噪声大
    const rate = this.pendingBytes * 8 / (elapsed / 1000);
    this.throughput = this.throughput
      ? this.throughput * (1 - THROUGHPUT_SMOOTHING) + rate * THROUGHPUT_SMOOTHING
      : rate;
    this.lastSampleAt = now;
    this.pendingBytes = 0;
    this.sampleCount++;
  }

  /** 按当前播放状态重新计算全部缓冲参数。 */
  retune(buffered: BufferedRange, state: BufferState, now: number) {
    const stock = this.stockOf(buffered, state.currentTime);
    const starved = stock < 1;
    this.seeking = now - this.lastSeekAt < 10_000 && this.seekBurst >= 3;
    if (!this.seeking && now - this.lastSeekAt >= 10_000) this.seekBurst = 0;
    if (starved) this.starvedSince = this.starvedSince || now;
    else this.starvedSince = 0;

    const health = this.health();
    // 目标 = 库存 + 库存能撑多久的余量。撑得久就多铺一点，撑不久就别白费流量。
    const runway = health >= HEALTH.high ? TARGET.max * 0.6
      : health >= HEALTH.low ? TARGET.max * 0.25
        : 0;
    let cap = TARGET.min + runway;
    if (!state.started) cap = Math.min(cap, 3);           // 起播阶段先出画面
    if (state.paused) cap = Math.min(cap, 5);             // 暂停不必继续铺
    if (this.seeking) cap = Math.min(cap, 4);             // 乱拖期间只保当前
    if (starved && now - this.starvedSince > 3_000) cap = Math.min(cap, 6);
    // 收紧立即生效，放开每步只 +2 秒。
    this.ahead = this.ahead > cap ? cap : Math.min(cap, this.ahead + 2);
    this.behind = health < HEALTH.low ? 6 : 20;
  }

  /** 播放头之后已经连续缓冲了多少秒。 */
  private stockOf(buffered: BufferedRange, currentTime: number): number {
    let stock = 0;
    for (let i = 0; i < buffered.length; i++) {
      const start = buffered.start(i), end = buffered.end(i);
      // 只计算覆盖播放头的连续库存。seek 后远处的离散缓冲段不能代表当前位置有数据。
      if (start <= currentTime + 0.1 && end > currentTime) stock = Math.max(stock, end - currentTime);
    }
    return stock;
  }

  /**
   * 健康度 R = 有效吞吐 / 媒体消费速率。
   *
   * 吞吐还没测出来时返回中性值 1：既不乐观铺开，也不先入为主地收缩，
   * 等第一批交付到了自然会修正。样本不足时同样按 1 处理。
   */
  private health(): number {
    // 多轨媒体的消费速率是所有轨道之和；片段统计尚未到达前使用保守先验。
    const consumption = this.trackBitrates.size
      ? [...this.trackBitrates.values()].reduce((sum, value) => sum + value, 0)
      : ASSUMED_BITRATE;
    if (!this.throughput || this.sampleCount < MIN_SAMPLES) return 1;
    const effective = Math.max(this.throughput, MIN_PLAUSIBLE_THROUGHPUT);
    return effective / consumption;
  }

  /** 播放倍率影响时间与字节的换算：倍速快时同样的秒数要更快补齐。 */
  scaleByRate(playbackRate: number) {
    return Math.max(1, Math.min(4, Math.abs(playbackRate)));
  }

  /**
   * 内存压力（SourceBuffer 配额异常）时压到最小。
   *
   * 配额异常说明已经无处可放，此时任何前瞻都是加剧问题，连 warmup 之上的
   * 放宽机会都不给，等下一次 retune 自己爬回来。
   */
  applyMemoryPressure() {
    this.ahead = TARGET.min;
    this.behind = 6;
  }

  /**
   * 导出成调度策略。
   *
   * 这是缓冲与调度的接缝：预取深度跟着健康度走——网络吃紧或刚拖动完时收到 0，
   * 让新位置的关键请求立刻拿到槽位。并发只按网络档位分三档：快网才值得开满，
   * 慢网减少连接数反而让流水线更浅（瓶颈是带宽不是连接）。
   */
  policy(overrides: Partial<ReadPolicy> = {}): ReadPolicy {
    const health = this.health();
    const starved = this.starvedSince > 0;
    const fast = health >= HEALTH.high;
    return {
      maxConcurrent: fast ? 8 : health >= HEALTH.low ? 4 : 2,
      prefetchBlocks: !this.seeking && !starved && (fast || health >= HEALTH.low) ? (fast ? 8 : 4) : this.seeking ? 2 : 0,
      cacheBytes: 32 * 1024 * 1024,
      ...overrides,
    };
  }
}
