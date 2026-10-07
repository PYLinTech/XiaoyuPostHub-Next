import { reactive } from "vue";

// 通知条。放在全局是因为它需要跨越视图存活：上传失败的提示不该因为用户
// 切了页面就消失，而上传本身又是在后台推进的。
//
// 单条插槽而不是堆栈：一次只展示最近的一条，新的到来直接顶掉旧的。
// 通知不是收件箱，堆叠只会培养出"无视提示"的习惯。

export type ToastKind = "info" | "success" | "error";

export interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
  detail?: string;
}

const state = reactive({ current: null as Toast | null });
let nextId = 1;
let hideTimer: number | undefined;
/** 计划消失的时刻；暂停期间靠它算出还剩多久。 */
let hideAt = 0;
/** 暂停时剩余的毫秒数。 */
let remaining = 0;

/** 默认存活时长。错误给得更久：它通常需要用户读完整句才能决定怎么做。 */
const TTL: Record<ToastKind, number> = { info: 4000, success: 3000, error: 10000 };

/**
 * 带 detail 的错误是两行：标题说"哪一类问题"，副行说"具体是哪一条"。
 * 管理员多半要读完副行才知道下一步做什么，因此这类提示多给 50% 时间。
 */
function ttlOf(kind: ToastKind, detail?: string): number {
  const base = TTL[kind];
  return detail ? Math.round(base * 1.5) : base;
}

function clearHideTimer(): void {
  if (hideTimer !== undefined) {
    window.clearTimeout(hideTimer);
    hideTimer = undefined;
  }
}

function schedule(ms: number): void {
  clearHideTimer();
  remaining = ms;
  hideAt = Date.now() + ms;
  hideTimer = window.setTimeout(dismiss, ms);
}

function push(kind: ToastKind, message: string, detail?: string): number {
  // 顶掉旧条目时必须连它的存活计时器一起清掉，否则旧计时器会把新通知提前带走。
  clearHideTimer();
  const id = nextId++;
  state.current = { id, kind, message, detail };
  const ttl = ttlOf(kind, detail);
  if (ttl > 0) {
    schedule(ttl);
  }
  return id;
}

export function dismiss(): void {
  clearHideTimer();
  remaining = 0;
  state.current = null;
}

/**
 * 悬停暂停。用户正在读的时候提示被收走，比提示消失得慢更让人恼火——
 * 尤其错误提示，读到一半没了就等于没给。
 */
function pause(): void {
  if (hideTimer === undefined) {
    return;
  }
  window.clearTimeout(hideTimer);
  hideTimer = undefined;
  remaining = Math.max(0, hideAt - Date.now());
}

/** 移开后按剩余时间续上。剩余为 0 说明本该已经消失，此刻直接收掉。 */
function resume(): void {
  if (hideTimer !== undefined || !state.current) {
    return;
  }
  if (remaining <= 0) {
    dismiss();
    return;
  }
  schedule(remaining);
}

export function useToasts(): {
  current: Toast | null;
  dismiss: () => void;
  pause: () => void;
  resume: () => void;
  info: (message: string, detail?: string) => number;
  success: (message: string, detail?: string) => number;
  error: (message: string, detail?: string) => number;
} {
  return {
    // 必须用 getter：current 是整体替换的引用，直接赋值会把调用那一刻的
    // 快照（通常是 null）固定在返回对象里，界面永远看不到后续更新。
    get current() {
      return state.current;
    },
    dismiss,
    pause,
    resume,
    info: (message, detail) => push("info", message, detail),
    success: (message, detail) => push("success", message, detail),
    error: (message, detail) => push("error", message, detail),
  };
}
