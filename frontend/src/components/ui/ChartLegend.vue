<script setup lang="ts">
import { computed } from "vue";
import { formatPercent } from "@/lib/format";
import { chartDefaults } from "./chartDefaults";

// 图表图例。
//
// 为什么存在：ChartArea（横排）与 ChartDonut（竖排）原本各带一套"色点 + 标签 + 数值"，
// 连那条 8px 圆点的规则都是逐字节相同的两份。改一次颜色或字号得记得改两处，
// 而图例恰恰是最容易在重排时漏掉的那一块——两份长得一样，diff 里也只是"看起来没变"。

// 这里只收敛"图例由哪几部分组成"，不收敛"长什么样"：layout 决定横排还是竖排，
// 两种排布各自的间距、字号、右对齐规则全部原样保留。两个调用点都在概览大屏上、
// 用户天天看，合并骨架不该带来任何一像素的位移。

export interface LegendItem {
  key: string;
  label: string;
  /** 序列色，通常传 CSS 变量（var(--c-accent)），这样深色模式不用另写一套。 */
  color: string;
  /** 传了才渲染右侧数值；趋势图的图例只有标签，不传。 */
  value?: number;
  /** 0~1 的占比，配合 showPercent 使用。 */
  percent?: number;
}

const props = withDefaults(
  defineProps<{
    items: LegendItem[];
    formatValue?: (value: number) => string;
    showPercent?: boolean;
    layout?: "inline" | "list";
  }>(),
  {
    // 只取 formatValue：chartDefaults 里还带着 emptyText，而图例没有"空态"这个概念
    // （空态是图表卡片的事，见 chartDefaults 的说明）。整份展开会给本组件注入一个
    // 没有声明过的 prop，Vue 会在每次渲染时告警。
    formatValue: chartDefaults.formatValue,
    showPercent: false,
    layout: "inline",
  },
);

// 竖排用真正的列表语义（ul/li），横排用 div/span——和原先两处的 DOM 保持一致。
const isList = computed(() => props.layout === "list");
const rootTag = computed(() => (isList.value ? "ul" : "div"));
const rowTag = computed(() => (isList.value ? "li" : "span"));

/**
 * 占比文本走 lib/format 的 formatPercent，而不是每个图表自己 Math.round：
 * 后者绕过了它里面的边界收拢与兜底。
 *
 * percent 已经是 0~1 的比例（Donut 的 segments 里算好了），所以除以 1
 * 只是为了复用 formatPercent 的收拢逻辑，不是在重算一遍占比。
 */
function percentText(item: LegendItem): string {
  return formatPercent(item.percent ?? 0, 1);
}
</script>

<template>
  <component :is="rootTag" class="legend" :class="`legend--${layout}`">
    <component :is="rowTag" v-for="item in items" :key="item.key" class="legend__row">
      <i class="legend__dot" :style="{ background: item.color }" />
      <!-- 竖排的标签要能省略号截断；横排的标签是紧跟圆点的一小段文字，
           原先就没有截断，所以这里不给它套容器，免得凭空多出一个 flex 项。 -->
      <span v-if="isList" class="legend__label"><slot :item="item">{{ item.label }}</slot></span>
      <slot v-else :item="item" />
      <b v-if="item.value !== undefined" class="legend__value">{{ formatValue(item.value) }}</b>
      <span v-if="showPercent && item.percent !== undefined" class="legend__pct">
        {{ percentText(item) }}
      </span>
    </component>
  </component>
</template>

<style scoped>
/* 横排（趋势图）：紧挨着的一行，宽度不够就换行。 */
.legend--inline {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-3);
  margin-top: var(--sp-2);
  color: var(--c-text-muted);
  font-size: var(--fs-xs);
}

/* 竖排（环形图）：占满剩下的横向空间，和旁边的圆盘并排。 */
.legend--list {
  flex: 1 1 200px;
  min-width: 0;
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.legend__row {
  display: flex;
  align-items: center;
}

.legend--inline .legend__row {
  display: inline-flex;
  gap: var(--sp-1);
}

/* 竖排行间距比横排宽、字号大一档：它是主体信息，不只是"这是什么颜色"的注解。 */
.legend--list .legend__row {
  gap: var(--sp-2);
  font-size: var(--fs-sm);
}

/* 两个图表原先各有这条完全相同的 8px 圆点。 */
.legend__dot {
  width: 8px;
  height: 8px;
  flex: none;
  border-radius: var(--r-pill);
}

.legend--list .legend__label {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--c-text-muted);
}

.legend__value {
  margin-left: auto;
  font-variant-numeric: tabular-nums;
}

.legend__pct {
  width: 42px;
  flex: none;
  text-align: right;
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  font-variant-numeric: tabular-nums;
}
</style>
