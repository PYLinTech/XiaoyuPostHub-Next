<script setup lang="ts">
import { computed, watch, ref, onMounted, onBeforeUnmount } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import TransferWater from "@/components/ui/TransferWater.vue";
import { aggregateTransferProgress, isByteTransfer } from "@/lib/transferProgress";
import { formatBytes } from "@/lib/format";
import { useUploads } from "@/stores/uploads";
import { useDownloads, canCancelDownload } from "@/stores/downloads";
import { transferPanel } from "@/stores/transferPanel";

const uploads = useUploads();
const downloads = useDownloads();
const downloadActive = computed(() => downloads.items.filter(item => item.status === "running").length);

const hasTasks = computed(() => uploads.items.value.length + downloads.items.length > 0);
const labels = { queued: "排队中", running: "进行中", done: "已完成", error: "失败", canceled: "已取消" };
const uploadMessages = { hashing: "准备上传", uploading: "上传中", finishing: "服务器处理中", done: "已完成" };
const downloadMessages = { preparing: "准备下载", fetching: "下载中", decrypting: "下载中", verifying: "正在校验", delivering: "正在保存", done: "已完成" };

watch(hasTasks, value => { if (!value) transferPanel.visible = false; }, { immediate: true, flush: "sync" });
const totalProgress = computed(() => aggregateTransferProgress([
  ...uploads.items.value.filter(item => isByteTransfer(item.status, item.progress.phase))
    .map(item => ({ done: item.progress.sent, total: item.file.size })),
  ...downloads.items.filter(item => isByteTransfer(item.status, item.progress.phase))
    .map(item => ({ done: item.progress.bytesDone, total: item.progress.bytesTotal })),
]));
const body = ref<HTMLElement | null>(null);
const rail = ref<HTMLElement | null>(null);
const thumbSize = ref(0), thumbTop = ref(0);
let observer: ResizeObserver | undefined;
let stopDrag: (() => void) | undefined;
function updateScrollbar() {
  const el = body.value, track = rail.value;
  if (!el || !track || transferPanel.collapsed) return;
  const overflow = el.scrollHeight - el.clientHeight;
  thumbSize.value = overflow > 1 ? Math.min(track.clientHeight, Math.max(24, track.clientHeight * el.clientHeight / el.scrollHeight)) : 0;
  thumbTop.value = overflow > 0 ? (track.clientHeight - thumbSize.value) * el.scrollTop / overflow : 0;
}
function dragThumb(event: PointerEvent) {
  const target = event.currentTarget as HTMLElement, el = body.value, track = rail.value;
  if (!el || !track) return;
  const range = track.clientHeight - thumbSize.value;
  if (range <= 0) return;
  stopDrag?.();
  event.preventDefault(); target.setPointerCapture(event.pointerId);
  const start = event.clientY, scroll = el.scrollTop, scale = (el.scrollHeight - el.clientHeight) / range;
  const move = (e: PointerEvent) => { el.scrollTop = scroll + (e.clientY - start) * scale; };
  const stop = () => { target.removeEventListener("pointermove", move); target.removeEventListener("lostpointercapture", stop); if (target.hasPointerCapture(event.pointerId)) target.releasePointerCapture(event.pointerId); stopDrag = undefined; };
  stopDrag = stop;
  target.addEventListener("pointermove", move); target.addEventListener("lostpointercapture", stop);
}
onMounted(() => { observer = new ResizeObserver(updateScrollbar); if (body.value) observer.observe(body.value); });
onBeforeUnmount(() => { stopDrag?.(); observer?.disconnect(); });
const tasks = computed(() => transferPanel.selected === "upload"
  ? uploads.items.value.map(item => ({
      id: item.id, name: item.file.name, status: item.status,
      active: item.status === "running" || item.status === "queued",
      ratio: isByteTransfer(item.status, item.progress.phase) ? item.progress.ratio : null,
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
      ratio: isByteTransfer(item.status, item.progress.phase) && item.progress.bytesTotal > 0 ? item.progress.bytesDone / item.progress.bytesTotal : null,
      bytesDone: item.progress.bytesDone, bytesTotal: item.progress.bytesTotal,
      message: downloadMessages[item.progress.phase], error: item.errorMessage, detail: formatBytes(item.progress.bytesTotal),
      cancel: canCancelDownload(item) ? item.cancel : null, retry: null,
      remove: () => downloads.remove(item.id),
    })));

watch(body, el => {
  observer?.disconnect();
  if (el) observer?.observe(el);
  updateScrollbar();
}, { flush: "post" });
watch(() => [transferPanel.collapsed, transferPanel.selected, tasks.value], () => {
  if (transferPanel.collapsed) stopDrag?.();
  updateScrollbar();
}, { flush: "post" });
</script>

