import { computed, reactive } from "vue";
import { announcementApi } from "@/api/endpoints";
import type { Announcement } from "@/api/types";
import { describeError, logError } from "@/lib/async";

// 公告与主题偏好。
//
// 已读状态只存在浏览器里：服务端不记录"谁读过哪条"，因为那会为每一条公告
// 维护一张增长无上限的已读表，而它的收益仅仅是"角标能少一个"。
// 代价是换设备/清缓存后会重新提示一次，可以接受。

type Theme = "light" | "dark";

interface SiteState {
  ticker: Announcement | null;
  items: Announcement[];
  readIds: string[];
  loaded: boolean;
  /** 正在请求公告列表。 */
  loading: boolean;
  /** 上一次请求的失败原因，供公告弹窗显示；空表示没问题。 */
  error: string;
  theme: Theme;
}

const READ_KEY = "xph.readAnnouncements";
const THEME_KEY = "xph.theme";

function readIdsFromStorage(): string[] {
  try {
    const raw = localStorage.getItem(READ_KEY);
    return raw ? (JSON.parse(raw) as string[]) : [];
  } catch {
    return [];
  }
}

function initialTheme(): Theme {
  const fromDom = document.documentElement.dataset.theme;
  if (fromDom === "dark" || fromDom === "light") {
    return fromDom;
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

const state = reactive<SiteState>({
  ticker: null,
  items: [],
  readIds: readIdsFromStorage(),
  loaded: false,
  loading: false,
  error: "",
  theme: initialTheme(),
});

/**
 * 拉取公告列表。
 *
 * 失败只记录不抛出：公告取不到不该影响主流程（登录、浏览文件）。加载与失败
 * 状态都留在 state 上，让公告弹窗能自己显示"正在刷新 / 刷新失败"，而不是
 * 静默给一份空列表。
 */
export async function loadAnnouncements(): Promise<void> {
  state.loading = true;
  try {
    const result = await announcementApi.list();
    state.ticker = result.ticker ?? null;
    state.items = result.items ?? [];
    state.error = "";
  } catch (err) {
    state.error = describeError(err);
    logError("announcements.load", err);
  } finally {
    state.loaded = true;
    state.loading = false;
  }
}

export function markRead(id: string): void {
  if (state.readIds.includes(id)) {
    return;
  }
  state.readIds = [...state.readIds, id];
  try {
    localStorage.setItem(READ_KEY, JSON.stringify(state.readIds));
  } catch {
    // 存不下只影响"下次是否重新提示"。
  }
}

export function markAllRead(): void {
  state.readIds = state.items.map((item) => item.id);
  try {
    localStorage.setItem(READ_KEY, JSON.stringify(state.readIds));
  } catch {
    // 同上。
  }
}

export function isRead(id: string): boolean {
  return state.readIds.includes(id);
}

export function setTheme(theme: Theme): void {
  state.theme = theme;
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(THEME_KEY, theme);
  } catch {
    // 忽略。
  }
}

export function toggleTheme(): void {
  setTheme(state.theme === "dark" ? "light" : "dark");
}

export function useSite() {
  return {
    state,
    unreadCount: computed(
      () => state.items.filter((item) => item.kind !== "ticker" && !state.readIds.includes(item.id)).length,
    ),
    messages: computed(() => state.items.filter((item) => item.kind === "message")),
    announcements: computed(() => state.items.filter((item) => item.kind === "announcement")),
    loadAnnouncements,
    markRead,
    markAllRead,
    isRead,
    setTheme,
    toggleTheme,
  };
}
