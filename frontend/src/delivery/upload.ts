import { ApiError } from "@/api/client";
import { uploadApi } from "@/api/endpoints";
import type { ConflictAction, InitUploadResult, Node, UploadJobStatus, UploadProgress } from "@/api/types";
import { Sha256, bytesToHex } from "@/crypto/sha256";
import { isAbortError } from "@/lib/async";

// 分片上传编排。
//
// 内容摘要与逻辑路径分离。新会话先开始暂存分片，摘要在后台并行计算；摘要
// 命中已有内容时撤销暂存会话并立即返回，未命中时再绑定摘要并提交收尾任务。

export interface UploadProgressInfo {
  fileName: string;
  /** 当前阶段已处理字节数；前端只在传输阶段展示字节计数。 */
  sent: number;
  total: number;
  /** 哈希与传输并行时表示两项客户端工作的合成进度；服务器收尾阶段为 null。 */
  ratio: number | null;
  phase: "hashing" | "uploading" | "finishing" | "done";
  message: string;
  /** 后端队列仍未领取任务时允许取消；processing 阶段不允许取消。 */
  canCancel?: boolean;
}

export interface UploadOutcome {
  node: Node;
  /** true 表示复用服务端已有对象；并行哈希期间可能已发送部分分片。 */
  dedup: boolean;
}

export interface UploadOptions {
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
  parentPath?: string;
  fileName?: string;
  fileSize?: number;
  lastModified?: number;
}

let workerSessionStore: Pick<Storage, "length" | "key" | "getItem" | "setItem" | "removeItem"> | undefined;
export function configureUploadSessionStore(store: NonNullable<typeof workerSessionStore>): void {
  workerSessionStore = store;
}
function sessionStore() { return workerSessionStore ?? localStorage; }

const SESSION_PREFIX = "xph.upload.";
/** 会话有效期短于服务端的过期时间，避免拿着已经失效的会话去续传。 */
const SESSION_MAX_AGE_MS = 12 * 60 * 60 * 1000;
const CLIENT_WORK_PROGRESS = 0.75;
const SERVER_FINALIZE_PROGRESS = 0.24;

