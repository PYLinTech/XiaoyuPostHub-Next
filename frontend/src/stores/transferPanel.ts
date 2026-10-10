import { reactive } from "vue";

export type TransferKind = "upload" | "download";
export const transferPanel = reactive({ selected: "upload" as TransferKind, collapsed: false });

/** 新任务打开对应标签；用户切换标签后，进度更新不抢回焦点。 */
export function showTransfer(kind: TransferKind): void {
  transferPanel.selected = kind;
  transferPanel.collapsed = false;
}
