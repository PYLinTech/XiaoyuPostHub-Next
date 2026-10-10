import type { getDocument, PDFDocumentLoadingTask, PDFDocumentProxy } from "pdfjs-dist";

/** PDF.js 的 loadingTask 同时负责请求和文档释放，不能只取消尚未完成的请求。 */
export function createPdfSession(loadDocument: typeof getDocument) {
  let current: PDFDocumentLoadingTask | null = null;
  let generation = 0;
  let disposed = false;
  const stopped = new WeakSet<PDFDocumentLoadingTask>();
  const stop = (task: PDFDocumentLoadingTask | null) => {
    if (!task || stopped.has(task)) return;
    stopped.add(task);
    void task.destroy().catch(() => {});
  };
  const releaseDocument = () => {
    stop(current);
    current = null;
  };
  return {
    get disposed() { return disposed; },
    releaseDocument,
    async load(options: Parameters<typeof getDocument>[0]): Promise<PDFDocumentProxy | null> {
      if (disposed) return null;
      const token = ++generation;
      releaseDocument();
      const task = loadDocument(options);
      current = task;
      try {
        const document = await task.promise;
        return disposed || token !== generation ? null : document;
      } catch (error) {
        if (disposed || token !== generation) return null;
        releaseDocument();
        throw error;
      }
    },
    dispose() {
      disposed = true;
      generation++;
      releaseDocument();
    },
  };
}
