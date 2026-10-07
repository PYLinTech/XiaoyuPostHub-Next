<script setup lang="ts">
import { ref } from "vue";
import AppButton from "@/components/ui/AppButton.vue";

// 统一筛选条：常用字段一行常显，回车即查；低频条件放 #extra 折叠区。
// immediate 模式（客户端即时过滤）不渲染查询/重置按钮。
// flat 模式去掉卡片边框与内边距，筛选控件直接落在页面上（配合下方 flush 表格）。
const props = defineProps<{ busy?: boolean; immediate?: boolean; flat?: boolean }>();
const emit = defineEmits<{ search: []; reset: [] }>();

const expanded = ref(false);

function submit(): void {
  emit("search");
}

function reset(): void {
  expanded.value = false;
  emit("reset");
}
</script>

<template>
  <form class="filterbar" :class="{ 'filterbar--flat': props.flat }" @submit.prevent="submit">
    <div class="filterbar__row">
      <slot />
      <span v-if="!props.immediate && $slots.extra" class="filterbar__sp" />
      <button
        v-if="$slots.extra"
        type="button"
        class="filterbar__more"
        :aria-expanded="expanded"
        @click="expanded = !expanded"
      >
        {{ expanded ? "收起筛选" : "更多筛选" }}
      </button>
      <span class="filterbar__sp" />
      <template v-if="!props.immediate">
        <AppButton size="sm" variant="primary" icon="search" :loading="props.busy" type="submit">
          查询
        </AppButton>
        <AppButton size="sm" icon="refresh" :disabled="props.busy" @click="reset">重置</AppButton>
      </template>
    </div>
    <div v-if="expanded && $slots.extra" class="filterbar__extra">
      <div class="filterbar__row">
        <slot name="extra" />
      </div>
    </div>
  </form>
</template>

<style scoped>
.filterbar {
  border: 1px solid var(--c-border-card);
  border-radius: var(--r-lg);
  background: var(--c-surface);
  box-shadow: var(--shadow-sm);
  padding: var(--sp-2) var(--sp-3);
}

.filterbar--flat {
  border: 0;
  background: transparent;
  box-shadow: none;
  padding: 0;
}

.filterbar--flat .filterbar__extra {
  margin-top: var(--sp-2);
}

.filterbar__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
}

/* 直接子元素一律允许收缩，否则 flex 项的 min-width:auto 会让长标签把整行撑破。
   原先还有两条 :deep(.form-grid) / :deep(.form-group) 在前面，但最后这条已经
   覆盖了全部直接子元素，那两条没有任何增量——而且 .form-group 在整个前端里
   根本没人用过（app.css 也没定义它），是一条纯死规则。 */
.filterbar__row > * {
  min-width: 0;
}

.filterbar__sp {
  flex: 1;
}

.filterbar__more {
  border: 0;
  background: transparent;
  padding: var(--sp-1) var(--sp-2);
  color: var(--c-accent);
  font: inherit;
  font-size: var(--fs-sm);
  cursor: pointer;
  border-radius: var(--r-sm);
  white-space: nowrap;
}

.filterbar__more:hover {
  background: var(--c-hover);
}

.filterbar__extra {
  margin-top: var(--sp-2);
  padding-top: var(--sp-2);
  border-top: 1px dashed color-mix(in srgb, var(--c-border) 70%, transparent);
}
</style>
