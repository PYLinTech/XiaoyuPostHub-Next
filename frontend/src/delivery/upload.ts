import { ApiError } from "@/api/client";
import { uploadApi } from "@/api/endpoints";
import type { ConflictAction, InitUploadResult, Node, UploadJobStatus, UploadProgress } from "@/api/types";
import { Sha256, bytesToHex } from "@/crypto/sha256";
import { isAbortError } from "@/lib/async";

// 分片上传编排。
//
// 后端把"内容"与"引用"分得很开：内容池按明文 SHA-256 去重，节点是用户自己的
// 逻辑路径。因此上传的入口动作是先算校验码——它决定了能否秒传，也决定了
// 断点续传能否找回原来的会话。

export interface UploadProgressInfo {
  fileName: string;
  /** 当前阶段已处理字节数；前端只在传输阶段展示字节计数。 */
  sent: number;
  total: number;
  /** 当前阶段进度；服务器收尾阶段未知时为 null，显示不确定态而不是虚假的 100%。 */
  ratio: number | null;
  phase: "hashing" | "uploading" | "finishing" | "done";
  message: string;
  /** 后端队列仍未领取任务时允许取消；processing 阶段不允许取消。 */
  canCancel?: boolean;
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
  onSessionId?: (sessionId: string) => void;
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
  const checksum = await hashFile(file, options.signal, (processed) => {
    const ratio = total > 0 ? processed / total : 1;
    report({
      fileName: file.name,
      sent: processed,
      total,
      ratio,
      phase: "hashing",
      message: `正在校验 ${Math.round(ratio * 100)}%`,
    });
  });

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

  if (session.completedNode) {
    report({ fileName: file.name, sent: total, total, ratio: 1, phase: "done", message: "已完成", canCancel: false });
    forgetSession(checksum, options.parentPath, file.name);
    return { node: session.completedNode, dedup: false };
  }

  const sessionId = session.sessionId;
  const chunkSize = session.chunkSize ?? 0;
  const chunkTotal = session.chunkTotal ?? 0;
  if (!sessionId) {
    throw new Error("上传会话缺少标识");
  }

  options.onSessionId?.(sessionId);
  rememberSession(checksum, options.parentPath, file.name, sessionId);

  if (session.finalizing) {
    report({
      fileName: file.name,
      sent: total,
      total,
      ratio: null,
      phase: "finishing",
      message: session.jobState === "queued" ? "等待服务器队列处理" : "服务器正在校验、加密并保存",
      canCancel: session.jobState === "queued",
    });
    const node = await waitForUploadJob(sessionId, file, report, options.signal);
    forgetSession(checksum, options.parentPath, file.name);
    return { node, dedup: false };
  }

  if (chunkSize <= 0 || chunkTotal <= 0) {
    // 空文件不会产生分片：服务端在 init 阶段就已经把节点建好了。
    if (session.node) {
      return { node: session.node, dedup: false };
    }
    throw new Error("上传会话缺少分片计划");
  }

  const pending = new Set<number>();
  for (let i = 0; i < chunkTotal; i++) {
    pending.add(i);
  }
  for (const index of session.received ?? []) {
    pending.delete(index);
  }

  const confirmed = new Set<number>();
  for (let i = 0; i < chunkTotal; i++) if (!pending.has(i)) confirmed.add(i);
  const inFlight = new Map<number, number>();
  const chunkLength = (index: number) => Math.min(chunkSize, total - index * chunkSize);
  const reportTransfer = () => {
    let sent = 0;
    for (const index of confirmed) sent += chunkLength(index);
    for (const [index, loaded] of inFlight) {
      if (!confirmed.has(index)) sent += Math.min(chunkLength(index), loaded);
    }
    sent = Math.min(total, sent);
    report({
      fileName: file.name,
      sent,
      total,
      ratio: total > 0 ? sent / total : 1,
      phase: "uploading",
      message: `正在发送 ${Math.min(100, Math.round((sent / Math.max(total, 1)) * 100))}%`,
    });
  };
  reportTransfer();

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
      await putChunkWithRetry(sessionId, index, slice, options.signal, (loaded) => {
        inFlight.set(index, loaded);
        reportTransfer();
      });
      inFlight.delete(index);
      confirmed.add(index);
      reportTransfer();
    }
  };

  await Promise.all(Array.from({ length: Math.min(concurrency, queue.length) }, pump));

  try {
    report({
      fileName: file.name,
      sent: total,
      total,
      ratio: null,
      phase: "finishing",
      message: "分片已接收，正在提交服务器收尾队列",
    });
    const initial = await uploadApi.complete(sessionId);
    report({
      fileName: file.name,
      sent: total,
      total,
      ratio: uploadJobRatio(initial, total),
      phase: "finishing",
      message: uploadJobMessage(initial),
      canCancel: initial.state === "queued" || initial.state === "receiving",
    });
    const node = initial.state === "done" && initial.node
      ? initial.node
      : await waitForUploadJob(sessionId, file, report, options.signal, initial);
    forgetSession(checksum, options.parentPath, file.name);
    report({
      fileName: file.name,
      sent: total,
      total,
      ratio: 1,
      phase: "done",
      message: "已完成",
      canCancel: false,
    });
    return { node, dedup: false };
  } catch (err) {
    // 收尾失败时保留会话标识：若任务仍有效，下次可从状态接口恢复或重试入队。
    throw err;
  }
}

