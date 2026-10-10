<script setup lang="ts">
import { runDelivery } from "@/delivery/transferClient";
import { computed, nextTick, onBeforeUnmount, ref, watch } from "vue";
import { createRequestGate, toastApiError } from "@/lib/async";
import AppButton from "@/components/ui/AppButton.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import { mailApi } from "@/api/endpoints";
import type { MailDetail, MailPart } from "@/api/types";
import { mailPartDeliverySource } from "@/delivery/sources";
import { saveBlob } from "@/delivery/download";
import { useToasts } from "@/stores/toast";
import { formatBytes, formatTime } from "@/lib/format";

// 邮件详情：信头 + 安全渲染的 HTML 正文 + 附件下载。
//
// 安全模型（务必同时满足，缺一条都可能变成 XSS / 追踪像素通道）：
//  1. iframe sandbox 只开脚本，以运行受 nonce 保护的资源错误监听器；不授予同源、弹窗、表单权限；
//  2. CSP default-src 'none'，邮件外链仅允许图像、样式、字体和媒体；
//  3. cid: 内嵌图在注入 srcdoc 之前替换为本地 blob URL；
//  4. <base target="_blank"> 让正文里的链接在新标签打开，外层文档不参与导航。

const props = defineProps<{ detail: MailDetail }>();
const emit = defineEmits<{
  changed: [];
}>();
const toasts = useToasts();

const bodyPart = computed<MailPart | undefined>(() =>
  props.detail.parts.find((p) => p.kind === "body"),
);
const attachments = computed<MailPart[]>(() =>
  props.detail.parts.filter((p) => p.kind === "attachment"),
);
const inlineParts = computed<MailPart[]>(() =>
  props.detail.parts.filter((p) => p.kind === "inline"),
);

const iframeEl = ref<HTMLIFrameElement | null>(null);
const rendering = ref(false);
const externalPromptOpen = ref(false);
const externalProxyBusy = ref(false);
const failedExternalResources = ref<ExternalResourceFailure[]>([]);
// 与代理接口的每用户每分钟请求上限保持一致，避免失败外链把队列和弹窗撑大。
const maxTrackedExternalResources = 60;
let blobUrls: string[] = [];
let externalPromptTimer: ReturnType<typeof setTimeout> | null = null;
let activeMonitorChannel = "";
let activeProxyController: AbortController | null = null;
/** 正文渲染的序号守卫：只有最新一次调用可以写 srcdoc（切信会连开好几次）。 */
const renderGate = createRequestGate();
let renderAbort: AbortController | null = null;

interface ExternalResourceFailure {
  url: string;
  kind: "image" | "style" | "font";
}
const externalPromptDetail = computed(() => {
  const domains = [...new Set(failedExternalResources.value.map((item) => {
    try { return new URL(item.url).hostname; } catch { return "外部站点"; }
  }))];
  return `资源来源：${domains.join("、")}。代理只重试本次失败的资源，并受管理员设置的单项和总量上限约束。`;
});

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => {
    switch (c) {
      case "&":
        return "&amp;";
      case "<":
        return "&lt;";
      case ">":
        return "&gt;";
      case '"':
        return "&quot;";
      default:
        return "&#39;";
    }
  });
}

function newMonitorNonce(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(18));
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

