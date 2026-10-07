// 图表组件共享的默认值与空态判定。
//
// formatValue / emptyText 原先在 ChartArea、ChartDonut、ChartBars 里各写一份字面量：
// 改一次默认文案要改三处，而漏掉的那一处只会在概览页显示成"另有一说"的空态提示，
// 这种差异在 review 里几乎看不出来，等用户报"这页字不一样"才知道。
// 收敛到一个模块、导出后用展开语法合进各自的 withDefaults，是因为 withDefaults
// 只吃对象字面量——想真正共用只能再抽一层基类组件转发 props，那点样板不划算。

export const chartDefaults: {
  formatValue: (value: number) => string;
  emptyText: string;
} = {
  formatValue: (value: number) => String(value),
  emptyText: "暂无数据",
};

/**
 * 空态判定：没有数据项，或所有项合计为 0。
 *
 * 三个图表原先各判各的——Area 看 max <= 0、Donut 看 total <= 0、Bars 只看 items.length。
 * 同一组全 0 的数据在 Area/Donut 是空卡片，在 Bars 却是一排占着位的空槽，
 * 概览页四张图并排时就会一张有数据一张没数据。现在统一成"合计为 0"。
 *
 * 用合计而不是最大值：这两个图的取值域都是非负，两者等价；但合计能顺带覆盖
 * "一项都没有"的情况，不必让每个调用方自己再判一次长度。
 */
export function isChartEmpty(values: readonly number[]): boolean {
  return values.reduce((sum, value) => sum + value, 0) <= 0;
}
