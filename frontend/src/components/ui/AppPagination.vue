<script setup lang="ts">
import { computed } from "vue";
import AppButton from "./AppButton.vue";
import AppIcon from "./AppIcon.vue";
import AppSelect from "./AppSelect.vue";

// 分页。
//
// 后端的 limit 上限是 500，越界会被静默改回 100，因此这里也把选项限制在
// 合法范围内——让界面只提供后端真正接受的取值，比让用户填完再被静默改掉好。

const props = withDefaults(
  defineProps<{
    total: number;
    limit: number;
    offset: number;
    /** total 是否精确。否为时只提供"下一页"，因为算不出总页数。 */
    exact?: boolean;
    /**
     * 不精确计数时是否还有下一页。列表接口不回总数，算不出总页数，但"本页
     * 返回的条数少于 limit"足以断定没有下一页——不传这个信号的话，翻到底后
     * "下一页"永远可点，点进去是一页空列表。
     */
    hasMore?: boolean;
  }>(),
  { exact: true, hasMore: true },
);

const emit = defineEmits<{ "update:limit": [number]; "update:offset": [number] }>();

const rowsOnPage = computed(() => Math.min(props.limit, Math.max(0, props.total - props.offset)));

const PAGE_SIZES = [20, 50, 100, 200].map((value) => ({ value, label: `${value} / 页` }));

function goto(next: number): void {
  emit("update:offset", Math.max(0, next));
}
</script>

<template>
  <!-- 一条记录都没有的时候整条都不显示：一串「已显示 0 条」加两个灰掉的翻页
       按钮，看着像坏了，也点不动任何东西。 -->
  <div v-if="total > 0 || offset > 0" class="pagination">
    <span>
      <template v-if="exact">
        第 {{ total === 0 ? 0 : offset + 1 }}–{{ Math.min(offset + limit, total) }} 条 / 共 {{ total }} 条
      </template>
      <template v-else>已显示 {{ rowsOnPage }} 条</template>
    </span>
    <div class="row">
      <AppSelect
        :model-value="limit"
        :options="PAGE_SIZES"
        aria-label="每页条数"
        size="sm"
        @update:model-value="emit('update:limit', $event)"
      />
      <AppButton size="sm" :disabled="offset <= 0" icon="chevronLeft" @click="goto(offset - limit)">
        上一页
      </AppButton>
      <AppButton
        size="sm"
        :disabled="exact ? offset + limit >= total : !hasMore"
        @click="goto(offset + limit)"
      >
        下一页
        <AppIcon name="chevronRight" :size="14" />
      </AppButton>
    </div>
  </div>
</template>
