import { getToken } from "@/api/client";
import { createPreviewParser } from "@/lib/previewParser";
import { streamUrlWithToken } from "@/api/endpoints";
import type { DeliveryPlan } from "@/api/types";
import { createClientKeyPair, encryptionSupported, resolveContentKey, type ClientKeyPair } from "@/crypto/clientkey";
import { bytesToBase64 } from "@/crypto/xph";
import { cipherSourceUrl, settleQuietly, type DeliverySource } from "./download";
import { mediaMimeType } from "@/lib/mediaFormats";

// 文档、图片等通用预览通道。音视频由 mediaPreview 独立管理。

type PreviewMode = "sw" | "stream" | "blob";

export interface PreviewHandle {
  /** 可直接交给 <img> / <video> / <audio> / <iframe> 的地址。 */
  url: string;
  mode: PreviewMode;
  plan: DeliveryPlan;
  /** 释放会话并结算额度。切换到下一个预览对象时务必调用。 */
  release(): Promise<void>;
}

const STREAM_PREFIX = "/__xph/";
const SW_SCRIPT = "/xph-sw.js";

let workerReady: Promise<ServiceWorkerRegistration | null> | null = null;

/**
 * 确保解密用的 Service Worker 就绪。
 *
 * 结果会被缓存：SW 的注册与激活只该发生一次，而预览会在用户每次点文件时触发。
 * 不可用时返回 null 而不是抛错——那是"能力不足"，不是"出错"。
 */
function ensureXphWorker(): Promise<ServiceWorkerRegistration | null> {
  if (workerReady) {
    return workerReady;
  }
  workerReady = (async () => {
    if (!("serviceWorker" in navigator) || !window.isSecureContext) {
      return null;
    }
    try {
      const probe = await fetch(SW_SCRIPT, { cache: "no-store" });
      if (!probe.ok) {
        return null;
      }
      const registration = await navigator.serviceWorker.register(SW_SCRIPT, {
        scope: "/",
        // 产物是 ES 模块；用 classic 注册会让里面的 import 直接被拒。
        type: "module",
      });
      // ready 不保证首次打开的当前页面已被接管；否则虚拟地址会落到普通路由。
      return await new Promise<ServiceWorkerRegistration | null>(resolve => {
        const finish = (result: ServiceWorkerRegistration | null) => {
          clearTimeout(timer);
          navigator.serviceWorker.removeEventListener("controllerchange", check);
          resolve(result);
        };
        const check = () => {
          if (registration.active && navigator.serviceWorker.controller?.scriptURL === new URL(SW_SCRIPT, location.href).href) {
            finish(registration);
          }
        };
        const timer = setTimeout(() => finish(null), 10_000);
        navigator.serviceWorker.addEventListener("controllerchange", check);
        check();
      });
    } catch {
      return null;
    }
  })();
  return workerReady.then(result => { if (!result) workerReady = null; return result; });
}

/** 准备一次预览。 */
export async function preparePreview(
  source: DeliverySource,
  options: { signal?: AbortSignal } = {},
): Promise<PreviewHandle> {
  options.signal?.throwIfAborted();
  const pair = encryptionSupported() ? await createClientKeyPair() : null;
  options.signal?.throwIfAborted();
  const plan = await source.plan(pair);
  // plan 一到手，服务端就已经按 reservedBytes 预扣了额度。此后任何一步失败
  // （取内容密钥、注册 SW、解密、拼 Blob）都必须结算，否则这笔预扣要等维护
  // 任务回收才释放——用户反复点开一个坏文件就能把额度吃光。下载路径本来就有
  // 这个兜底，预览路径原先漏了。
  try {
    options.signal?.throwIfAborted();
    return await buildHandle(plan, pair, options);
  } catch (err) {
    await settleQuietly(plan);
    throw err;
  }
}

async function buildHandle(
  plan: DeliveryPlan,
  pair: ClientKeyPair | null,
  options: { signal?: AbortSignal },
): Promise<PreviewHandle> {
  // 中转解密：服务端已把明文准备好，直接把（带令牌的）中转地址交给媒体元素。
  if (plan.mode === "proxy_decrypt") {
    if (!plan.streamUrl) {
      throw new Error("服务端未提供可预览的内容地址");
    }
    return {
      url: streamUrlWithToken(plan.streamUrl),
      mode: "stream",
      plan,
      // 中转流由服务端在写出完成后自行结算，前端再结算一次会重复记账。
      async release() {},
    };
  }

  // 直链加密与中转加密：密文来源不同（直链 URL / 同源中转 URL），但拿到的
  // 都是密文，本地解密路径完全一致。
  if (plan.contentForm !== "ciphertext" || !plan.encryption) {
    throw new Error("交付计划形态不正确，无法预览");
  }
  const cipherUrl = cipherSourceUrl(plan);

  const dek = await resolveContentKey(plan.encryption, pair);
  const registration = await ensureXphWorker();

  if (registration?.active) {
    const id = await registerStreamSession(registration, plan, dek, cipherUrl, options.signal);
    if (id) {
      return makeSwHandle(plan, id, registration);
    }
  }

  options.signal?.throwIfAborted();

  // 没有 SW 时，在独立线程中解密为 Blob。
  const parser = createPreviewParser();
  const onAbort = () => parser.cancel();
  options.signal?.addEventListener("abort", onAbort, { once: true });
  let blob: Blob;
  try {
    blob = await parser.parse("decrypt", { plan, dek, token: getToken() });
  } finally {
    options.signal?.removeEventListener("abort", onAbort);
  }
  options.signal?.throwIfAborted();
  const url = URL.createObjectURL(blob);

  return {
    url,
    mode: "blob",
    plan,
    async release() {
      URL.revokeObjectURL(url);
      await settleQuietly(plan);
    },
  };
}

function makeSwHandle(plan: DeliveryPlan, id: string, registration: ServiceWorkerRegistration): PreviewHandle {
  return {
    url: `${STREAM_PREFIX}${id}`,
    mode: "sw",
    plan,
    async release() {
      registration.active?.postMessage({ type: "xph:revoke", id });
      // 密文通道的用量按预扣全额结算，SW 实际交付了多少字节不参与记账。
      await settleQuietly(plan);
    },
  };
}

async function registerStreamSession(
  registration: ServiceWorkerRegistration,
  plan: DeliveryPlan,
  dek: Uint8Array,
  cipherUrl: string,
  signal?: AbortSignal,
): Promise<string | null> {
  const active = registration.active;
  if (!active || (!cipherUrl && !plan.parts?.length)) {
    return null;
  }
  signal?.throwIfAborted();
  const id = crypto.randomUUID();
  const channel = new MessageChannel();
  return new Promise<string | null>((resolve) => {
    let finished = false;
    const finish = (result: string | null) => {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      channel.port1.close();
      channel.port2.close();
      if (!result) active.postMessage({ type: "xph:revoke", id });
      resolve(result);
    };
    const onAbort = () => finish(null);
    const timer = setTimeout(() => finish(null), 10_000);
    signal?.addEventListener("abort", onAbort, { once: true });
    channel.port1.onmessage = (event: MessageEvent) => {
      const data = event.data as { ok: boolean; id?: string };
      finish(data.ok && data.id === id ? id : null);
    };
    try {
      active.postMessage({
        type: "xph:register", id, cipherUrl, cipherParts: plan.parts,
        dek: bytesToBase64(dek), mimeType: mediaMimeType(plan.fileName, plan.mimeType), fileName: plan.fileName,
      }, [channel.port2]);
    } catch { finish(null); }
  });
}
