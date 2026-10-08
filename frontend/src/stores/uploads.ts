import { computed, reactive } from "vue";
import type { ConflictAction, Node } from "@/api/types";
import { cancelUpload, uploadFile, type UploadProgressInfo } from "@/delivery/upload";
import { describeError, isAbortError } from "@/lib/async";
import { useSession } from "./session";
import { useToasts } from "./toast";

// 全局上传队列。
//
// 上传放在全局而不是文件页内部，是因为它天然是"后台任务"：用户点了上传之后
// 往往会切到别的页面去看别的东西，此时进度条不能跟着页面一起消失，
// 也不能因为页面卸载而中断。
//
// 两级并发都由后端设置驱动：文件级"前端最大任务数"（pump 的水位线），
// 文件内"前端最大并发数"（分片 worker 数，见 delivery/upload.ts）。

export type UploadStatus = "queued" | "running" | "done" | "error" | "canceled";

export interface UploadItem {
  id: string;
  file: File;
  parentPath: string;
  conflictAction: ConflictAction;
  status: UploadStatus;
  progress: UploadProgressInfo;
  errorMessage: string;
  node: Node | null;
  dedup: boolean;
  controller: AbortController | null;
  sessionId: string | null;
}

const state = reactive({ items: [] as UploadItem[] });

/** 当前生效的上传运行参数；profile 未到位时使用与后端默认一致的兜底值。 */
function uploadLimits(): { maxTasks: number; maxConcurrency: number } {
  const upload = useSession().state.upload;
  return {
    maxTasks: Math.max(1, upload?.maxTasks ?? 2),
    maxConcurrency: Math.max(1, upload?.maxConcurrency ?? 3),
  };
}

/** 上传完成后的回调（用于刷新目录列表）。 */
const completionListeners = new Set<(node: Node, parentPath: string) => void>();

export function onUploadComplete(listener: (node: Node, parentPath: string) => void): () => void {
  completionListeners.add(listener);
  return () => completionListeners.delete(listener);
}

let nextId = 1;

function emptyProgress(fileName: string, total: number): UploadProgressInfo {
  return { fileName, sent: 0, total, ratio: 0, phase: "hashing", message: "排队中" };
}

/** 入队一批文件，返回它们的队列标识。 */
export function enqueueUploads(
  files: File[],
  parentPath: string,
  conflictAction: ConflictAction = "rename",
): string[] {
  const ids: string[] = [];
  for (const file of files) {
    const id = `up-${nextId++}`;
    state.items.push({
      id,
      file,
      parentPath,
      conflictAction,
      status: "queued",
      progress: emptyProgress(file.name, file.size),
      errorMessage: "",
      node: null,
      dedup: false,
      controller: null,
      sessionId: null,
    });
    ids.push(id);
  }
  void pump();
  return ids;
}

function runningCount(): number {
  return state.items.filter((item) => item.status === "running").length;
}

async function pump(): Promise<void> {
  const { maxTasks } = uploadLimits();
  while (runningCount() < maxTasks) {
    const next = state.items.find((item) => item.status === "queued");
    if (!next) {
      return;
    }
    // 先置为 running 再异步执行：否则并发调用 pump 时同一个文件会被取两次。
    next.status = "running";
    void runItem(next);
  }
}

async function runItem(item: UploadItem): Promise<void> {
  const controller = new AbortController();
  item.controller = controller;
  try {
    const outcome = await uploadFile(item.file, {
      parentPath: item.parentPath,
      conflictAction: item.conflictAction,
      signal: controller.signal,
      concurrency: uploadLimits().maxConcurrency,
      onSessionId: (sessionId) => { item.sessionId = sessionId; },
      onProgress: (info) => {
        item.progress = info;
      },
    });
    item.node = outcome.node;
    item.dedup = outcome.dedup;
    item.status = "done";
    for (const listener of completionListeners) {
      listener(outcome.node, item.parentPath);
    }
  } catch (err) {
    if (isAbortError(err)) {
      item.status = "canceled";
      item.progress = { ...item.progress, message: "已取消" };
    } else {
      item.status = "error";
      item.errorMessage = describeError(err);
      // 失败必须弹出来：上传是在后台跑的，不提示的话用户会以为已经传完了。
      useToasts().error(`${item.file.name} 上传失败`, item.errorMessage);
    }
  } finally {
    item.controller = null;
    if (item.status === "done" || item.status === "error" || item.status === "canceled") item.sessionId = null;
    void pump();
  }
}

/** 取消一个排队中或进行中的上传。 */
export async function cancelUploadItem(id: string): Promise<void> {
  const item = state.items.find((entry) => entry.id === id);
  if (!item) {
    return;
  }
  if (item.status === "queued") {
    item.status = "canceled";
    item.progress = { ...item.progress, message: "已取消" };
    void pump();
    return;
  }
  // 收尾排队期间先让服务端原子取消，再停止前端轮询；worker 已经领取时
  // 服务端会拒绝取消，界面不能误报成“已取消”。
  if (item.progress.phase === "finishing" && item.sessionId) {
    const canceled = await cancelUpload(item.sessionId);
    if (!canceled) {
      useToasts().info("服务器已开始处理，此任务暂时不能取消");
      return;
    }
    item.controller?.abort();
    return;
  }
  item.controller?.abort();
  // 服务端会话也要清掉，否则临时文件与预扣的额度会挂到过期为止。
  if (item.sessionId) await cancelUpload(item.sessionId);
}

/** 重试一个失败的上传。 */
export function retryUpload(id: string): void {
  const item = state.items.find((entry) => entry.id === id);
  if (!item || item.status !== "error") {
    return;
  }
  item.status = "queued";
  item.errorMessage = "";
  item.progress = emptyProgress(item.file.name, item.file.size);
  void pump();
}

export function removeUpload(id: string): void {
  const index = state.items.findIndex((entry) => entry.id === id);
  if (index >= 0) {
    state.items.splice(index, 1);
  }
}

/** 清掉所有已结束的条目。 */
export function clearFinishedUploads(): void {
  for (let i = state.items.length - 1; i >= 0; i--) {
    const item = state.items[i];
    if (item.status === "done" || item.status === "error" || item.status === "canceled") {
      state.items.splice(i, 1);
    }
  }
}

export function useUploads() {
  return {
    items: computed(() => state.items),
    activeCount: computed(
      () => state.items.filter((item) => item.status === "running" || item.status === "queued").length,
    ),
    failedCount: computed(() => state.items.filter((item) => item.status === "error").length),
    enqueueUploads,
    cancelUploadItem,
    retryUpload,
    removeUpload,
    clearFinishedUploads,
  };
}
