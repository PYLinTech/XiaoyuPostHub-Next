<script setup lang="ts">
import { computed } from "vue";
import AppEmpty from "./AppEmpty.vue";
import ChartLegend from "./ChartLegend.vue";
import { chartDefaults, isChartEmpty } from "./chartDefaults";

// 环形图：用一个 circle 的 stroke-dasharray 切出各段，比逐段画 path 少一半节点，
// 也天然避免了相邻扇区之间因浮点误差裂开的缝。

export interface DonutItem {
  key: string;
  label: string;
  value: number;
  color: string;
}

const props = withDefaults(
  defineProps<{
    items: DonutItem[];
    size?: number;
    thickness?: number;
    centerLabel?: string;
    formatValue?: (value: number) => string;
    emptyText?: string;
  }>(),
  {
    ...chartDefaults,
    size: 168,
    thickness: 22,
    centerLabel: "",
  },
);

const total = computed(() => props.items.reduce((sum, item) => sum + item.value, 0));

// 与 Area/Bars 同一口径（见 chartDefaults）。Donut 的 total 本来就是这同一个求和，
// 这里仍走共享判定，是为了让"什么算没数据"只存在于一个地方。
const isEmpty = computed(() => isChartEmpty(props.items.map((item) => item.value)));

const radius = computed(() => (props.size - props.thickness) / 2);
const center = computed(() => props.size / 2);
const circumference = computed(() => 2 * Math.PI * radius.value);

const segments = computed(() => {
  if (total.value <= 0) return [];
  let offset = 0;
  return props.items
    .filter((item) => item.value > 0)
    .map((item) => {
      const ratio = item.value / total.value;
      // 段与段之间留 2px 的缝：直接首尾相接时，深色描边上会看到细密的锯齿。
      const len = Math.max(0, circumference.value * ratio - 2);
      const seg = {
        key: item.key,
        label: item.label,
        value: item.value,
        color: item.color,
        percent: ratio,
        dash: `${len} ${circumference.value - len}`,
        // 负号让剩余段从 12 点方向顺时针排开；这里用正偏移配合 rotate(-90) 实现。
        offset: -offset,
      };
      offset += circumference.value * ratio;
      return seg;
    });
});
</script>

<template>
  <!-- 空态直接换成全站统一的 AppEmpty，而不是在 168px 的圆盘框里塞一行小字：
       那样空卡片会比有数据的卡片矮一大截，一行卡片里高低不齐。 -->
  <AppEmpty v-if="isEmpty" :title="emptyText" />

  <div v-else class="donut">
    <div class="donut__figure" :style="{ width: `${size}px`, height: `${size}px` }">
      <svg :width="size" :height="size" role="img" aria-label="占比分布">
        <g :transform="`rotate(-90 ${center} ${center})`">
          <circle
            v-for="seg in segments"
            :key="seg.key"
            :cx="center"
            :cy="center"
            :r="radius"
            fill="none"
            :stroke="seg.color"
            :stroke-width="thickness"
            :stroke-dasharray="seg.dash"
            :stroke-dashoffset="seg.offset"
            stroke-linecap="butt"
          />
        </g>
      </svg>

      <div class="donut__center">
        <strong>{{ formatValue(total) }}</strong>
        <span v-if="centerLabel">{{ centerLabel }}</span>
      </div>
    </div>

    <!-- 图例骨架与趋势图共用 ChartLegend（色点/标签/数值原本在这里有一份抄的）。
         数据源用 segments 而不是 items：占比 segments 里已经算过一遍，模板里再写
         Math.round(value/total*100) 属于把算好的东西重算；而且 segments 已经滤掉了
         0 值项，图例里挂一条"0 个 / 0%"只是噪音。百分比由 formatPercent 负责收拢。 -->
    <ChartLegend layout="list" show-percent :items="segments" :format-value="formatValue" />
  </div>
</template>

<style scoped>
.donut {
  display: flex;
  align-items: center;
  gap: var(--sp-5);
  flex-wrap: wrap;
}

.donut__figure {
  position: relative;
  flex: none;
  display: grid;
  place-items: center;
}

.donut__figure svg {
  position: absolute;
  inset: 0;
}

.donut__center {
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 2px;
  text-align: center;
}

.donut__center strong {
  font-size: 22px;
  font-weight: 700;
  line-height: 1.1;
  font-variant-numeric: tabular-nums;
}

.donut__center span {
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
}
</style>