/** 请求服务端取消尚未开始处理的上传任务。 */
export async function cancelUpload(sessionId: string): Promise<boolean> {
  try {
    await uploadApi.cancel(sessionId);
    return true;
  } catch {
    // 会话可能已经过期或完成。
    return false;
  }
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
    if (progress.state === "error") {
      forgetSession(checksum, parentPath, fileName);
      return null;
    }
    if (progress.state === "done" && progress.node) {
      return { dedup: false, sessionId: stored.sessionId, completedNode: progress.node };
    }
    if (progress.state === "queued" || progress.state === "processing") {
      return {
        dedup: false,
        sessionId: stored.sessionId,
        finalizing: true,
        jobState: progress.state,
      };
    }
    return {
      dedup: false,
      sessionId: progress.sessionId,
      chunkSize: progress.chunkSize ?? 0,
      chunkTotal: progress.chunkTotal ?? 0,
      received: progress.received ?? [],
      expiresAt: progress.expiresAt ?? 0,
    };
  } catch (err) {
    if (err instanceof ApiError && (err.status === 404 || err.status === 403)) {
      forgetSession(checksum, parentPath, fileName);
      return null;
    }
    throw err;
  }
}

async function waitForUploadJob(
  sessionId: string,
  file: File,
  report: (info: UploadProgressInfo) => void,
  signal?: AbortSignal,
  initial?: UploadJobStatus,
): Promise<Node> {
  let status: UploadJobStatus | UploadProgress | undefined = initial;
  let delay = 500;
  for (;;) {
    if (signal?.aborted) throw new DOMException("已取消", "AbortError");
    if (!status) {
      try {
        status = await uploadApi.status(sessionId, signal);
      } catch (error) {
        if (isAbortError(error)) throw error;
        if (error instanceof ApiError && error.status >= 400 && error.status < 500 && error.status !== 429) throw error;
        await sleep(delay, signal);
        delay = Math.min(5000, Math.round(delay * 1.5));
        continue;
      }
    }
    if (status.state === "done" && status.node) return status.node;
    if (status.state === "error") throw new Error(status.error || "服务器处理上传失败");
    report({
      fileName: file.name,
      sent: file.size,
      total: file.size,
      ratio: uploadJobRatio(status, file.size),
      phase: "finishing",
      message: uploadJobMessage(status),
      canCancel: status.state === "queued" || status.state === "receiving",
    });
    status = undefined;
    await sleep(delay, signal);
    delay = Math.min(2000, Math.round(delay * 1.25));
  }
}

function uploadJobRatio(status: UploadJobStatus | UploadProgress, fallbackTotal: number): number | null {
  if (status.state === "done") return 1;
  const total = status.totalBytes || fallbackTotal;
  const done = status.progressBytes ?? 0;
  if (total <= 0 || done <= 0) return null;
  return Math.min(0.99, done / total);
}

function uploadJobMessage(status: UploadJobStatus | UploadProgress): string {
  if (status.state === "queued") return "等待服务器队列处理";
  if (status.state === "receiving") return "服务器在处理已接收部分，继续接收文件";
  const ratio = uploadJobRatio(status, status.totalBytes ?? 0);
  if (ratio !== null) return `服务器正在加密并保存 ${Math.round(ratio * 100)}%`;
  return "服务器正在校验、加密并保存";
}

async function putChunkWithRetry(
  sessionId: string,
  index: number,
  data: Blob,
  signal?: AbortSignal,
  onProgress?: (loaded: number) => void,
  attempts = 4,
): Promise<void> {
  let lastError: unknown;
  for (let attempt = 0; attempt < attempts; attempt++) {
    if (signal?.aborted) {
      throw new DOMException("已取消", "AbortError");
    }
    try {
      await uploadApi.chunk(sessionId, index, data, signal, (loaded) => onProgress?.(loaded));
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
async function hashFile(
  file: File,
  signal?: AbortSignal,
  onProgress?: (processed: number) => void,
): Promise<string> {
  if (typeof Worker !== "undefined") {
    try {
      return await hashInWorker(file, signal, onProgress);
    } catch (error) {
      if (isAbortError(error)) throw error;
      // Worker 不可用时退回主线程实现，保证旧浏览器仍能上传。
    }
  }
  const sliceSize = 4 * 1024 * 1024;
  const hasher = new Sha256();
  for (let offset = 0; offset < file.size; offset += sliceSize) {
    if (signal?.aborted) {
      throw new DOMException("已取消", "AbortError");
    }
    const buffer = await file.slice(offset, Math.min(offset + sliceSize, file.size)).arrayBuffer();
    hasher.update(new Uint8Array(buffer));
    onProgress?.(Math.min(offset + buffer.byteLength, file.size));
  }
  return bytesToHex(hasher.digest());
}

function hashInWorker(file: File, signal?: AbortSignal, onProgress?: (processed: number) => void): Promise<string> {
  return new Promise((resolve, reject) => {
    const worker = new Worker(new URL("./hash.worker.ts", import.meta.url), { type: "module" });
    const cleanup = () => {
      signal?.removeEventListener("abort", abort);
      worker.terminate();
    };
    const abort = () => {
      cleanup();
      reject(new DOMException("已取消", "AbortError"));
    };
    signal?.addEventListener("abort", abort, { once: true });
    if (signal?.aborted) return abort();
    worker.onerror = (event) => {
      cleanup();
      reject(new Error(event.message || "文件校验失败"));
    };
    worker.onmessage = (event: MessageEvent<{ type: string; processed?: number; checksum?: string; message?: string }>) => {
      const result = event.data;
      if (result.type === "progress") onProgress?.(result.processed ?? 0);
      if (result.type === "done") {
        cleanup();
        resolve(result.checksum ?? "");
      }
      if (result.type === "error") {
        cleanup();
        reject(new Error(result.message || "文件校验失败"));
      }
    };
    worker.postMessage({ file });
  });
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
