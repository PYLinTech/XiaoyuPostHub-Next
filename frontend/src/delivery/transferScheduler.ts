/** 上传、下载与附件预览共用执行额度，避免大量工作线程争抢 CPU。 */
const MAX_CLIENT_WORK = 2;
let active = 0;
const waiting: Array<() => void> = [];
export function acquireTransferSlot(signal?: AbortSignal): Promise<() => void> {
  return new Promise((resolve, reject) => {
    const abort = () => {
      const index = waiting.indexOf(start);
      if (index >= 0) waiting.splice(index, 1);
      signal?.removeEventListener("abort", abort);
      reject(new DOMException("已取消", "AbortError"));
    };
    const start = () => {
      signal?.removeEventListener("abort", abort);
      active++;
      let released = false;
      resolve(() => {
        if (released) return;
        released = true;
        active--;
        waiting.shift()?.();
      });
    };
    if (signal?.aborted) { abort(); return; }
    if (active < MAX_CLIENT_WORK) start();
    else {
      waiting.push(start);
      signal?.addEventListener("abort", abort, { once: true });
    }
  });
}
