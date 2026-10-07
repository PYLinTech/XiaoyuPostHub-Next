// 文件类型判定。
//
// 列表接口只给名字，不给 MIME，因此这里按扩展名判定"能不能预览、用哪种元素
// 预览"。判定结果只影响界面呈现，真正的 MIME 由服务端在下发时给出——
// 前端不参与决定，避免两处规则不一致。

export type FileKind = "image" | "video" | "audio" | "pdf" | "text" | "archive" | "other";

const IMAGE = new Set(["png", "jpg", "jpeg", "gif", "webp", "avif", "bmp", "svg", "ico", "heic"]);
const VIDEO = new Set(["mp4", "webm", "mkv", "mov", "avi", "m4v", "ts"]);
const AUDIO = new Set(["mp3", "wav", "flac", "aac", "ogg", "opus", "m4a", "wma"]);
const TEXT = new Set([
  "txt", "md", "markdown", "json", "xml", "yaml", "yml", "toml", "ini", "conf",
  "log", "csv", "tsv", "html", "htm", "css", "js", "mjs", "ts", "go", "py", "rs",
  "java", "c", "h", "cpp", "sh", "sql",
]);
const ARCHIVE = new Set(["zip", "rar", "7z", "tar", "gz", "bz2", "xz", "zst"]);

export function extensionOf(name: string): string {
  const index = name.lastIndexOf(".");
  if (index <= 0 || index === name.length - 1) {
    return "";
  }
  return name.slice(index + 1).toLowerCase();
}

export function fileKind(name: string): FileKind {
  const ext = extensionOf(name);
  if (IMAGE.has(ext)) return "image";
  if (VIDEO.has(ext)) return "video";
  if (AUDIO.has(ext)) return "audio";
  if (ext === "pdf") return "pdf";
  if (TEXT.has(ext)) return "text";
  if (ARCHIVE.has(ext)) return "archive";
  return "other";
}

/** 图片、音视频、PDF、纯文本可以就地预览；其它类型只能下载。 */
export function isPreviewable(name: string): boolean {
  const kind = fileKind(name);
  return kind === "image" || kind === "video" || kind === "audio" || kind === "pdf" || kind === "text";
}

/**
 * 预览时使用的容器。
 *
 * 文本走的是先整份解密再显示的路径（它通常很小），音视频与图片走流式，
 * 因此两者在界面上是不同的组件。
 */
export function previewElement(kind: FileKind): "img" | "video" | "audio" | "iframe" | "text" | "none" {
  switch (kind) {
    case "image":
      return "img";
    case "video":
      return "video";
    case "audio":
      return "audio";
    case "pdf":
      return "iframe";
    case "text":
      return "text";
    default:
      return "none";
  }
}
