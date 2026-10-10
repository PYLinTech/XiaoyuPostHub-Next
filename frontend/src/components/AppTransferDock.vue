<script setup lang="ts">
import { computed, watch } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppProgress from "@/components/ui/AppProgress.vue";
import { formatBytes } from "@/lib/format";
import { useUploads } from "@/stores/uploads";
import { useDownloads, canCancelDownload } from "@/stores/downloads";
import { transferPanel } from "@/stores/transferPanel";

const uploads = useUploads();
const downloads = useDownloads();
const downloadActive = computed(() => downloads.items.filter(item => item.status === "running").length);
const activeCount = computed(() => uploads.activeCount.value + downloadActive.value);
const hasTasks = computed(() => uploads.items.value.length + downloads.items.length > 0);
const labels = { queued: "排队中", running: "进行中", done: "已完成", error: "失败", canceled: "已取消" };
const uploadMessages = { hashing: "准备上传", uploading: "上传中", finishing: "服务器处理中", done: "已完成" };
const downloadMessages = { preparing: "准备下载", fetching: "下载中", decrypting: "下载中", verifying: "正在校验", delivering: "正在保存", done: "已完成" };

watch(() => [uploads.items.value.length, downloads.items.length], ([up, down]) => {
  if (transferPanel.selected === "upload" && !up && down) transferPanel.selected = "download";
  if (transferPanel.selected === "download" && !down && up) transferPanel.selected = "upload";
}, { immediate: true });

const tasks = computed(() => transferPanel.selected === "upload"
  ? uploads.items.value.map(item => ({
      id: item.id, name: item.file.name, status: item.status,
      active: item.status === "running" || item.status === "queued",
      ratio: item.status === "queued" ? 0 : item.progress.ratio,
      bytesDone: item.progress.phase === "hashing" ? 0 : item.progress.sent,
      bytesTotal: item.file.size,
      message: item.status === "queued" ? "排队中" : uploadMessages[item.progress.phase], error: item.errorMessage,
      detail: item.dedup ? "秒传命中，未传输数据" : formatBytes(item.file.size),
      cancel: item.status === "queued" || (item.status === "running" && (item.progress.canCancel ?? item.progress.phase !== "finishing"))
        ? () => uploads.cancelUploadItem(item.id) : null,
      retry: item.status === "error" ? () => uploads.retryUpload(item.id) : null,
      remove: () => uploads.removeUpload(item.id),
    }))
  : downloads.items.map(item => ({
      id: item.id, name: item.fileName, status: item.status, active: item.status === "running",
      ratio: item.progress.bytesTotal > 0 ? item.progress.bytesDone / item.progress.bytesTotal : null,
      bytesDone: item.progress.bytesDone, bytesTotal: item.progress.bytesTotal,
      message: downloadMessages[item.progress.phase], error: item.errorMessage, detail: formatBytes(item.progress.bytesTotal),
      cancel: canCancelDownload(item) ? item.cancel : null, retry: null,
      remove: () => downloads.remove(item.id),
    })));

function clearFinished(): void {
  uploads.clearFinishedUploads();
  downloads.clearFinished();
}
</script>

