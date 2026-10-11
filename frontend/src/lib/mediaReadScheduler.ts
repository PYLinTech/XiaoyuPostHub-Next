import { MediaSourceError } from "./mediaRangeSource";
import type { ReadPolicy } from "./mediaPreviewProtocol";

/**
 * 读取调度器。
 *
 * 职责边界：只决定"谁先走、谁被取消"，不碰密文、不碰认证、不碰缓存内容。
 * 密文映射与 GCM 认证留在 MediaRangeSource，明文块缓存由它持有。
 *
 * ## 最重要的一条约束
 *
 * **取消一个读者不能误取消其他读者正在共享的认证块请求。**
 *
 * 这是这里最容易写错的地方。同一块可能被多个消费者等待：播放轨道在读，
 * 时长探测也在读，或者两条轨道恰好要同一块。取消其中一个读者时，块请求本身
 * 必须继续跑——否则另一个读者的读取会莫名其妙地失败，而它的signal 并没有
 * 被取消过。
 *
 * 实现方式是把"块请求"与"读者"分成两层：
 *
 *   消费者（reader）  各自持有自己的 AbortSignal，互不干扰
 *        ↓ 引用计数
 *   块请求（in-flight）只在最后一个消费者离开时才允许取消
 *
 * 所以取消走的是"读者退出"路径，不是"取消请求"路径：读者离开时先看还有没有
 * 别人，没有了才abort 底层请求，而且这个 abort 也只针对那一块的请求组。
 */

export type ReadPriority = "critical" | "playback" | "prefetch";

/** 读上下文。signal 属于调用方，调度器只据此判断"这个读者还在不在"。 */
export interface ReadContext {
  generation: number;
  priority: ReadPriority;
  signal: AbortSignal;
}

const DEFAULT_POLICY: ReadPolicy = { maxConcurrent: 8, prefetchBlocks: 8, cacheBytes: 32 * 1024 * 1024 };

/** 优先级排序用：小的先走。 */
const ORDER: Record<ReadPriority, number> = { critical: 0, playback: 1, prefetch: 2 };

/** seek 后仍保留旧位置预取的代际数。给"来回小幅拖动"留缓存命中。 */
const STALE_GENERATION_KEEP = 1;

/**
 * 一个消费者的登记。每个 readBlock 调用产生一个。
 *
 * 消费者级别的 signal 是关键：它让"这个读者不要了"这件事只影响它自己，
 * 而不是顺着 promise 链把共享的块请求一起掐掉。
 */
interface Reader {
  index: number;
  priority: ReadPriority;
  generation: number;
  /** 调用方的 signal。abort 时只摘掉这一个读者。 */
  signal: AbortSignal;
  /** 从请求上摘掉自己，不影响其他读者。 */
  detach(): void;
  /** 拿到结果：成功给数据，失败给原因。 */
  settle(value: Uint8Array | undefined, error?: Error): void;
}

/**
 * 一个正在进行的块请求。可以被多个 Reader 共享。
 */
interface InFlight {
  index: number;
  /** 当前最高优先级（多个读者取最大）。 */
  priority: ReadPriority;
  /** 当前最新代际。旧代际的读者退出不影响它被新代际复用。 */
  generation: number;
  /** 共享这个请求的读者。归零才允许取消。 */
  readers: Set<Reader>;
  /** 整组请求的取消器：只在本块彻底无人需要时abort。 */
  abort?: AbortController;
  /** 排队等槽位时的等待项。 */
  waiter?: Waiter;
  settled: boolean;
  /** 结算结果：settle 时写入，供之后挂上来的读者读取。 */
  result?: Uint8Array;
  error?: Error;
}

/** 排队等槽位的等待项。与 InFlight 分开，避免用类型强转把两者混在一个数组里。 */
interface Waiter {
  request: InFlight;
  grant(): void;
  cancel(reason: unknown): void;
}

/** 底层取数回调：由 MediaRangeSource 提供，负责密文取数与认证。 */
export type FetchRange = (
  indices: number[],
  signal: AbortSignal,
  publish: (index: number, bytes: Uint8Array) => void,
) => Promise<void>;

