import { shallowRef } from "vue";

/**
 * 顶栏右侧的操作区。
 *
 * 各页面用 `<Teleport :to="topbarSlot">` 把"这一页的按钮"投递上来，这样操作条
 * 的长相只有一处定义，页面也不必各自维护一条工具栏。
 *
 * 为什么传元素而不是写 `#id` 选择器：外壳是"先把子树建好、再插入文档"的，
 * 首次挂载时目标节点还不在文档里，`querySelector` 找不到它，Teleport 会直接
 * 放弃渲染（并且只在控制台留一条警告）。传元素引用就没有这个时序问题——
 * 哪怕它还挂在未插入的子树里，Teleport 也能把内容塞进去，随子树一起进文档。
 */
export const topbarSlot = shallowRef<HTMLElement | null>(null);
