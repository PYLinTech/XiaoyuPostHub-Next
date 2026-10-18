export interface DroppedFileBatch {
  files: File[];
  /** 与 files 一一对应，相对路径包含拖入/选择的顶层目录名。 */
  relativePaths: string[];
  /** 相对路径，不含根目录外的目标路径；包含空目录（仅拖放 API 可枚举）。 */
  directories: string[];
}

interface RootItem {
  entry?: FileSystemEntry;
  file?: File;
}

/**
 * DataTransfer.files 在 Chromium/Safari 中通常不会包含拖入目录的子文件；
 * 目录必须通过 webkitGetAsEntry 递归读取。文件列表作为旧浏览器/普通文件的兜底。
 */
export async function collectDroppedFiles(
  items: readonly DataTransferItem[],
  fallbackFiles: ArrayLike<File>,
): Promise<DroppedFileBatch> {
  // 必须在 drop 事件同步阶段快照 FileList 与 entry/File，之后浏览器可能清空 DataTransfer。
  const fallback = Array.from(fallbackFiles);
  const roots: RootItem[] = [];
  for (const item of items) {
    if (item.kind !== "file") continue;
    const entry = typeof item.webkitGetAsEntry === "function" ? item.webkitGetAsEntry() : null;
    if (entry) {
      roots.push({ entry });
      continue;
    }
    const file = item.getAsFile();
    if (file) roots.push({ file });
  }

  const fromFallback = (): DroppedFileBatch => ({
    files: fallback,
    relativePaths: fallback.map((file) => relativePathOf(file)),
    directories: [],
  });
  if (roots.length === 0) return fromFallback();

  const result: DroppedFileBatch = { files: [], relativePaths: [], directories: [] };
  try {
    await Promise.all(roots.map(async (root) => {
      if (root.entry) {
        await collectEntry(root.entry, "", result);
      } else if (root.file) {
        result.files.push(root.file);
        result.relativePaths.push(relativePathOf(root.file));
      }
    }));
  } catch (error) {
    // 仅在 entry 完全没有产出文件时回退，避免异常时悄悄漏掉已递归到的部分内容。
    if (result.files.length === 0 && fallback.length > 0) return fromFallback();
    throw error;
  }

  // 某些拖放来源会给出空 entry；此时使用 drop 同步阶段保存的 FileList。
  if (result.files.length === 0 && fallback.length > 0) return fromFallback();
  return result;
}

function relativePathOf(file: File): string {
  const path = (file as File & { webkitRelativePath?: string }).webkitRelativePath;
  return path || file.name;
}

async function collectEntry(entry: FileSystemEntry, prefix: string, result: DroppedFileBatch): Promise<void> {
  const path = prefix ? `${prefix}/${entry.name}` : entry.name;
  if (entry.isFile) {
    const fileEntry = entry as FileSystemFileEntry;
    const file = await new Promise<File>((resolve, reject) => fileEntry.file(resolve, reject));
    result.files.push(file);
    result.relativePaths.push(path);
    return;
  }
  if (!entry.isDirectory) return;

  result.directories.push(path);
  const reader = (entry as FileSystemDirectoryEntry).createReader();
  while (true) {
    const entries = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
    if (entries.length === 0) return;
    await Promise.all(entries.map((child) => collectEntry(child, path, result)));
  }
}
