<script setup lang="ts">
import { runDelivery } from "@/delivery/transferClient";
import { computed, onBeforeUnmount, ref, watch } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppModal from "@/components/ui/AppModal.vue";
import { fileKind, previewElement } from "@/lib/filekind";
import { createRequestGate, describeError, logError } from "@/lib/async";
import { formatBytes } from "@/lib/format";
import { preparePreview, type PreviewHandle } from "@/delivery/preview";
import { saveBlob, type DeliverySource } from "@/delivery/download";
import { useToasts } from "@/stores/toast";

// 预览浮层。
//
// 文件页与分享页共用它，因此它只接收一个 DeliverySource——"怎么拿到交付计划"
// 由调用方决定，浮层不关心对象属于谁、走的是不是分享。
//
// 音视频走 Service Worker 提供的虚拟明文流，因此是可以边下边播、可以拖动的；
// 文本类则必须先整份拿到（它很小），所以走一次性解密后直接显示。

const props = defineProps<{
  open: boolean;
  source: DeliverySource | null;
  fileName: string;
}>();

const emit = defineEmits<{ close: [] }>();

const toasts = useToasts();

const handle = ref<PreviewHandle | null>(null);
const loading = ref(false);
const error = ref("");
const textContent = ref("");

const kind = computed(() => fileKind(props.fileName));
const element = computed(() => previewElement(kind.value));

// 序号守卫：准备预览要建 SW 会话或整份解密，慢得很（A 先开、B 后开、B 更快很常见）。
// 少了它，A 后返回会把 handle 覆盖成 A，而标题栏上的文件名还是 B。
// 更要紧的是 close() 只把 handle 置空，在途请求照样会再赋值一次——那个会话再也没人
// release，额度不结算，等于泄漏。
const gate = createRequestGate();
let previewAbort: AbortController | null = null;

const modeLabel = computed(() => {
  switch (handle.value?.mode) {
    case "sw":
      return "本地解密（直连存储）";
    case "stream":
      return "服务端中转";
    case "blob":
      return "本地解密（整份载入）";
    default:
      return "";
  }
});

async function loadPreview(): Promise<void> {
  void resetPreview()?.release();
  if (!props.source) return;
  const controller = new AbortController();
  previewAbort = controller;
  const token = gate.next();
  error.value = "";
  textContent.value = "";
  loading.value = true;
  try {
    if (kind.value === "text") {
      // 文本没有流式渲染的必要，直接整份取回后放进 <pre>。
      const result = await runDelivery(props.source, { signal: controller.signal });
      if (!gate.isCurrent(token)) return;
      if (!result.blob) {
        throw new Error("文本内容未能载入");
      }
      const text = await result.blob.text();
      if (!gate.isCurrent(token)) return;
      textContent.value = text;
      loading.value = false;
      return;
    }
    const prepared = await preparePreview(props.source, { signal: controller.signal });
    // 过期结果绝不写进 handle.value：那样界面会显示上一个文件的画面，
    // 而这个会话也再没人管。preparePreview 已经把额度预扣了，必须就地释放。
    if (!gate.isCurrent(token)) {
      void prepared.release();
      return;
    }
    handle.value = prepared;
  } catch (err) {
    // 过期请求的失败不该盖掉当前文件的状态。
    if (!gate.isCurrent(token)) return;
    logError("preview", err);
    error.value = describeError(err);
  } finally {
    // 过期请求的 finally 不能关 loading：转圈属于当前这次请求。
    if (gate.isCurrent(token)) loading.value = false;
  }
}

function resetPreview(): PreviewHandle | null {
  gate.next();
  previewAbort?.abort();
  previewAbort = null;
  const current = handle.value;
  handle.value = null;
  textContent.value = "";
  loading.value = false;
  return current;
}

async function close(): Promise<void> {
  const current = resetPreview();
  emit("close");
  await current?.release();
}

async function downloadToo(): Promise<void> {
  if (!props.source) {
    return;
  }
  try {
    const result = await runDelivery(props.source);
    if (result.blob) {
      saveBlob(result.blob, result.fileName);
      toasts.success(`已下载 ${result.fileName}`);
    } else if (result.savedAs) {
      toasts.success(`已保存到 ${result.savedAs}`);
    }
  } catch (err) {
    logError("preview-download", err);
    toasts.error("下载失败", describeError(err));
  }
}

watch(
  () => [props.open, props.source] as const,
  ([isOpen]) => {
    if (isOpen) void loadPreview();
    else void resetPreview()?.release();
  },
  { immediate: true },
);
onBeforeUnmount(() => { void resetPreview()?.release(); });
</script>

<template>
  <AppModal :open="open" :title="fileName" wide @close="close()">
    <div class="preview">
      <div v-if="loading" class="preview__state">
        <span class="spinner" />
        <span class="muted">正在准备预览</span>
      </div>

      <div v-else-if="error" class="stack">
        <p class="notice notice--danger">{{ error }}</p>
        <p class="faint">可以改用下载：下载不依赖浏览器能否就地播放。</p>
      </div>

      <template v-else>
        <img v-if="element === 'img' && handle" :src="handle.url" :alt="fileName" class="preview__image" />

        <video
          v-else-if="element === 'video' && handle"
          :src="handle.url"
          controls
          playsinline
          preload="metadata"
          class="preview__video"
        />

        <audio v-else-if="element === 'audio' && handle" :src="handle.url" controls class="preview__audio" />

        <iframe
          v-else-if="element === 'iframe' && handle"
          :src="handle.url"
          class="preview__frame"
          title="PDF 预览"
        />

        <pre v-else-if="element === 'text'" class="preview__text">{{ textContent }}</pre>

        <div v-else class="preview__state">
          <AppIcon name="file" :size="26" />
          <p class="muted">该类型无法就地预览，请下载后查看。</p>
        </div>
      </template>
    </div>

    <template #footer>
      <span v-if="modeLabel" class="faint" style="margin-right: auto; font-size: var(--fs-xs)">
        {{ modeLabel }}
        <template v-if="handle?.plan.encryption">
          · {{ formatBytes(handle.plan.encryption.plainSize) }}
        </template>
      </span>
      <AppButton icon="download" @click="downloadToo">下载</AppButton>
      <AppButton variant="primary" @click="close()">关闭</AppButton>
    </template>
  </AppModal>
</template>

<style scoped>
.preview {
  min-height: 200px;
  display: flex;
  flex-direction: column;
  justify-content: center;
}

.preview__state {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-8) 0;
  text-align: center;
}

.preview__image {
  max-width: 100%;
  max-height: 68dvh;
  object-fit: contain;
  margin: 0 auto;
  display: block;
  border-radius: var(--r-sm);
}

.preview__video {
  width: 100%;
  max-height: 68dvh;
  background: #000;
  border-radius: var(--r-sm);
}

.preview__audio {
  width: 100%;
}

.preview__frame {
  width: 100%;
  height: 68dvh;
  border: 1px solid var(--c-border);
  border-radius: var(--r-sm);
  background: var(--c-surface);
}

.preview__text {
  margin: 0;
  max-height: 68dvh;
  overflow: auto;
  background: var(--c-surface-sunken);
  border-radius: var(--r-sm);
  padding: var(--sp-4);
  white-space: pre-wrap;
  word-break: break-word;
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  line-height: 1.6;
}
</style>
