import { readonly, ref } from "vue";

const open = ref(false);
let resolvePending: ((allowed: boolean) => void) | null = null;

function copyCurrentPageUrl(): void {
  const value = window.location.href;
  if (navigator.clipboard?.writeText) {
    void navigator.clipboard.writeText(value).catch(() => {});
    return;
  }
  const input = document.createElement("textarea");
  input.value = value;
  input.setAttribute("readonly", "");
  input.style.position = "fixed";
  input.style.opacity = "0";
  document.body.appendChild(input);
  input.select();
  try { document.execCommand("copy"); } catch { /* 剪贴板权限不可用时静默忽略 */ }
  input.remove();
}

/** 仅用于用户主动保存文件；不影响预览、正文资源和直链。 */
export function confirmDownloadBrowser(): Promise<boolean> {
  const isRestrictedUA = /MicroMessenger|\bQQ\/|AlipayClient|AlipayDefined/i.test(navigator.userAgent);
  if (!isRestrictedUA) return Promise.resolve(true);
  // 已有引导时不覆盖原操作，也不重复弹窗。
  if (resolvePending) return Promise.resolve(false);
  copyCurrentPageUrl();
  open.value = true;
  return new Promise(resolve => { resolvePending = resolve; });
}

export function finishDownloadBrowser(allowed: boolean): void {
  const resolve = resolvePending;
  resolvePending = null;
  open.value = false;
  resolve?.(allowed);
}

export const downloadBrowserOpen = readonly(open);
