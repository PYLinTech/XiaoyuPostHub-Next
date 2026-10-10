import { reactive } from "vue";
import { showTransfer } from "./transferPanel";
import type { DeliveryProgress } from "@/delivery/download";

export interface DownloadItem {
  id: number;
  fileName: string;
  status: "running" | "done" | "error" | "canceled";
  progress: DeliveryProgress;
  errorMessage: string;
  cancel: () => void;
}

const items = reactive<DownloadItem[]>([]);
let nextId = 0;

export function addDownload(fileName: string, cancel: () => void): DownloadItem {
  items.unshift({ id: ++nextId, fileName, status: "running", progress: {
    phase: "preparing", bytesDone: 0, bytesTotal: 0, message: "正在准备",
  }, errorMessage: "", cancel });
  showTransfer("download");
  return items[0];
}

export function useDownloads() {
  function remove(id: number): void {
    const index = items.findIndex(item => item.id === id && item.status !== "running");
    if (index >= 0) items.splice(index, 1);
  }
  function clearFinished(): void {
    for (let i = items.length - 1; i >= 0; i--) {
      if (items[i].status !== "running") items.splice(i, 1);
    }
  }
  return { items, remove, clearFinished };
}

/** 文件已开始落盘或交给浏览器后，取消不能再撤回结果。 */
export function canCancelDownload(item: DownloadItem): boolean {
  return item.status === "running" && item.progress.phase !== "delivering" && item.progress.phase !== "done";
}
