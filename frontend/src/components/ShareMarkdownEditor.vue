<script setup lang="ts">
import { computed, ref } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import ShareMarkdown from "./ShareMarkdown.vue";
const props = defineProps<{ modelValue: string; maxChars: number; disabled?: boolean }>();
const emit = defineEmits<{ "update:modelValue": [value: string]; importing: [value: boolean] }>();
const preview = ref(false);
const fileInput = ref<HTMLInputElement | null>(null);
const importing = ref(false);
const error = ref("");
const count = computed(() => Array.from(props.modelValue).length);
async function importFile(event: Event) {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  input.value = "";
  if (!file) return;
  error.value = "";
  importing.value = true;
  emit("importing", true);
  try {
    if (!/\.(md|markdown|txt)$/i.test(file.name)) throw new Error("请选择 .md、.markdown 或 .txt 文件");
    if (file.size > props.maxChars * 4 + 3) throw new Error(`文件过大，最多允许 ${props.maxChars.toLocaleString()} 个字符`);
    const text = new TextDecoder("utf-8", { fatal: true }).decode(await file.arrayBuffer());
    if (Array.from(text).length > props.maxChars) throw new Error(`内容超出 ${props.maxChars.toLocaleString()} 个字符的上限`);
    emit("update:modelValue", text);
  } catch (err) { error.value = err instanceof TypeError ? "文件须使用 UTF-8 编码" : (err as Error).message; }
  finally { importing.value = false; emit("importing", false); }
}
</script>
<template>
  <section class="markdown-editor" aria-label="分享自定义内容">
    <header class="markdown-editor__header">
      <div><strong>自定义内容</strong><p class="faint">在分享页展示说明、使用指南或其他 Markdown 内容。</p></div>
      <div class="markdown-editor__tools">
        <AppButton size="sm" :disabled="disabled || importing" @click="preview = !preview">{{ preview ? '编辑' : '预览' }}</AppButton>
        <AppButton size="sm" icon="upload" :disabled="disabled" :loading="importing" @click="fileInput?.click()">导入文件</AppButton>
      </div>
    </header>
    <input ref="fileInput" class="markdown-editor__file" type="file" accept=".md,.markdown,.txt,text/plain,text/markdown" :disabled="disabled || importing" @change="importFile" />
    <ShareMarkdown v-if="preview" :content="modelValue" class="markdown-editor__preview" />
    <textarea v-else :value="modelValue" class="input markdown-editor__input" :disabled="disabled || importing" aria-label="Markdown 自定义内容" placeholder="# 分享说明&#10;&#10;在这里输入内容，支持标题、列表、链接和代码块…" @input="emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)" />
    <footer class="markdown-editor__footer"><span>支持 UTF-8 的 .md / .markdown / .txt 文件，导入将替换当前内容。</span><span :class="{ 'markdown-editor__over': count > maxChars }">{{ count.toLocaleString() }} / {{ maxChars.toLocaleString() }}</span></footer>
    <p v-if="error || count > maxChars" class="notice notice--danger">{{ error || '内容超出字符上限，请精简后保存。' }}</p>
  </section>
</template>
<style scoped>
.markdown-editor { padding: 18px; border: 1px solid var(--c-border-card); border-radius: 14px; background: var(--c-surface-2); display: grid; gap: 14px; min-width: 0; }
.markdown-editor__header { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.markdown-editor__header strong { font-size: 14px; font-weight: 600; }
.markdown-editor__header p { margin: 5px 0 0; font-size: 12px; line-height: 1.6; }
.markdown-editor__tools { display: flex; gap: 8px; }
.markdown-editor__file { display: none; }
.markdown-editor__input { width: 100%; min-height: 220px; resize: vertical; font-family: var(--font-mono, monospace); font-size: 13px; line-height: 1.8; padding: 14px; box-sizing: border-box; }
.markdown-editor__preview { min-height: 220px; max-height: 480px; overflow: auto; padding: 14px; background: var(--c-surface); border: 1px solid var(--c-border); border-radius: 10px; }
.markdown-editor__footer { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; flex-wrap: wrap; color: var(--c-text-muted); font-size: 11px; line-height: 1.6; }
.markdown-editor__footer > :last-child { font-variant-numeric: tabular-nums; white-space: nowrap; }
.markdown-editor__over { color: var(--c-danger); }
</style>
