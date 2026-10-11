<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import type { DeliverySource } from "@/delivery/download";
import { prepareMediaPreview } from "@/delivery/mediaPreview";
import { createNativeMediaUrl } from "@/delivery/preview";
import type { NativeMediaUrlHandle } from "@/delivery/preview";
import { probeNativePlayable } from "@/lib/mediaKernelChoice";
import { describePhase, resolveFallbackCapability } from "@/lib/mediaFallbackCapability";
import type { CompletePhase } from "@/lib/mediaFallbackCapability";
import MediaNativePlayer from "@/components/MediaNativePlayer.vue";
import { createMediaPreviewClient } from "@/lib/mediaPreviewClient";
import type { MediaPreviewClient } from "@/lib/mediaPreviewClient";
import { FULL_PREVIEW_LIMIT } from "@/lib/mediaPreviewLimits";
import { mediaSourceConstructor } from "@/lib/mediaSourceSupport";
import { attachMediaPlayback } from "@/lib/mediaPlayback";
import { mediaMimeType } from "@/lib/mediaFormats";
import { describeError, logError } from "@/lib/async";
import { isRecoverable } from "@/lib/mediaRangeSource";
import { newRecovery, recoveryPlan, sleep, RECOVERY_BUDGET } from "@/lib/mediaRecovery";
import { PlaybackStateMachine, classifyFailure, describeState } from "@/lib/playbackStateMachine";
import type { PlaybackState } from "@/lib/playbackStateMachine";

const props = defineProps<{ source: DeliverySource; fileName: string; kind: "audio" | "video" }>();
const player = ref<HTMLMediaElement>();
/**
 * 播放状态机的唯一真源。
 *
 * 此前 `message` / `error` / `ready` 三个 ref 各自代表一块拼图，谁也说不清当前
 * 到底处于什么阶段。集中成状态机之后，界面文案与"该不该重建资源"都由同一处判断，
 * 不再出现"恢复中却已经把播放器拆了"这类矛盾。
 */
const machine = new PlaybackStateMachine();
const phase = ref<PlaybackState>("idle");
machine.subscribe(state => { phase.value = state; });
/** 状态决定文案；错误原文只在 failed 时展示，便于排查。 */
/**
 * 兜底下载有自己的阶段文案（下载 / 校验 / 准备），优先于状态机的通用提示——
 * 那是一段可能持续几十秒的等待，必须说清正在做什么。
 */
const fallbackHint = ref("");
const message = computed(() => fallbackHint.value
  || describeState(phase.value, machine.failureStage));
const error = ref("");
const lifetime = new AbortController();
let playback = new AbortController();
let handle: Awaited<ReturnType<typeof prepareMediaPreview>> | undefined;
let client: MediaPreviewClient | undefined, fallbackAttempted = false, blob = "";
/** 已判定不可原生直放，避免每次失败都重新探测一遍文件头。 */
let nativeRejected = false;
/** 原生内核的 SW 会话；非 null 时表示正在走原生直放。 */
let nativeHandle: NativeMediaUrlHandle | null = null;
/** 供模板判断走哪套内核；ref 包裹是为了让 v-if 感知异步建立的会话。 */
const nativeUrl = ref("");
/** 恢复尝试的累计状态；成功播放一段时间后清零，让后续故障重新获得完整预算。 */
let recovery = newRecovery();
/** 终止态才显示居中提示；准备中只显示进度，不遮挡尚不可用的播放器。 */
const stopped = computed(() => phase.value === "failed" || phase.value === "disposed");
/** 有可播内容时才暴露播放器控件，避免出现空控件。 */
const playable = computed(() => phase.value === "ready" || phase.value === "buffering" || phase.value === "stalled");
function markReady() { machine.observeEvent("canplay"); }
/**
 * 故障后尝试自愈，不直接终止播放。
 *
 * 保留 `currentTime` 和暂停/播放状态是关键：用户不该因为网抖了一下就得自己
 * 再拖回刚才的位置。`wait` 期间不释放会话（重连不需要换直链），只有 `replan`
 * 才重新走一遍 prepareMediaPreview 拿新票据和新密钥——所以大文件不会被拖进
 * 完整解密兜底，那条路对大文件是灾难级的开销。
 */
