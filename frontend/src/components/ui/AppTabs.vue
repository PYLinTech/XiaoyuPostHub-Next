<script setup lang="ts">
import { ref } from "vue";
import { useOverlayScroll } from "@/lib/overlayScroll";

// 页签。
//
// 窄屏上横向滚动而不是折行：折行会让页签区高度随内容变化，切换时页面跳动，
// 而横向滚动的位置是稳定的。滚动条自绘：原生条会占掉一条高度。

export interface TabItem {
  key: string;
  label: string;
  badge?: number;
}

defineProps<{ tabs: TabItem[]; modelValue: string }>();
const emit = defineEmits<{ "update:modelValue": [string] }>();

const scroller = ref<HTMLElement | null>(null);
const { visible, style, onThumbPointerDown, onThumbPointerMove, onThumbPointerUp } =
  useOverlayScroll(scroller, "x");
</script>

<template>
  <!-- 滚动条要浮在页签条上且不随内容滚走，所以挂在外层宿主上而不是滚动容器里。 -->
  <div class="ovscroll-layer ovscroll-layer--x">
    <!-- 刻意不用 role="tablist"/"tab"：那套角色要求每个页签对应一个 role="tabpanel"
         的内容容器，两者靠 id/aria-controls 互指，并且要实现方向键切换。
         两个调用点（AnnouncementsView、MailListPage）的内容都写在各自页面里，
         并没有 tabpanel，组件也拿不到那些容器的 id。只声明角色而不实现行为，
         比不用角色更糟：读屏会念出"选项卡 1/3"，却找不到对应的面板。
         退成普通按钮组 + aria-pressed：当前是哪一档照样读得出来，也不撒谎。 -->
    <div ref="scroller" class="tabs ovscroll">
      <button
        v-for="tab in tabs"
        :key="tab.key"
        type="button"
        class="tab"
        :class="{ 'tab--active': tab.key === modelValue }"
        :aria-pressed="tab.key === modelValue"
        @click="emit('update:modelValue', tab.key)"
      >
        {{ tab.label }}
        <span v-if="tab.badge" class="badge badge--accent" style="margin-left: 6px">{{ tab.badge }}</span>
      </button>
    </div>

    <div class="ovscroll__track" :class="{ 'ovscroll__track--on': visible }">
      <div
        class="ovscroll__thumb"
        :style="style"
        @pointerdown="onThumbPointerDown"
        @pointermove="onThumbPointerMove"
        @pointerup="onThumbPointerUp"
      />
    </div>
  </div>
</template>