<template>
  <section v-if="hasTasks && transferPanel.visible" class="dock" :class="{ 'dock--collapsed': transferPanel.collapsed }" aria-label="传输任务">
    <button v-if="transferPanel.collapsed" class="dock__toggle" type="button" aria-expanded="false" aria-controls="transfer-body" :aria-label="totalProgress === null ? '传输任务，点击展开' : `正在传输，总进度 ${Math.round(totalProgress * 100)}%，点击展开`" @click="transferPanel.collapsed = false">
      <TransferWater :ratio="totalProgress" />
      <AppIcon name="arrow-up-down-line" :size="18" /><span>{{ totalProgress !== null ? "正在传输" : "传输任务" }}</span>
      <span v-if="totalProgress !== null" class="dock__percent">{{ Math.round(totalProgress * 100) }}%</span>
    </button>
    <header v-else class="dock__head">
      <div class="dock__tabs" role="group" aria-label="任务类型">
        <button type="button" :aria-pressed="transferPanel.selected === 'upload'" @click="transferPanel.selected = 'upload'">
          <AppIcon name="upload-2-line" :size="14" />上传 <span v-if="uploads.activeCount.value">{{ uploads.activeCount.value }}</span>
        </button>
        <button type="button" :aria-pressed="transferPanel.selected === 'download'" @click="transferPanel.selected = 'download'">
          <AppIcon name="download-2-line" :size="14" />下载 <span v-if="downloadActive">{{ downloadActive }}</span>
        </button>
      </div>
      <AppButton size="sm" class="dock__collapse" aria-label="折叠传输任务" title="折叠" @click="transferPanel.collapsed = true"><AppIcon name="collapse-diagonal-line" :size="14" /></AppButton>
    </header>
    <div v-show="!transferPanel.collapsed" id="transfer-body" class="dock__content">
      <div ref="body" class="dock__body" tabindex="0" aria-label="传输任务列表" @scroll="updateScrollbar">
        <p v-if="!tasks.length" class="dock__empty">暂无{{ transferPanel.selected === 'upload' ? '上传' : '下载' }}任务</p>
        <article v-for="task in tasks" :key="`${transferPanel.selected}-${task.id}`" class="dock__item">
          <div class="dock__row">
            <span class="truncate dock__name" :title="task.name">{{ task.name }}</span>
            <span class="badge" :class="{ 'badge--success': task.status === 'done', 'badge--danger': task.status === 'error', 'badge--accent': task.active }">{{ task.active ? task.message : labels[task.status] }}</span>
          </div>
          <div v-if="task.active" class="progress" :class="{ 'progress--indeterminate': task.ratio === null }" role="progressbar" :aria-label="transferPanel.selected === 'upload' ? '上传进度' : '下载进度'" aria-valuemin="0" aria-valuemax="100" :aria-valuenow="task.ratio === null ? undefined : Math.round(task.ratio * 100)" :aria-valuetext="task.message">
            <div class="progress__bar" :style="{ width: `${task.ratio === null ? 0 : Math.max(0, Math.min(100, task.ratio * 100))}%` }" />
          </div>
          <div class="dock__footer">
            <p v-if="task.active" class="dock__detail">{{ task.message }}<span v-if="task.bytesTotal > 0"> · {{ formatBytes(task.bytesDone) }} / {{ formatBytes(task.bytesTotal) }}</span></p>
            <p v-else-if="task.status === 'error'" class="dock__error">{{ task.error }}</p>
            <p v-else class="dock__detail">{{ task.detail }}</p>
            <div class="dock__actions">
              <AppButton v-if="task.cancel" size="sm" @click="task.cancel()">取消</AppButton>
              <AppButton v-if="task.retry" size="sm" @click="task.retry()">重试</AppButton>
              <AppButton v-if="!task.active" size="sm" @click="task.remove()">移除</AppButton>
            </div>
          </div>
        </article>
      </div>
      <div ref="rail" class="dock__scrollbar" aria-hidden="true"><div v-if="thumbSize" class="dock__thumb" :style="{ height: `${thumbSize}px`, top: `${thumbTop}px` }" @pointerdown="dragThumb" /></div>
    </div>
  </section>
</template>

