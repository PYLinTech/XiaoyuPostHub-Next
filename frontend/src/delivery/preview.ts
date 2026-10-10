import { streamUrlWithToken } from "@/api/endpoints";
import type { DeliveryPlan } from "@/api/types";
import { createClientKeyPair, encryptionSupported, resolveContentKey, type ClientKeyPair } from "@/crypto/clientkey";
import { bytesToBase64, decryptAll, importContentKey, parseXphHeader, HEADER_SIZE } from "@/crypto/xph";
import { assertHeaderMatchesMeta, cipherSourceUrl, fetchCipherPlanRange, settleQuietly, type DeliverySource } from "./download";

// 预览的取数通道。
//
// 音视频必须支持浏览器的原生 Range/seek，而浏览器不会替我们解密，因此这里有
// 三种通道，按能力从优到劣：
//
//   sw     —— Service Worker 暴露一个本源"虚拟明文文件"，浏览器按 Range 取密文
//             （直链 URL 或本机中转 URL 皆可）、页面本地解密，支持 seek。
//   stream —— 中转解密：服务端下发明文流，浏览器直接播放。全部出流量经服务端。
//   blob   —— 不支持 SW 时（老浏览器、非安全上下文）退化为先整份解密再预览。
//             只对图片、PDF、小文件可接受。

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
      await navigator.serviceWorker.register(SW_SCRIPT, {
        scope: "/",
        // 产物是 ES 模块；用 classic 注册会让里面的 import 直接被拒。
        type: "module",
      });
      return await navigator.serviceWorker.ready;
    } catch {
      return null;
    }
  })();
  return workerReady;
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
      return makeSwHandle(plan, id);
    }
  }

  // 没有 SW 时的兜底：整份解密成 Blob。对视频意味着"先等整份下完"，
  // 因此界面上会明确提示走的是兜底通道。
  const key = await importContentKey(dek);
  const headerBytes = await fetchCipherPlanRange(plan, 0, HEADER_SIZE - 1, options.signal);
  const header = parseXphHeader(headerBytes);
  // 对象与记录的一致性校验：下载路径一直有，预览原先没有。缺了它，
  // 一旦拿到的密文与计划里的记录对不上，唯一的症状就是 GCM 认证失败——
  // 而那句话不会告诉你哪一步错了，正是这里最该给出诊断的地方。
  assertHeaderMatchesMeta(header, plan.encryption);
  const parts: Uint8Array[] = [];
  await decryptAll(
    key,
    header,
    (start, endExclusive, signal) => fetchCipherPlanRange(plan, start, endExclusive - 1, signal),
    (plain) => {
      parts.push(plain);
    },
    { concurrency: 6, signal: options.signal },
  );
  const blob = new Blob(parts as BlobPart[], { type: plan.mimeType || "application/octet-stream" });
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

function makeSwHandle(plan: DeliveryPlan, id: string): PreviewHandle {
  return {
    url: `${STREAM_PREFIX}${id}`,
    mode: "sw",
    plan,
    async release() {
      const registration = await navigator.serviceWorker.ready.catch(() => null);
      registration?.active?.postMessage({ type: "xph:revoke", id });
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
  const channel = new MessageChannel();
  try {
    return await new Promise<string | null>((resolve, reject) => {
      const timer = setTimeout(() => resolve(null), 10_000);
      // 具名且一次性：成功、超时、失败三条出口都要摘掉它，否则调用方复用同一个
      // AbortSignal 时，每次预览都会叠加一个闭包（连带 MessageChannel 与 resolve）。
      const onAbort = (): void => {
        clearTimeout(timer);
        resolve(null);
      };
      signal?.addEventListener("abort", onAbort, { once: true });
      channel.port1.onmessage = (event: MessageEvent) => {
        clearTimeout(timer);
        signal?.removeEventListener("abort", onAbort);
        const data = event.data as { ok: boolean; id?: string; error?: string };
        if (data.ok && data.id) {
          resolve(data.id);
        } else {
          reject(new Error(data.error ?? "Service Worker 拒绝了流会话"));
        }
      };
      active.postMessage(
        {
          type: "xph:register",
          cipherUrl,
          cipherParts: plan.parts,
          dek: bytesToBase64(dek),
          mimeType: plan.mimeType,
          fileName: plan.fileName,
        },
        [channel.port2],
      );
    });
  } catch {
    return null;
  }
}