// 允许浏览器直接尝试邮件外链；仅注入带随机 nonce 的错误监视器，邮件自身
// 脚本仍被 CSP 拦截。iframe 保持 opaque origin，外层无法读取邮件 DOM。
function buildSrcDoc(html: string, cspNonce: string, channel: string): string {
  const monitor = `<script nonce="${cspNonce}">
document.currentScript?.removeAttribute("nonce");
(() => {
  const send = (url, kind) => {
    try {
      const parsed = new URL(url, document.baseURI);
      if (parsed.protocol === "http:" || parsed.protocol === "https:") {
        parent.postMessage({ type: "xph-mail-resource-error", channel: "${channel}", url: parsed.href, kind }, "*");
      }
    } catch { /* 忽略无效和非 HTTP(S) 地址。 */ }
  };
  document.addEventListener("error", (event) => {
    const target = event.target;
    if (target instanceof HTMLImageElement) send(target.currentSrc || target.src, "image");
    else if (target instanceof HTMLLinkElement && target.relList.contains("stylesheet")) send(target.href, "style");
  }, true);
  document.fonts.addEventListener("loadingerror", () => {
    let foundFontURL = false;
    for (const entry of performance.getEntriesByType("resource")) {
      if (/\\.(woff2?|ttf|otf)(?:[?#]|$)/i.test(entry.name)) {
        foundFontURL = true;
        send(entry.name, "font");
      }
    }
    // 字体引用常驻在外链样式表里；同时代理样式表，才能把其中的字体 URL
    // 改成已获准的同批代理地址，解决字体 CORS 失败。
    if (foundFontURL) {
      for (const link of document.querySelectorAll('link[rel~="stylesheet"]')) send(link.href, "style");
    }
  });
  addEventListener("message", (event) => {
    if (event.source !== parent || event.data?.type !== "xph-mail-proxy-resources" || event.data.channel !== "${channel}") return;
    for (const item of event.data.resources || []) {
      for (const image of document.images) {
        if (image.currentSrc === item.original || image.src === item.original) image.src = item.proxy;
      }
      for (const link of document.querySelectorAll('link[rel~="stylesheet"]')) {
        if (link.href === item.original) link.href = item.proxy;
      }
      for (const style of document.querySelectorAll("style, [style]")) {
        if (style.tagName === "STYLE" && style.textContent) {
          style.textContent = style.textContent.split(item.original).join(item.proxy);
        }
        const inline = style.getAttribute("style");
        if (inline) style.setAttribute("style", inline.split(item.original).join(item.proxy));
      }
    }
  });
})();
</scr${"ipt"}>`;
  return (
    '<!doctype html><html><head><meta charset="utf-8">' +
    '<meta http-equiv="Content-Security-Policy" content="' +
    `default-src 'none'; script-src 'nonce-${cspNonce}'; img-src blob: data: http: https:; ` +
    "style-src 'unsafe-inline' blob: http: https:; font-src blob: data: http: https:; " +
    "media-src blob: data: http: https:; connect-src 'none'; object-src 'none'; frame-src 'none'; " +
    "base-uri 'none'; form-action 'none'\">" +
    '<base target="_blank"></head>' +
    monitor +
    '<body>' +
    html +
    "</body></html>"
  );
}

