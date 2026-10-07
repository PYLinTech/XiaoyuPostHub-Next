<script setup lang="ts">
import { ref, watch } from "vue";
import AppBreadcrumb from "@/components/ui/AppBreadcrumb.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppModal from "@/components/ui/AppModal.vue";
import { fsApi } from "@/api/endpoints";
import type { ListNode } from "@/api/types";
import { createRequestGate, describeError, logError } from "@/lib/async";
import { pathSegments } from "@/lib/format";

// 目录选择器。
//
// 移动文件需要一个目的地，而"手输路径"是最容易出错的做法（拼错、忘记前导斜杠、
// 把文件当成目录）。这里直接浏览。
//
// 关键约束：**不能把节点移进它自己的子树**。后端用环检测兜住，但让用户点进去
// 再报错是糟糕的体验，因此这里直接把那段子树展示为不可进入。

const props = defineProps<{
  open: boolean;
  title: string;
  /** 起始位置。 */
  initialPath: string;
  /** 不允许选中的子树根（通常是正在移动的节点本身）。 */
  excludePath?: string;
}>();

const emit = defineEmits<{ select: [string]; close: [] }>();

const currentPath = ref("/");
const folders = ref<ListNode[]>([]);
const loading = ref(false);
const error = ref("");

// 序号守卫：currentPath（面包屑与"选择此目录"提交的值）和 folders（列表）来自同一次
// 响应，必须一起写回。连点目录时后发先至很常见，少了守卫会出现面包屑停在 A、
// 列表却是 B 的子目录——用户看着 B 选中了 A，文件就被移到了错误的地方。
const gate = createRequestGate();

async function load(path: string): Promise<void> {
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await fsApi.list(path);
    if (!gate.isCurrent(token)) return;
    currentPath.value = result.path || path;
    folders.value = result.items.filter((item) => item.isFolder);
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    logError("folder-picker", err);
    error.value = describeError(err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

watch(
  () => props.open,
  (open) => {
    if (open) {
      void load(props.initialPath || "/");
    }
  },
  // immediate：挂载时就处于打开态也要先列出起始目录，否则进来是一片空白。
  { immediate: true },
);

function isBlocked(path: string): boolean {
  if (!props.excludePath) {
    return false;
  }
  // 前缀判定必须带分隔符，否则 /a/b 会被 /a/bc 误判为子树。
  const root = props.excludePath.replace(/\/+$/, "");
  return path === root || path.startsWith(`${root}/`);
}

function choose(): void {
  if (isBlocked(currentPath.value)) {
    return;
  }
  emit("select", currentPath.value);
}
</script>

<template>
  <AppModal :open="open" :title="title" @close="emit('close')">
    <div class="stack">
      <div class="row row--between">
        <AppBreadcrumb :segments="pathSegments(currentPath)" @navigate="load" />
        <AppButton size="sm" icon="refresh" :loading="loading" @click="load(currentPath)">刷新</AppButton>
      </div>

      <p v-if="error" class="notice notice--danger">{{ error }}</p>

      <div class="picker__list">
        <button
          v-for="folder in folders"
          :key="folder.path"
          type="button"
          class="picker__item"
          :disabled="isBlocked(folder.path)"
          :title="isBlocked(folder.path) ? '不能移动到自身或自身的子目录里' : folder.name"
          @click="load(folder.path)"
        >
          <AppIcon name="folder" :size="16" />
          <span class="truncate">{{ folder.name }}</span>
          <span v-if="isBlocked(folder.path)" class="badge badge--danger">不可选</span>
          <AppIcon v-else name="chevronRight" :size="14" class="faint" />
        </button>

        <AppEmpty
          v-if="!loading && folders.length === 0"
          icon="folder"
          title="该目录下没有子目录"
        />
      </div>
    </div>

    <template #footer>
      <span class="faint" style="margin-right: auto; font-size: var(--fs-xs)">
        当前目标：<code class="mono">{{ currentPath }}</code>
      </span>
      <AppButton @click="emit('close')">取消</AppButton>
      <AppButton variant="primary" :disabled="isBlocked(currentPath)" @click="choose">
        选择此目录
      </AppButton>
    </template>
  </AppModal>
</template>

<style scoped>
.picker__list {
  max-height: 46dvh;
  overflow-y: auto;
  border: 1px solid var(--c-border);
  border-radius: var(--r-sm);
  padding: var(--sp-1);
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.picker__item {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  border: 0;
  background: transparent;
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-sm);
  cursor: pointer;
  font-size: var(--fs-sm);
  text-align: left;
  color: var(--c-text);
  min-height: 36px;
}

.picker__item:hover:not(:disabled) {
  background: var(--c-hover);
}

.picker__item:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

@media (max-width: 640px) {
  .picker__item {
    min-height: var(--tap);
  }
}
</style>
