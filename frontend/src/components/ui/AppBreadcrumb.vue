<script setup lang="ts">
import AppIcon from "./AppIcon.vue";

// 面包屑。
//
// 根节点单独一个入口，中间段可点击跳转——路径越深，这个"能直接跳回去"的能力
// 越重要，因为浏览器后退在单页应用里未必对应上一级目录。

export interface Crumb {
  name: string;
  path: string;
}

defineProps<{
  segments: Crumb[];
  rootLabel?: string;
}>();

const emit = defineEmits<{ navigate: [string] }>();
</script>

<template>
  <nav class="breadcrumb" aria-label="路径">
    <button
      type="button"
      class="breadcrumb__item breadcrumb__item--root"
      :class="{ 'breadcrumb__item--current': segments.length === 0 }"
      @click="emit('navigate', '/')"
    >
      <AppIcon name="folder" :size="14" />
      <span>{{ rootLabel ?? "根目录" }}</span>
    </button>
    <template v-for="(segment, index) in segments" :key="segment.path">
      <AppIcon name="chevronRight" :size="12" class="breadcrumb__sep" />
      <button
        type="button"
        class="breadcrumb__item"
        :class="{ 'breadcrumb__item--current': index === segments.length - 1 }"
        @click="emit('navigate', segment.path)"
      >
        {{ segment.name }}
      </button>
    </template>
  </nav>
</template>

<style scoped>
/* 一行展示，空间不够时**从最早的一段开始压缩**：最深的那一段（当前目录）是用户
   最需要看见的，所以它的收缩优先级最低，只有挤到极限才会被截断。 */
.breadcrumb {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  font-size: var(--fs-sm);
  min-width: 0;
  flex-wrap: nowrap;
  overflow: hidden;
}

.breadcrumb__item {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-1);
  border: 0;
  background: transparent;
  font: inherit;
  font-size: var(--fs-sm);
  color: var(--c-text-muted);
  padding: 4px 6px;
  border-radius: var(--r-sm);
  cursor: pointer;
  /* 能被压缩，但压到底也留一点宽度（表现为省略号） */
  flex: 0 60 auto;
  min-width: 34px;
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.breadcrumb__item:hover {
  background: var(--c-hover);
  color: var(--c-text);
}

.breadcrumb__item--root {
  /* 比中间段晚一步被压缩：它是"跳回根目录"的出口 */
  flex-shrink: 20;
}

.breadcrumb__item--current {
  /* 最深目录：最后才让位，且可以用满整行 */
  flex-shrink: 1;
  min-width: 60px;
  max-width: 100%;
  color: var(--c-text);
  font-weight: 600;
  cursor: default;
}

.breadcrumb__sep {
  flex: none;
  color: var(--c-text-faint);
}
</style>
