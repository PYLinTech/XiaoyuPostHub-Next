import { createClientKeyPair, encryptionSupported, resolveContentKey } from "../crypto/clientkey";
import { cipherSourceUrl, settleQuietly } from "./download";
import type { DeliverySource } from "./download";
import type { MediaCipherSource } from "../lib/mediaRangeSource";

/** 音视频只接受密文交付；整个流式/完整预览共享一个票据。 */
export async function prepareMediaPreview(source: DeliverySource, signal: AbortSignal) {
  signal.throwIfAborted();
  if (!encryptionSupported()) throw new Error("当前浏览器不支持安全解密，请使用 HTTPS 和现代浏览器");
  const pair = await createClientKeyPair();
  signal.throwIfAborted();
  const plan = await source.plan(pair);
  let released = false;
  const release = async () => { if (!released) { released = true; await settleQuietly(plan); } };
  try {
    signal.throwIfAborted();
    if (plan.contentForm !== "ciphertext" || !plan.encryption) throw new Error("音视频预览需要前端解密的密文交付计划");
    const descriptor: MediaCipherSource = {
      url: cipherSourceUrl(plan), parts: plan.mode === "direct" ? plan.parts : undefined,
      key: await resolveContentKey(plan.encryption, pair), plainSize: plan.plainSize, cipherSize: plan.encryption.cipherSize,
    };
    signal.throwIfAborted();
    return { plan, descriptor, release };
  } catch (error) { await release(); throw error; }
}
