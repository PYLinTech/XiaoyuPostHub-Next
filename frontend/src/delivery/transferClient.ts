import type { DeliveryPlan } from "@/api/types";
import { acquireTransferSlot } from "./transferScheduler";
import { getToken, notifyUnauthorized } from "@/api/client";
import { settleQuietly, tryOpenDiskSink, type DeliverySource, type DeliveryOptions, type DeliveryResult, type DiskSink, type DeliveryProgress } from "./download";
import type { UploadOptions, UploadOutcome, UploadProgressInfo } from "./upload";
import { packError, unpackError, type Start, type HostMessage, type WorkerMessage } from "./transferProtocol";

type TransferInput = Omit<Extract<Start, { kind: "upload" }>, "token" | "sessions"> | Omit<Extract<Start, { kind: "download" }>, "token">;

/** 每个活动任务有独立执行线程；文件内容不进入 Vue 状态。 */
async function execute<T>(start: TransferInput, options: DeliveryOptions | UploadOptions, source?: DeliverySource): Promise<T> {
  const release = await acquireTransferSlot(options.signal);
  try {
    return await new Promise<T>((resolve, reject) => {
      if (options.signal?.aborted) { reject(new DOMException("已取消", "AbortError")); return; }
      const worker = new Worker(new URL("./transfer.worker.ts", import.meta.url), { type: "module" });
      let sink: DiskSink | null = null;
      let activePlan: DeliveryPlan | null = null;
      let finished = false;
      let observerError: Error | null = null;
      const cancel = () => send({ type: "cancel" });
      const cleanup = () => {
        finished = true;
        options.signal?.removeEventListener("abort", cancel);
        worker.terminate();
      };
      const fail = (error: Error, crashed = false) => {
        if (finished) return;
        cleanup();
        void sink?.abort().catch(() => {});
        if (crashed && activePlan) void settleQuietly(activePlan);
        reject(error);
      };
      const send = (message: HostMessage) => {
        if (finished) return;
        try { worker.postMessage(message); }
        catch (error) { fail(error instanceof Error ? error : new Error(String(error)), true); }
      };
      const notify = (callback: () => void) => {
        if (observerError) return;
        try { callback(); }
        catch (error) {
          observerError = error instanceof Error ? error : new Error(String(error));
          cancel();
        }
      };
      options.signal?.addEventListener("abort", cancel, { once: true });
      worker.onerror = (event) => fail(new Error(event.message || "传输线程异常"), true);
      worker.onmessageerror = () => fail(new Error("传输线程消息无法读取"), true);
      worker.onmessage = async (event: MessageEvent<WorkerMessage>) => {
        if (finished) return;
        const message = event.data;
        switch (message.type) {
          case "progress":
            // 上传校验与发送已结束；等待服务器收尾不阻塞其它文件的处理。
            if (start.kind === "upload" && message.value.phase === "finishing") release();
            notify(() => {
              if (start.kind === "upload") (options as UploadOptions).onProgress?.(message.value as UploadProgressInfo);
              else (options as DeliveryOptions).onProgress?.(message.value as DeliveryProgress);
            });
            break;
          case "session": notify(() => (options as UploadOptions).onSessionId?.(message.value)); break;
          case "storage":
            try { if (message.value === null) localStorage.removeItem(message.key); else localStorage.setItem(message.key, message.value); } catch { /* 禁用存储不阻断上传。 */ }
            break;
          case "unauthorized":
            if (message.token === getToken()) notifyUnauthorized();
            break;
          case "result":
            cleanup();
            if (observerError) reject(observerError); else resolve(message.value as T);
            break;
          case "error": fail(observerError ?? unpackError(message.error)); break;
          case "request": {
            try {
              let value: unknown;
              switch (message.method) {
                case "plan": {
                  if (!source) throw new Error("缺少交付来源");
                  const publicKeyBase64 = message.value as string | null;
                  const plan = await source.plan(publicKeyBase64 === null ? null : {
                    publicKeyBase64,
                    async unwrap() { throw new Error("私钥只保留在传输线程"); },
                  });
                  if (finished) { await settleQuietly(plan); return; }
                  activePlan = plan;
                  value = plan;
                  break;
                }
                case "open":
                  sink = await ((options as DeliveryOptions).openDiskSink ?? tryOpenDiskSink)(message.value as string);
                  if (finished) { await sink?.abort(); return; }
                  value = sink?.name ?? null;
                  break;
                case "write": if (!sink) throw new Error("保存通道已关闭"); await sink.write(message.value as Uint8Array); break;
                case "close": await sink?.close(); sink = null; break;
                case "abort": await sink?.abort(); sink = null; break;
              }
              if (!finished) send({ type: "reply", id: message.id, value });
            } catch (error) {
              if (!finished) send({ type: "reply", id: message.id, error: packError(error) });
            }
            break;
          }
        }
      };
      send(start.kind === "upload"
        ? { ...start, token: getToken(), sessions: readUploadSessions() }
        : { ...start, token: getToken() });
    });
  } finally {
    release();
  }
}
function readUploadSessions(): [string, string][] {
  const sessions: [string, string][] = [];
  try {
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i);
      if (key?.startsWith("xph.upload.")) { const value = localStorage.getItem(key); if (value !== null) sessions.push([key, value]); }
    }
  } catch { /* 无持久会话时创建新上传。 */ }
  return sessions;
}
export function uploadFile(file: File, options: UploadOptions): Promise<UploadOutcome> {
  return execute({ type: "start", kind: "upload", file,
    parentPath: options.parentPath, conflictAction: options.conflictAction, concurrency: options.concurrency }, options);
}
export function runDelivery(source: DeliverySource, options: DeliveryOptions = {}): Promise<DeliveryResult> {
  return execute({ type: "start", kind: "download", streamToDiskAbove: options.streamToDiskAbove }, options, source);
}
