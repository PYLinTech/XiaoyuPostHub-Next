import { setToken, setUnauthorizedHandler } from "@/api/client";
import { runDelivery, type DeliveryProgress } from "./download";
import { configureUploadSessionStore, uploadFile, type UploadProgressInfo } from "./upload";
import { packError, unpackError, type HostMessage, type WorkerMessage, type Start } from "./transferProtocol";
import type { DeliveryPlan } from "@/api/types";

const controller = new AbortController();
const pending = new Map<number, { resolve: (value: unknown) => void; reject: (error: Error) => void }>();
let nextId = 0;
let started = false;
const send = (message: WorkerMessage) => self.postMessage(message);
function request(method: "plan" | "open" | "write" | "close" | "abort", value?: unknown): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const id = ++nextId;
    pending.set(id, { resolve, reject });
    try {
      if (method === "write" && value instanceof Uint8Array) {
        // 在执行线程复制后转移所有权，避免显示线程反序列化整块内容。
        const chunk = value.slice();
        self.postMessage({ type: "request", id, method, value: chunk }, [chunk.buffer]);
      } else send({ type: "request", id, method, value });
    } catch (error) {
      pending.delete(id);
      reject(error instanceof Error ? error : new Error(String(error)));
    }
  });
}
// 数据处理留在此线程，显示层每秒最多接收十次同阶段快照。
let lastReport = 0;
let lastPhase = "";
let lastCanCancel: boolean | undefined;
function report(value: DeliveryProgress | UploadProgressInfo) {
  const now = performance.now();
  const canCancel = "canCancel" in value ? value.canCancel : undefined;
  if (value.phase !== lastPhase || canCancel !== lastCanCancel || now - lastReport >= 100) {
    lastPhase = value.phase;
    lastCanCancel = canCancel;
    lastReport = now;
    send({ type: "progress", value });
  }
}
async function execute(message: Start) {
  setToken(message.token);
  setUnauthorizedHandler(() => send({ type: "unauthorized", token: message.token }));
  try {
    if (message.kind === "upload") {
      const records = new Map(message.sessions);
      configureUploadSessionStore({
        get length() { return records.size; },
        key: (index) => [...records.keys()][index] ?? null,
        getItem: (key) => records.get(key) ?? null,
        setItem(key, value) { records.set(key, value); send({ type: "storage", key, value }); },
        removeItem(key) { records.delete(key); send({ type: "storage", key, value: null }); },
      });
      const value = await uploadFile(message.file, {
        parentPath: message.parentPath, conflictAction: message.conflictAction, concurrency: message.concurrency,
        signal: controller.signal, onProgress: report, onSessionId: (value) => send({ type: "session", value }),
      });
      send({ type: "result", value });
    } else {
      const value = await runDelivery({
        plan: (pair) => request("plan", pair?.publicKeyBase64 ?? null) as Promise<DeliveryPlan>,
      }, {
        signal: controller.signal, onProgress: report, streamToDiskAbove: message.streamToDiskAbove,
        async openDiskSink(fileName) {
          const name = await request("open", fileName) as string | null;
          return name === null ? null : {
            name,
            async write(chunk) { await request("write", chunk); },
            async close() { await request("close"); },
            async abort() { await request("abort"); },
          };
        },
      });
      send({ type: "result", value });
    }
  } catch (error) { send({ type: "error", error: packError(error) }); }
}
self.onmessage = (event: MessageEvent<HostMessage>) => {
  const message = event.data;
  if (message.type === "cancel") controller.abort();
  if (message.type === "start" && !started) { started = true; void execute(message); }
  if (message.type === "reply") {
    const waiter = pending.get(message.id);
    pending.delete(message.id);
    if (message.error) waiter?.reject(unpackError(message.error));
    else waiter?.resolve(message.value);
  }
};