export interface MediaReadSchedulerApi {
  readBlock(blockIndex: number, context: ReadContext): Promise<Uint8Array>;
  seek(nextGeneration: number): void;
  updatePolicy(policy: Partial<ReadPolicy>): void;
  dispose(): void;
}

export class MediaReadScheduler implements MediaReadSchedulerApi {
  /** 正在进行的块请求，按块序号索引。这是去重的唯一依据。 */
  private inFlight = new Map<number, InFlight>();
  private waiters: Waiter[] = [];
  private active = 0;
  private generation = 0;
  private policy: ReadPolicy = { ...DEFAULT_POLICY };
  private disposed = false;

  /**
   * @param cache 缓存读写交给上层——调度器不该持有明文。
   * @param fetch 真正取数与认证。由 MediaRangeSource 提供。
   * @param batch 一次请求覆盖几个块。由上层按 BATCH_BYTES 决定。
   */
  constructor(
    private cache: {
      get(index: number): Uint8Array | undefined;
      touch(index: number): void;
    },
    private fetch: FetchRange,
    private batch: () => number,
    private blockCount: () => number,
    /** 块交付回调，参数是已认证的明文字节数。缓冲控制器靠它测真实吞吐。 */
    private onDelivered: (bytes: number) => void = () => {},
  ) {}

  get currentGeneration() { return this.generation; }
  get policySnapshot() { return { ...this.policy }; }

  /**
   * 读取一个块。
   *
   * 四条路径，命中哪条都不影响语义：
   *  1. 缓存命中 → 立即返回，零网络（回拖到刚播过的地方就走这条）；
   *  2. 已有同块请求 → **加入**为共享读者，不重复发起；
   *  3. 没有 → 登记读者并发起请求；
   *  4. 调用方已取消 → 立即 reject，不留下任何登记。
   */
  readBlock(index: number, context: ReadContext): Promise<Uint8Array> {
    if (this.disposed) return Promise.reject(new MediaSourceError("读取会话已结束", "network", false));
    if (context.signal.aborted) return Promise.reject(context.signal.reason ?? new DOMException("已取消", "AbortError"));

    const cached = this.cache.get(index);
    if (cached) { this.cache.touch(index); return Promise.resolve(cached); }

    // 去重：复用同块的在途请求，成为它的又一个消费者。
    let request = this.inFlight.get(index);
    if (!request) {
      request = { index, priority: context.priority, generation: context.generation, readers: new Set(), settled: false };
      this.inFlight.set(index, request);
      this.launch(request);
    } else {
      // 提级：更高的优先级与更新的代际都要吸收，否则新位置的关键读取会排在
      // 旧位置的预取后面。
      if (ORDER[context.priority] < ORDER[request.priority]) {
        request.priority = context.priority;
        this.resort();
      }
      if (context.generation > request.generation) request.generation = context.generation;
    }
    return this.attach(request, context);
  }

  /**
   * 挂一个读者到在途请求上，并保证调用方的取消只影响它自己。
   *
   * 这里是"取消不能误伤共享请求"的落点：signal 的 abort 回调只把这个 reader 从
   * 集合里摘掉；只有 readers 归零时才abort 底层请求。另一个读者——比如同时在读的
   * 另一条轨道，或时长探测——完全不受影响。
   */
  private attach(request: InFlight, context: ReadContext): Promise<Uint8Array> {
    return new Promise<Uint8Array>((resolve, reject) => {
      const onAbort = () => {
        // 只摘掉自己。别人还在等，这个块请求必须继续跑。
        request.readers.delete(reader);
        const reason = context.signal.reason ?? new DOMException("已取消", "AbortError");
        this.maybeCancel(request, reason);
        reject(reason);
      };
      const reader: Reader = {
        index: request.index, priority: context.priority, generation: context.generation,
        signal: context.signal,
        detach: () => {
          context.signal.removeEventListener("abort", onAbort);
          request.readers.delete(reader);
        },
        settle: (value, error) => {
          context.signal.removeEventListener("abort", onAbort);
          if (error !== undefined) reject(error); else resolve(value!);
        },
      };
      request.readers.add(reader);
      context.signal.addEventListener("abort", onAbort, { once: true });
      // 两个"已经结束了"的检查都要做，缺一个这个 promise 就永远不结算：
      //   - settled：请求在我们挂上之前就已结算（本地缓存等同步路径）；
      //   - aborted：signal 在挂上之前就已中止（launch 里的 awaitacquireSlot 是
      //     异步的，调用方完全可能在它挂上监听之前就 abort）。后者是实测踩到的：
      //     abort 之后 read 永不结算，连接与内存一起泄漏。
      if (request.settled) { request.readers.delete(reader); reader.settle(request.result, request.error); }
      else if (context.signal.aborted) onAbort();
    });
  }

