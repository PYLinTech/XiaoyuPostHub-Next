import { ApiError } from "@/api/client";
import { uploadApi } from "@/api/endpoints";
import type { ConflictAction, InitUploadResult, Node } from "@/api/types";
import { Sha256, bytesToHex } from "@/crypto/sha256";
import { isAbortError } from "@/lib/async";

// 分片上传编排。
//
// 后端把"内容"与"引用"分得很开：内容池按明文 SHA-256 去重，节点是用户自己的
// 逻辑路径。因此上传的入口动作是先算校验码——它决定了能否秒传，也决定了
// 断点续传能否找回原来的会话。

export interface UploadProgressInfo {
  fileName: string;
  /** 已上传字节数（含秒传命中的整份）。 */
  sent: number;
  total: number;
  /** 0..1；总长为 0 时按 1 处理。 */
  ratio: number;
  phase: "hashing" | "uploading" | "finishing" | "done";
  message: string;
}

interface UploadOutcome {
  node: Node;
  /** true 表示服务端已有同内容对象，本次没有真正传输数据。 */
  dedup: boolean;
}

interface UploadOptions {
  parentPath: string;
  conflictAction?: ConflictAction;
  onProgress?: (info: UploadProgressInfo) => void;
  signal?: AbortSignal;
  /** 并发分片数。过高会触发上游限流，也会让弱网下的重传代价变大。 */
  concurrency?: number;
}

/** 断点续传的会话记录。只有它能把"中断过的上传"接回原来的会话。 */
interface StoredSession {
  sessionId: string;
  checksum: string;
  savedAt: number;
}

const SESSION_PREFIX = "xph.upload.";
/** 会话有效期短于服务端的过期时间，避免拿着已经失效的会话去续传。 */
const SESSION_MAX_AGE_MS = 12 * 60 * 60 * 1000;

export async function uploadFile(file: File, options: UploadOptions): Promise<UploadOutcome> {
  const report = options.onProgress ?? (() => {});
  const total = file.size;

  report({
    fileName: file.name,
    sent: 0,
    total,
    ratio: 0,
    phase: "hashing",
    message: "正在计算校验码",
  });
  const checksum = await hashFile(file, options.signal);

  const resumed = await tryResume(checksum, options.parentPath, file.name);
  let session: InitUploadResult;
  if (resumed) {
    session = resumed;
  } else {
    session = await uploadApi.init({
      checksum,
      sizePlain: total,
      parentPath: options.parentPath,
      name: file.name,
      conflictAction: options.conflictAction ?? "rename",
    });
  }

  if (session.dedup && session.node) {
    report({
      fileName: file.name,
      sent: total,
      total,
      ratio: 1,
      phase: "done",
      message: "已秒传完成",
    });
    return { node: session.node, dedup: true };
  }

  const sessionId = session.sessionId;
  const chunkSize = session.chunkSize ?? 0;
  const chunkTotal = session.chunkTotal ?? 0;
  if (!sessionId || chunkSize <= 0 || chunkTotal <= 0) {
    // 空文件不会产生分片：服务端在 init 阶段就已经把节点建好了。
    if (session.node) {
      return { node: session.node, dedup: false };
    }
    throw new Error("上传会话缺少分片计划");
  }

  rememberSession(checksum, options.parentPath, file.name, sessionId);

  const pending = new Set<number>();
  for (let i = 0; i < chunkTotal; i++) {
    pending.add(i);
  }
  for (const index of session.received ?? []) {
    pending.delete(index);
  }

  let sent = (chunkTotal - pending.size) * chunkSize;
  if (sent > total) {
    sent = total;
  }

  const concurrency = Math.max(1, options.concurrency ?? 3);
  const queue = [...pending];

  const pump = async (): Promise<void> => {
    for (;;) {
      if (options.signal?.aborted) {
        throw new DOMException("已取消", "AbortError");
      }
      const index = queue.shift();
      if (index === undefined) {
        return;
      }
      const start = index * chunkSize;
      const end = Math.min(start + chunkSize, total);
      const slice = file.slice(start, end);
      await putChunkWithRetry(sessionId, index, slice, options.signal);
      sent += end - start;
      report({
        fileName: file.name,
        sent,
        total,
        ratio: total > 0 ? sent / total : 1,
        phase: "uploading",
        message: `正在上传 ${Math.min(100, Math.round((sent / Math.max(total, 1)) * 100))}%`,
      });
    }
  };

  await Promise.all(Array.from({ length: Math.min(concurrency, queue.length) }, pump));

  report({
    fileName: file.name,
    sent: total,
    total,
    ratio: 1,
    phase: "finishing",
    message: "正在收尾",
  });

  try {
    const result = await uploadApi.complete(sessionId);
    forgetSession(checksum, options.parentPath, file.name);
    report({
      fileName: file.name,
      sent: total,
      total,
      ratio: 1,
      phase: "done",
      message: "已完成",
    });
    return { node: result.node, dedup: false };
  } catch (err) {
    // 收尾失败时**保留**会话记录：分片已经在服务端了，下次可以直接重试 complete，
    // 不必重传整份文件。
    throw err;
  }
}

