<script setup lang="ts">
// 全站唯一的统计条形态：1–4 格，label+value，可选 hint 或徽标化。
export interface StatItem {
  label: string;
  value: string | number;
  hint?: string;
  /** label 以徽标呈现时的色调类。 */
  tone?: string;
  /** value 以徽标呈现（如"运行中"）。 */
  badge?: boolean;
}

defineProps<{ items: StatItem[] }>();
</script>

<template>
  <div class="stat-strip">
    <div v-for="item in items" :key="item.label" class="stat-strip__cell">
      <span class="stat-strip__label">
        <span v-if="item.tone !== undefined" class="badge" :class="item.tone">{{ item.label }}</span>
        <template v-else>{{ item.label }}</template>
      </span>
      <strong class="stat-strip__value">
        <span v-if="item.badge" class="badge" :class="item.tone">{{ item.value }}</span>
        <template v-else>{{ item.value }}</template>
      </strong>
      <span v-if="item.hint" class="stat-strip__hint">{{ item.hint }}</span>
    </div>
  </div>
</template>

<style scoped>
.stat-strip {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(150px, 100%), 1fr));
  gap: var(--sp-3);
}

.stat-strip__cell {
  min-width: 0;
  padding: var(--sp-3) var(--sp-4);
  border: 1px solid var(--c-border-card);
  border-radius: var(--r-lg);
  background: var(--c-surface);
  box-shadow: var(--shadow-sm);
}

.stat-strip__label {
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
}

.stat-strip__value {
  display: block;
  margin-top: var(--sp-2);
  font-size: clamp(20px, 1.8vw, 26px);
  line-height: 1.15;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
}

.stat-strip__hint {
  display: block;
  margin-top: var(--sp-2);
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  line-height: 1.5;
}
</style>