  /**
   * 无人需要时才真正取消。注意只在 readers 归零时触发。
   *
   * 取消分两步，顺序不能反：先 settle（告诉调用方"没了"），再 abort（掐掉网络）。
   * 反过来的话，abort 引发的那条异步异常链要绕一圈才回到 settle，而调用方可能
   * 在那之前就等下去——实测表现为 abort 后 read 永不结算。同步把结果送到手上，
   * 才不依赖异常链的时序。
   */
  private maybeCancel(request: InFlight, reason?: unknown) {
    if (request.readers.size || request.settled) return;
    this.inFlight.delete(request.index);
    const failure = reason ?? new MediaSourceError("预取已被新的播放位置取代", "network", false);
    this.settle(request, undefined, failure);
    const batch = request.abort;
    if (!batch) { request.waiter?.cancel(failure); return; }
    // 一个 Range 批次里的兄弟块共享底层请求。单块无人需要时不能连带取消仍有读者
    // 的兄弟块；只有整批都无人等待时才取消网络请求和并发队列项。
    const batchStillNeeded = [...this.inFlight.values()].some(entry =>
      entry.abort === batch && !entry.settled && entry.readers.size > 0);
    if (!batchStillNeeded) {
      request.waiter?.cancel(failure);
      batch.abort();
    }
  }

  /**
   * 发起一个块请求（必要时连同相邻块合成一批）。
   *
   * 批次大小由上层按 BATCH_BYTES 决定；这里负责去重相邻、申请槽位、串起生命周期。
   * 整批共享一个取消器，但**每个兄弟块各自独立结算**——它们共享一次往返，却各有各的
   * 读者生命周期。
   */
  private launch(request: InFlight) {
    const { indices, entries } = this.plannedBatch(request.index, request.priority, request.generation);
    const controller = new AbortController();
    for (const entry of entries) entry.abort = controller;
    const run = async () => {
      try {
        const delivered = new Set<number>();
        const publish = (index: number, value: Uint8Array) => {
          const entry = entries.find(item => item.index === index);
          if (!entry || entry.settled) return;
          delivered.add(index);
          this.onDelivered(value.byteLength);
          this.settle(entry, value, undefined);
        };
        // 已认证的块可以先交付；最后一块由上层在响应长度核验后发布。
        await this.fetch(indices, controller.signal, publish);
        for (const entry of entries) {
          if (delivered.has(entry.index)) continue;
          const value = this.cache.get(entry.index);
          this.settle(entry, value, value ? undefined : new MediaSourceError("块数据缺失", "range"));
        }
      } catch (error) {
        const failure = error instanceof Error ? error : new Error(String(error));
        for (const entry of entries) if (!entry.settled) this.settle(entry, undefined, failure);
      } finally { this.releaseSlot(); }
    };
    void this.acquireSlot(request, controller.signal).then(run, error => {
      for (const entry of entries) this.settle(entry, undefined, error);
    });
  }

  /**
   * 这一批要覆盖哪些块：从 index 起连续若干块，跳过已在途或在缓存里的。
   *
   * 兄弟块会各自登记为 InFlight——它们和发起块共享同一次网络请求，但必须有
   * 独立的结算通道，否则等待它们的人会永远悬着。它们的 abort 指向同一个
   * controller：这一批整体无人需要时一起取消，而只要还有任一兄弟块有读者，
   * 控制器就不会被 abort（见 maybeCancel，它按块独立判断）。
   */
  private plannedBatch(index: number, priority: ReadPriority, generation: number): { indices: number[]; entries: InFlight[] } {
    const count = Math.max(1, this.batch());
    const total = this.blockCount();
    const indices: number[] = [index];
    const entries: InFlight[] = [];
    for (let i = index; i < Math.min(total, index + count); i++) {
      const known = this.inFlight.get(i);
      if (known) {
        // 已有独立的在途请求：不并入本批，它有自己的一次往返。
        if (i !== index && known.abort && !known.settled) break;
        if (i === index) entries.push(known);
        continue;
      }
      const entry: InFlight = { index: i, priority, generation, readers: new Set(), settled: false };
      this.inFlight.set(i, entry);
      entries.push(entry);
      if (i !== index) indices.push(i);
    }
    return { indices, entries };
  }

