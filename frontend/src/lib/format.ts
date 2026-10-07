// 展示层的纯函数。集中在一处是因为"字节怎么显示"在各页面必须一致——
// 各自实现会出现"这一屏用 KB 那一屏用 KiB"，对账时非常难解释。

const UNITS = ["B", "KB", "MB", "GB", "TB", "PB"];

/** 人类可读的字节数。按 1024 进制，与后端的 size 配置口径一致。 */
export function formatBytes(bytes: number | undefined | null): string {
  if (bytes === undefined || bytes === null || Number.isNaN(bytes)) {
    return "—";
  }
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = value >= 100 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(digits)} ${UNITS[unit]}`;
}

/**
 * 补零到两位。
 *
 * 提到模块级并导出：这个一行函数原先在本文件里定义了三次，另外两个组件
 * （日期时间选择器、邮件列表的搜索高亮）又各写了一份。同一条规则散成五份
 * 拷贝，改一次要改五处。
 */
export const pad = (n: number): string => String(n).padStart(2, "0");

/** Unix 秒 → 本地时间字符串。 */
export function formatTime(unixSeconds: number | undefined | null): string {
  if (!unixSeconds) {
    return "—";
  }
  const date = new Date(unixSeconds * 1000);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** Unix 秒 → 只到日期。 */
export function formatDate(unixSeconds: number | undefined | null): string {
  if (!unixSeconds) {
    return "—";
  }
  const date = new Date(unixSeconds * 1000);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** 相对时间。超过 30 天回落到绝对日期——"3 个月前"这种说法没有信息量。 */
export function formatRelative(unixSeconds: number | undefined | null): string {
  if (!unixSeconds) {
    return "—";
  }
  const diffSeconds = Math.floor(Date.now() / 1000) - unixSeconds;
  if (diffSeconds < 0) {
    return formatTime(unixSeconds);
  }
  if (diffSeconds < 60) {
    return "刚刚";
  }
  if (diffSeconds < 3600) {
    return `${Math.floor(diffSeconds / 60)} 分钟前`;
  }
  if (diffSeconds < 86400) {
    return `${Math.floor(diffSeconds / 3600)} 小时前`;
  }
  if (diffSeconds < 86400 * 30) {
    return `${Math.floor(diffSeconds / 86400)} 天前`;
  }
  return formatDate(unixSeconds);
}

/** 秒 → "1 天 3 小时"这类时长描述。 */
/** 把 datetime-local 输入框的值转成 Unix 秒；空串表示"不设"。 */
export function dateTimeLocalToUnix(value: string): number {
  if (!value) {
    return 0;
  }
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? Math.floor(parsed / 1000) : 0;
}

/** Unix 秒 → datetime-local 输入框的值。 */
export function unixToDateTimeLocal(unixSeconds: number | undefined | null): string {
  if (!unixSeconds) {
    return "";
  }
  const date = new Date(unixSeconds * 1000);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** 百分比，带边界收拢。 */
export function formatPercent(part: number, total: number): string {
  if (!total) {
    return "—";
  }
  const ratio = Math.min(1, Math.max(0, part / total));
  return `${(ratio * 100).toFixed(ratio === 1 ? 0 : 1)}%`;
}

/** 路径的最后一段。 */
export function baseName(path: string): string {
  const trimmed = path.replace(/\/+$/, "");
  const index = trimmed.lastIndexOf("/");
  return index >= 0 ? trimmed.slice(index + 1) : trimmed;
}

/** 路径的父目录。 */
export function parentPath(path: string): string {
  const trimmed = path.replace(/\/+$/, "");
  const index = trimmed.lastIndexOf("/");
  if (index <= 0) {
    return "/";
  }
  return trimmed.slice(0, index);
}

/** 把绝对路径拆成面包屑片段。 */
export function pathSegments(path: string): Array<{ name: string; path: string }> {
  const normalized = path.startsWith("/") ? path : `/${path}`;
  const parts = normalized.split("/").filter(Boolean);
  const out: Array<{ name: string; path: string }> = [];
  let cursor = "";
  for (const part of parts) {
    cursor += `/${part}`;
    out.push({ name: part, path: cursor });
  }
  return out;
}
