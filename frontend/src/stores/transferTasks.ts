import { reactive } from "vue";
import { uploadApi } from "@/api/endpoints";
import type { UploadJobStatus } from "@/api/types";
import { clearDownloads } from "./downloads";
import { clearUploadTasks } from "./uploads";
import { showTransfer, transferPanel } from "./transferPanel";

export interface ServerUploadTask {
  id: string;
  sessionId: string;
  fileName: string;
  status: "queued" | "running" | "done" | "error" | "canceled";
  message: string;
  errorMessage: string;
  progressBytes: number;
  totalBytes: number;
}

const serverUploads = reactive<ServerUploadTask[]>([]);
let actorKey = "";
let generation = 0;
let pollTimer: ReturnType<typeof setInterval> | undefined;
let pollInFlight = false;

function resetPanel(): void {
  transferPanel.visible = false;
  transferPanel.collapsed = false;
  transferPanel.selected = "upload";
}

function clearCurrentTasks(): void {
  clearUploadTasks();
  clearDownloads();
  serverUploads.splice(0);
  resetPanel();
}

function applyStatus(task: ServerUploadTask, status: UploadJobStatus): void {
  task.status = status.state === "processing"
    ? "running"
    : status.state === "receiving"
      ? "queued"
      : status.state === "error" && status.error === "已取消"
        ? "canceled"
        : status.state;
  task.message = status.message;
  if (status.state === "receiving") task.message = "等待重新选择文件以续传";
  task.errorMessage = status.error ?? "";
  task.progressBytes = status.progressBytes;
  task.totalBytes = status.totalBytes;
}

async function refreshServerUploads(expectedGeneration: number): Promise<void> {
  if (pollInFlight || expectedGeneration !== generation || !actorKey) return;
  pollInFlight = true;
  try {
    const active = serverUploads.filter(task => task.status === "queued" || task.status === "running");
    await Promise.all(active.map(async task => {
      try {
        const status = await uploadApi.status(task.sessionId);
        if (expectedGeneration === generation && status.state) {
          applyStatus(task, {
            sessionId: status.sessionId,
            state: status.state,
            message: status.message ?? "",
            error: status.error,
            progressBytes: status.progressBytes ?? 0,
            totalBytes: status.totalBytes ?? 0,
            node: status.node,
          });
        }
      } catch {
        // 临时网络错误不清除任务；下一轮继续查询。
      }
    }));
  } finally {
    pollInFlight = false;
  }
}

/**
 * 会话身份变化时丢弃旧用户的本地传输，再加载新用户的服务端收尾任务。
 * 空身份表示已退出或访客状态，面板保持隐藏。
 */
export function syncTransferTasksForUser(nextActorKey: string): void {
  if (nextActorKey === actorKey) return;
  actorKey = nextActorKey;
  const requestGeneration = ++generation;
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = undefined;
  pollInFlight = false;
  clearCurrentTasks();
  if (!nextActorKey) return;

  void uploadApi.tasks().then(({ items }) => {
    if (requestGeneration !== generation || actorKey !== nextActorKey) return;
    for (const item of items) {
      const task: ServerUploadTask = {
        id: item.sessionId,
        sessionId: item.sessionId,
        fileName: item.fileName || "上传文件",
        status: "queued",
        message: "等待服务器处理",
        errorMessage: "",
        progressBytes: 0,
        totalBytes: 0,
      };
      applyStatus(task, item);
      serverUploads.push(task);
    }
    if (serverUploads.length) showTransfer("upload");
    pollTimer = setInterval(() => void refreshServerUploads(requestGeneration), 1500);
  }).catch(() => {
    // 登录成功不应被任务列表请求失败影响；网络恢复后可通过重新登录刷新。
  });
}

export function useServerUploadTasks() {
  async function cancel(task: ServerUploadTask): Promise<void> {
    if (task.status !== "queued") return;
    try {
      await uploadApi.cancel(task.sessionId);
      task.status = "canceled";
      task.message = "已取消";
      task.errorMessage = "";
    } catch {
      // 任务可能已被 worker 领取，保留现状并由轮询刷新。
    }
  }

  function remove(id: string): void {
    const index = serverUploads.findIndex(task => task.id === id && task.status !== "queued" && task.status !== "running");
    if (index >= 0) serverUploads.splice(index, 1);
  }

  return { items: serverUploads, cancel, remove };
}