/** 取消一次上传：先尽力取消服务端会话，再清掉本地记录。 */
export async function cancelUpload(
  file: File,
  parentPath: string,
): Promise<void> {
  const checksum = await hashFile(file);
  const stored = readSession(checksum, parentPath, file.name);
  if (stored) {
    try {
      await uploadApi.cancel(stored.sessionId);
    } catch {
      // 会话可能已经过期；本地记录照删。
    }
  }
  forgetSession(checksum, parentPath, file.name);
}

/**
 * 尝试接回未完成的会话。
 *
 * 只有在本地还留着会话标识时才可能续传：服务端没有"按校验码查会话"的接口，
 * 这是刻意的——那会让校验码变成一个可以用来探测"某内容是否正在被上传"的信道。
 * 拿不到旧会话时，代价只是重传，不影响正确性。
 */
async function tryResume(
  checksum: string,
  parentPath: string,
  fileName: string,
): Promise<InitUploadResult | null> {
  const stored = readSession(checksum, parentPath, fileName);
  if (!stored) {
    return null;
  }
  try {
    const progress = await uploadApi.status(stored.sessionId);
    return {
      dedup: false,
      sessionId: progress.sessionId,
      chunkSize: progress.chunkSize,
      chunkTotal: progress.chunkTotal,
      received: progress.received,
      expiresAt: progress.expiresAt,
    };
  } catch (err) {
    if (err instanceof ApiError && (err.status === 404 || err.status === 403)) {
      forgetSession(checksum, parentPath, fileName);
      return null;
    }
    throw err;
  }
}

async function putChunkWithRetry(
  sessionId: string,
  index: number,
  data: Blob,
  signal?: AbortSignal,
  attempts = 4,
): Promise<void> {
  let lastError: unknown;
  for (let attempt = 0; attempt < attempts; attempt++) {
    if (signal?.aborted) {
      throw new DOMException("已取消", "AbortError");
    }
    try {
      await uploadApi.chunk(sessionId, index, data, signal);
      return;
    } catch (err) {
      lastError = err;
      if (isAbortError(err)) {
        throw err;
      }
      // 4xx 里除了限流以外都是确定性拒绝，重试没有意义。
      if (err instanceof ApiError && err.status >= 400 && err.status < 500 && err.status !== 429) {
        throw err;
      }
      // 指数退避 + 抖动：整批分片同时重试会把上游再次打垮。
      const backoff = Math.min(8000, 400 * 2 ** attempt) + Math.random() * 200;
      // 服务端给的 Retry-After 也要封顶：自建退避封了 8s，透传的服务端值
      // 却是原样使用，一个 Retry-After: 3600 能让一个分片在界面上挂一小时
      // 而看不出任何异常。超上限就按封顶值等，真不行下一轮自然失败。
      const retryAfter = err instanceof ApiError && err.retryAfter
        ? Math.min(err.retryAfter * 1000, 30_000)
        : 0;
      await sleep(retryAfter || backoff, signal);
    }
  }
  throw lastError instanceof Error ? lastError : new Error("分片上传失败");
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(new DOMException("已取消", "AbortError"));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/** 分片读取整个文件求摘要。大文件上必须分片，否则会一次性读进内存。 */
async function hashFile(file: File, signal?: AbortSignal): Promise<string> {
  const sliceSize = 4 * 1024 * 1024;
  const hasher = new Sha256();
  for (let offset = 0; offset < file.size; offset += sliceSize) {
    if (signal?.aborted) {
      throw new DOMException("已取消", "AbortError");
    }
    const buffer = await file.slice(offset, Math.min(offset + sliceSize, file.size)).arrayBuffer();
    hasher.update(new Uint8Array(buffer));
  }
  return bytesToHex(hasher.digest());
}

function sessionKey(checksum: string, parentPath: string, fileName: string): string {
  return `${SESSION_PREFIX}${checksum}|${parentPath}|${fileName}`;
}

function rememberSession(
  checksum: string,
  parentPath: string,
  fileName: string,
  sessionId: string,
): void {
  const record: StoredSession = { sessionId, checksum, savedAt: Date.now() };
  try {
    localStorage.setItem(sessionKey(checksum, parentPath, fileName), JSON.stringify(record));
  } catch {
    // 存不下只影响断点续传，上传本身照常。
  }
}

function readSession(
  checksum: string,
  parentPath: string,
  fileName: string,
): StoredSession | null {
  try {
    const raw = localStorage.getItem(sessionKey(checksum, parentPath, fileName));
    if (!raw) {
      return null;
    }
    const parsed = JSON.parse(raw) as StoredSession;
    if (!parsed.sessionId || Date.now() - parsed.savedAt > SESSION_MAX_AGE_MS) {
      forgetSession(checksum, parentPath, fileName);
      return null;
    }
    return parsed;
  } catch {
    return null;
  }
}

function forgetSession(checksum: string, parentPath: string, fileName: string): void {
  try {
    localStorage.removeItem(sessionKey(checksum, parentPath, fileName));
  } catch {
    // 忽略。
  }
}
