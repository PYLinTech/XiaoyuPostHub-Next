<script setup lang="ts">
import { computed } from "vue";
import { useRoute } from "vue-router";
import AppIcon from "@/components/ui/AppIcon.vue";
import { topbarSlot } from "@/stores/shell";

// 顶栏：左侧是当前页面名，右侧是这一页自己的操作项。
//
// 操作项不在这里硬编码，而是各页用 <Teleport :to="topbarSlot"> 投递进来：
// 「操作条」的长相因此只有一处定义，各页也省掉自己那条工具栏。
// 全局入口不在这儿——公告与主题在侧栏底部的工具行，账号与退出登录也在侧栏。

const emit = defineEmits<{ "toggle-drawer": [] }>();

const route = useRoute();

const title = computed(() => String(route.meta.title ?? ""));

/** 函数式 ref：挂载时同步登记，页面才来得及在同一个渲染批次里投递内容。 */
function bindSlot(element: unknown): void {
  topbarSlot.value = (element as HTMLElement | null) ?? null;
}
</script>

<template>
  <header class="shell__topbar">
    <div v-if="title" class="topbar__heading">
      <button
        type="button"
        class="btn btn--ghost btn--sm topbar__burger"
        aria-label="打开导航"
        @click="emit('toggle-drawer')"
      >
        <AppIcon name="menu" :size="18" />
      </button>
      <span class="topbar__title truncate">{{ title }}</span>
    </div>

    <!-- 撑杆：不带宽度，只吃本行剩下的空格，把操作项顶到右端。 -->
    <span v-if="title" class="topbar__gap" />

    <div :ref="bindSlot" class="topbar__actions" />
  </header>
</template>

<style scoped>
/* 一行 flex：标题 + 撑杆 + 操作项。操作项不生成盒子，是这一行的普通项，
   所以标题那行放得下就先用掉、放不下才换行，换行后的行照样靠右；顺序即各页的书写顺序。
   垂直方向沿用 .shell__topbar 自己的 align-items: center。 */
.shell__topbar {
  flex-wrap: wrap;
  justify-content: flex-end;
  row-gap: var(--sp-1);
}

.topbar__gap {
  flex: 1 1 0;
}

.topbar__actions {
  display: contents;
}

.topbar__heading {
  min-width: 0;
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.topbar__title {
  font-size: var(--fs-lg);
  font-weight: 680;
  color: var(--c-text);
  min-width: 0;
}

.topbar__burger {
  display: none;
}

@media (max-width: 880px) {
  .topbar__burger {
    display: inline-flex;
  }
}
</style>