async function renderBody(): Promise<void> {
  const part = bodyPart.value;
  const el = iframeEl.value;
  if (!part || !el) {
    return;
  }
  // 序号守卫：切信时上一轮的内嵌图循环还在 await，它回来后会往（早已被换掉的）
  // blobUrls 里 push 自己的 blob，并用它那份旧 HTML 覆写同一个 srcdoc——
  // 结果是"上一封的正文配这一封的附件"，而且旧 blob 永远没人回收。
  renderAbort?.abort();
  const controller = new AbortController();
  renderAbort = controller;
  const token = renderGate.next();
  rendering.value = true;
  // 本轮建出来的 blob 先扣在本地：只有确认自己仍是最新一轮才移交给 blobUrls。
  // 过期或失败退出时只回收这份，绝不碰下一轮正在用的那些。
  const mine: string[] = [];
  /** 回收本轮尚未移交给 blobUrls 的 blob（过期退出与失败退出共用）。 */
  const dropMine = (): void => {
    mine.forEach((u) => URL.revokeObjectURL(u));
    mine.length = 0;
  };
  // 切邮件时释放上一封的 blob 与旧正文。
  blobUrls.forEach((u) => URL.revokeObjectURL(u));
  blobUrls = [];
  el.srcdoc = "";
  try {
    const result = await runDelivery(mailPartDeliverySource(part.id), { signal: controller.signal });
    if (!renderGate.isCurrent(token)) return;
    if (!result.blob) {
      throw new Error("正文获取失败");
    }
    // 正文容器是 JSON：{html?, text?}，由落信管线统一封装。
    const raw = await result.blob.text();
    if (!renderGate.isCurrent(token)) return;
    const parsed = raw
      ? (JSON.parse(raw) as { html?: string; text?: string })
      : {};
    let html = parsed.html?.trim()
      ? parsed.html
      : `<pre style="white-space:pre-wrap;font-family:inherit;margin:0;">${escapeHtml(
          parsed.text ?? "",
        )}</pre>`;

    // 内嵌图逐张拉明文 blob，并把 cid:xxx 引用替换为 blob URL。
    for (const inline of inlineParts.value) {
      if (!inline.contentId) {
        continue;
      }
      const r = await runDelivery(mailPartDeliverySource(inline.id), { signal: controller.signal });
      // 每次 await 之后都要验一次：过期就是过期，直接收工，别再往下走。
      if (!renderGate.isCurrent(token)) {
        dropMine();
        return;
      }
      if (!r.blob) {
        continue;
      }
      const url = URL.createObjectURL(r.blob);
      mine.push(url);
      const cid = inline.contentId.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      html = html.replace(new RegExp(`cid:${cid}`, "gi"), url);
    }
    if (!renderGate.isCurrent(token)) {
      dropMine();
      return;
    }
    // 仍是最新一轮：blob 移交给 blobUrls 统一托管（切信与卸载时从那里回收）。
    // 复制一份再清空 mine，之后任何兜底回收都不会 revoke 到 iframe 正在用的 URL。
    blobUrls = [...mine];
    mine.length = 0;
    activeMonitorChannel = newMonitorNonce();
    el.srcdoc = buildSrcDoc(html, newMonitorNonce(), activeMonitorChannel);
  } catch (err) {
    dropMine();
    if (!renderGate.isCurrent(token)) return;
    toastApiError(toasts, err);
  } finally {
    if (renderGate.isCurrent(token)) rendering.value = false;
  }
}

