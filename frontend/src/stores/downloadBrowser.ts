import { readonly, ref } from "vue";

const open = ref(false);
let resolvePending: ((allowed: boolean) => void) | null = null;

/** 仅用于用户主动保存文件；不影响预览、正文资源和直链。 */
export function confirmDownloadBrowser(): Promise<boolean> {
  const isRestrictedUA = /MicroMessenger|\bQQ\/|AlipayClient|AlipayDefined/i.test(navigator.userAgent);
  if (!isRestrictedUA) return Promise.resolve(true);
  // 已有引导时不覆盖原操作，也不重复弹窗。
  if (resolvePending) return Promise.resolve(false);
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