async function recover(reason: unknown) {
  if (lifetime.signal.aborted || !player.value) { fatal(reason); return; }
  const element = player.value;
  // 恢复必须保住的三样：位置、暂停/播放状态、播放速度。
  // 位置与暂停从元素上取（此刻仍在），速度要显式存——MSE 重建期间元素可能被
  // 重置，回到 1x 的话，用户设的 2 倍速会无声无息地消失。
  const time = element.currentTime || 0, resume = !element.paused, rate = element.playbackRate;
  if (!machine.handleFailure(reason, { recoverable: true, currentTime: time, resume, playbackRate: rate,
    attempt: recovery.spent.reconnect + recovery.spent.replan,
    budget: RECOVERY_BUDGET.reconnect + RECOVERY_BUDGET.replan })) { fatal(reason); return; }

  for (;;) {
    if (!machine.preservesResources) { fatal(reason); return; }
    const plan = recoveryPlan(reason, recovery);
    if (plan.kind === "giveup") { fatal(plan.reason); return; }
    error.value = "";
    try {
      if (plan.kind === "wait") { await sleep(plan.delay, lifetime.signal); continue; }
      // 重连沿用原直链；重新授权才换 URL 与密钥——但旧票据必须先结算，
      // 否则那笔预扣额度要等维护任务回收才释放。
      if (plan.kind === "replan" || !client || !handle) {
        playback.abort(); playback = new AbortController(); client?.stop();
        const previousClient = client; client = undefined;
        await previousClient?.close();
        await handle?.release().catch(error => logError("media-preview-replan", error));
        handle = await prepareMediaPreview(props.source, lifetime.signal);
        if (!handle.descriptor) throw new Error("当前交付方式不支持兼容式媒体预览");
        client = createMediaPreviewClient(handle.descriptor, lifetime.signal);
        await client.open(); lifetime.signal.throwIfAborted();
      } else {
        playback.abort(); playback = new AbortController(); client.stop();
      }
      await attachMediaPlayback(element, client, AbortSignal.any([lifetime.signal, playback.signal]), recover, machine);
      // 回到原来的位置与状态；此前暂停就不要替他自动播。
      element.playbackRate = rate;
      element.currentTime = time;
      if (resume) void element.play().catch(() => {});
      machine.recovered();
      recovery = newRecovery();
      return;
    } catch (failure) {
      reason = failure;
      if (lifetime.signal.aborted) return;
    }
  }
}
/**
 * 尝试启用原生直放内核。
 *
 * 顺序刻意如此：先拿 SW 会话（拿到虚拟明文地址），再从这个地址读一段明文做
 * 容器/编码判定，最后让浏览器自己的解码器表点头。三关都过才启用，否则把会话
 * 释放掉、回兼容内核。
 *
 * 判定读的是真实明文而不是密文，也不是猜扩展名——这是整个选型的地基：猜错
 * 的代价是"能播但卡"或"直接黑屏"，比多走一次兼容内核糟糕得多。
 */
