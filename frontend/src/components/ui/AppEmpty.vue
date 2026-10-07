<script setup lang="ts">
import AppIcon from "./AppIcon.vue";

defineProps<{
  title?: string;
  hint?: string;
  icon?: string;
}>();
</script>

<template>
  <div class="empty">
    <AppIcon :name="icon ?? 'file'" :size="28" />
    <p class="empty__title">{{ title ?? "暂无数据" }}</p>
    <p v-if="hint" class="faint">{{ hint }}</p>
    <slot />
  </div>
</template>

<style scoped>
.empty :deep(svg) {
  color: var(--c-text-faint);
}
</style>
