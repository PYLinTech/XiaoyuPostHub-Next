// 仅打开预览时加载组件；PDF.js 在预览 PDF 时才加载。
let library: Promise<typeof import("@eternalheart/vue-file-preview")> | null = null;
export function loadPreviewLibrary() {
  if (!library) library = Promise.all([
    import("@eternalheart/vue-file-preview"),
    import("@eternalheart/vue-file-preview/style.css"),
  ]).then(([lib]) => lib).catch(error => { library = null; throw error; });
  return library;
}