async function tryNative(prepared: NonNullable<typeof handle>): Promise<boolean> {
  if (nativeRejected) return false;
  let candidate: NativeMediaUrlHandle | null = null;
  try {
    const dek = prepared.descriptor?.key.slice();
    try { candidate = await createNativeMediaUrl(prepared.plan, dek, lifetime.signal); }
    finally { dek?.fill(0); }
    if (!candidate) return false;
    const probe = await probeNativePlayable(candidate.url, mediaMimeType(props.fileName, prepared.plan.mimeType), lifetime.signal);
    if (!probe.native) {
      nativeRejected = true;
      logError("media-kernel-choice", probe.reason || "不适用原生直放");
      await candidate.release();
      return false;
    }
    nativeHandle = candidate;
    machine.to("buffering");
    nativeUrl.value = candidate.url;
    return true;
  } catch (reason) {
    await candidate?.release().catch(error => logError("media-native-release", error));
    if (lifetime.signal.aborted) return false;
    nativeRejected = true;
    logError("media-native-prepare", reason);
    return false;
  }
}
/** 释放原生内核会话，回到兼容内核或终止态时调用。 */
async function releaseNative() {
  const handle = nativeHandle; nativeHandle = null; nativeUrl.value = "";
  await handle?.release().catch(reason => logError("media-native-release", reason));
}
function fatal(reason: unknown) {
  if (lifetime.signal.aborted) return;
  // 先问状态机：这次故障能不能自愈。能就不该走到这里（由 recover 处理）。
  const stage = classifyFailure(reason).stage;
  machine.to("failed", stage);
  error.value = describeError(reason);
  playback.abort(); lifetime.abort();
  player.value?.pause(); player.value?.removeAttribute("src"); player.value?.load();
  void releaseNative();
  if (blob) { URL.revokeObjectURL(blob); blob = ""; }
  void handle?.release().catch(reason => logError("media-preview-release", reason));
}
async function fallback(reason: unknown) {
  if (lifetime.signal.aborted) return;
  if (fallbackAttempted) return;
  // 网络/票据/认证错误先走自愈重连或重新授权，只有恢复预算用尽才终止。
  const { reconnect, replan } = isRecoverable(reason);
  if (reconnect || replan) { void recover(reason); return; }
  if (reason && typeof reason === "object" && "code" in reason && reason.code === "network"
    && !classifyFailure(reason).recoverable) { fatal(reason); return; }
  if (reason instanceof Error && reason.name === "XphFormatError") { fatal(reason); return; }
  if (reason instanceof Error && reason.name === "AbortError") { fatal(reason); return; }
  if (!client || !handle || !player.value) { fatal(reason); return; }
  // 兜底不是万能药：先分诊这次失败值不值得下载整个文件。
  // 解码层失败（浏览器不认这个编码）下载完整文件也救不回来，让用户在几十秒的
  // 等待之后收到同一个错误是最糟的结果。
  const verdict = resolveFallbackCapability(reason, {
    plainSize: handle.plan.plainSize,
    exceedsBlobLimit: handle.plan.plainSize > FULL_PREVIEW_LIMIT,
    hasDiskCache: !!globalThis.navigator?.storage?.getDirectory,
    hasMediaSource: !!mediaSourceConstructor(),
  });
  if (!verdict.attempt) {
    logError("media-fallback-skipped", verdict.reason);
    fatal(new Error(verdict.reason));
    return;
  }
  machine.to("recovering");
  logError("media-fallback-decision", verdict.reason);
  fallbackAttempted = true;
  const time = player.value?.currentTime || 0, resume = player.value ? !player.value.paused : false;
  playback.abort(); client?.stop();
  if (!client || !handle || !player.value) { fatal(reason); return; }
  fallbackHint.value = "正在完整解密，准备兼容预览";
  try {
    const mime = mediaMimeType(props.fileName, handle.plan.mimeType);
    const complete = await client.complete(mime, (ratio, stage) => {
      // 下载与校验是两段等待，合并成一个百分比会让用户在下载早已结束后
      // 盯着一个不动的数字，全然不知道程序还在忙。
      fallbackHint.value = describePhase(stage as CompletePhase, ratio);
    }, !!mediaSourceConstructor());
    lifetime.signal.throwIfAborted();
    if (!(complete instanceof Blob)) {
      fallbackHint.value = "文件已完整缓存，正在准备本地播放";
      playback = new AbortController();
      await attachMediaPlayback(player.value, client, AbortSignal.any([lifetime.signal, playback.signal]), fatal, machine);
      if (time > 0) player.value.currentTime = time;
      if (resume) void player.value.play().catch(() => {});
      return;
    }
    blob = URL.createObjectURL(complete);
    machine.to("buffering");
    player.value.addEventListener("loadeddata", markReady, { once: true, signal: lifetime.signal });
    player.value.addEventListener("loadedmetadata", () => { if (player.value) { player.value.currentTime = time; if (resume) void player.value.play().catch(() => {}); } }, { once: true, signal: lifetime.signal });
    player.value.src = blob; player.value.load();
  } catch (failure) { fatal(failure); }
  finally { fallbackHint.value = ""; }
}
function nativeError() { if (fallbackAttempted && player.value?.error) fatal(new Error(player.value.error.message || "浏览器不支持此文件的音视频编码，请下载后播放")); }
/** 原生内核播放失败：不是终局，改走兼容内核重封装/转码。 */
async function nativeFailed(reason: unknown) {
  if (lifetime.signal.aborted) return;
  // 原生这条路已经证明不行，别再探测第二次；直接进兼容内核。
  nativeRejected = true;
  logError("media-native-playback", reason);
  await releaseNative();
  try {
    await attachCompat(handle);
  } catch (failure) { await fallback(failure); }
}
onMounted(async () => {
  machine.to("preparing");
  try {
    // 一次计划同时服务于内核探测与实际播放，探测不通过时复用它进入兼容内核。
    const prepared = await prepareMediaPreview(props.source, lifetime.signal);
    handle = prepared;
    lifetime.signal.throwIfAborted();
    // 常见 H.264/AAC 的 MP4 走原生直放：链路更短，没有跨 Worker 传输与
    // SourceBuffer 追加。判定不过就地回兼容内核，Mediabunny 继续兜住复杂格式。
    if (await tryNative(prepared)) return;
    if (lifetime.signal.aborted) return;
    await attachCompat(prepared);
  } catch (reason) { if (client && handle) await fallback(reason); else if (isRecoverable(reason).reconnect || isRecoverable(reason).replan) void recover(reason); else fatal(reason); }
});
/** 建立兼容内核：Mediabunny Worker + MediaSource，负责重封装与转码。 */
async function attachCompat(prepared = handle) {
  if (!prepared) throw new Error("媒体预览计划尚未准备好");
  if (!prepared.descriptor) throw new Error("当前交付方式不支持兼容式媒体预览");
  if (lifetime.signal.aborted) return;
  handle = prepared; client = createMediaPreviewClient(prepared.descriptor, lifetime.signal);
  await client.open(); lifetime.signal.throwIfAborted();
  await attachMediaPlayback(player.value!, client, AbortSignal.any([lifetime.signal, playback.signal]), reason => { void fallback(reason); }, machine);
}
onBeforeUnmount(() => {
  playback.abort(); lifetime.abort();
  machine.dispose();
  void releaseNative();
  if (blob) URL.revokeObjectURL(blob);
  void handle?.release().catch(reason => logError("media-preview-release", reason));
});
</script>

