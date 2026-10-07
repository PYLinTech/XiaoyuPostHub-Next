import { ref } from "vue";
import { runDelivery, saveBlob, type DeliveryProgress, type DeliverySource } from "./download";
import { describeError, isAbortError, logError } from "@/lib/async";
import { useToasts } from "@/stores/toast";

// 下载动作的封装。
//
// 文件页、分享页、取件码页都需要"下载一个对象"，而它们各自都要处理进度、
// 成功提示、失败提示、以及"进行中要禁用按钮"。合到一处才不会出现
// "分享页下载失败没有任何提示"这类只在某一条路径上才暴露的问题。

export function useDeliveryAction() {
  const busy = ref(false);
  const progress = ref<DeliveryProgress | null>(null);
  const controller = ref<AbortController | null>(null);

  async function run(source: DeliverySource, options: { silent?: boolean } = {}): Promise<boolean> {
    if (busy.value) {
      return false;
    }
    const toasts = useToasts();
    const abort = new AbortController();
    controller.value = abort;
    busy.value = true;
    progress.value = { phase: "preparing", bytesDone: 0, bytesTotal: 0, message: "正在准备" };

    try {
      const result = await runDelivery(source, {
        signal: abort.signal,
        onProgress: (info) => {
          progress.value = info;
        },
      });

      if (result.blob) {
        saveBlob(result.blob, result.fileName);
      }
      if (!options.silent) {
        toasts.success(
          result.savedAs ? `已保存到 ${result.savedAs}` : `已下载 ${result.fileName}`,
          undefined,
        );
      }
      return true;
    } catch (err) {
      if (isAbortError(err)) {
        toasts.info("已取消");
        return false;
      }
      logError("download", err);
      toasts.error("下载失败", describeError(err));
      return false;
    } finally {
      busy.value = false;
      progress.value = null;
      controller.value = null;
    }
  }

  function cancel(): void {
    controller.value?.abort();
  }

  return { busy, progress, run, cancel };
}
