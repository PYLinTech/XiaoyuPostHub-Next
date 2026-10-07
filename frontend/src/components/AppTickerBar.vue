<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from "vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import { useSite } from "@/stores/site";

// 顶部滚动公告。
//
// 只有一条（服务端用部分唯一索引保证）。无缝跑马灯：内容渲染 N 份，轨道每次
// 恰好位移一份的宽度（含接缝间隔）就复位，看起来永远接得上。份数按
// 「铺满视口 + 1」动态计算——内容比视口短时两份不够，会出现滚到一半的空白。
// 时长按一份宽度折算成恒定速度：写死秒数的话，长文会飞快、短文会爬行。
// 尊重 prefers-reduced-motion：动效敏感的用户看到静止的整段文字。

const site = useSite();
const ticker = computed(() => site.state.ticker);
const text = computed(() => {
  const item = ticker.value;
  if (!item) {
    return "";
  }
  return item.title ? `${item.title}：${item.body}` : item.body;
});

const SPEED = 60; // px/s
const copies = ref(2);
const step = ref(0);
const duration = ref(22);
const viewportEl = ref<HTMLElement | null>(null);
const trackEl = ref<HTMLElement | null>(null);

function measure(): void {
  const track = trackEl.value;
  const viewport = viewportEl.value;
  const first = track?.firstElementChild as HTMLElement | null;
  if (!track || !viewport || !first) {
    return;
  }
  const width = first.offsetWidth;
  if (width <= 0) {
    return;
  }
  step.value = width;
  duration.value = Math.max(6, Math.round(width / SPEED));
  copies.value = Math.max(2, Math.ceil(viewport.clientWidth / width) + 1);
}

onMounted(() => {
  void nextTick(measure);
  // 字体就绪会改变文本实测宽度，就绪后重算一遍步长与份数。
  void document.fonts?.ready.then(measure);
});

watch(text, () => void nextTick(measure));
</script>

<template>
  <div v-if="ticker" class="ticker">
    <span class="ticker__label"><AppIcon name="info" :size="14" /> 公告</span>
    <span ref="viewportEl" class="ticker__viewport">
      <span
        ref="trackEl"
        class="ticker__track"
        :style="{ '--ticker-step': `${step}px`, animationDuration: `${duration}s` }"
      >
        <span
          v-for="index in copies"
          :key="index"
          class="ticker__item"
          :aria-hidden="index > 1 ? 'true' : undefined"
        >{{ text }}</span>
      </span>
    </span>
  </div>
</template>