function onExternalResourceMessage(event: MessageEvent): void {
  if (event.source !== iframeEl.value?.contentWindow || !event.data ||
      event.data.type !== "xph-mail-resource-error" || event.data.channel !== activeMonitorChannel) return;
  const kind = event.data.kind as ExternalResourceFailure["kind"];
  if (kind !== "image" && kind !== "style" && kind !== "font") return;
  let parsed: URL;
  try {
    parsed = new URL(String(event.data.url));
  } catch {
    return;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return;
  if (failedExternalResources.value.some((item) => item.url === parsed.href) ||
      failedExternalResources.value.length >= maxTrackedExternalResources) return;
  failedExternalResources.value = [...failedExternalResources.value, { url: parsed.href, kind }];
  // 把同一轮打开时近乎同时失败的资源合并成一个确认，避免一封邮件弹出多次。
  if (!externalPromptTimer) {
    externalPromptTimer = setTimeout(() => {
      externalPromptTimer = null;
      externalPromptOpen.value = failedExternalResources.value.length > 0;
    }, 700);
  }
}

function absolutizeCSSResources(css: string, stylesheetURL: string): string {
  const resolve = (raw: string): string => {
    const value = raw.trim();
    if (!value || /^(?:data:|blob:|cid:|#)/i.test(value)) return value;
    try {
      const url = new URL(value, stylesheetURL);
      return url.protocol === "http:" || url.protocol === "https:" ? url.href : value;
    } catch {
      return value;
    }
  };
  let result = css.replace(/(@import\s+)(["'])([^"']+)\2/gi, (_all, prefix: string, quote: string, url: string) =>
    prefix + quote + resolve(url) + quote,
  );
  result = result.replace(/url\(\s*(?:(["'])(.*?)\1|([^)]*?))\s*\)/gi, (_all, _quote: string | undefined, quoted: string | undefined, bare: string | undefined) => {
    const value = resolve(quoted ?? bare ?? "");
    const escaped = value.replace(/["\\\r\n]/g, (char) => "\\" + char);
    return 'url("' + escaped + '")';
  });
  return result;
}

async function proxyFailedExternalResources(): Promise<void> {
  if (externalProxyBusy.value || failedExternalResources.value.length === 0) return;
  const messageId = props.detail.message.id;
  const monitorChannel = activeMonitorChannel;
  const batchId = crypto.randomUUID();
  const controller = new AbortController();
  const resources = [...failedExternalResources.value];
  externalProxyBusy.value = true;
  activeProxyController = controller;
  const stagedURLs: string[] = [];
  let adoptedURLs = false;
  try {
    const fetched: PromiseSettledResult<{
      resource: ExternalResourceFailure;
      body: Blob;
      contentLocation: string;
    }>[] = [];
    // 同一确认批次按序请求，服务端据此精确累计总量，不会因并行预留额度
    // 把尚未使用的空间提前占满。
    for (const resource of resources) {
      if (controller.signal.aborted) break;
      try {
        const response = await mailApi.proxyExternalResource(
          messageId, batchId, resource.url, resource.kind, controller.signal,
        );
        fetched.push({
          status: "fulfilled",
          value: { resource, body: response.blob, contentLocation: response.contentLocation },
        });
      } catch (reason) {
        fetched.push({ status: "rejected", reason });
      }
    }
    if (props.detail.message.id !== messageId || activeMonitorChannel !== monitorChannel) return;
    const successful = fetched.flatMap((result) => result.status === "fulfilled" ? [result.value] : []);
    const failed = fetched.filter((result) => result.status === "rejected");
    // 先为成功取回的非 CSS 资源建 Blob 地址，再改写代理 CSS 中指向它们的 URL。
    const mappings: Array<{ original: string; proxy: string }> = [];
    const mappingByURL = new Map<string, string>();
    for (const { resource, body } of successful) {
      if (resource.kind === "style") continue;
      const proxy = URL.createObjectURL(body);
      stagedURLs.push(proxy);
      mappingByURL.set(resource.url, proxy);
      mappings.push({ original: resource.url, proxy });
    }
    for (const { resource, body, contentLocation } of successful) {
      if (resource.kind !== "style") continue;
      let css = absolutizeCSSResources(await body.text(), contentLocation || resource.url);
      for (const [original, proxy] of mappingByURL) css = css.split(original).join(proxy);
      const proxy = URL.createObjectURL(new Blob([css], { type: "text/css" }));
      stagedURLs.push(proxy);
      mappingByURL.set(resource.url, proxy);
      mappings.push({ original: resource.url, proxy });
    }
    if (props.detail.message.id === messageId && activeMonitorChannel === monitorChannel && mappings.length > 0) {
      blobUrls.push(...stagedURLs);
      adoptedURLs = true;
      iframeEl.value?.contentWindow?.postMessage({
        type: "xph-mail-proxy-resources", channel: activeMonitorChannel, resources: mappings,
      }, "*");
    }
    if (props.detail.message.id !== messageId || activeMonitorChannel !== monitorChannel) return;
    const proxiedURLs = new Set(mappings.map((item) => item.original));
    failedExternalResources.value = failedExternalResources.value.filter((item) => !proxiedURLs.has(item.url));
    externalPromptOpen.value = false;
    if (failed.length > 0) {
      toasts.error(String(failed.length) + " 个外部资源仍未能通过代理加载");
    }
  } finally {
    if (!adoptedURLs) stagedURLs.forEach((url) => URL.revokeObjectURL(url));
    if (activeProxyController === controller) {
      activeProxyController = null;
      externalProxyBusy.value = false;
    }
  }
}

function dismissExternalResourcePrompt(): void {
  externalPromptOpen.value = false;
  failedExternalResources.value = [];
}

async function downloadAttachment(part: MailPart): Promise<void> {
  try {
    const result = await runDelivery(mailPartDeliverySource(part.id));
    // 大附件可能直接走流式落盘（savedAs 非空）；小附件存内存时再手工触发保存。
    if (result.blob) {
      saveBlob(result.blob, part.fileName || result.fileName);
    }
  } catch (err) {
    toastApiError(toasts, err);
  }
}

async function star(): Promise<void> {
  try {
    await mailApi.star(props.detail.message.id, !props.detail.box.isStarred);
    emit("changed");
  } catch (err) {
    toastApiError(toasts, err);
  }
}

// 删除（原「更多」下拉里唯一的一项）与彻底删除并排放在头部操作区：
// 一个条目撑不起一个下拉，遮罩、caret、定位与阴影全是为"多项"准备的。
async function archive(): Promise<void> {
  try {
    await mailApi.archive(props.detail.message.id);
    toasts.success("已移入归档");
    emit("changed");
  } catch (err) {
    toastApiError(toasts, err);
  }
}

async function restore(): Promise<void> {
  try {
    await mailApi.restore(props.detail.message.id);
    toasts.success("已恢复");
    emit("changed");
  } catch (err) {
    toastApiError(toasts, err);
  }
}

/** 彻底删除不可恢复，走全站统一的确认窗而不是内联 window.confirm。 */
const purgeOpen = ref(false);
const purgeBusy = ref(false);

async function confirmPurge(): Promise<void> {
  if (purgeBusy.value) {
    return;
  }
  purgeBusy.value = true;
  try {
    await mailApi.purge(props.detail.message.id);
    toasts.success("已彻底删除");
    purgeOpen.value = false;
    emit("changed");
  } catch (err) {
    toastApiError(toasts, err);
  } finally {
    purgeBusy.value = false;
  }
}

watch(
  () => props.detail.message.id,
  async () => {
    renderGate.next();
    renderAbort?.abort();
    renderAbort = null;
    activeProxyController?.abort();
    activeProxyController = null;
    externalProxyBusy.value = false;
    if (externalPromptTimer) clearTimeout(externalPromptTimer);
    externalPromptTimer = null;
    externalPromptOpen.value = false;
    failedExternalResources.value = [];
    activeMonitorChannel = "";
    // immediate watcher 在 setup 阶段同步触发，模板里的 iframe 此时还没挂载。
    // 等一个渲染周期后再取 ref，否则 renderBody 会静默返回、正文永远空白。
    await nextTick();
    await renderBody();
  },
  { immediate: true },
);

window.addEventListener("message", onExternalResourceMessage);

onBeforeUnmount(() => {
  renderGate.next();
  renderAbort?.abort();
  renderAbort = null;
  activeProxyController?.abort();
  activeProxyController = null;
  window.removeEventListener("message", onExternalResourceMessage);
  activeMonitorChannel = "";
  if (externalPromptTimer) clearTimeout(externalPromptTimer);
  blobUrls.forEach((u) => URL.revokeObjectURL(u));
  blobUrls = [];
});

const toRecipients = computed(() => props.detail.recipients.filter((r) => r.kind === "to"));
const ccRecipients = computed(() => props.detail.recipients.filter((r) => r.kind === "cc"));
const recipientsExpanded = ref(false);

function formatRecipient(r: { name?: string; address: string }): string {
  return r.name ? `${r.name} <${r.address}>` : r.address;
}

const isTrash = computed(() => props.detail.box.status === "archived");

// 头像：取展示名首字符，按字符码生成稳定色相。
const avatarText = computed(() => {
  const name = props.detail.message.fromName || props.detail.message.fromAddress;
  return (name.trim()[0] ?? "?").toUpperCase();
});
const avatarStyle = computed(() => {
  const name = props.detail.message.fromName || props.detail.message.fromAddress || "?";
  let sum = 0;
  for (const ch of name) sum += ch.codePointAt(0) ?? 0;
  return { background: `hsl(${sum % 360} 58% 46%)` };
});

const spfBadgeClass = computed(() => {
  switch (props.detail.message.spfResult) {
    case "pass":
      return "md__spf--pass";
    case "fail":
      return "md__spf--fail";
    default:
      return "md__spf--neutral";
  }
});
const spfLabel: Record<string, string> = {
  pass: "SPF 通过",
  fail: "SPF 失败",
  softfail: "SPF 软失败",
  neutral: "SPF 中立",
  none: "SPF 无记录",
};
</script>

<template>
  <div class="md">
    <header class="md__head">
      <h2 class="md__subject">{{ detail.message.subject || "（无主题）" }}</h2>
      <div class="md__head-actions">
        <button
          type="button"
          class="md__iconbtn"
          :class="{ 'md__iconbtn--on': detail.box.isStarred }"
          :title="detail.box.isStarred ? '取消星标' : '加星标'"
          :aria-label="detail.box.isStarred ? '取消星标' : '加星标'"
          :aria-pressed="detail.box.isStarred"
          @click="star"
        >
          <i :class="detail.box.isStarred ? 'ri-star-fill' : 'ri-star-line'" />
        </button>
        <template v-if="isTrash">
          <AppButton size="sm" icon="ri-arrow-go-back-line" @click="restore">恢复</AppButton>
          <AppButton size="sm" variant="danger" icon="trash" @click="purgeOpen = true">彻底删除</AppButton>
        </template>
        <!-- 危险色沿用原菜单项：它标的是"邮件会离开收件箱"，不是"不可恢复"
             （不可恢复的那一项是左边的彻底删除，已经单独占了 danger 按钮）。 -->
        <AppButton v-else size="sm" variant="danger" icon="trash" @click="archive">删除</AppButton>
      </div>
    </header>

    <div class="md__envelope">
      <div class="md__avatar" :style="avatarStyle">{{ avatarText }}</div>
      <div class="md__envelope-main">
        <p class="md__sender-line">
          <span class="md__sender-name truncate">{{ detail.message.fromName || detail.message.fromAddress }}</span>
          <span
            v-if="detail.message.spfResult"
            class="md__spf"
            :class="spfBadgeClass"
            :title="`发件人 SPF 校验：${detail.message.spfResult}`"
          >
            <i :class="detail.message.spfResult === 'pass' ? 'ri-shield-check-line' : 'ri-shield-line'" />
            {{ spfLabel[detail.message.spfResult] ?? detail.message.spfResult }}
          </span>
        </p>
        <p class="md__addr-line">
          <span class="truncate">&lt;{{ detail.message.fromAddress }}&gt;</span>
        </p>
        <button
          v-if="toRecipients.length || ccRecipients.length"
          type="button"
          class="md__recipients-toggle"
          @click="recipientsExpanded = !recipientsExpanded"
        >
          <template v-if="!recipientsExpanded">
            发件人
            {{ toRecipients[0] ? formatRecipient(toRecipients[0]) : "—" }}
            <template v-if="toRecipients.length > 1"> 等 {{ toRecipients.length }} 人</template>
          </template>
          <template v-else>收起收件人</template>
          <i :class="recipientsExpanded ? 'ri-arrow-up-s-line' : 'ri-arrow-down-s-line'" />
        </button>
        <dl v-if="recipientsExpanded" class="md__recipients">
          <div v-if="toRecipients.length" class="md__recipients-row">
            <dt>收件人</dt>
            <dd>{{ toRecipients.map(formatRecipient).join(", ") }}</dd>
          </div>
          <div v-if="ccRecipients.length" class="md__recipients-row">
            <dt>抄送</dt>
            <dd>{{ ccRecipients.map(formatRecipient).join(", ") }}</dd>
          </div>
        </dl>
        <p class="md__time">{{ formatTime(detail.message.sentAt) }}</p>
      </div>
    </div>

    <p v-if="rendering" class="md__hint">正文加载中…</p>
    <iframe
      ref="iframeEl"
      title="邮件正文"
      sandbox="allow-scripts"
      referrerpolicy="no-referrer"
      class="md__frame"
    ></iframe>

    <section v-if="attachments.length" class="md__atts">
      <h3>附件（{{ attachments.length }}）</h3>
      <ul>
        <li v-for="p in attachments" :key="p.id">
          <i class="ri-file-line md__att-icon" />
          <span class="md__att-name" :title="p.fileName">{{ p.fileName || "未命名附件" }}</span>
          <span class="md__att-size">{{ formatBytes(p.sizePlain) }}</span>
          <AppButton size="sm" icon="download" @click="downloadAttachment(p)">下载</AppButton>
        </li>
      </ul>
    </section>

    <ConfirmDialog
      :open="purgeOpen"
      title="彻底删除邮件"
      message="彻底删除该邮件？"
      detail="邮件配额立即释放且不可恢复。"
      danger
      confirm-text="彻底删除"
      :loading="purgeBusy"
      @confirm="confirmPurge"
      @cancel="purgeOpen = false"
    />

    <ConfirmDialog
      :open="externalPromptOpen"
      title="加载未验证的外部资源？"
      message="部分邮件样式或图片未能直接加载。是否使用本站代理重试？"
      :detail="externalPromptDetail"
      confirm-text="代理加载"
      :loading="externalProxyBusy"
      @confirm="proxyFailedExternalResources"
      @cancel="dismissExternalResourcePrompt"
    />
  </div>
</template>

<style scoped>
.md {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  min-width: 0;
}

.md__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sp-3);
  flex: none;
}

.md__subject {
  margin: 0;
  font-size: var(--fs-lg);
  font-weight: 650;
  line-height: 1.4;
  word-break: break-word;
}

.md__head-actions {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex: none;
}

.md__iconbtn {
  width: 32px;
  height: 32px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface);
  color: var(--c-text-muted);
  cursor: pointer;
  font-size: 16px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.md__iconbtn:hover {
  color: var(--c-accent);
  border-color: var(--c-accent);
}

.md__iconbtn--on {
  color: var(--c-warn);
  border-color: color-mix(in srgb, var(--c-warn) 45%, var(--c-border));
}

/* 信封区 */
.md__envelope {
  display: flex;
  gap: var(--sp-3);
  padding: var(--sp-3);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface-sunken);
}

.md__avatar {
  flex: none;
  width: 40px;
  height: 40px;
  border-radius: var(--r-pill);
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  font-size: 16px;
  font-weight: 600;
  user-select: none;
}

.md__envelope-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.md__sender-line {
  margin: 0;
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.md__sender-name {
  min-width: 0;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--c-text);
}

.md__spf {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 1px 7px;
  border-radius: var(--r-pill);
  font-size: 10px;
  font-weight: 500;
}

.md__spf i {
  font-size: 12px;
}

.md__spf--pass {
  color: var(--c-success);
  background: var(--c-success-weak);
}

.md__spf--fail {
  color: var(--c-danger);
  background: var(--c-danger-weak);
}

.md__spf--neutral {
  color: var(--c-text-muted);
  background: var(--c-hover);
}

.md__addr-line {
  margin: 0;
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.md__recipients-toggle {
  align-self: flex-start;
  display: inline-flex;
  align-items: center;
  gap: 2px;
  margin: 1px 0 0;
  padding: 2px 6px 2px 0;
  border: 0;
  background: transparent;
  color: var(--c-accent);
  font-size: var(--fs-xs);
  cursor: pointer;
  max-width: 100%;
}

.md__recipients-toggle i {
  font-size: 14px;
  flex: none;
}

.md__recipients {
  margin: 2px 0 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: var(--sp-2);
  border: 1px solid var(--c-border);
  border-radius: var(--r-sm);
  background: var(--c-surface);
}

.md__recipients-row {
  display: flex;
  gap: var(--sp-2);
  font-size: var(--fs-xs);
  min-width: 0;
}

.md__recipients dt {
  flex: none;
  width: 44px;
  color: var(--c-text-faint);
}

.md__recipients dd {
  margin: 0;
  min-width: 0;
  word-break: break-all;
  color: var(--c-text-muted);
}

.md__time {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.md__hint {
  margin: 0;
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
}

/* 正文 iframe：占满剩余高度。背景与正文颜色由内部文档自理，
   这里只负责边框与圆角，sandbox 把样式与脚本都关在文档内部。 */
.md__frame {
  height: 56vh;
  min-height: 320px;
  width: 100%;
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: #fff;
}

.md__atts h3 {
  margin: 0 0 var(--sp-2);
  font-size: var(--fs-sm);
}

.md__atts ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.md__atts li {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface-sunken);
  font-size: var(--fs-xs);
}

.md__att-icon {
  flex: none;
  font-size: 16px;
  color: var(--c-text-muted);
}

.md__att-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.md__att-size {
  color: var(--c-text-faint);
  flex: none;
}
</style>
