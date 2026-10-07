<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useId } from "vue";
import AppEmpty from "./AppEmpty.vue";
import ChartLegend, { type LegendItem } from "./ChartLegend.vue";
import { chartDefaults, isChartEmpty } from "./chartDefaults";

// 趋势面积图。
//
// 手写 SVG 而不是引图表库：这个站点的界面几乎全是"列表 + 表单"，为了一张图
// 引入几百 KB 的依赖、并额外承担一套主题适配与深色模式适配，不划算。
//
// 宽度靠 ResizeObserver 实测而不是 SVG 的 viewBox 缩放：viewBox 会连同文字
// 一起等比缩小，窄屏上坐标轴标签会缩到看不清；而图表的横轴密度本来就该跟着
// 可用宽度自适应，实测宽度才能既保住字号、又决定刻度间隔。

export interface AreaSeries {
  key: string;
  label: string;
  /** 直接传 CSS 变量（var(--c-accent)），这样深色模式不用另写一套。 */
  color: string;
  values: number[];
}

const props = withDefaults(
  defineProps<{
    series: AreaSeries[];
    /** 横轴标签，与 series[].values 等长。 */
    labels: string[];
    height?: number;
    formatValue?: (value: number) => string;
    /** 空态文案。有数据时忽略。 */
    emptyText?: string;
  }>(),
  {
    ...chartDefaults,
    height: 240,
  },
);

const wrap = ref<HTMLElement | null>(null);
const width = ref(720);
let observer: ResizeObserver | null = null;

onMounted(() => {
  if (!wrap.value) return;
  observer = new ResizeObserver((entries) => {
    const w = entries[0]?.contentRect.width ?? 0;
    // 低于 1px 说明容器还没布局完成，先不写回，避免和布局互相触发。
    if (w > 1) width.value = w;
  });
  observer.observe(wrap.value);
  width.value = wrap.value.clientWidth;
});
onBeforeUnmount(() => {
  observer?.disconnect();
  observer = null;
});

// 渐变 id 必须逐实例唯一。url(#id) 是在整篇文档作用域里解析的，不受组件边界约束，
// 而 key 是调用方给的数据：同页放两张 key 相同的趋势图时，第二张的 fill 会静默命中
// 第一张的 <defs>，界面上完全看不出异常——两张图长得一模一样，数据却不同。
// useId() 给每个实例一个稳定且唯一的序列号（SSR/水合时两侧也一致）。
const uid = useId();

function gradId(key: string): string {
  return `area-grad-${uid}-${key}`;
}

// 留白：左边给 Y 轴刻度文字，底部给日期。
const PAD = { top: 14, right: 10, bottom: 26, left: 52 };

const count = computed(() => props.series[0]?.values.length ?? 0);
const innerW = computed(() => Math.max(10, width.value - PAD.left - PAD.right));
const innerH = computed(() => Math.max(10, props.height - PAD.top - PAD.bottom));

const maxValue = computed(() => {
  let max = 0;
  for (const s of props.series) {
    for (const v of s.values) {
      if (v > max) max = v;
    }
  }
  return max;
});

/** 把最大值抬到 1/2/5×10^k 的整数档，让 Y 轴刻度是能读出来的数。 */
function niceCeil(raw: number): number {
  if (raw <= 0) return 1;
  const exp = Math.floor(Math.log10(raw));
  const base = 10 ** exp;
  const frac = raw / base;
  const step = frac <= 1 ? 1 : frac <= 2 ? 2 : frac <= 5 ? 5 : 10;
  return step * base;
}

const top = computed(() => niceCeil(maxValue.value));

function xAt(i: number): number {
  // 单点时画在正中：除以 (n-1) 会除零。
  if (count.value <= 1) return PAD.left + innerW.value / 2;
  return PAD.left + (i / (count.value - 1)) * innerW.value;
}

function yAt(v: number): number {
  return PAD.top + innerH.value - (v / top.value) * innerH.value;
}

