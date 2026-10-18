// 仅保存在当前页面实例的内存中：站内离开后返回可恢复，关闭标签页/刷新后不恢复，
// 同时避免多个标签页互相覆盖目录状态。
const lastPathByActor = new Map<string, string>();

function actorKey(actor: string): string {
  return actor || "guest";
}

/** 记录当前文件目录；根目录也是有效值，表示用户明确回到了根目录。 */
export function rememberFilePath(actor: string, path: string): void {
  if (!path.startsWith("/") || path.startsWith("//")) return;
  lastPathByActor.set(actorKey(actor), path);
}

/** 返回当前标签页中最近访问的目录；没有记录时停留根目录。 */
export function readRememberedFilePath(actor: string): string {
  return lastPathByActor.get(actorKey(actor)) ?? "/";
}