  /**
   * 结算一个块请求：把结果或错误发给它的所有读者，并从登记表移除。
   *
   * 读者是在 attach 里各自持有 resolve/reject 的，所以这里只需把结果写进 request，
   * 再让每个读者去取——中途新挂上来的读者也能读到（见 attach 末尾的 settled 检查）。
   */
  private settle(request: InFlight, value: Uint8Array | undefined, error: unknown) {
    if (request.settled) return;
    request.settled = true;
    request.result = value;
    request.error = error instanceof Error ? error : undefined;
    if (this.inFlight.get(request.index) === request) this.inFlight.delete(request.index);
    // 通知所有读者结算。各自 reject/resolve，然后检查是否该取消底层请求。
    const readers = [...request.readers];
    for (const reader of readers) {
      reader.settle(value, error instanceof Error ? error : undefined);
      request.readers.delete(reader);
    }
  }

  /** 申请并发槽位。满了就排队，按优先级与代际排序。 */
  private async acquireSlot(request: InFlight, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted();
    if (this.active < this.policy.maxConcurrent) { this.active++; return; }
    await new Promise<void>((resolve, reject) => {
      const waiter: Waiter = {
        request,
        grant: () => { cleanup(); this.active++; resolve(); },
        cancel: (reason) => { cleanup(); reject(reason); },
      };
      const cleanup = () => {
        signal.removeEventListener("abort", onAbort);
        const i = this.waiters.indexOf(waiter);
        if (i >= 0) this.waiters.splice(i, 1);
        if (request.waiter === waiter) request.waiter = undefined;
      };
      const onAbort = () => waiter.cancel(signal.reason);
      request.waiter = waiter;
      this.waiters.push(waiter);
      this.sortQueue();
      signal.addEventListener("abort", onAbort, { once: true });
    });
  }

  private sortQueue() {
    this.waiters.sort((a, b) =>
      ORDER[a.request.priority] - ORDER[b.request.priority]
      || b.request.generation - a.request.generation);
  }
  private resort() { this.sortQueue(); }

  /**
   * 切换播放代际。
   *
   * 已完成解密的块不清除——它们在缓存里，回拖时直接命中。
   * 旧位置的**无主预取**取消：有别的读者（其他轨道、时长探测）就留着。
   * 这一点就是"取消不能误伤共享请求"在 seek 场景的体现。
   */
  seek(nextGeneration: number) {
    if (nextGeneration <= this.generation) return;
    this.generation = nextGeneration;
    const keep = nextGeneration - STALE_GENERATION_KEEP;
    for (const [, request] of this.inFlight) {
      if (request.generation >= keep) continue;
      // 还有读者就不动：哪怕是旧代际的，另一条轨道可能正在等它。
      if (request.readers.size) continue;
      if (request.priority === "critical" || request.priority === "playback") continue;
      this.maybeCancel(request);
    }
  }

  updatePolicy(policy: Partial<ReadPolicy>) {
    this.policy = { ...this.policy, ...policy };
    // 并发调大后立刻放行队列里等着的请求，不必等下一批自然结束。
    while (this.active < this.policy.maxConcurrent && this.waiters.length) {
      this.waiters.shift()?.grant();
    }
  }

  /** 释放槽位并唤醒队列里优先级最高的一个。 */
  private releaseSlot() {
    this.active--;
    this.sortQueue();
    if (this.active < this.policy.maxConcurrent) this.waiters.shift()?.grant();
  }

  dispose() {
    this.disposed = true;
    const reason = new MediaSourceError("读取会话已结束", "network", false);
    for (const [, request] of this.inFlight) {
      request.abort?.abort();
      for (const reader of [...request.readers]) reader.detach();
      this.settle(request, undefined, reason);
    }
    this.inFlight.clear();
    this.waiters = [];
    this.active = 0;
  }

}
