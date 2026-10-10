<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import type { DeliverySource } from "@/delivery/download";
import { prepareMediaPreview } from "@/delivery/mediaPreview";
import { createMediaPreviewClient } from "@/lib/mediaPreviewClient";
import type { MediaPreviewClient } from "@/lib/mediaPreviewClient";
import { FULL_PREVIEW_LIMIT } from "@/lib/mediaPreviewLimits";
import { mediaSourceConstructor } from "@/lib/mediaSourceSupport";
import { attachMediaPlayback } from "@/lib/mediaPlayback";
import { mediaMimeType } from "@/lib/mediaFormats";
import { describeError, logError } from "@/lib/async";

const props = defineProps<{ source: DeliverySource; fileName: string; kind: "audio" | "video" }>();
const player = ref<HTMLMediaElement>();
const message = ref("正在准备预览"), error = ref("");
/** 已经拿到可追加的媒体数据：此前不渲染播放器，避免出现空控件。 */
const ready = ref(false);
const lifetime = new AbortController();
let playback = new AbortController();
let handle: Awaited<ReturnType<typeof prepareMediaPreview>> | undefined;
let client: MediaPreviewClient | undefined, fallbackAttempted = false, blob = "";
/** 终止态才显示居中提示；准备中只显示进度，不遮挡尚不可用的播放器。 */
const stopped = computed(() => !!error.value);
function markReady() { ready.value = true; message.value = ""; }
function fatal(reason: unknown) {
  if (lifetime.signal.aborted) return;
  message.value = ""; error.value = describeError(reason);
  playback.abort(); lifetime.abort();
  player.value?.pause(); player.value?.removeAttribute("src"); player.value?.load();
  if (blob) { URL.revokeObjectURL(blob); blob = ""; }
  void handle?.release().catch(reason => logError("media-preview-release", reason));
}
async function fallback(reason: unknown) {
  if (lifetime.signal.aborted) return;
  if (fallbackAttempted) return;
  // 网络/票据/认证错误不能通过重复完整下载修复。
  const code = reason && typeof reason === "object" && "code" in reason ? reason.code : undefined;
  if ((reason instanceof Error && (reason.name === "XphFormatError" || reason.name === "AbortError")) || code === "auth" || code === "network") { fatal(reason); return; }
  if (handle && handle.plan.plainSize > FULL_PREVIEW_LIMIT && reason instanceof Error
    && ["MediaCodecError", "UnsupportedInputFormatError", "QuotaExceededError"].includes(reason.name)) { fatal(reason); return; }
  fallbackAttempted = true;
  const time = player.value?.currentTime || 0, resume = player.value ? !player.value.paused : false;
  playback.abort(); client?.stop();
  if (!client || !handle || !player.value) { fatal(reason); return; }
  message.value = "正在完整解密，准备兼容预览";
  try {
    const mime = mediaMimeType(props.fileName, handle.plan.mimeType);
    const complete = await client.complete(mime, bytes => { message.value = `正在完整获取并校验 ${Math.floor(bytes / handle!.plan.plainSize * 100)}%`; }, !!mediaSourceConstructor());
    lifetime.signal.throwIfAborted();
    if (!(complete instanceof Blob)) {
      message.value = "文件已完整缓存，正在准备本地播放";
      playback = new AbortController();
      await attachMediaPlayback(player.value, client, AbortSignal.any([lifetime.signal, playback.signal]), fatal);
      if (time > 0) player.value.currentTime = time;
      if (resume) void player.value.play().catch(() => {});
      return;
    }
    blob = URL.createObjectURL(complete);
    player.value.addEventListener("loadeddata", markReady, { once: true, signal: lifetime.signal });
    player.value.addEventListener("loadedmetadata", () => { if (player.value) { player.value.currentTime = time; if (resume) void player.value.play().catch(() => {}); } }, { once: true, signal: lifetime.signal });
    player.value.src = blob; player.value.load();
  } catch (failure) { fatal(failure); }
}
function nativeError() { if (fallbackAttempted && player.value?.error) fatal(new Error(player.value.error.message || "浏览器不支持此文件的音视频编码，请下载后播放")); }
onMounted(async () => {
  try {
    const prepared = await prepareMediaPreview(props.source, lifetime.signal);
    if (lifetime.signal.aborted) { await prepared.release(); return; }
    handle = prepared; client = createMediaPreviewClient(prepared.descriptor, lifetime.signal);
    await client.open(); lifetime.signal.throwIfAborted();
    await attachMediaPlayback(player.value!, client, AbortSignal.any([lifetime.signal, playback.signal]), reason => { void fallback(reason); });
  } catch (reason) { if (client && handle) await fallback(reason); else fatal(reason); }
});
onBeforeUnmount(() => {
  playback.abort(); lifetime.abort();
  if (blob) URL.revokeObjectURL(blob);
  void handle?.release().catch(reason => logError("media-preview-release", reason));
});
</script>

<template>
  <div class="media-preview">
    <div v-if="stopped" class="media-preview-state" role="alert">
      <AppIcon name="information-fill" :size="40" />
      <p>{{ error }}</p>
    </div>
    <div v-else-if="!ready" class="media-preview-state" role="status">
      <span class="spinner" /><span>{{ message }}</span>
    </div>
    <div v-if="!stopped" class="media-preview-stage" :aria-hidden="!ready">
      <component :is="kind" ref="player" controls playsinline preload="auto" :aria-label="fileName"
        @loadeddata="markReady" @error="nativeError" />
      <div v-if="message && ready" class="media-preview-status" role="status">
        <span class="spinner" />{{ message }}
      </div>
    </div>
  </div>
</template>
<style scoped>
.media-preview { width: 100%; height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; position: relative; background: #111; }
/* 播放器必须始终挂载：attachMediaPlayback 在 onMounted 里就绑定原生元素，
   用 v-if 移除会让它拿到 undefined。这里只控制可见性，准备阶段不暴露空控件。 */
.media-preview-stage { position: absolute; inset: 0; display: flex; align-items: center; justify-content: center; }
.media-preview-stage[aria-hidden="true"] { visibility: hidden; pointer-events: none; }
.media-preview video { width: 100%; height: 100%; min-height: 0; }
.media-preview audio { width: min(90%, 640px); }
.media-preview-state { height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: var(--sp-4); padding: var(--sp-5); text-align: center; color: var(--c-text-muted); }
.media-preview-state p { margin: 0; overflow-wrap: anywhere; }
/* 准备中与兼容兜底的等待态：全局 .spinner 只有 16px、轨道用 --c-border-strong，
   在 #111 的深色舞台上几乎看不见，这里放大并换成半透明白色轨道。 */
.media-preview-state .spinner { width: 28px; height: 28px; border-width: 3px; border-color: rgba(255, 255, 255, 0.18); border-top-color: var(--c-accent); animation-duration: 0.8s; }
.media-preview-status { position: absolute; top: 16px; max-width: 90%; display: flex; align-items: center; gap: 12px; padding: 10px 14px; border-radius: 8px; color: #fff; background: #222d; overflow-wrap: anywhere; pointer-events: none; }
</style>
