import { isRecoverable } from "./mediaRangeSource";

/**
 * 预览故障的三层恢复策略。
 *
 * 底层 fetchBlocks 已经把「单个 Range 请求失败」重试满额（2 次 + 退避），所以它
 * 抛到上层的 network 已经不是单请求级别的小抖动，而是「整段网络断了」或「连不上海」。
 * 这里不再重复同层重试，而是按故障类型分三档往上走：
 *
 *  1. transient 网络错 → 原地重连：同一个 URL 换个时间再试，不动票据和播放位置；
 *  2. auth 错         → 重新授权：回后端 plan() 换新的直链与密钥，再从原位置恢复；
 *  3. range / 格式 / 大小 → 重试多少次都一样，只能终止。
 *
 * 退避是指数的但有上限，且**总预算有限**：一个真的断了的网不该让播放器僵在那里
 * 转两分钟，那和直接报错没有区别，只是在浪费用户的耐心。超预算就如实终止。
 */

/** 一次故障在终止前允许的恢复次数，按类型分开。 */
export const RECOVERY_BUDGET = {
  /** 网络抖动：重连即可，不动票据。 */
  reconnect: 3,
  /** 票据过期：每次都要回后端换直链，贵，少做。 */
  replan: 2,
} as const;

/** 退避序列（毫秒）。取到最后一档就固定在该值，不无限增长。 */
const BACKOFF = [1_000, 2_000, 5_000] as const;

export function newRecovery(): RecoveryAttempt {
  return { spent: { reconnect: 0, replan: 0 }, waited: 0 };
}

/**
 * 下一步该做什么：`wait` 先退避再重试，`reconnect` 原地重来，
 * `replan` 换直链，`giveup` 只能终止。
 *
 * `kind` 记录本轮故障选定的恢复方式，换类型（重连→重新授权）时重新起算等待
 * 预算：那确实是新的一轮故障，不该继承上一轮已经等掉的时间。
 */
export type RecoveryAction =
  | { kind: "wait"; delay: number; attempt: RecoveryAttempt }
  | { kind: "reconnect"; attempt: RecoveryAttempt }
  | { kind: "replan"; attempt: RecoveryAttempt }
  | { kind: "giveup"; reason: unknown };

export interface RecoveryAttempt {
  /** 已经用过几次 reconnect / replan。 */
  spent: Record<"reconnect" | "replan", number>;
  /** 已经等过的总时长，用于判断是否还值得继续。 */
  waited: number;
  /** 本轮故障选定的恢复方式；为空表示尚未开始。 */
  kind?: "reconnect" | "replan";
  /** 最近一次调用已决定退避；等待结束后下一步应执行恢复，而非再次计数。 */
  waiting?: boolean;
}

/** 总预算：网络抖动累计最多等 8 秒，否则不如直接报错让用户重开。 */
const TOTAL_WAIT_BUDGET = 8_000;

export function recoveryPlan(reason: unknown, attempt: RecoveryAttempt): RecoveryAction {
  const { reconnect, replan } = isRecoverable(reason);
  if (!reconnect && !replan) return { kind: "giveup", reason };
  const kind = replan ? "replan" : "reconnect";
  // 恢复方式变了就是新的一轮故障，等待预算重新起算。
  if (attempt.kind !== kind) { attempt.kind = kind; attempt.waited = 0; attempt.waiting = false; }
  if (attempt.waiting) {
    attempt.waiting = false;
    return { kind, attempt };
  }
  const budget = RECOVERY_BUDGET[kind];
  if (attempt.spent[kind] >= budget || attempt.waited >= TOTAL_WAIT_BUDGET) return { kind: "giveup", reason };
  attempt.spent[kind]++;
  const delay = BACKOFF[Math.min(attempt.spent[kind] - 1, BACKOFF.length - 1)];
  // 预算没花完就先退避再重试，让瞬时抖动自己消失；最后一次直接重连，不再多等一拍。
  if (attempt.spent[kind] < budget && attempt.waited + delay < TOTAL_WAIT_BUDGET) {
    attempt.waited += delay;
    attempt.waiting = true;
    return { kind: "wait", delay, attempt };
  }
  return { kind, attempt };
}

export function sleep(ms: number, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted();
  return new Promise((resolve, reject) => {
    const abort = () => { clearTimeout(timer); reject(signal.reason); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, ms);
    signal.addEventListener("abort", abort, { once: true });
  });
}
