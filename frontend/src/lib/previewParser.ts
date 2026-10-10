import { packError, unpackError } from "@/delivery/transferProtocol";

export type PreviewParseKind = "docx" | "xlsx" | "highlight" | "decrypt" | "markdown";
let worker: Worker | null = null;
let sequence = 0;
let generation = 0;
const pending = new Map<number, { resolve(value: any): void; reject(error: Error): void }>();

/** 一个预览窗口共用一个解析线程，关闭/切换时立即销毁，不留后台解析任务。 */
export function stopPreviewParser(): void {
  generation++;
  worker?.terminate();
  worker = null;
  for (const request of pending.values()) request.reject(new DOMException("已取消", "AbortError"));
  pending.clear();
}
function parsePreview(kind: PreviewParseKind, value: unknown, transfer: Transferable[] = []): Promise<any> {
  if (!worker) {
    worker = new Worker(new URL("./previewParser.worker.ts", import.meta.url), { type: "module" });
    worker.onmessage = ({ data }) => {
      const request = pending.get(data.id);
      if (!request) return;
      pending.delete(data.id);
      if (data.error) request.reject(unpackError(data.error)); else request.resolve(data.value);
    };
    worker.onerror = event => {
      const error = new Error(event.message || "预览解析线程异常");
      for (const request of pending.values()) request.reject(error);
      pending.clear();
      stopPreviewParser();
    };
    worker.onmessageerror = () => stopPreviewParser();
  }
  const id = ++sequence;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    try { worker!.postMessage({ id, kind, value }, transfer); }
    catch (error) { pending.delete(id); reject(unpackError(packError(error))); }
  });
}
export function createPreviewParser() {
  const token = generation;
  function parse(kind: PreviewParseKind, value: unknown, transfer: Transferable[] = []): Promise<any> {
    if (token !== generation) return Promise.reject(new DOMException("已取消", "AbortError"));
    return parsePreview(kind, value, transfer);
  }
  return {
    parse,
    cancel() { if (token === generation) stopPreviewParser(); },
    highlight(code: string, options: { lang: string; theme: string; transformers?: unknown[] }): Promise<string> {
      // 发布包唯一的 transformer 是行号函数，函数不能 structured-clone。
      const { transformers, ...serializable } = options;
      return parse("highlight", { code, options: serializable, lineNumbers: !!transformers?.length });
    },
  };
}
