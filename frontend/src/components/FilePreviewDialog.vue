<script setup lang="ts">
import { ApiError } from "@/api/client";
import { computed, nextTick, onBeforeUnmount, shallowRef, ref, watch } from "vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import { useSite } from "@/stores/site";
import { useDeliveryAction } from "@/delivery/actions";
import { preparePreview, type PreviewHandle } from "@/delivery/preview";
import type { DeliverySource } from "@/delivery/download";
import { createRequestGate, describeError, logError } from "@/lib/async";
import { stopPreviewParser } from "@/lib/previewParser";
import { loadPreviewLibrary } from "@/lib/previewLibrary";
import { mediaMimeType } from "@/lib/mediaFormats";

const props = withDefaults(defineProps<{
  open: boolean;
  fileName: string;
  source: DeliverySource | null;
  downloadSource: DeliverySource | null;
  previewAllowed?: boolean;
  downloadAllowed?: boolean;
}>(), { previewAllowed: true, downloadAllowed: true });
const emit = defineEmits<{ close: [] }>();
const site = useSite();
const download = useDeliveryAction();
const canDownload = computed(() => props.downloadAllowed && !!props.downloadSource);
const library = shallowRef<Awaited<ReturnType<typeof loadPreviewLibrary>> | null>(null);
const handle = shallowRef<PreviewHandle | null>(null);
const contentRoot = ref<HTMLElement | null>(null);
const loading = ref(false);
const error = ref("");
const supported = ref(false);
const compatibility = ref(false);
let media = false;
let fallbackAttempted = false;
let stopMseObserver: (() => void) | null = null;
const compatibilityMessage = "流式预览失败，正在自动尝试兼容方法（需先完整获取文件）";
const gate = createRequestGate();
let controller: AbortController | null = null;
let activeToken = 0;
const files = computed(() => handle.value ? [{
  name: props.fileName,
  type: mediaMimeType(props.fileName, handle.value.plan.mimeType) || "application/octet-stream",
  url: handle.value.url,
}] : []);

