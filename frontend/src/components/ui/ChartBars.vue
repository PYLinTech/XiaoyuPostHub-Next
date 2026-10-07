<script setup lang="ts">
import { computed } from "vue";
import AppEmpty from "./AppEmpty.vue";
import { chartDefaults, isChartEmpty } from "./chartDefaults";

// 横向条形图。用于"分组占比""动作类型"这类条目少、标签长的分布——
// 横向比纵向好读：标签不会被截断，条长可以直接横向对比。

export interface BarItem {
  key: string;
  label: string;
  value: number;
  /** CSS 变量或色值。缺省用主色。 */
  color?: string;
  /** 右侧补充说明，如占比。 */
  hint?: string;
}

const props = withDefaults(
  defineProps<{
    items: BarItem[];
    formatValue?: (value: number) => string;
    emptyText?: string;
  }>(),
  {
    ...chartDefaults,
  },
);

const max = computed(() => props.items.reduce((m, item) => Math.max(m, item.value), 0));

// 原先只看 items.length，于是"分组都在、但人数都是 0"会画出一排占着位的空槽，
// 而同一屏的 Area/Donut 已经是空卡片了。统一成"合计为 0"（见 chartDefaults）。
const isEmpty = computed(() => isChartEmpty(props.items.map((item) => item.value)));

/** 百分比宽度：最大值占满，最小值给 2% 免得看起来像 0。 */
function ratio(value: number): string {
  if (max.value <= 0) return "0%";
  if (value <= 0) return "0%";
  return `${Math.max(2, Math.round((value / max.value) * 100))}%`;
}
</script>

<template>
  <ul v-if="!isEmpty" class="bars">
    <li v-for="item in items" :key="item.key" class="bars__row">
      <span class="bars__label" :title="item.label">{{ item.label }}</span>
      <span class="bars__track">
        <span
          class="bars__fill"
          :style="{ width: ratio(item.value), background: item.color ?? 'var(--c-accent)' }"
        />
      </span>
      <b class="bars__value">{{ formatValue(item.value) }}</b>
      <span v-if="item.hint" class="bars__hint">{{ item.hint }}</span>
    </li>
  </ul>
  <AppEmpty v-else :title="emptyText" />
</template>

<style scoped>
.bars {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

.bars__row {
  display: grid;
  grid-template-columns: minmax(64px, 96px) minmax(0, 1fr) auto;
  align-items: center;
  gap: var(--sp-2);
}

.bars__label {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
}

.bars__track {
  height: 8px;
  border-radius: var(--r-pill);
  background: var(--c-surface-sunken);
  overflow: hidden;
}

.bars__fill {
  display: block;
  height: 100%;
  border-radius: var(--r-pill);
  transition: width 0.3s ease;
}

.bars__value {
  font-size: var(--fs-sm);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.bars__hint {
  grid-column: 2 / -1;
  margin-top: calc(var(--sp-2) * -1 + 2px);
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  font-variant-numeric: tabular-nums;
}

</style>
