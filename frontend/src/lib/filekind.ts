// 文件类型判定。
//
// 列表接口按文件名选择类型图标；预览支持范围由预览库统一判断。

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