export async function uploadFile(file: File, options: UploadOptions): Promise<UploadOutcome> {
  options.signal?.throwIfAborted();
  const report = options.onProgress ?? (() => {});
  const total = file.size;
  const hashController = new AbortController();
  const chunksController = new AbortController();
  const hashSignal = combineSignals(options.signal, hashController.signal);
  const signal = combineSignals(options.signal, chunksController.signal);
  try {
    let hashRatio = 0;
    let checksum = "";
    let hashDone = false;
    let transferStarted = false;
    let transferSent = 0;
    report({
      fileName: file.name,
      sent: 0,
      total,
      ratio: 0,
      phase: "hashing",
      message: "正在计算校验码",
    });
    // 本地摘要与临时会话初始化同时启动，避免哈希阶段空等网络请求。
    const hashPromise = hashFile(file, hashSignal, (processed) => {
      hashRatio = total > 0 ? processed / total : 1;
      const transferRatio = total > 0 ? transferSent / total : 0;
      report({
        fileName: file.name,
        sent: transferStarted ? transferSent : processed,
        total,
        ratio: Math.min(0.99, CLIENT_WORK_PROGRESS * (hashRatio + transferRatio) / 2),
        phase: transferStarted ? "uploading" : "hashing",
        message: transferStarted
          ? `正在发送 ${Math.round(Math.min(0.99, transferSent / Math.max(total, 1)) * 100)}%，校验 ${Math.round(hashRatio * 100)}%`
          : `正在校验 ${Math.round(hashRatio * 100)}%`,
      });
    }).then((value) => {
      checksum = value;
      hashDone = true;
      hashRatio = 1;
      return value;
    });
    // 会话初始化可能比本地哈希更慢，提前挂起 rejection handler 避免短暂的未处理拒绝。
    void hashPromise.catch(() => {});
    const initSession = () => uploadApi.init({
      checksum: hashDone ? checksum : undefined,
      sizePlain: total,
      parentPath: options.parentPath,
      name: file.name,
      conflictAction: options.conflictAction ?? "rename",
    });
    let session: InitUploadResult;
    // 已有断点会话时先算摘要确认本地文件身份，再安全续传；新上传则立即建会话，
    // 哈希和分片网络传输并行进行。
    if (hasStoredSession(options.parentPath, file.name)) {
      checksum = await hashPromise;
      const resumed = await tryResume(checksum, options.parentPath, file);
      session = resumed ?? await initSession();
    } else {
      session = await initSession();
    }
    if (session.dedup && session.node) {
      report({ fileName: file.name, sent: total, total, ratio: 1, phase: "done", message: "已秒传完成" });
      return { node: session.node, dedup: true };
    }

    if (session.completedNode) {
      report({ fileName: file.name, sent: total, total, ratio: 1, phase: "done", message: "已完成", canCancel: false });
      forgetSession(checksum, options.parentPath, file, session.sessionId);
      return { node: session.completedNode, dedup: false };
    }

    const sessionId = session.sessionId;
    const chunkSize = session.chunkSize ?? 0;
    const chunkTotal = session.chunkTotal ?? 0;
    if (!sessionId) {
      throw new Error("上传会话缺少标识");
    }

    options.onSessionId?.(sessionId);
    rememberSession(checksum, options.parentPath, file, sessionId);
    if (signal.aborted) {
      await uploadApi.cancel(sessionId).catch(() => {});
      forgetSession(checksum, options.parentPath, file, sessionId);
      throw new DOMException("已取消", "AbortError");
    }
    if (session.finalizing) {
      // 服务端已接收并处理文件，无需让并行摘要继续占用客户端工作额度。
      hashController.abort();
      await hashPromise.catch(() => {});
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
      forgetSession(checksum, options.parentPath, file, sessionId);
      return { node, dedup: false };
    }

    if (chunkSize <= 0 || (chunkTotal <= 0 && total > 0)) {
      // 空文件没有分片，但仍需绑定 SHA-256 并走服务器收尾流程。
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

    const inFlight = new Map<number, number>();
    const chunkLength = (index: number) => Math.min(chunkSize, total - index * chunkSize);
    let confirmedBytes = 0;
    for (let i = 0; i < chunkTotal; i++) if (!pending.has(i)) confirmedBytes += chunkLength(i);
    const reportTransfer = () => {
      let sent = confirmedBytes;
      for (const [index, loaded] of inFlight) sent += Math.min(chunkLength(index), loaded);
      sent = Math.min(total, sent);
      transferSent = sent;
      report({
        fileName: file.name,
        sent,
        total,
        ratio: Math.min(0.99, CLIENT_WORK_PROGRESS * (hashRatio + (total > 0 ? sent / total : 0)) / 2),
        phase: "uploading",
        message: hashDone
          ? `校验完成，正在发送 ${Math.min(99, Math.round((sent / Math.max(total, 1)) * 100))}%`
          : `正在发送 ${Math.min(99, Math.round((sent / Math.max(total, 1)) * 100))}%，校验 ${Math.round(hashRatio * 100)}%`,
      });
    };
    transferStarted = true;
    reportTransfer();

    const concurrency = Math.max(1, options.concurrency ?? 3);
    const queue = [...pending];
    let nextChunk = 0;

    const pump = async (): Promise<void> => {
      for (;;) {
        if (signal.aborted) {
          throw new DOMException("已取消", "AbortError");
        }
        const index = queue[nextChunk++];
        if (index === undefined) {
          return;
        }
        const start = index * chunkSize;
        const end = Math.min(start + chunkSize, total);
        const slice = file.slice(start, end);
        await putChunkWithRetry(sessionId, index, slice, signal, (loaded) => {
          inFlight.set(index, loaded);
          reportTransfer();
        });
        inFlight.delete(index);
        confirmedBytes += chunkLength(index);
        reportTransfer();
      }
    };

    let transferFailed = false;
    const handleClientWorkFailure = async (error: unknown): Promise<never> => {
      chunksController.abort();
      hashController.abort();
      const permanentRejection = error instanceof ApiError &&
        error.status >= 400 && error.status < 500 && error.status !== 429;
      if (options.signal?.aborted || !transferFailed || permanentRejection) {
        if (!options.signal?.aborted) await uploadApi.cancel(sessionId).catch(() => {});
        forgetSession(checksum, options.parentPath, file, sessionId);
      }
      throw error;
    };
    const transferPromise = Promise.all(Array.from({ length: Math.min(concurrency, queue.length) }, pump)).catch((error) => {
      transferFailed = true;
      chunksController.abort();
      throw error;
    });
    void transferPromise.catch(() => {});
    const transferFailure = transferPromise.then(() => new Promise<never>(() => {}));

    try {
      checksum = await Promise.race([hashPromise, transferFailure]);
      hashRatio = 1;
      rememberSession(checksum, options.parentPath, file, sessionId);
      reportTransfer();
    } catch (error) {
      await handleClientWorkFailure(error);
    }

    let resolveResult: { dedup: boolean; node?: Node | null };
    let delay = 250;
    const resolveDeadline = Date.now() + 60_000;
    try {
      for (;;) {
        try {
          resolveResult = await uploadApi.resolve(sessionId, checksum, options.signal);
          break;
        } catch (error) {
          if (!(error instanceof ApiError) || error.status !== 409 || options.signal?.aborted || Date.now() >= resolveDeadline) {
            throw error;
          }
          await sleep(delay, options.signal);
          delay = Math.min(2000, Math.round(delay * 1.5));
        }
      }
    } catch (error) {
      // 摘要绑定失败时停掉剩余分片，避免 UI 已报错但仍持续占用带宽。
      // 会话和已收分片保留，用户重试时可从断点继续。
      chunksController.abort();
      throw error;
    }

    if (resolveResult.dedup && resolveResult.node) {
      chunksController.abort();
      void transferPromise.catch(() => {});
      forgetSession(checksum, options.parentPath, file, sessionId);
      report({ fileName: file.name, sent: total, total, ratio: 1, phase: "done", message: "已秒传完成", canCancel: false });
      return { node: resolveResult.node, dedup: true };
    }

    try {
      await transferPromise;
    } catch (error) {
      await handleClientWorkFailure(error);
    }

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
    forgetSession(checksum, options.parentPath, file, sessionId);
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
  } finally {
    // 每条退出路径都停止后台工作，并解除兼容实现中的父信号监听。
    hashController.abort();
    chunksController.abort();
  }
}

function combineSignals(parent: AbortSignal | undefined, local: AbortSignal): AbortSignal {
  if (!parent) return local;
  if (typeof AbortSignal.any === "function") return AbortSignal.any([parent, local]);
  const controller = new AbortController();
  const cleanup = () => {
    parent.removeEventListener("abort", abort);
    local.removeEventListener("abort", abort);
  };
  const abort = () => {
    cleanup();
    controller.abort();
  };
  if (parent.aborted || local.aborted) abort();
  else {
    parent.addEventListener("abort", abort, { once: true });
    local.addEventListener("abort", abort, { once: true });
  }
  return controller.signal;
}

function hasStoredSession(parentPath: string, fileName: string): boolean {
  try {
    const suffix = `|${parentPath}|${fileName}`;
    for (let index = 0; index < sessionStore().length; index++) {
      const key = sessionStore().key(index);
      if (!key?.startsWith(SESSION_PREFIX)) continue;
      const record = parseStoredSession(sessionStore().getItem(key));
      if (!isStoredSessionValid(record)) continue;
      if (key.endsWith(suffix)) return true; // 兼容旧版摘要键
      if (key.startsWith(`${SESSION_PREFIX}session.`) &&
          record.parentPath === parentPath && record.fileName === fileName) return true;
    }
  } catch {
    // 禁用 localStorage 时按新会话上传。
  }
  return false;
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
  file: File,
): Promise<InitUploadResult | null> {
  const stored = readSession(checksum, parentPath, file);
  if (!stored) {
    return null;
  }
  try {
    const progress = await uploadApi.status(stored.sessionId);
    if (progress.state === "error") {
      forgetSession(checksum, parentPath, file, stored.sessionId);
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
      forgetSession(checksum, parentPath, file, stored.sessionId);
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
  const stageRatio = uploadJobStageRatio(status, fallbackTotal);
  if (stageRatio === null) return null;
  return CLIENT_WORK_PROGRESS + SERVER_FINALIZE_PROGRESS * stageRatio;
}

function uploadJobStageRatio(status: UploadJobStatus | UploadProgress, fallbackTotal: number): number | null {
  const total = status.totalBytes || fallbackTotal;
  const done = status.progressBytes ?? 0;
  if (total <= 0 || done <= 0) return null;
  return Math.min(0.99, done / total);
}

function uploadJobMessage(status: UploadJobStatus | UploadProgress): string {
  if (status.state === "queued") return "等待服务器队列处理";
  if (status.state === "receiving") return "服务器在处理已接收部分，继续接收文件";
  const ratio = uploadJobStageRatio(status, status.totalBytes ?? 0);
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
      onProgress?.(0);
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
  if (signal?.aborted) return Promise.reject(new DOMException("已取消", "AbortError"));
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
  // 整条上传链已运行在传输 Worker 内，不再创建嵌套校验线程。
  const sliceSize = 4 * 1024 * 1024;
  const hasher = new Sha256();
  for (let offset = 0; offset < file.size; offset += sliceSize) {
    if (signal?.aborted) {
      throw new DOMException("已取消", "AbortError");
    }
    const buffer = await file.slice(offset, Math.min(offset + sliceSize, file.size)).arrayBuffer();
    signal?.throwIfAborted();
    hasher.update(new Uint8Array(buffer));
    onProgress?.(Math.min(offset + buffer.byteLength, file.size));
  }
  return bytesToHex(hasher.digest());
}

function sessionKey(checksum: string, parentPath: string, fileName: string): string {
  return `${SESSION_PREFIX}${checksum}|${parentPath}|${fileName}`;
}

function sessionRecordKey(sessionId: string): string {
  return `${SESSION_PREFIX}session.${sessionId}`;
}

function rememberSession(
  checksum: string,
  parentPath: string,
  file: File,
  sessionId: string,
): void {
  const record: StoredSession = {
    sessionId,
    checksum,
    savedAt: Date.now(),
    parentPath,
    fileName: file.name,
    fileSize: file.size,
    lastModified: file.lastModified,
  };
  try {
    sessionStore().setItem(sessionRecordKey(sessionId), JSON.stringify(record));
  } catch {
    // 存不下只影响断点续传，上传本身照常。
  }
}

function readSession(
  checksum: string,
  parentPath: string,
  file: File,
): StoredSession | null {
  try {
    const legacy = parseStoredSession(sessionStore().getItem(sessionKey(checksum, parentPath, file.name)));
    if (isStoredSessionValid(legacy)) return legacy;

    const candidates: StoredSession[] = [];
    for (let index = 0; index < sessionStore().length; index++) {
      const key = sessionStore().key(index);
      if (!key?.startsWith(`${SESSION_PREFIX}session.`)) continue;
      const parsed = parseStoredSession(sessionStore().getItem(key));
      if (!isStoredSessionValid(parsed)) continue;
      if (parsed.parentPath !== parentPath || parsed.fileName !== file.name ||
          parsed.fileSize !== file.size || parsed.lastModified !== file.lastModified) continue;
      if (parsed.checksum && parsed.checksum !== checksum) continue;
      candidates.push(parsed);
    }
    candidates.sort((a, b) => Number(b.checksum === checksum) - Number(a.checksum === checksum) || b.savedAt - a.savedAt);
    return candidates[0] ?? null;
  } catch {
    return null;
  }
}

function parseStoredSession(raw: string | null): StoredSession | null {
  if (!raw) return null;
  try {
    return JSON.parse(raw) as StoredSession;
  } catch {
    return null;
  }
}

function isStoredSessionValid(record: StoredSession | null): record is StoredSession {
  return Boolean(record?.sessionId && Date.now() - record.savedAt <= SESSION_MAX_AGE_MS);
}

function forgetSession(checksum: string, parentPath: string, file: File, sessionId?: string): void {
  try {
    if (sessionId) {
      const keys: string[] = [];
      for (let index = 0; index < sessionStore().length; index++) {
        const key = sessionStore().key(index);
        if (key?.startsWith(SESSION_PREFIX) &&
            parseStoredSession(sessionStore().getItem(key))?.sessionId === sessionId) keys.push(key);
      }
      for (const key of keys) sessionStore().removeItem(key);
    }
    if (checksum && !sessionId) sessionStore().removeItem(sessionKey(checksum, parentPath, file.name));
  } catch {
    // 忽略。
  }
}