function release(preview: PreviewHandle | null): void {
  if (preview) void preview.release().catch(err => logError("preview-release", err));
}
function reset(): void {
  stopMseObserver?.();
  stopMseObserver = null;
  gate.next();
  stopPreviewParser();
  controller?.abort();
  controller = null;
  const previous = handle.value;
  handle.value = null;
  release(previous);
  loading.value = false;
  error.value = "";
  supported.value = false;
  compatibility.value = false;
  media = false;
  fallbackAttempted = false;
}
async function observeMedia(token: number, preview: PreviewHandle): Promise<void> {
  stopMseObserver?.();
  stopMseObserver = null;
  if (!media || preview.mode === "blob" || !library.value || !contentRoot.value) return;
  await nextTick();
  if (!gate.isCurrent(token) || !props.open || !contentRoot.value) return;
  stopMseObserver = library.value.observeXphMsePreview?.(contentRoot.value, preview.url) ?? null;
}
async function load(): Promise<void> {
  reset();
  if (!props.open || !props.previewAllowed || !props.source) return;
  const token = gate.next();
  activeToken = token;
  const abort = new AbortController();
  controller = abort;
  const source = props.source;
  const name = props.fileName;
  loading.value = true;
  try {
    const lib = await loadPreviewLibrary();
    if (!gate.isCurrent(token)) return;
    library.value = lib;
    const kind = lib.getFileType({ name, type: "", url: "" });
    supported.value = kind !== "unsupported";
    media = kind === "audio" || kind === "video";
    // 不支持的文件只展示下载入口，不申请票据或读取内容。
    if (!supported.value) return;
    const prepared = await preparePreview(source, { signal: abort.signal, streamingOnly: media });
    if (!gate.isCurrent(token)) { release(prepared); return; }
    const preparedKind = lib.getFileType({ name, type: mediaMimeType(name, prepared.plan.mimeType), url: prepared.url });
    media = preparedKind === "audio" || preparedKind === "video";
    handle.value = prepared;
    await observeMedia(token, prepared);
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    if (media && err instanceof Error && err.name !== "AbortError" && !(err instanceof ApiError && err.status > 0)) {
      await fallback(token);
    } else {
      logError("preview", err);
      error.value = describeError(err);
    }
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}
async function fallback(token: number): Promise<void> {
  if (!gate.isCurrent(token) || fallbackAttempted || !props.source || !props.open) return;
  fallbackAttempted = true;
  compatibility.value = true;
  loading.value = true;
  controller?.abort();
  stopPreviewParser();
  const previous = handle.value;
  stopMseObserver?.();
  stopMseObserver = null;
  handle.value = null;
  // 释放流式票据后再申请兼容票据，避免同时占用两份预扣额度。
  if (previous) await previous.release().catch(err => logError("preview-release", err));
  if (!gate.isCurrent(token)) return;
  const abort = new AbortController();
  controller = abort;
  try {
    const prepared = await preparePreview(props.source, { signal: abort.signal, forceFull: true });
    if (!gate.isCurrent(token)) { release(prepared); return; }
    handle.value = prepared;
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    logError("preview-compatibility", err);
    error.value = `兼容预览失败：${describeError(err)}`;
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}
function streamError(url: string): void {
  if (!media || fallbackAttempted || !handle.value || handle.value.mode === "blob") return;
  if (url !== new URL(handle.value.url, location.href).href) return;
  void fallback(activeToken);
}
function mediaError(event: Event): void {
  if (event.target instanceof HTMLMediaElement) streamError(event.target.currentSrc || event.target.src);
}
function rendererError(event: Event): void {
  const url = (event as CustomEvent<{ url?: string }>).detail?.url;
  if (url) streamError(new URL(url, location.href).href);
}
function close(): void { reset(); emit("close"); }
async function downloadFile(): Promise<void> {
  if (canDownload.value && props.downloadSource) {
    await download.run(props.downloadSource, { fileName: props.fileName });
  }
}
// 解析器的 fetch 与预览生命周期绑定，关闭后停止尚在读取的文本/文档。
async function requestPreview(url: string, init?: RequestInit): Promise<Response> {
  const signal = controller?.signal;
  if (!signal || signal.aborted) throw new DOMException("已取消", "AbortError");
  return fetch(url, { ...init, signal: init?.signal ? AbortSignal.any([signal, init.signal]) : signal });
}
watch(() => [props.open, props.source, props.fileName, props.previewAllowed] as const, () => {
  if (props.open) void load(); else reset();
}, { immediate: true });
onBeforeUnmount(reset);
</script>

<template>
  <AppModal :open="open" :title="fileName" :close-on-backdrop="false" wide panel-class="file-preview-dialog" @close="close">
    <template #actions>
      <AppButton v-if="canDownload" size="sm" variant="primary" icon="download-2-line" :loading="download.busy.value" @click="downloadFile">下载</AppButton>
    </template>
    <div ref="contentRoot" class="file-preview-content vfp-root" :data-theme="site.state.theme" @error.capture="mediaError" @xph-preview-error="rendererError">
      <div v-if="loading" class="file-preview-state" role="status"><span class="spinner" /><span>{{ compatibility ? compatibilityMessage : "正在准备预览" }}</span></div>
      <div v-else-if="error || !previewAllowed || !supported" class="file-preview-state">
        <AppIcon name="information-fill" :size="40" />
        <p>{{ error || (!previewAllowed ? '因分享者设置，该文件不可预览' : '该格式暂不支持预览') }}</p>
        <AppButton v-if="canDownload" variant="primary" icon="download-2-line" :loading="download.busy.value" @click="downloadFile">下载</AppButton>
      </div>
      <component v-else-if="library && handle" :is="library.FilePreviewContent" :key="handle.url"
        :files="files" :current-index="0" mode="embed" headless
        :show-close="false" :show-download="false" :show-navigation="false"
        :theme="site.state.theme" locale="zh-CN" :request-handler="requestPreview"
        :on-download="downloadFile" />
    </div>
  </AppModal>
</template>

<style>
.file-preview-dialog .modal__title { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.file-preview-dialog .modal__body { padding: 0; }
.file-preview-content { height: min(68dvh, 680px); min-height: 220px; overflow: hidden; }
.file-preview-content.xph-mse-frame-ready .vfp-renderer-loading { display: none !important; }
.file-preview-content.xph-mse-frame-ready .vjs-control-bar { visibility: visible !important; opacity: 1 !important; }
.file-preview-state { height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: var(--sp-4); padding: var(--sp-5); text-align: center; color: var(--c-text-muted); }
.file-preview-state p { margin: 0; overflow-wrap: anywhere; }
</style>
