import { ALL_FORMATS, CustomSource, Input } from "mediabunny";
import { blockCipherLen, blockCipherOffset, decryptBlock, HEADER_SIZE, importContentKey, MAX_BLOCK_LOG2, MIN_BLOCK_LOG2, parseXphHeader, TAG_SIZE, XphFormatError } from "../crypto/xph";
import type { XphHeader } from "../crypto/xph";
import { cipherPartRanges } from "../delivery/cipherParts";
import type { CipherPart } from "../delivery/cipherParts";
import { MediaReadScheduler } from "./mediaReadScheduler";
import type { ReadPriority } from "./mediaReadScheduler";
import type { ReadPolicy } from "./mediaPreviewProtocol";

export interface MediaCipherSource { url: string; parts?: CipherPart[]; key: Uint8Array; plainSize: number; cipherSize: number }
import { FULL_PREVIEW_LIMIT } from "./mediaPreviewLimits";
export { FULL_PREVIEW_LIMIT } from "./mediaPreviewLimits";
const CACHE_BYTES = 32 * 1024 * 1024;
const MAX_WARMUP_BYTES = 4 * 1024 * 1024;
const MAX_PREFETCH_BYTES = 8 * 1024 * 1024;
/** 单次请求的目标字节数，取 1 MiB 与默认加密块对齐。 */
const BATCH_BYTES = 1024 * 1024;
/**
 * 预热一次最多取几块。
 *
 * 一块太少——解析器紧接着还要读 moov 与首个采样，又是一轮往返；四块够覆盖这段，
 * 又有 4 MiB 上限兜着（块本身很大时不至于把用不上的字节拖回来）。
 */
const MAX_WARMUP_BLOCKS = 4;
/**
 * 顺序性观察器。
 *
 * 解析器对同一个输入有两种截然不同的读法：探测时会跳着读（找moov、找索引），
 * 播放时则一路往后读。给探测开大预取窗口等于为不会用到的字节付带宽和延迟——
 * 实测 mediabunny 的探测跨度常常只有几百字节，预取窗口再大也只是把无关块拉下来。
 *
 * 所以从"按需读取"起步：连续读到第 threshold 次、且每次都紧挨上一次的位置，
 * 就认定这是顺序播放，才把预取窗口打开。跳着读则始终保持按需。
 */
class SequentialProbe {
  private lastEnd = -1;
  private consecutive = 0;
  /** 连续几次紧邻读取后判定为顺序消费。 */
  constructor(private threshold = 3) {}
  /** 记录一次读取，返回此刻是否应当预取。 */
  observe(start: number, end: number): boolean {
    // 紧邻上一次结束位置，且没有往回跳——这就是顺序。
    if (start === this.lastEnd) this.consecutive++; else this.consecutive = 0;
    this.lastEnd = end;
    return this.consecutive >= this.threshold;
  }
}
export class MediaSourceError extends Error {
  constructor(message: string, public readonly code: "auth" | "range" | "network" | "size", /** 能否靠上层重连或重新授权自愈，见 RecoveryPlan。 */
    public readonly transient = false) { super(message); this.name = "MediaSourceError"; }
}
/**
 * 三层恢复能力的判定依据。
 *
 * 底层 fetchBlocks 已经把"单个 Range 请求失败"重试满额（2 次 + 退避），所以它
 * 抛出来的 network 已经不是单请求级别的小抖动，而是"整段网络都断了"或"连不上海"。
 * 上层据此分三档，而不是一律终止播放：
 *
 *  - transient 网络错 → 重连即可，换个时间重试同一个 URL，不动票据；
 *  - auth 错         → 票据过期/直链失效，必须回后端重新 plan() 换新的 URL 与 key；
 *  - range/格式/大小 → 重试多少次都一样，只能终止。
 *
 * `transient` 在构造时就带上，避免上层再按 message 或错误码反推语义。
 */
