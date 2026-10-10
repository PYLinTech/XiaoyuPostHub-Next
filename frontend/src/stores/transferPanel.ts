import { reactive } from "vue";

export type TransferKind = "upload" | "download";
export const transferPanel = reactive({ selected: "upload" as TransferKind, collapsed: false, visible: false });

/** 首个任务展开面板；已有任务时保留用户的展开/收起选择。 */
export function showTransfer(kind: TransferKind): void {
  if (!transferPanel.visible) transferPanel.collapsed = false;
  transferPanel.visible = true;
  transferPanel.selected = kind;
}
