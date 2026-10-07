import type { ApiEnvelope, ApiError as ApiErrorShape } from "./types";

// 统一的请求封装。
//
// 只有一处出口，是因为有几件事必须对每个请求都成立、漏一个就会出问题：
// 信封解包、Bearer 令牌、401 后的会话清理、429 的 Retry-After 透传。

export class ApiError extends Error {
  readonly status: number;
  readonly detail?: string;
  readonly retryAfter?: number;

  constructor(status: number, message: string, detail?: string, retryAfter?: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.detail = detail;
    this.retryAfter = retryAfter;
  }

  /** 网络层失败（压根没拿到响应），与业务拒绝区分开：重试策略不同。 */
  get isNetworkError(): boolean {
    return this.status === 0;
  }
}

const TOKEN_STORAGE_KEY = "xph.token";

let token: string = safeReadToken();
let onUnauthorized: (() => void) | null = null;
/** 本轮"未授权"是否已经通知过界面。见 request() 里的 401 分支。 */
let unauthorizedFired = false;

function safeReadToken(): string {
  try {
    return localStorage.getItem(TOKEN_STORAGE_KEY) ?? "";
  } catch {
    return "";
  }
}

export function getToken(): string {
  return token;
}

export function setToken(next: string): void {
  token = next;
  // 拿到新令牌就允许下一次 401 再次触发通知：否则用户重新登录后，
  // 下一个真正过期的令牌会静默地把会话清掉而不通知界面。
  unauthorizedFired = false;
  try {
    if (next) {
      localStorage.setItem(TOKEN_STORAGE_KEY, next);
    } else {
      localStorage.removeItem(TOKEN_STORAGE_KEY);
    }
  } catch {
    // 隐私模式下 localStorage 会抛异常。此时令牌只活在内存里，功能不受影响。
  }
}

/**
 * 注册 401 的全局处理。由会话层注入，避免 api 层反向依赖 store。
 */
export function setUnauthorizedHandler(handler: (() => void) | null): void {
  onUnauthorized = handler;
}

export interface RequestOptions {
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  /** 任意可 JSON 序列化的请求体；传 Blob/ArrayBuffer 时按二进制原样发送。 */
  body?: unknown;
  query?: Record<string, string | number | boolean | undefined | null>;
  signal?: AbortSignal;
  /** 401 时是否吊销本地会话。访问访客接口时要关掉。 */
  clearSessionOn401?: boolean;
  /** 额外请求头。跨域直链只允许带 Range，因此该选项仅用于同源请求。 */
  headers?: Record<string, string>;
}

function buildUrl(path: string, query?: RequestOptions["query"]): string {
  if (!query) {
    return path;
  }
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === "") {
      continue;
    }
    search.set(key, String(value));
  }
  const qs = search.toString();
  return qs ? `${path}?${qs}` : path;
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, query, signal, headers, clearSessionOn401 = true } = options;

  const init: RequestInit = { method, signal };
  const finalHeaders: Record<string, string> = { ...headers };

  if (token) {
    finalHeaders["Authorization"] = `Bearer ${token}`;
  }
  if (body !== undefined) {
    if (isBinary(body)) {
      init.body = body as BodyInit;
    } else {
      finalHeaders["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
  }
  if (Object.keys(finalHeaders).length > 0) {
    init.headers = finalHeaders;
  }

  let response: Response;
  try {
    response = await fetch(buildUrl(path, query), init);
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") {
      throw err;
    }
    throw new ApiError(0, "网络连接失败，请检查网络后重试", String(err));
  }

  const retryAfter = parseRetryAfter(response.headers.get("Retry-After"));

  if (response.status === 204) {
    return undefined as T;
  }

  const text = await response.text();
  let payload: ApiEnvelope<T> | null = null;
  if (text) {
    try {
      payload = JSON.parse(text) as ApiEnvelope<T>;
    } catch {
      payload = null;
    }
  }

  if (!response.ok) {
    if (response.status === 401 && clearSessionOn401 && onUnauthorized) {
      // 一个列表页并发五六个请求，令牌同时过期时会连着进来六次 401。
      // 去重不是为了省请求，而是避免"清会话 → 通知界面"这条链被触发六遍
      // （界面侧会跟着重拉公告、跳登录页）。令牌一旦写回就复位。
      if (!unauthorizedFired) {
        unauthorizedFired = true;
        try {
          onUnauthorized();
        } catch {
          // 刻意吞掉：这里的异常会顶掉下面那个真正的 ApiError，
          // 于是界面上显示的是一句与本次请求毫不相干的话。
        }
      }
    }
    const shape: ApiErrorShape | undefined = payload?.error;
    throw new ApiError(
      response.status,
      shape?.message ?? defaultMessageFor(response.status),
      shape?.detail,
      retryAfter,
    );
  }

  if (payload && payload.error) {
    throw new ApiError(response.status, payload.error.message, payload.error.detail);
  }

  const data = payload?.data;
  // 第二道防线：后端的 Go json 编码已把 nil 切片写成 []（而不是 null），
  // 但任何新接口漏了那一步都会让前端在 .map 上抛错、整页空白。
  // 在唯一的解码出口兜住它，比要求每个页面各自判空可靠。
  if (data && typeof data === "object" && "items" in data) {
    const holder = data as { items: unknown };
    if (holder.items === null || holder.items === undefined) {
      holder.items = [];
    }
  }
  // data 缺失（例如某些接口只回 {"data":{"ok":true}} 之外的形态）时返回空对象，
  // 让调用方拿到的永远是对象而不是 undefined。
  return (data ?? ({} as T)) as T;
}

function isBinary(value: unknown): boolean {
  return (
    value instanceof FormData ||
    value instanceof Blob ||
    value instanceof ArrayBuffer ||
    ArrayBuffer.isView(value) ||
    (typeof ReadableStream !== "undefined" && value instanceof ReadableStream)
  );
}

function parseRetryAfter(raw: string | null): number | undefined {
  if (!raw) {
    return undefined;
  }
  const seconds = Number.parseInt(raw, 10);
  return Number.isFinite(seconds) && seconds > 0 ? seconds : undefined;
}

function defaultMessageFor(status: number): string {
  if (status === 401) return "登录已过期，请重新登录";
  if (status === 403) return "没有权限执行该操作";
  if (status === 404) return "目标不存在";
  if (status === 413) return "文件超出上限";
  if (status === 429) return "操作过于频繁，请稍后再试";
  if (status >= 500) return "服务器暂时不可用，请稍后重试";
  return "请求失败";
}
