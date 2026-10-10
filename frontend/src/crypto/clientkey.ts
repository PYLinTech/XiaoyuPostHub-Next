import { asBuffer, base64ToBytes, bytesToBase64, KEY_SIZE, XphFormatError } from "./xph";

// 内容密钥的信封通道。
//
// 服务端只通过 RSA-OAEP 信封下发内容密钥：浏览器现场生成临时密钥对，只把
// 公钥发出去，私钥不出内存；明文密钥通道已移除，不存在"旁观响应体就能拿到
// 密钥"的降级路径。

export interface ClientKeyPair {
  /** base64 SPKI DER，随交付请求一起提交。 */
  publicKeyBase64: string;
  /** 解开信封，得到 32 字节内容密钥。 */
  unwrap(envelopeBase64: string): Promise<Uint8Array>;
}

/**
 * 前端能否在本地解密。
 *
 * 判据是安全上下文而不是"浏览器是否支持 WebCrypto"：非 HTTPS 下
 * crypto.subtle 干脆不存在，此时应当明确降级到服务端解密，而不是生成一对
 * 根本用不了的密钥。
 */
export function encryptionSupported(): boolean {
  return typeof crypto !== "undefined" && !!crypto.subtle && globalThis.isSecureContext;
}

/**
 * 现场生成一对临时密钥。
 *
 * 每次请求都新建：私钥的生命周期只覆盖一次交付，用完即弃。
 * 复用密钥对会让"泄漏一次 = 泄漏全部"，而生成成本完全可以忽略。
 */
export async function createClientKeyPair(): Promise<ClientKeyPair> {
  if (!encryptionSupported()) {
    throw new XphFormatError("当前环境不支持前端解密（需要 HTTPS 与 WebCrypto）");
  }
  const pair = await crypto.subtle.generateKey(
    {
      name: "RSA-OAEP",
      modulusLength: 2048,
      publicExponent: new Uint8Array([0x01, 0x00, 0x01]),
      hash: "SHA-256",
    },
    false,
    ["encrypt", "decrypt"],
  );
  const spki = await crypto.subtle.exportKey("spki", pair.publicKey);
  const publicKeyBase64 = bytesToBase64(new Uint8Array(spki));

  return {
    publicKeyBase64,
    async unwrap(envelopeBase64: string): Promise<Uint8Array> {
      const envelope = base64ToBytes(envelopeBase64);
      let raw: ArrayBuffer;
      try {
        raw = await crypto.subtle.decrypt({ name: "RSA-OAEP" }, pair.privateKey, asBuffer(envelope));
      } catch (err) {
        throw new XphFormatError(`解开内容密钥失败：${String(err)}`);
      }
      const dek = new Uint8Array(raw);
      if (dek.byteLength !== KEY_SIZE) {
        throw new XphFormatError(`内容密钥长度应为 ${KEY_SIZE}，实得 ${dek.byteLength}`);
      }
      return dek;
    },
  };
}

/**
 * 取出本次交付的内容密钥。
 *
 * 密钥只可能以 RSA-OAEP 信封形式下发；本地没有可用私钥时直接失败，
 * 不提供任何明文降级。
 */
export async function resolveContentKey(
  meta: { keyEnvelope: string },
  pair: ClientKeyPair | null,
): Promise<Uint8Array> {
  if (!pair) {
    throw new XphFormatError("当前环境没有可用的本地临时密钥对");
  }
  return pair.unwrap(meta.keyEnvelope);
}
