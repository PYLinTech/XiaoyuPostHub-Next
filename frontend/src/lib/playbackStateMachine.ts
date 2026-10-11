/**
 * 播放器状态机。
 *
 * ## 为什么要显式建模
 *
 * 现状是状态散在三个地方：组件的 ready/error 两个 ref、播放器事件监听、Worker 的
 * generation 计数。出问题时说不清"现在到底处于什么状态"，恢复逻辑也就无从判断
 * 该重建哪部分、该保留哪部分。
 *
 * 集中成显式状态机之后，最关键的一条边界才写得出来：
 *
 *   **recovering 不销毁播放器资源。**
 *
 * 网络抖一下就把整条链路拆了重建，等于把可恢复故障当成终止故障——用户不该为一次
 * 丢包付出重新选内核、重新拿票据、回到起点的代价。所以进入 recovering 时保留：
 * 已解密的块（在 LRU 缓存里）、当前播放位置、暂停状态与播放速度。
 *
 * 只有三类情况进 failed：认证失败、确实不支持的编码、无法恢复的权限问题。
 * 这三类无论重试多少次结果都一样，早失败比让用户干等更好。
 */

export type PlaybackState =
  | "idle"        // 尚未开始准备
  | "preparing"   // 正在选内核、拿交付计划
  | "buffering"   // 数据在途，还没有可播的部分
  | "ready"       // 已经能播
  | "stalled"     // 曾经能播，现在等数据（waiting）
  | "recovering"  // 出故障了，正在自愈；资源保留
  | "failed"      // 终止态
  | "disposed";   // 组件卸载

/** 状态迁移表。非法迁移直接拒绝，而不是静默接受——那是状态机失去意义的开始。 */
const TRANSITIONS: Record<PlaybackState, PlaybackState[]> = {
  idle: ["preparing", "disposed"],
  preparing: ["buffering", "ready", "recovering", "failed", "disposed"],
  buffering: ["ready", "stalled", "recovering", "failed", "disposed"],
  ready: ["stalled", "buffering", "recovering", "failed", "disposed"],
  // stalled 可以自己回到 ready（数据补上了），也可以进入 recovering（补不上）
  stalled: ["ready", "buffering", "recovering", "failed", "disposed"],
  // recovering 保留资源：成功回到原位置继续，或重试耗尽才终止
  recovering: ["ready", "buffering", "recovering", "failed", "disposed"],
  // failed 是终态，只能被卸载清掉——重试由 recovering 承担，不在这里
  failed: ["disposed"],
  disposed: [],
};

/** 进入这些状态时，播放器资源必须保留，不要拆。 */
const PRESERVES = new Set<PlaybackState>(["recovering", "stalled", "failed"]);

export interface RecoveryContext {
  /** 故障是否可自愈（网络抖动可，格式/权限不可）。 */
  recoverable: boolean;
  /** 当前播放位置（秒），恢复时要回到这里。 */
  currentTime: number;
  /** 恢复后是否应当续播。 */
  resume: boolean;
  /** 播放速度。恢复时必须还原，不能悄悄回到 1x。 */
  playbackRate: number;
  /** 已尝试次数与上限。 */
  attempt: number;
  budget: number;
}

/**
 * 故障分类：决定进 recovering 还是 failed。
 *
 * 关键区分是「可自愈」与「重试也没用」：
 *  - 网络错、票据过期 → 可自愈，走重连/重新授权
 *  - 认证失败（内容对不上）、编解码不支持、权限确实拿不到 → 立刻终止
 *
 * 底层 fetchBlocks 已经把单请求抖动重试满额，所以它抛出来的 network 已经不是
 * 小抖动，而是整段网络断了——那正是值得上层自愈的规模。
 */
export function classifyFailure(reason: unknown): { recoverable: boolean; stage: "network" | "auth" | "format" | "codec" | "permission" | "unknown" } {
  const name = reason instanceof Error ? reason.name : "";
  const code = reason && typeof reason === "object" && "code" in reason ? reason.code : undefined;
  // 调度器抢占造成的取消不是故障：它意味着调用方已经不要这些数据了。
  if (code === "network" && reason instanceof Error && /调度器取消|新的播放位置/.test(reason.message)) {
    return { recoverable: false, stage: "unknown" };
  }
  if (code === "network" && reason && typeof reason === "object" && "transient" in reason && reason.transient === false) {
    return { recoverable: false, stage: "unknown" };
  }
  if (name === "XphFormatError") return { recoverable: false, stage: "format" };
  if (code === "auth") return { recoverable: true, stage: "auth" };
  if (code === "network") return { recoverable: true, stage: "network" };
  if (name === "MediaCodecError" || name === "UnsupportedInputFormatError" || name === "NotEncodableError") {
    return { recoverable: false, stage: "codec" };
  }
  if (name === "QuotaExceededError") return { recoverable: true, stage: "network" };
  // 401/403 是授权问题，403 若是权限不足则重试无用——保守起见先按可重试。
  if (name === "NotAllowedError") return { recoverable: false, stage: "permission" };
  return { recoverable: true, stage: "unknown" };
}