<style scoped>
.dock { position:fixed; right:var(--sp-4); bottom:var(--sp-4); width:min(400px,calc(100vw - 32px)); max-height:min(60dvh,520px); display:flex; flex-direction:column; background:var(--c-surface); border:1px solid color-mix(in srgb,var(--c-accent) 30%,var(--c-border)); border-radius:var(--r-md); box-shadow:var(--shadow-lg); overflow:hidden; z-index:70; }
.dock--collapsed { width:auto; }
.dock__toggle { position:relative; isolation:isolate; display:flex; align-items:center; justify-content:center; gap:8px; min-width:180px; width:100%; padding:12px 16px; border:0; background:transparent; color:var(--c-text); font:inherit; font-size:var(--fs-sm); font-weight:600; cursor:pointer; }
.dock__toggle > span,.dock__toggle > .icon { position:relative; z-index:1; }
.dock__percent { font-size:var(--fs-xs); font-variant-numeric:tabular-nums; }
.dock__toggle:hover,.dock__tabs button:hover { color:var(--c-accent-hover); }
.dock__toggle:focus-visible,.dock__tabs button:focus-visible { outline:2px solid var(--c-accent); outline-offset:-2px; }
.dock__head { display:flex; align-items:center; justify-content:space-between; gap:8px; padding:8px 12px; border-bottom:1px solid var(--c-border); flex-shrink:0; }
.dock__tabs { display:flex; gap:8px; }
.dock__tabs button { display:flex; align-items:center; gap:6px; padding:6px 8px; border:0; background:transparent; color:var(--c-text-muted); font:inherit; font-size:var(--fs-sm); cursor:pointer; }
.dock__tabs button[aria-pressed=true] { color:var(--c-accent); }
.dock__tabs button:hover { color:var(--c-accent-hover); }
.dock__tabs span { font-size:var(--fs-xs); }
.dock__collapse { background:var(--c-surface-2); }
.dock__content { position:relative; display:flex; min-height:0; flex-direction:column; }
.dock__body { overflow-y:auto; scrollbar-width:none; padding:12px 20px; display:flex; flex-direction:column; gap:12px; min-height:0; }
.dock__body::-webkit-scrollbar { display:none; }
.dock__scrollbar { position:absolute; right:5px; top:10px; bottom:10px; width:5px; pointer-events:none; }
.dock__thumb { position:absolute; width:100%; border-radius:8px; background:color-mix(in srgb,var(--c-text-muted) 45%,transparent); cursor:grab; touch-action:none; pointer-events:auto; }
.dock__thumb:hover { background:var(--c-text-muted); }
.dock__item { display:flex; flex-direction:column; gap:8px; padding-bottom:12px; border-bottom:1px solid var(--c-border); }
.dock__item:last-child { border:0; padding-bottom:0; }
.dock__row { display:flex; align-items:center; justify-content:space-between; gap:8px; }
.dock__name { font-weight:550; min-width:0; }
.dock__row .badge { flex-shrink:0; }
.dock__footer { display:flex; align-items:center; justify-content:space-between; gap:12px; }
.dock__actions { display:flex; gap:4px; flex-shrink:0; }
.dock__detail { min-width:0; overflow-wrap:anywhere; }
.dock__detail,.dock__empty,.dock__error { margin:0; font-size:var(--fs-xs); color:var(--c-text-muted); }
.dock__error { color:var(--c-danger); overflow-wrap:anywhere; min-width:0; }
.dock__empty { text-align:center; padding:28px 0; }
.progress__bar { transition:width .45s ease; animation:dock-progress-start .45s ease; transform-origin:left; }
@keyframes dock-progress-start { from { transform:scaleX(0); } to { transform:scaleX(1); } }
.progress--indeterminate { position:relative; }
.progress--indeterminate .progress__bar { visibility:hidden; animation:none; }
.progress--indeterminate::before,.progress--indeterminate::after { content:""; position:absolute; top:0; bottom:0; background:var(--c-accent); border-radius:inherit; }
.progress--indeterminate::before { animation:dock-primary 2.1s cubic-bezier(.65,.815,.735,.395) infinite; }
.progress--indeterminate::after { left:-200%; right:100%; animation:dock-secondary 2.1s cubic-bezier(.165,.84,.44,1) 1.15s infinite; }
@keyframes dock-primary { 0% { left:-35%; right:100%; } 60%,100% { left:100%; right:-90%; } }
@keyframes dock-secondary { 0% { left:-200%; right:100%; } 60%,100% { left:107%; right:-8%; } }
@media(prefers-reduced-motion:reduce) { .progress__bar { transition:none; animation:none; } .progress--indeterminate::before,.progress--indeterminate::after { animation:none; left:20%; right:20%; } }
@media(max-width:720px) { .dock { left:var(--sp-2); right:var(--sp-2); bottom:max(var(--sp-2),env(safe-area-inset-bottom)); width:auto; max-height:50dvh; } }
</style>