/** 折线用直线段而不是平滑曲线：流量是每天的离散值，插出来的中间点是假的。 */
function linePath(values: number[]): string {
  return values.map((v, i) => `${i === 0 ? "M" : "L"}${xAt(i).toFixed(2)},${yAt(v).toFixed(2)}`).join(" ");
}

function areaPath(values: number[]): string {
  if (!values.length) return "";
  const base = PAD.top + innerH.value;
  const head = values.map((v, i) => `${i === 0 ? "M" : "L"}${xAt(i).toFixed(2)},${yAt(v).toFixed(2)}`).join(" ");
  return `${head} L${xAt(values.length - 1).toFixed(2)},${base} L${xAt(0).toFixed(2)},${base} Z`;
}

const gridLines = computed(() =>
  [0, 0.25, 0.5, 0.75, 1].map((t) => ({
    y: PAD.top + innerH.value - t * innerH.value,
    label: props.formatValue(top.value * t),
  })),
);

/** 横轴最多 6 个标签，均匀取样——比让浏览器自己抽稀更可控。 */
const xLabels = computed(() => {
  const n = count.value;
  if (!n) return [];
  const want = Math.min(6, n);
  const step = Math.max(1, Math.floor((n - 1) / (want - 1 || 1)));
  const out: { x: number; text: string }[] = [];
  for (let i = 0; i < n; i += step) {
    out.push({ x: xAt(i), text: props.labels[i] ?? "" });
  }
  return out;
});

// 空态判定与 Donut/Bars 共用同一个口径（见 chartDefaults 的说明）：
// 一条 series 都没有、或所有点加起来是 0，都算没有数据。
const isEmpty = computed(() => isChartEmpty(props.series.flatMap((s) => s.values)));

/** 喂给共享图例的形状。图例本身只有色点和标签，数值与占比是 Donut 才有的。 */
const legendItems = computed<LegendItem[]>(() =>
  props.series.map((s) => ({ key: s.key, label: s.label, color: s.color })),
);

/** #legend 插槽要的是整个 series 对象，这里按 key 取回；取不到就退到第一个（此时列表为空，不会渲染）。 */
function seriesByKey(key: string): AreaSeries {
  return props.series.find((s) => s.key === key) ?? props.series[0];
}

// ---- 悬停 ----
const hoverIndex = ref<number | null>(null);

function onMove(e: MouseEvent) {
  const el = e.currentTarget as SVGSVGElement;
  const rect = el.getBoundingClientRect();
  const x = e.clientX - rect.left - PAD.left;
  if (count.value <= 1) {
    hoverIndex.value = 0;
    return;
  }
  const ratio = Math.min(1, Math.max(0, x / innerW.value));
  hoverIndex.value = Math.round(ratio * (count.value - 1));
}

const hoverX = computed(() =>
  hoverIndex.value === null ? 0 : xAt(hoverIndex.value),
);
const tooltip = computed(() => {
  const i = hoverIndex.value;
  if (i === null || i < 0 || i >= count.value) return null;
  return {
    left: hoverX.value,
    rows: props.series.map((s) => ({
      key: s.key,
      label: s.label,
      color: s.color,
      value: props.formatValue(s.values[i] ?? 0),
    })),
  };
});
</script>