export function isRecoverable(reason: unknown): { reconnect: boolean; replan: boolean } {
  const code = reason && typeof reason === "object" && "code" in reason ? reason.code : undefined;
  const transient = reason && typeof reason === "object" && "transient" in reason ? reason.transient : undefined;
  if (code === "auth") return { reconnect: false, replan: true };
  if (code === "network" && transient === false) return { reconnect: false, replan: false };
  if (code === "network") return { reconnect: true, replan: false };
  return { reconnect: false, replan: false };
}

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
  private cacheLimit = CACHE_BYTES;
  private rangeUnsupported = false;
  /** open() 时预热并已认证入缓存的块数。 */
  private warmBlocks = 0;
  /** open() 预热块数上限，由构造参数决定。 */
  private warmupLimit = MAX_WARMUP_BLOCKS;
  /**
   * 读调度器：请求优先级、并发槽位、代际与seek 取消都在它那里。
   * 本类只保留密文映射、块认证与明文 LRU 缓存。
   */
  private scheduler!: MediaReadScheduler;
  /**
   * @param onDelivered 已认证交付的明文字节回调。缓冲控制器靠它测真实吞吐——
   *   取数发生在 worker 线程，主线程拿不到本地状态，只能靠这个通道回传。
   */
  constructor(
    private descriptor: MediaCipherSource,
    private onDelivered: (bytes: number) => void = () => {},
    /** open() 预热几块。0 表示只读文件头、不预取——弱网或超大文件上可关掉。 */
    warmupBlocks = MAX_WARMUP_BLOCKS,
  ) { this.warmupLimit = warmupBlocks; }
  get size() { return this.header.plainSize; }

  /**
   * 动态调整调度参数（并发、预取视野、缓存上限）。
   *
   * 由缓冲控制器驱动：网络吃力或连续 seek 时预取视野收到 0，新位置的关键请求
   * 才能立刻拿到槽位。
   */
  updatePolicy(policy: Partial<ReadPolicy>) {
    if (policy.cacheBytes !== undefined && Number.isFinite(policy.cacheBytes)) {
      this.cacheLimit = Math.max(1024 * 1024, Math.min(128 * 1024 * 1024, Math.floor(policy.cacheBytes)));
      this.evictCache();
    }
    this.scheduler.updatePolicy(policy);
  }

  async open(signal: AbortSignal): Promise<void> {
    this.signal = signal;
    signal.throwIfAborted();
    this.key = await importContentKey(this.descriptor.key);
    this.descriptor.key.fill(0);
    // 交付计划里带着 plainSize 与 cipherSize，足以在发出第一个请求之前就反推出
    // 块大小与块数：cipherSize = HEADER + plainSize + blockCount × 16。发请求前
    // 先算清要覆盖哪些块，才能把 Header 与首块合并进一次 Range——那正是要省掉的
    // 一次串行往返。算不出来（计划与实际不符）就退回只读 Header 的老路，
    // 校验仍在 parseXphHeader 之后照做，不因为图快而跳过。
    const plan = this.plannedLayout();
    // 多拉几块而不是一块：解析器通常紧接着要读 moov 与首个采样，正好落在同一段里。
    // 上限 4 MiB，避免块本身很大时把不必要的字节也拖回来。
    // warmupLimit 由调用方指定（0 = 只读文件头）。默认 4 块：够覆盖解析器
    // 紧接着要读的 moov 与首采样，又不至于在弱设备上白拉字节。
    const byteBound = plan ? Math.floor(MAX_WARMUP_BYTES / plan.blockSize) : 0;
    const warmBlocks = Math.min(plan?.blockCount ?? 0, Math.max(0, this.warmupLimit), byteBound);
    const warmEnd = plan && warmBlocks > 0
      ? Math.min(plan.cipherSize, blockCipherOffset(plan, warmBlocks - 1) + blockCipherLen(plan, warmBlocks - 1))
      : HEADER_SIZE;
    const warm = new Uint8Array(warmEnd);
    for (let attempt = 0; ; attempt++) {
      try {
        let offset = 0;
        for await (const bytes of this.cipher(0, warmEnd)) {
          if (offset + bytes.length > warm.byteLength) break;
          warm.set(bytes, offset); offset += bytes.length;
        }
        if (offset < HEADER_SIZE) throw new XphFormatError("媒体文件头被截断");
        break;
      } catch (error) {
        signal.throwIfAborted();
        if (attempt >= 2 || !(error instanceof MediaSourceError && error.code === "network")) throw error;
        await this.delay(150 * 2 ** attempt);
      }
    }
    // 先解析并核对 Header，再考虑缓存里的预热块——顺序不能反：
    // 块 0 的认证要用 Header 里的 nonce 前缀与 AAD，Header 不可信时它没有意义。
    this.header = parseXphHeader(warm.subarray(0, HEADER_SIZE));
    if (this.header.plainSize !== this.descriptor.plainSize || this.header.cipherSize !== this.descriptor.cipherSize) {
      throw new XphFormatError("媒体文件头与交付计划不一致");
    }
    if (plan && (plan.blockSize !== this.header.blockSize || plan.blockCount !== this.header.blockCount)) {
      // 计划反推的布局与文件头不一致：多拉的字节没有意义，清掉重来。
      this.warmBlocks = 0;
    } else {
      this.warmBlocks = Math.min(warmBlocks, this.header.blockCount);
      await this.authenticateWarmup(warm, signal);
    }
    signal.throwIfAborted();
    // scheduler 要在 header 解析后才能创建（批次大小依赖 blockSize），此时再注入
    // 缓存视图与取数回调。缓存上限通过 policy 交给调度器，预取深度随缓冲动态调整。
    this.scheduler = new MediaReadScheduler(
      {
        get: (index) => {
          const value = this.cache.get(index);
          // 命中即提为最近使用：回拖到刚播过的地方要能命中 LRU。
          if (value) { this.cache.delete(index); this.cache.set(index, value); }
          return value;
        },
        touch: () => {},
      },
      (indices, signal, publish) => this.fetchBlocks(indices, signal, publish),
      () => this.batchCount(true),
      () => this.header.blockCount,
      this.onDelivered,
    );
    signal.addEventListener("abort", () => { this.cache.clear(); this.cachedBytes = 0; this.scheduler.dispose(); }, { once: true });
  }

  /**
   * 从交付计划反推块布局。推不出来返回 null，调用方退回只读 Header。
   *
   * cipherSize = HEADER_SIZE + plainSize + blockCount × TAG_SIZE，且 blockCount 是
   * plainSize / blockSize 的上取整。blockSize 限定在格式允许的 2^9..2^26，所以候选
   * 只有 18 个，逐一验算即可，无需信任任何未验证的字段。
   */
  private plannedLayout(): XphHeader | null {
    const { plainSize, cipherSize } = this.descriptor;
    if (!Number.isSafeInteger(plainSize) || !Number.isSafeInteger(cipherSize) || plainSize <= 0) return null;
    for (let log2 = MIN_BLOCK_LOG2; log2 <= MAX_BLOCK_LOG2; log2++) {
      const blockSize = 2 ** log2;
      const blockCount = Math.ceil(plainSize / blockSize);
      if (HEADER_SIZE + plainSize + blockCount * TAG_SIZE !== cipherSize) continue;
      // 造一个只用于算偏移的壳：offset/len 只依赖 blockSize 与 blockCount，
      // 真正的认证数据（key、aad、nonce）要到 parseXphHeader 之后才有。
      return { raw: new Uint8Array(0), aad: new Uint8Array(0), algo: 1, blockLog2: log2,
        blockSize, plainSize, noncePrefix: 0, blockCount, cipherSize } as unknown as XphHeader;
    }
    return null;
  }

  /**
   * 认证并缓存预热块。
   *
   * 每块都走完整的 GCM 认证后才进缓存——认证失败的块直接抛，绝不缓存半明文。
   * 这一步只是把"本来马上要发生的解密"提前到 open 里做，省的是网络往返，
   * 不是省认证；没省到的那部分由失败重试照常兜底。
   */
  private async authenticateWarmup(warm: Uint8Array, signal: AbortSignal) {
    for (let index = 0; index < this.warmBlocks; index++) {
      const offset = blockCipherOffset(this.header, index), length = blockCipherLen(this.header, index);
      if (offset + length > warm.byteLength) break;
      const plain = await decryptBlock(this.key, this.header, index, warm.slice(offset, offset + length));
      signal.throwIfAborted();
      this.remember(index, plain);
    }
  }

  input(signal: AbortSignal, readBudget = Infinity): Input {
    const touched = new Set<number>(); let count = 0;
    const probe = new SequentialProbe();
    // 有预算的输入一定是探测：直接按需读，不给预取留任何余地。
    const isProbe = Number.isFinite(readBudget);
    return new Input({ formats: ALL_FORMATS, source: new CustomSource({
      getSize: () => this.size,
      read: (start, end) => {
        signal.throwIfAborted();
        if (isProbe) {
          for (let i = Math.floor(start / this.header.blockSize); i * this.header.blockSize < end; i++) {
            if (!touched.has(i)) { touched.add(i); count += blockCipherLen(this.header, i); }
          }
          if (count > readBudget) throw new Error("媒体元数据探测达到读取上限");
          // lookahead 关闭：不排跨批预取，探测读完即止。
          return this.stream(start, end, signal, false, false);
        }
        // 播放输入：观察到顺序消费才开预取，否则按需。
        const sequential = probe.observe(start, end);
        return this.stream(start, end, signal, sequential, sequential);
      },
      // 小缓存仅供解析器回看；认证块缓存与网络并发只在本类管理。
      maxCacheSize: 1024 * 1024, prefetchProfile: "none", handleUnhandledError: () => {},
    }) });
  }

  /**
   * `lookahead` 表示消费方会持续往后读（解复用播放），可以安全地把下一批也排在途；
   * 一次性有明确终点的读取（`read()`、元数据探测）不排——那些块本来就不会被消费，
   * 提前取回只是白花一次请求。
   *
   * 每次调用视为一次新的顺序消费，即开启新代际：mediabunny 的 demuxer 随 seek
   * 重建读路径，而 seek 恰好就是上一轮顺序消费结束、新一轮开始，所以这里是
   * 判定"换位置了"的唯一可靠信号。有限区间读取（lookahead=false）不换代际，
   * 否则时长探测会误伤正在播放的预取。
   */
  private stream(start: number, end: number, signal: AbortSignal, prefetch = true, lookahead = true) {
    let position = start, cancelled = false;
    const readerLifetime = new AbortController();
    const readerSignal = AbortSignal.any([signal, readerLifetime.signal]);
    // 每次顺序消费即新代际：mediabunny 的 demuxer 随 seek 重建读路径，
    // 而 seek 恰好就是上一轮顺序消费结束、新一轮开始。有限区间读取不换代际，
    // 否则时长探测会误伤正在播放的预取。
    if (lookahead) this.scheduler.seek(this.scheduler.currentGeneration + 1);
    const generation = this.scheduler.currentGeneration;
    return new ReadableStream<Uint8Array>({
      pull: async output => {
        try {
          signal.throwIfAborted();
          if (position >= end) { output.close(); return; }
          const index = Math.floor(position / this.header.blockSize);
          const current = this.block(index, prefetch, "critical", generation, readerSignal);
          // 预取窗口按块推进：消费消耗完当前块时，后续请求早已在途，
          // 才能把网络往返藏在消费背后。深度由缓冲控制器经 policy 给出，
          // 与批次大小解耦——批怎么调，预取视野都是 PREFETCH_BLOCKS。
          if (prefetch && lookahead && this.scheduler.policySnapshot.prefetchBlocks > 0) {
            // 深度以块为单位但预算以字节为单位；大加密块时禁止意外预取数百 MiB。
            const depth = Math.min(this.scheduler.policySnapshot.prefetchBlocks,
              Math.floor(MAX_PREFETCH_BYTES / this.header.blockSize));
            for (let i = index + 1; i <= index + depth; i++) {
              if (i >= this.header.blockCount) break;
              void this.block(i, true, "prefetch", generation, readerSignal).catch(() => {});
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
      }, cancel: reason => {
        cancelled = true;
        // 读者离开当前输入流时马上从共享块请求中摘除，seek 后不要继续占网络槽位。
        readerLifetime.abort(reason ?? new DOMException("读取已取消", "AbortError"));
      },
    }, { highWaterMark: 0 });
  }

  async read(start: number, end: number, signal: AbortSignal): Promise<Uint8Array> {
    if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end > this.size || end <= start) throw new Error("媒体读取区间无效");
    // 有限区间：终点明确，不排跨批预取——区间之后的块不会被这次读取消费。
    const reader = this.stream(start, end, signal, true, false).getReader();
    const result = new Uint8Array(end - start); let offset = 0;
    // 取消兜底：signal 中止时让读取立刻结束，不依赖底层请求链路把错误送上来。
    // 那条链路要绕调度器与流两层，实测存在挂住不结算的路径——调用方等的是一个
    // 永远不会到来的结果，连接与内存都跟着泄漏。这里直接以竞态参与，
    // 无论哪边先到，调用方都能拿到确定结论。
    // 注意：这里**不能**顺手 cancel 读流——那会让下面的竞速立刻以"已读完"
    // 收场，把一次被取消的读取伪装成成功的空结果。取消只由 signal 触发。
    let onAbort!: () => void;
    const cancelled = new Promise<never>((_, reject) => {
      onAbort = () => reject(signal.reason ?? new DOMException("已取消", "AbortError"));
      signal.addEventListener("abort", onAbort, { once: true });
    });
    try {
      for (;;) {
        const { value, done } = await Promise.race([reader.read(), cancelled]);
        if (done) break; result.set(value, offset); offset += value.length;
      }
    } finally {
      // 摘掉兜底监听：正常读完或已取消之后都不该留着它。
      // 留着会在长会话里累积——每个 signal 上挂着越来越多个闭包。
      signal.removeEventListener("abort", onAbort);
      // 不 await：流已经 error 时 cancel() 可能永不结算，await 它会把调用方一起挂死。
      void reader.cancel().catch(() => {});
    }
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
   * 按 1 MiB 目标折算并封顶 8 块——封顶只为在加密块异常小（格式下限 512 B）时
   * 不至于把整份文件一次性预取进内存。默认 1 MiB 块配 1 MiB 目标下恒为 1，
   * 即一个请求对应一个完整加密块。
   */
  private batchCount(prefetch: boolean): number {
    return prefetch ? Math.max(1, Math.min(8, Math.floor(BATCH_BYTES / this.header.blockSize))) : 1;
  }

  /**
   * 取一个块。缓存命中的判断在调度器内部，这里只转交——它同时拥有缓存视图与调度状态。
   *
   * 签名对应调度器的 readBlock：读者与块请求是两层，一个读者取消不会掐掉共享块。
   */
  private block(index: number, _prefetch = true, priority: ReadPriority = "prefetch",
    generation = this.scheduler.currentGeneration, callerSignal: AbortSignal = this.signal): Promise<Uint8Array> {
    // 批次大小由调度器按 policy 决定，这里的 prefetch 不再影响开批行为——
    // 读法（按需还是顺序）由 input() 的 SequentialProbe 判定，见那里的注释。
    //
    // signal 必须是**这次读取自己**的取消信号，不能固定用会话信号：read() 与
    // input() 传的可能是不同的 AbortSignal（前者是调用方的，后者是会话的）。
    // 混用会导致调用方取消了自己那次读取、底层请求却还在跑——读永远不结算，
    // 连接与内存一起泄漏。两者都存在时合成：任一取消，本次读取就该结束。
    return this.scheduler.readBlock(index, {
      generation, priority,
      signal: callerSignal === this.signal ? this.signal : AbortSignal.any([callerSignal, this.signal]),
    });
  }

  /**
   * 取数与认证一批块，写进缓存。
   *
   * 调度（谁先走、谁被取消、占几个槽位）全在 MediaReadScheduler；这里只负责
   * 把密文取回来、解密、认证后 `remember`。返回即代表这批数据都在缓存里了。
   *
   * 网络断开只重试尚未认证的后缀，已经交付的块不再请求——重试时它们已在缓存里。
   */
  private async fetchBlocks(
    indices: number[],
    signal: AbortSignal,
    publish: (index: number, bytes: Uint8Array) => void,
  ) {
    const controller = new AbortController();
    const combined = AbortSignal.any([signal, controller.signal, this.signal]);
    // 无条件让出一次：async generator 的首次 pull 与 fetch 建立之间有微任务间隔，
    // 若调用方在同一个 tick 里就abort，fetch 可能还没发出——那正是"请求已发出但
    // 立刻取消"这类时序。提前检查一次可以让这类取消走干净的路径。
    await Promise.resolve();
    combined.throwIfAborted();
    let next = 0;
    for (let attempt = 0; next < indices.length; attempt++) {
      try {
        let block = new Uint8Array(blockCipherLen(this.header, indices[next])), used = 0;
        let finalPlain: Uint8Array | undefined;
        const last = indices[indices.length - 1];
        for await (const chunk of this.cipher(blockCipherOffset(this.header, indices[next]), blockCipherOffset(this.header, last) + blockCipherLen(this.header, last), combined)) {
          let offset = 0;
          while (offset < chunk.length) {
            if (next >= indices.length) throw new MediaSourceError("媒体分片长度不匹配", "range");
            const take = Math.min(block.length - used, chunk.length - offset);
            block.set(chunk.subarray(offset, offset + take), used); used += take; offset += take;
            if (used !== block.length) continue;
            const index = indices[next];
            const plain = await decryptBlock(this.key, this.header, index, block);
            combined.throwIfAborted();
            if (next === indices.length - 1) finalPlain = plain;
            else {
              this.remember(index, plain);
              publish(index, plain);
            }
            next++; used = 0;
            if (next < indices.length) block = new Uint8Array(blockCipherLen(this.header, indices[next]));
          }
        }
        if (finalPlain) {
          this.remember(last, finalPlain);
          publish(last, finalPlain);
        }
      } catch (error) {
        this.signal.throwIfAborted();
        // 被抢占取消的批次不重试：它的消费者已经不需要了，重试只是白占带宽。
        if (combined.aborted) throw error;
        if (next === indices.length) next--; // 最后一块须等精确响应结束后才交付。
        if (attempt >= 2 || !(error instanceof TypeError || error instanceof MediaSourceError && error.code === "network")) throw error;
        await this.delay(150 * 2 ** attempt);
      }
    }
  }

  private remember(index: number, plain: Uint8Array) {
    const existing = this.cache.get(index);
    if (existing) this.cachedBytes -= existing.length;
    this.cache.delete(index); this.cache.set(index, plain); this.cachedBytes += plain.length;
    this.evictCache();
  }

  private evictCache() {
    while (this.cachedBytes > this.cacheLimit && this.cache.size > 1) {
      const oldest = this.cache.keys().next().value!;
      this.cachedBytes -= this.cache.get(oldest)!.length; this.cache.delete(oldest);
    }
  }

  private delay(ms: number) {
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => { this.signal.removeEventListener("abort", abort); resolve(); }, ms);
      const abort = () => { clearTimeout(timer); reject(this.signal.reason); };
      this.signal.addEventListener("abort", abort, { once: true });
    });
  }

  private async *cipher(start: number, end: number, cancel?: AbortSignal): AsyncGenerator<Uint8Array> {
    if (this.localFile) {
      const reader = this.localFile.slice(start, end).stream().getReader();
      try { for (;;) { this.signal.throwIfAborted(); cancel?.throwIfAborted(); const { value, done } = await reader.read(); if (done) break; yield value; } }
      finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
      return;
    }
    const parts = this.descriptor.parts?.length ? this.descriptor.parts : [{ url: this.descriptor.url, offset: 0, size: this.descriptor.cipherSize }];
    for (const range of cipherPartRanges(parts, start, end - 1)) {
      const timeout = new AbortController();
      const signal = cancel ? AbortSignal.any([this.signal, cancel, timeout.signal]) : AbortSignal.any([this.signal, timeout.signal]);
      // 限制无数据等待时间，而非整个请求时长；大文件完整缓存可能持续数分钟。
      const timed = async <T>(operation: () => Promise<T>): Promise<T> => {
        const timer = setTimeout(() => timeout.abort(), 30_000);
        try { return await operation(); } finally { clearTimeout(timer); }
      };
      let response: Response;
      try { response = await timed(() => fetch(range.url, { headers: { Range: `bytes=${range.start}-${range.end}` }, signal, cache: "no-store" })); }
      catch (error) {
        this.signal.throwIfAborted();
        // 被调度器抢占取消：不可恢复，重连只会白白重建播放。
        if (cancel?.aborted) throw new MediaSourceError("媒体请求被调度器取消", "network", false);
        if (signal.aborted || error instanceof TypeError) throw new MediaSourceError(signal.aborted ? "媒体请求超时" : "媒体网络连接中断", "network", true); throw error;
      }
      const expected = range.end - range.start + 1;
      const contentRange = response.headers.get("Content-Range");
      const match = contentRange?.match(/^bytes (\d+)-(\d+)\/(\d+)$/);
      const part = parts.find(part => part.url === range.url && start + range.offset >= part.offset && start + range.offset < part.offset + part.size)!;
      if (response.status === 401 || response.status === 403) { await response.body?.cancel(); throw new MediaSourceError("预览票据已失效，请重新打开文件", "auth", true); }
      // 5xx/429 是服务端或链路的临时不可用，同一个 URL 过会儿就能通，不必重新授权。
      if (response.status >= 500 || response.status === 429) { await response.body?.cancel(); throw new MediaSourceError("媒体服务暂时不可用", "network", true); }
      // 200 只有在请求区间恰好覆盖整卷时才算合法：那是显式的全量读取，
      // 而不是"上游无视 Range"。放过后者会让流式路径把整份文件当区间用，
      // 分片映射随即失效——所以这里必须拒绝，并把 rangeUnsupported 置上，
      // 让调用方改走 complete() 的全量路径。
      if (!(response.status === 206 || response.status === 200 && range.start === 0 && expected === part.size)
        || contentRange && (!match || +match[1] !== range.start || +match[2] !== range.end || +match[3] !== part.size)) {
        await response.body?.cancel(); this.rangeUnsupported = true; throw new MediaSourceError("媒体源未返回正确的分片区间", "range");
      }
      const reader = response.body?.getReader();
      if (!reader) throw new MediaSourceError("媒体响应为空", "network", true);
      let received = 0;
      try {
        for (;;) {
          const { value, done } = await timed(() => reader.read()); if (done) break;
          received += value.length;
          if (received > expected) throw new MediaSourceError("媒体分片长度不匹配", "range");
          yield value;
        }
        if (received !== expected) throw new MediaSourceError("媒体分片被截断", "network", true);
      } catch (error) {
        this.signal.throwIfAborted();
        if (cancel?.aborted) throw new MediaSourceError("媒体请求被调度器取消", "network", false);
        if (signal.aborted || error instanceof TypeError) throw new MediaSourceError(signal.aborted ? "媒体请求超时" : "媒体网络连接中断", "network", true); throw error;
      } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
    }
  }

  /**
   * 文件只保存密文；关闭会话时删除，超出磁盘配额时不会转为无限内存下载。
   *
   * 分两个阶段上报进度：下载密文是一段可观的网络等待，之后的全量认证对大文件
   * 同样要跑一遍 AES-GCM——那是纯 CPU 时间，却原本完全没有反馈，用户只能对着一个
   * 停在 100% 的进度条干等。把它们分成两段，用户才知道程序还在动。
   */
  private async cacheCompleteCipher(progress: (stage: "download" | "verify", ratio: number) => void): Promise<void> {
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
        progress("download", received / this.descriptor.cipherSize);
      }
      this.signal.throwIfAborted(); await writer.close(); completed = true;
      this.localFile = await file.getFile();
      this.signal.throwIfAborted();
      this.header = parseXphHeader(new Uint8Array(await this.localFile.slice(0, HEADER_SIZE).arrayBuffer()));
      if (this.header.plainSize !== this.descriptor.plainSize || this.header.cipherSize !== this.descriptor.cipherSize) throw new XphFormatError("媒体文件头与交付计划不一致");
      // 先校验全部认证块，后续播放才可以完全依赖本地密文。
      // 这一段是纯 CPU 时间，所以按块数报进度；否则用户只看到 100% 卡住。
      const total = this.header.blockCount || 1;
      for (let i = 0; i < this.header.blockCount; i++) {
        this.signal.throwIfAborted();
        const offset = blockCipherOffset(this.header, i);
        await decryptBlock(this.key, this.header, i, new Uint8Array(await this.localFile.slice(offset, offset + blockCipherLen(this.header, i)).arrayBuffer()));
        // 每块都上报会让主线程被消息淹没，按固定步长汇报就够了。
        if (i % 16 === 0 || i === this.header.blockCount - 1) progress("verify", (i + 1) / total);
      }
    } finally { if (!completed) await writer.abort().catch(() => {}); }
  }

  async dispose(): Promise<void> {
    await this.diskTask?.catch(() => {});
    this.localFile = undefined; this.cache.clear(); this.cachedBytes = 0;
    const temporary = this.temporary; this.temporary = undefined;
    if (temporary) await temporary.directory.removeEntry(temporary.name).catch(() => {});
  }

  /**
   * 小文件使用完整 Blob；大文件仅密文落盘，再经同一解密接口本地播放。
   *
   * `progress` 统一报(阶段, 比例)，避免 OPFS 下载完成后出现进度停滞。
   */
  async complete(mime: string, progress: (stage: "download" | "verify" | "prepare", ratio: number) => void, allowDisk = true): Promise<Blob | { cached: true }> {
    if (this.descriptor.plainSize > FULL_PREVIEW_LIMIT) {
      if (!allowDisk) throw new MediaSourceError("此浏览器无法流式播放大文件，请下载后播放", "size");
      this.diskTask = this.cacheCompleteCipher(progress);
      await this.diskTask;
      return { cached: true };
    }
    progress("prepare", 0);
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
            chunks.push(new Blob([bytes as Uint8Array<ArrayBuffer>]));
            progress("download", (index + 1) / (this.header.blockCount || 1));
          }
          index++; used = 0;
          if (index < this.header.blockCount) buffer = new Uint8Array(blockCipherLen(this.header, index));
        }
      }
      this.signal.throwIfAborted();
      if (index !== this.header.blockCount || used) throw new XphFormatError("完整媒体被截断");
      return new Blob(chunks, { type: mime });
    }
    const total = this.header.blockCount || 1;
    for (let i = 0; i < this.header.blockCount; i++) {
      const current = this.block(i);
      if (i + 1 < this.header.blockCount) void this.block(i + 1).catch(() => {});
      const bytes = await current; this.signal.throwIfAborted();
      chunks.push(new Blob([bytes as Uint8Array<ArrayBuffer>]));
      // 块是边下载边认证的，所以一个阶段就够，不该让用户看到"下载 100%"之后
      // 又要等一段没有反馈的"校验"。
      progress("download", (i + 1) / total);
    }
    return new Blob(chunks, { type: mime });
  }
}
