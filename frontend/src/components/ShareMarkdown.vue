<script setup lang="ts">
import { computed, onBeforeUnmount, ref, shallowRef, watch } from "vue";
import { loadPreviewLibrary } from "@/lib/previewLibrary";
import { logError } from "@/lib/async";
import { useSite } from "@/stores/site";
const props = defineProps<{ content: string }>();
const site = useSite();
const library = shallowRef<Awaited<ReturnType<typeof loadPreviewLibrary>> | null>(null);
const url = ref("");
const error = ref(false);
const files = computed(() => [{ name: "分享说明.md", type: "text/markdown", url: url.value }]);
let revision = 0;
let controller: AbortController | null = null;
function release(): void {
  controller?.abort();
  controller = null;
  if (url.value) URL.revokeObjectURL(url.value);
  url.value = "";
}
async function requestPreview(resource: string, init?: RequestInit): Promise<Response> {
  const signal = controller?.signal;
  if (!signal || signal.aborted) throw new DOMException("已取消", "AbortError");
  return fetch(resource, { ...init, signal: init?.signal ? AbortSignal.any([signal, init.signal]) : signal });
}
watch(() => props.content, async content => {
  const current = ++revision;
  release();
  error.value = false;
  if (!content) return;
  try {
    const lib = await loadPreviewLibrary();
    if (current !== revision) return;
    library.value = lib;
    controller = new AbortController();
    url.value = URL.createObjectURL(new Blob([content], { type: "text/markdown;charset=utf-8" }));
  } catch (reason) { if (current === revision) { error.value = true; logError("share-markdown-preview", reason); } }
}, { immediate: true });
onBeforeUnmount(() => { revision++; release(); });
</script>
<template>
  <div class="share-markdown vfp-root" :data-theme="site.state.theme">
    <p v-if="error" class="notice notice--danger">Markdown 预览加载失败，请稍后重试。</p>
    <component v-else-if="library && url" :is="library.FilePreviewContent" :key="url"
      :files="files" :current-index="0" mode="embed" headless
      :show-close="false" :show-download="false" :show-navigation="false"
      :theme="site.state.theme" locale="zh-CN" :request-handler="requestPreview" />
    <p v-else class="faint">{{ content ? '正在准备预览…' : '输入 Markdown 内容后即可预览。' }}</p>
  </div>
</template>
<style scoped>
.share-markdown { min-width: 0; color: var(--c-text); font-size: var(--fs-base); line-height: 1.8; overflow-wrap: anywhere;
  --fp-fg-primary: var(--c-text); --fp-fg-secondary: var(--c-text-muted); --fp-fg-muted: var(--c-text-muted);
  --fp-line: var(--c-border); --fp-line-weak: var(--c-border-line);
  --fp-surface-1: var(--c-surface-2); --fp-code-bg: var(--c-surface-sunken);
}
/* 嵌入内容随文档展开，滚动交给分享页或编辑器预览容器。 */
.share-markdown :deep(.vfp-h-full) { height: auto; }
.share-markdown :deep(.vfp-flex-1) { flex: none; width: 100%; }
.share-markdown :deep(.vfp-overflow-auto), .share-markdown :deep(.vfp-overflow-hidden) { overflow: visible; }
.share-markdown :deep(.vfp-py-6), .share-markdown :deep(.vfp-px-4) { padding: 0; }
.share-markdown :deep(.markdown-body) { width: 100%; max-width: none; margin: 0; font-family: var(--font-sans); }
.share-markdown :deep(.markdown-body > :first-child) { margin-top: 0; }
.share-markdown :deep(.markdown-body > :last-child) { margin-bottom: 0; }
.share-markdown :deep(.markdown-body a) { color: var(--c-accent); }
.share-markdown :deep(.markdown-body img) { max-width: 100%; height: auto; border-radius: var(--r-md); }
.share-markdown :deep(.table-wrapper), .share-markdown :deep(.code-block-wrapper), .share-markdown :deep(.katex-display) { max-width: 100%; overflow-x: auto; }
</style>
