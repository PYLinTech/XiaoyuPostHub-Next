<script setup lang="ts">
// 统一面板：head（标题 + 计数 + 右侧动作）/ body（flush 表格或 stack 内容）。
defineProps<{
  title?: string;
  /** head 右侧的灰色计数文案，如 "12 个"。 */
  count?: string | number;
  /** body 不留内边距（内嵌 AppTable 时使用）。 */
  flush?: boolean;
}>();
</script>

<template>
  <section class="panelbox">
    <div v-if="title || $slots.actions" class="panelbox__head">
      <h2 class="panelbox__title">{{ title }}</h2>
      <span v-if="count !== undefined && count !== ''" class="panelbox__count">{{ count }}</span>
      <span class="panelbox__sp" />
      <slot name="actions" />
    </div>
    <div class="panelbox__body" :class="{ 'panelbox__body--flush': flush }">
      <slot />
    </div>
  </section>
</template>

<style scoped>
.panelbox {
  border: 1px solid var(--c-border-card);
  border-radius: var(--r-lg);
  background: var(--c-surface);
  box-shadow: var(--shadow-sm);
  overflow: hidden;
}

.panelbox__head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-4);
  border-bottom: 1px solid var(--c-border-line);
}

.panelbox__title {
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 650;
}

.panelbox__count {
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
}

.panelbox__sp {
  flex: 1;
}

.panelbox__body {
  padding: var(--sp-4);
}

.panelbox__body--flush {
  padding: 0;
}
</style>
