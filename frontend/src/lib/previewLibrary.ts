// 仅打开预览时加载组件；PDF.js 在预览 PDF 时才加载。
import { observeXphMsePreview } from "@/lib/xphMsePreview";
import { mediaKind } from "@/lib/mediaFormats";

type PreviewModule = typeof import("@eternalheart/vue-file-preview");
type LoadedPreviewModule = PreviewModule & { observeXphMsePreview: typeof observeXphMsePreview };

let library: Promise<LoadedPreviewModule> | null = null;
export function loadPreviewLibrary() {
  if (!library) library = Promise.all([
    import("@eternalheart/vue-file-preview"),
    import("@eternalheart/vue-file-preview/style.css"),
  ]).then(([lib]) => ({ ...lib, observeXphMsePreview,
    getFileType: (file: Parameters<PreviewModule["getFileType"]>[0]) => mediaKind(file.name, file.type) || lib.getFileType(file),
  }))
    .catch(error => { library = null; throw error; });
  return library;
}
