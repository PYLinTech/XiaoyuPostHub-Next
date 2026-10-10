import { runDelivery } from "@/delivery/transferClient";
import { addDownload, canCancelDownload } from "@/stores/downloads";
import { ref } from "vue";
import { saveBlob, type DeliverySource } from "./download";
import { describeError, isAbortError, logError } from "@/lib/async";
import { useToasts } from "@/stores/toast";

// 下载动作的封装。
//
// 文件页、分享页、取件码页都需要"下载一个对象"，而它们各自都要处理进度、
// 成功提示、失败提示、以及"进行中要禁用按钮"。合到一处才不会出现
// "分享页下载失败没有任何提示"这类只在某一条路径上才暴露的问题。

export function useDeliveryAction() {
  const busy = ref(false);

  async function run(source: DeliverySource, options: { silent?: boolean; fileName?: string } = {}): Promise<boolean> {
    if (busy.value) {
      return false;
    }
    const toasts = useToasts();
    const abort = new AbortController();
    const item = addDownload(options.fileName ?? "正在准备文件", () => {
      if (canCancelDownload(item)) abort.abort();
    });
    busy.value = true;

    try {
      const result = await runDelivery({
        async plan(pair) {
          const plan = await source.plan(pair);
          item.fileName = plan.fileName;
          return plan;
        },
      }, {
        signal: abort.signal,
        onProgress: (info) => {
          item.progress = info;
        },
      });

      if (result.blob) {
        saveBlob(result.blob, result.fileName);
      }
      item.status = "done";
      if (!options.silent) {
        toasts.success(
          result.savedAs ? `已保存到 ${result.savedAs}` : `已下载 ${result.fileName}`,
          undefined,
        );
      }
      return true;
    } catch (err) {
      if (isAbortError(err)) {
        item.status = "canceled";
        toasts.info("已取消");
        return false;
      }
      item.status = "error";
      item.errorMessage = describeError(err);
      logError("download", err);
      toasts.error("下载失败", describeError(err));
      return false;
    } finally {
      item.cancel = () => {};
      busy.value = false;
    }
  }

  return { busy, run };
}
