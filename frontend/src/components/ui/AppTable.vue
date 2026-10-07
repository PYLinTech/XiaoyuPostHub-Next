<script setup lang="ts" generic="T">
import { ref, useSlots, watchEffect } from "vue";
import AppEmpty from "./AppEmpty.vue";
import { useOverlayScroll } from "@/lib/overlayScroll";

// 数据表格。
//
// 只做一件有分量的事：在窄屏上自动把每行折叠成"字段名 + 值"的卡片。
// 让二十几个列表页各自处理这个转换是不现实的——那样一定会有页面直接用
// 横向滚动，而横向滚动的表格在手机上基本没法读。
//
// 行类型做成泛型是为了让具名插槽里的 row 直接带上实体类型。否则每个调用点
// 都要写 `(row as SomeType)`，而这类断言一旦写错，编译器不会再帮忙。
//
// 列的 mobile 取 title 时，该列在卡片模式下作为标题单独成行（通常是最关键的
// 一列，例如文件名），不再重复显示字段名。
//
// 宽表在中等视口下仍会横向滚动，滚动条走站内自绘那套（写法见 AppTabs）：
// 原生条会占掉一条宽度，样式也跟着系统走。

export interface Column {
  key: string;
  label: string;
  align?: "left" | "right";
  width?: string;
  /**
   * 卡片模式下作为标题：单独成行且不显示字段名（通常是最关键的一列，例如文件名）。
   *
   * 刻意只提供这一个变体：曾经还有一个"窄屏隐藏此列"，很快就被误用在操作列上，
   * 结果是手机上只剩一个文件名、所有按钮都点不到。**不要**加回"隐藏"这个选项。
   */
  mobile?: "title";
}

const props = withDefaults(
  defineProps<{
    columns: Column[];
    rows: T[];
    loading?: boolean;
    emptyTitle?: string;
    emptyHint?: string;
    /** 卡片模式是否启用；窗口小且列较多时才有意义。 */
    cards?: boolean;
  }>(),
  { loading: false, emptyTitle: "暂无数据", emptyHint: "", cards: true },
);

const slots = useSlots();
const scroller = ref<HTMLElement | null>(null);
const { visible, style, onThumbPointerDown, onThumbPointerMove, onThumbPointerUp } =
  useOverlayScroll(scroller, "x");

/** 兜底取值：列没有对应插槽时按字段名取值。 */
function rawValue(row: T, key: string): unknown {
  return (row as Record<string, unknown>)[key];
}

// 列键与行字段名不一致、又没有为该列提供插槽时，兜底路径会安静地渲染成"—"，
// 而"—"本身是合法展示值（例如文件夹没有大小），因此这类错误在界面上看不出来。
// 只在开发构建里检查，生产不付这个代价。
if (import.meta.env.DEV) {
  watchEffect(() => {
    const first = props.rows[0];
    if (!first || typeof first !== "object") {
      return;
    }
    const holder = first as Record<string, unknown>;
    for (const column of props.columns) {
      if (column.key in holder || column.key in slots) {
        continue;
      }
      console.warn(
        `[AppTable] 列「${column.label}」(key=${column.key}) 既没有插槽，行对象上也没有该字段，会渲染成"—"。`,
      );
    }
  });
}
</script>

<template>
  <!-- 轨道挂在不滚动的宿主上：绝对定位的盒子放进滚动容器会跟着内容一起滚走。 -->
  <div class="ovscroll-layer ovscroll-layer--x">
    <div ref="scroller" class="table-wrap ovscroll">
      <table class="table" :class="{ 'table--cards': cards }">
        <!-- 没有数据时不渲染表头：「还没有…」的说明配上几列空白表头只会显得像坏了。 -->
        <thead v-if="loading || rows.length > 0">
          <tr>
            <th
              v-for="column in columns"
              :key="column.key"
              :style="column.width ? { width: column.width } : undefined"
              :class="{ num: column.align === 'right' }"
            >
              {{ column.label }}
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="loading" class="table__row--state">
            <td class="table__state" :colspan="columns.length">
              <div class="table-state"><span class="spinner" /> 正在加载</div>
            </td>
          </tr>
          <tr v-else-if="rows.length === 0" class="table__row--state">
            <td class="table__state" :colspan="columns.length">
              <AppEmpty :title="emptyTitle" :hint="emptyHint" />
            </td>
          </tr>
          <tr v-for="(row, index) in loading ? [] : rows" :key="index">
            <td
              v-for="column in columns"
              :key="column.key"
              :data-label="column.mobile === 'title' ? '' : column.label"
              :class="{
                num: column.align === 'right',
                'card-title': column.mobile === 'title',
                /* 操作列在卡片模式下要独占一行放按钮，见 app.css 的 .table--cards */
                'table__cell--actions': column.key === 'actions',
              }"
            >
              <slot :name="column.key" :row="row" :value="rawValue(row, column.key)" :index="index">
                {{ rawValue(row, column.key) ?? "—" }}
              </slot>
            </td>
          </tr>
        </tbody>
      </table>
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

<style scoped>
/* 状态行（加载 / 空）：单元格自己不占内边距，留白交给里面的状态块，
   否则会出现"表格式内边距 + 状态块内边距"两层叠加的大空洞。 */
.table__state {
  padding: 0;
}

/* 状态行不是一条记录。窄屏下每行都会被渲染成"带边框圆角的卡片"，
   空状态那一行若跟着一起套，面板里就会凭空多出一层框——看上去像
   "空列表里放了一张空卡片"，比不套卡片还难看。 */
.table--cards tr.table__row--state {
  border: 0;
  border-radius: 0;
  margin-bottom: 0;
  background: transparent;
  padding: 0;
}

@media (max-width: 720px) {
  .table--cards :deep(td.card-title) {
    display: block;
    font-weight: 600;
    font-size: var(--fs-md);
    padding-top: var(--sp-2);
  }

  .table--cards :deep(td.card-title::before) {
    content: none;
  }
}
</style>