<template>
  <section v-if="hasTasks" class="dock" :class="{ 'dock--collapsed': transferPanel.collapsed }" aria-label="传输任务">
    <header class="dock__head">
      <button class="dock__toggle" type="button" :aria-expanded="!transferPanel.collapsed" aria-controls="transfer-body" @click="transferPanel.collapsed = !transferPanel.collapsed">
        <AppIcon :name="transferPanel.collapsed ? transferPanel.selected : 'chevronDown'" :size="16" />
        <span>传输任务<template v-if="activeCount">（{{ activeCount }} 进行中）</template></span>
      </button>
      <AppButton size="sm" variant="ghost" title="清除已结束" aria-label="清除已结束的传输任务" @click="clearFinished">
        <AppIcon name="close" :size="16" />
      </AppButton>
    </header>
    <div v-show="!transferPanel.collapsed" id="transfer-body" class="dock__content">
      <div class="dock__tabs" role="group" aria-label="任务类型">
        <button type="button" :aria-pressed="transferPanel.selected === 'upload'" :class="{ selected: transferPanel.selected === 'upload' }" @click="transferPanel.selected = 'upload'">
          <AppIcon name="upload" :size="14" />上传 <span v-if="uploads.activeCount.value">{{ uploads.activeCount.value }}</span>
        </button>
        <button type="button" :aria-pressed="transferPanel.selected === 'download'" :class="{ selected: transferPanel.selected === 'download' }" @click="transferPanel.selected = 'download'">
          <AppIcon name="download" :size="14" />下载 <span v-if="downloadActive">{{ downloadActive }}</span>
        </button>
      </div>
      <div class="dock__body">
        <p v-if="!tasks.length" class="dock__empty">暂无{{ transferPanel.selected === 'upload' ? '上传' : '下载' }}任务</p>
        <article v-for="task in tasks" :key="`${transferPanel.selected}-${task.id}`" class="dock__item">
          <div class="dock__row">
            <span class="truncate dock__name" :title="task.name">{{ task.name }}</span>
            <span class="badge" :class="{ 'badge--success': task.status === 'done', 'badge--danger': task.status === 'error', 'badge--accent': task.active }">{{ task.status === 'running' ? (transferPanel.selected === 'upload' ? '上传中' : '下载中') : labels[task.status] }}</span>
          </div>
          <AppProgress v-if="task.active" :name="transferPanel.selected === 'upload' ? '上传进度' : '下载进度'" :ratio="task.ratio" :bytes-done="task.bytesDone" :bytes-total="task.bytesTotal" :label="task.message" />
          <p v-else-if="task.status === 'error'" class="dock__error">{{ task.error }}</p>
          <p v-else class="dock__detail">{{ task.detail }}</p>
          <div class="row" style="gap: 4px">
            <AppButton v-if="task.cancel" size="sm" variant="ghost" @click="task.cancel()">取消</AppButton>
            <AppButton v-if="task.retry" size="sm" variant="ghost" @click="task.retry()">重试</AppButton>
            <AppButton v-if="!task.active" size="sm" variant="ghost" @click="task.remove()">移除</AppButton>
          </div>
        </article>
      </div>
    </div>
  </section>
</template>

<style scoped>
.dock {
  position: fixed;
  right: var(--sp-4);
  bottom: var(--sp-4);
  width: min(400px, calc(100vw - 32px));
  max-height: min(60dvh, 520px);
  display: flex;
  flex-direction: column;
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  box-shadow: var(--shadow-lg);
  overflow: hidden;
  z-index: 70;
}
.dock--collapsed { width: auto; }
.dock__head { display: flex; align-items: center; justify-content: space-between; gap: var(--sp-2); padding: var(--sp-2) var(--sp-3); flex-shrink: 0; }
.dock__toggle { display: flex; align-items: center; gap: var(--sp-2); border: 0; background: transparent; cursor: pointer; font-size: var(--fs-sm); font-weight: 600; color: var(--c-text); padding: 0; }
.dock__toggle:hover { color: var(--c-accent); }
.dock__toggle:focus-visible, .dock__tabs button:focus-visible { outline: 2px solid var(--c-accent); outline-offset: -2px; }
.dock__content { display: flex; flex-direction: column; min-height: 0; }
.dock__tabs { display: flex; flex-shrink: 0; border-top: 1px solid var(--c-border); border-bottom: 1px solid var(--c-border); padding: 0 var(--sp-3); gap: var(--sp-3); }
.dock__tabs button { display: flex; align-items: center; gap: var(--sp-1); padding: var(--sp-2); border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--c-text-muted); cursor: pointer; font-size: var(--fs-sm); }
.dock__tabs button:hover { background: var(--c-hover); }
.dock__tabs button:active { color: var(--c-accent-hover); }
.dock__tabs button.selected { color: var(--c-accent); border-bottom-color: var(--c-accent); }
.dock__tabs span { font-size: var(--fs-xs); background: var(--c-accent-weak); border-radius: 4px; padding: 0 5px; }
.dock__body { overflow-y: auto; padding: var(--sp-3); display: flex; flex-direction: column; gap: var(--sp-3); min-height: 0; }
.dock__item { display: flex; flex-direction: column; gap: var(--sp-2); padding-bottom: var(--sp-3); border-bottom: 1px solid var(--c-border); }
.dock__item:last-child { border: 0; padding-bottom: 0; }
.dock__row { display: flex; align-items: center; justify-content: space-between; gap: var(--sp-2); }
.dock__name { font-weight: 550; min-width: 0; }
.dock__row .badge { flex-shrink: 0; }
.dock__detail, .dock__empty, .dock__error { margin: 0; font-size: var(--fs-xs); color: var(--c-text-faint); }
.dock__error { color: var(--c-danger); overflow-wrap: anywhere; }
.dock__empty { text-align: center; padding: var(--sp-3); }
@media (max-width: 640px) {
  .dock { left: var(--sp-2); right: var(--sp-2); bottom: max(var(--sp-2), env(safe-area-inset-bottom)); width: auto; max-height: 50dvh; }
}
</style>