<template>
  <div ref="wrap" class="area" :style="{ height: `${height}px` }">
    <svg
      v-if="!isEmpty"
      class="area__svg"
      :width="width"
      :height="height"
      role="img"
      :aria-label="series.map((s) => s.label).join('、') + '趋势'"
      @mousemove="onMove"
      @mouseleave="hoverIndex = null"
    >
      <g class="area__grid">
        <line
          v-for="line in gridLines"
          :key="line.y"
          :x1="PAD.left"
          :x2="PAD.left + innerW"
          :y1="line.y"
          :y2="line.y"
        />
      </g>
      <g class="area__ylab">
        <text v-for="line in gridLines" :key="line.y" :x="PAD.left - 8" :y="line.y" text-anchor="end" dominant-baseline="middle">
          {{ line.label }}
        </text>
      </g>

      <g v-for="s in series" :key="s.key">
        <path :d="areaPath(s.values)" :fill="`url(#${gradId(s.key)})`" />
        <path class="area__line" :d="linePath(s.values)" :stroke="s.color" />
      </g>

      <g v-if="hoverIndex !== null">
        <line class="area__cross" :x1="hoverX" :x2="hoverX" :y1="PAD.top" :y2="PAD.top + innerH" />
        <circle
          v-for="s in series"
          :key="s.key"
          :cx="hoverX"
          :cy="yAt(s.values[hoverIndex] ?? 0)"
          r="4"
          :fill="s.color"
        />
      </g>

      <g class="area__xlab">
        <text v-for="item in xLabels" :key="item.x" :x="item.x" :y="PAD.top + innerH + 18" text-anchor="middle">
          {{ item.text }}
        </text>
      </g>

      <defs>
        <linearGradient v-for="s in series" :id="gradId(s.key)" :key="s.key" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" :stop-color="s.color" stop-opacity="0.26" />
          <stop offset="100%" :stop-color="s.color" stop-opacity="0.02" />
        </linearGradient>
      </defs>
    </svg>

    <AppEmpty v-else :title="emptyText" />

    <div
      v-if="tooltip"
      class="area__tip"
      :style="{ left: `${Math.min(Math.max(tooltip.left, 70), width - 70)}px` }"
    >
      <span class="area__tip-day">{{ labels[hoverIndex ?? 0] }}</span>
      <span v-for="row in tooltip.rows" :key="row.key" class="area__tip-row">
        <i class="area__dot" :style="{ background: row.color }" />
        <span class="area__tip-label">{{ row.label }}</span>
        <b>{{ row.value }}</b>
      </span>
    </div>

    <!-- 图例骨架搬到了 ChartLegend：圆点/标签的规则原本和 Donut 那份逐字节相同。
         #legend 插槽照旧透传 series，调用点不感知这次合并。 -->
    <ChartLegend v-if="series.length > 1 && !isEmpty" :items="legendItems">
      <template #default="{ item }">
        <slot name="legend" :series="seriesByKey(item.key)">{{ item.label }}</slot>
      </template>
    </ChartLegend>
  </div>
</template>

<style scoped>
.area {
  position: relative;
  width: 100%;
  /* 撑满卡片剩余高度：有数据时图表居中，空态时 AppEmpty 居中，
     两者在同一个盒子里换位，卡片高度才不会因为有无数据而跳。 */
  display: flex;
  flex-direction: column;
  justify-content: center;
}

.area__svg {
  display: block;
  overflow: visible;
}

.area__grid line {
  stroke: color-mix(in srgb, var(--c-border) 70%, transparent);
  stroke-width: 1;
}

.area__line {
  fill: none;
  stroke-width: 2;
  stroke-linejoin: round;
  stroke-linecap: round;
}

.area__cross {
  stroke: var(--c-border-strong);
  stroke-width: 1;
  stroke-dasharray: 3 3;
}

.area__ylab text,
.area__xlab text {
  fill: var(--c-text-faint);
  font-size: 11px;
  font-variant-numeric: tabular-nums;
}

.area__tip {
  position: absolute;
  top: 0;
  transform: translateX(-50%);
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface);
  box-shadow: var(--shadow-md);
  font-size: var(--fs-xs);
  pointer-events: none;
  white-space: nowrap;
  z-index: 2;
}

.area__tip-day {
  color: var(--c-text-faint);
  margin-bottom: 2px;
}

.area__tip-row {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
}

.area__tip-label {
  color: var(--c-text-muted);
}

.area__tip-row b {
  margin-left: auto;
  font-variant-numeric: tabular-nums;
}

/* 悬停浮层里的圆点。趋势图自己的图例已搬到 ChartLegend，但浮层这一份还在用。 */
.area__dot {
  width: 8px;
  height: 8px;
  flex: none;
  border-radius: var(--r-pill);
}
</style>