<template>
  <div class="media-preview">
    <!-- 终止态：只在这里显示最终错误。故障原因由状态机给出，比原始异常更可读。 -->
    <div v-if="stopped" class="media-preview-state" role="alert">
      <AppIcon name="information-fill" :size="40" />
      <p>{{ error || message }}</p>
    </div>
    <!-- 进行中：准备、缓冲、等待数据、恢复中。恢复时不遮挡播放器——资源还在，
         用户应该能看到画面停在原处，而不是看到一个空白的重来界面。 -->
    <div v-else-if="!playable" class="media-preview-state" role="status">
      <span class="spinner" /><span>{{ message }}</span>
    </div>
    <div v-if="!stopped" class="media-preview-stage" :aria-hidden="!playable">
      <!-- 原生内核自带播放器元素；兼容内核的播放器必须始终挂载，
           attachMediaPlayback 在挂载时就绑定了原生元素，v-if 移除会让它拿到 undefined。 -->
      <MediaNativePlayer v-if="nativeUrl" :key="nativeUrl" :url="nativeUrl" :file-name="fileName" :kind="kind"
        @ready="markReady"
        @playing="machine.observeEvent('playing')" @waiting="machine.observeEvent('waiting')" @failed="nativeFailed" />
      <component v-else :is="kind" ref="player" controls playsinline preload="auto" :aria-label="fileName"
        @loadeddata="markReady" @error="nativeError" />
      <!-- 恢复中的浮层：告知正在自愈，但保留画面可见。 -->
      <div v-if="phase === 'recovering' || (phase === 'stalled' && message)" class="media-preview-status" role="status">
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