/** 给用户的文案。终止态的原因必须说清楚，否则用户只能干瞪眼。 */
export function describeState(state: PlaybackState, stage: ReturnType<typeof classifyFailure>["stage"]): string {
  switch (state) {
    case "idle": return "";
    case "preparing": return "正在准备预览";
    case "buffering": return "正在缓冲";
    case "ready": return "";
    case "stalled": return "正在等待数据…";
    case "recovering":
      return stage === "auth" ? "预览授权已过期，正在重新获取…" : "网络不稳定，正在重连…";
    case "failed":
      return stage === "codec" ? "浏览器无法解码该文件的音视频编码，请下载后播放"
        : stage === "format" ? "文件格式已损坏或不被支持"
          : stage === "permission" ? "没有访问该文件的权限"
            : "预览失败，请重试或下载后播放";
    case "disposed": return "";
  }
}

export class PlaybackStateMachine {
  private state: PlaybackState = "idle";
  private stage: ReturnType<typeof classifyFailure>["stage"] = "unknown";
  private listeners = new Set<(state: PlaybackState, stage: string) => void>();

  get current() { return this.state; }
  get failureStage() { return this.stage; }
  /** 该状态下是否应当保留播放器资源。 */
  get preservesResources() { return PRESERVES.has(this.state); }

  subscribe(listener: (state: PlaybackState, stage: string) => void) {
    this.listeners.add(listener);
    listener(this.state, this.stage);
    return () => { this.listeners.delete(listener); };
  }

  /**
   * 请求迁移。非法迁移返回 false（不抛错——调用点多在事件回调里，抛出去会变成
   * 未捕获异常），由调用方决定要不要重试或降级。
   */
  to(next: PlaybackState, stage = this.stage): boolean {
    if (next === this.state) return true;
    if (!TRANSITIONS[this.state].includes(next)) {
      console.warn("[playback] 非法状态迁移", this.state, "→", next);
      return false;
    }
    this.state = next;
    if (next === "failed" || next === "recovering") this.stage = stage;
    for (const listener of this.listeners) listener(next, this.stage);
    return true;
  }

  /** 播放器事件 → 状态。信号全部来自真实的媒体元素，不另造判定。 */
  observeEvent(event: "loadstart" | "loadedmetadata" | "canplay" | "playing" | "waiting" | "pause" | "emptied" | "error") {
    switch (event) {
      case "loadstart": this.to("preparing"); break;
      case "loadedmetadata": break; // 元数据就绪但还没有画面，不足以称ready
      case "canplay": if (this.state === "buffering" || this.state === "preparing" || this.state === "stalled") this.to("ready"); break;
      case "playing": if (this.state !== "recovering" && this.state !== "disposed") this.to("ready"); break;
      case "waiting": if (this.state === "ready" || this.state === "buffering") this.to("stalled"); break;
      case "pause": break; // 暂停不是状态变化，只是节奏变了
      case "emptied": if (this.state !== "disposed") this.to("preparing"); break;
      case "error": break; // 错误走 handleFailure，不在这里判
    }
  }

  /** 故障入口。返回 true 表示应当走自愈，false 表示应当终止。 */
  handleFailure(reason: unknown, context: RecoveryContext): boolean {
    const { recoverable, stage } = classifyFailure(reason);
    if (!recoverable) { this.to("failed", stage); return false; }
    // 预算用尽就不再自愈——已经试够了，剩下的交给用户。
    if (context.attempt >= context.budget) { this.to("failed", stage); return false; }
    this.to("recovering", stage);
    return true;
  }

  /** 恢复成功，回到可播状态。 */
  recovered() {
    this.stage = "unknown";
    this.to(this.state === "recovering" ? "buffering" : this.state);
  }

  dispose() { this.to("disposed"); this.listeners.clear(); }
}
