import { readonly, ref } from "vue";
import { useToasts } from "@/stores/toast";

const open = ref(false);
let resolvePending: ((allowed: boolean) => void) | null = null;

async function copyCurrentPageUrl(): Promise<boolean> {
  const value = window.location.href;
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value);
      return true;
    }
    const input = document.createElement("textarea");
    input.value = value;
    input.setAttribute("readonly", "");
    input.style.position = "fixed";
    input.style.opacity = "0";
    document.body.appendChild(input);
    try {
      input.select();
      return document.execCommand("copy");
    } finally {
      input.remove();
    }
  } catch {
    return false;
  }
}

/** 仅用于用户主动保存文件；不影响预览、正文资源和直链。 */
export function confirmDownloadBrowser(): Promise<boolean> {
  const isRestrictedUA = /MicroMessenger|\bQQ\/|AlipayClient|AlipayDefined/i.test(navigator.userAgent);
  if (!isRestrictedUA) return Promise.resolve(true);
  // 已有引导时不覆盖原操作，也不重复弹窗。
  if (resolvePending) return Promise.resolve(false);
  void copyCurrentPageUrl().then(copied => {
    const toasts = useToasts();
    if (copied) toasts.success("已复制链接", undefined, "bottom");
    else toasts.error("复制链接失败，请您手动操作", undefined, "bottom");
  });
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
