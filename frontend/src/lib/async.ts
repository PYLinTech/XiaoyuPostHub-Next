import { ref, type Ref } from "vue";
import { ApiError } from "@/api/client";

// 视图里反复出现的三件事：加载状态、错误文案、刷新。
//
// 抽成一个 composable 而不是每页各写一遍，是为了让"错误怎么展示"有统一答案：
// 服务的 message 是通用标题、detail 是具体原因，两者一起显示。

export interface AsyncState<T> {
  data: Ref<T>;
  loading: Ref<boolean>;
  error: Ref<string>;
  /** 执行一次并写入 data；失败时写入 error 且不抛出，便于模板直接用。 */
  run: () => Promise<T | null>;
  reset: () => void;
}

export function useAsync<T>(loader: () => Promise<T>, initial: T): AsyncState<T> {
  const data = ref(initial) as Ref<T>;
  const loading = ref(false);
  const error = ref("");
  // 守卫放在这里而不是让每个调用方自己记得配：这四个使用点全都挂在
  // "某个参数变了要重载" 上（目录、页长、offset），连点两次就一定会交叉。
  // 少了它，先返回的那次会把 loading 提前关掉，慢的那次还会用旧结果盖掉新的。
  const gate = createRequestGate();

  async function run(): Promise<T | null> {
    const token = gate.next();
    loading.value = true;
    error.value = "";
    try {
      const result = await loader();
      // 过期结果一律不写回：data 只能反映最后一次请求。
      if (!gate.isCurrent(token)) return null;
      data.value = result;
      return result;
    } catch (err) {
      if (!gate.isCurrent(token)) return null;
      error.value = describeError(err);
      return null;
    } finally {
      if (gate.isCurrent(token)) loading.value = false;
    }
  }

  function reset(): void {
    data.value = initial;
    error.value = "";
  }

  return { data, loading, error, run, reset };
}

/**
 * 把任意异常转成可展示的文案。
 *
 * message 是服务端的通用标题，detail 才是"具体发生了什么"。两者都要给用户：
 * 只显示 message 的话，「域名 example.com 下仍有 2 个邮箱地址，无法解除绑定」
 * 会被压成一句「当前状态不允许该操作」——管理员既不知道是哪一步被挡住的，
 * 也不知道下一步该做什么。detail 的哨兵前缀已由后端剥掉，这里直接拼接。
 */
export function describeError(err: unknown): string {
  if (err instanceof ApiError) {
    const detail = err.detail?.trim();
    // message 已被 detail 完整涵盖时（少数错误两者同文）不重复拼接。
    if (detail && !err.message.includes(detail)) {
      return `${err.message}：${detail}`;
    }
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return String(err);
}

/** 供排障用：把完整的错误上下文打到控制台。展示文案见 describeError。 */
export function logError(context: string, err: unknown): void {
  if (err instanceof ApiError && err.detail) {
    console.warn(`[${context}] ${err.message} :: ${err.detail}`);
    return;
  }
  console.warn(`[${context}]`, err);
}

/**
 * 把 API 错误弹成 toast。
 *
 * AppToast 本来就有 detail 副文案那一行，但此前没有任何地方喂它，于是
 * 通用标题与具体原因被挤成同一句话。这里把两者拆开：标题是"哪一类问题"，
 * 副行是"具体是哪一条"，排版上主次分明，也顺手激活了那块预留的展示位。
 *
 * 只声明用到的结构，不 import toast store：lib 不该反向依赖 store。
 */
export function toastApiError(
  toasts: { error: (message: string, detail?: string) => number },
  err: unknown,
): void {
  if (err instanceof ApiError) {
    const detail = err.detail?.trim();
    if (detail && !err.message.includes(detail)) {
      toasts.error(err.message, detail);
      return;
    }
  }
  toasts.error(describeError(err));
}

/**
 * 判断异常是不是"请求被主动取消"。
 *
 * client 把 AbortError 原样抛出（不包装成网络错误），所以调用方必须自己
 * 把它和真失败区分开：取消是预期行为，不该弹错误提示、更不该写进页面错误区。
 */
export function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

/**
 * 请求序号守卫：防止较早发出的请求后返回、覆盖掉较新的结果。
 *
 * 典型场景是搜索框——输入 "abc" 回车、马上改成 "xyz" 再回车。两个请求并发，
 * 若 "abc" 那个更慢，它后返回就会把表格刷成 abc 的结果，而筛选框里写着 xyz。
 * 全站列表页原先都没有这层保护（client 的 signal 管道也一直没被用上）。
 *
 * 用丢弃过期结果而不是 AbortController：这些都是 GET，中断与否无所谓，
 * 而序号判断不依赖 fetch 的实现细节，行为可预测。
 */
export function createRequestGate(): {
  /** 开始新一轮请求，返回的令牌用于后续判断自己是否已过期。 */
  next: () => number;
  /** 令牌是否仍代表最新一轮请求。 */
  isCurrent: (token: number) => boolean;
} {
  let latest = 0;
  return {
    next: () => ++latest,
    isCurrent: (token: number) => token === latest,
  };
}

/** 复制到剪贴板。非安全上下文下 clipboard API 不可用，退回到选中 + execCommand。 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // 继续尝试兜底方案。
  }
  try {
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    const ok = document.execCommand("copy");
    area.remove();
    return ok;
  } catch {
    return false;
  }
}
