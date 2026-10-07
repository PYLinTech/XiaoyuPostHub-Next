// XPH-Crypt v1 的浏览器端实现。
//
// 独立实现：后端（Go 侧 internal/xph）另有一份，两侧不共享代码、不共享夹具，
// 也不做自动化比对，格式一致性靠约定维持。因此任何数值、偏移、字节序的改动
// 都必须两侧同步，否则表现为"解密认证失败"——GCM 的报错不会告诉你哪一步错了，
// 所以这里每个常量都标了出处。

const MAGIC = "XPHCRPT1";
export const HEADER_SIZE = 64;
const AAD_SIZE = 40;
const TAG_SIZE = 16;
const NONCE_SIZE = 12;
export const KEY_SIZE = 32;
const MIN_BLOCK_LOG2 = 9;
const MAX_BLOCK_LOG2 = 26;

export class XphFormatError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "XphFormatError";
  }
}

/** 头部解析结果。`raw` 保留原始 64 字节，因为 AAD 必须与远端字节完全一致。 */
export interface XphHeader {
  raw: Uint8Array;
  /** 参与每块认证的前 40 字节。 */
  aad: Uint8Array;
  algo: number;
  blockLog2: number;
  blockSize: number;
  plainSize: number;
  noncePrefix: number;
  blockCount: number;
  cipherSize: number;
}

/** 一段按块对齐的密文区间。 */
export interface BlockSpan {
  first: number;
  last: number;
  /** 密文起始偏移（含文件头）。 */
  start: number;
  /** 密文结束偏移（不含）。 */
  end: number;
}

export function parseXphHeader(raw: Uint8Array): XphHeader {
  if (raw.byteLength < HEADER_SIZE) {
    throw new XphFormatError(`文件头被截断：需要 ${HEADER_SIZE} 字节，实得 ${raw.byteLength}`);
  }
  const magic = latin1(raw.subarray(0, MAGIC.length));
  if (magic !== MAGIC) {
    throw new XphFormatError(`魔数不匹配：期望 ${MAGIC}，实得 ${JSON.stringify(magic)}`);
  }
  const algo = raw[8];
  if (algo !== 1) {
    throw new XphFormatError(`不支持的加密算法标识 ${algo}`);
  }
  const blockLog2 = raw[9];
  if (blockLog2 < MIN_BLOCK_LOG2 || blockLog2 > MAX_BLOCK_LOG2) {
    throw new XphFormatError(`非法的块大小指数 ${blockLog2}`);
  }

  const view = new DataView(raw.buffer, raw.byteOffset, HEADER_SIZE);
  // plainSize 是 u64 小端。用 getBigUint64 而不是拼两个 getUint32：
  // 手拼字节序是这份格式里最容易错且最难发现的一处。
  const plainSizeBig = view.getBigUint64(28, true);
  if (plainSizeBig > 1n << 62n) {
    throw new XphFormatError("非法的明文长度");
  }
  const plainSize = Number(plainSizeBig);
  if (!Number.isSafeInteger(plainSize)) {
    throw new XphFormatError("明文长度超出可安全表示范围，无法在本页解密");
  }
  const noncePrefix = view.getUint32(36, true);

  const blockSize = 2 ** blockLog2;
  const blockCount = plainSize <= 0 ? 0 : Math.ceil(plainSize / blockSize);

  return {
    raw: raw.subarray(0, HEADER_SIZE),
    aad: raw.subarray(0, AAD_SIZE),
    algo,
    blockLog2,
    blockSize,
    plainSize,
    noncePrefix,
    blockCount,
    cipherSize: HEADER_SIZE + plainSize + blockCount * TAG_SIZE,
  };
}

/** 第 index 块的密文（含 tag）起始偏移。 */
export function blockCipherOffset(header: XphHeader, index: number): number {
  return HEADER_SIZE + index * (header.blockSize + TAG_SIZE);
}

/** 第 index 块承载的明文长度；末块可能不足一个块。 */
function payloadLen(header: XphHeader, index: number): number {
  if (index < 0 || index >= header.blockCount) {
    return 0;
  }
  if (index < header.blockCount - 1) {
    return header.blockSize;
  }
  return header.plainSize - (header.blockCount - 1) * header.blockSize;
}

/** 第 index 块的密文（含 tag）长度。 */
export function blockCipherLen(header: XphHeader, index: number): number {
  return payloadLen(header, index) + TAG_SIZE;
}

/**
 * 算出覆盖明文区间 [offset, offset+limit) 所需的完整密文区间。
 *
 * 返回的是**按块对齐**的区间：GCM 的 tag 覆盖整块，因此读取必须取满整块，
 * 首尾多余的字节由 decryptCipherRange 按明文位置裁掉。
 * limit <= 0 表示读到明文末尾。
 */
export function chunkRange(header: XphHeader, offset: number, limit: number): BlockSpan {
  if (!Number.isFinite(offset) || offset < 0) {
    throw new XphFormatError("非法的读取区间起点");
  }
  if (offset > header.plainSize) {
    throw new XphFormatError("读取区间超出明文长度");
  }
  let effective = limit;
  if (effective <= 0 || offset + effective > header.plainSize) {
    effective = header.plainSize - offset;
  }
  if (effective === 0) {
    return { first: 0, last: 0, start: 0, end: 0 };
  }
  const first = Math.floor(offset / header.blockSize);
  const last = Math.floor((offset + effective - 1) / header.blockSize);
  return {
    first,
    last,
    start: blockCipherOffset(header, first),
    end: blockCipherOffset(header, last) + blockCipherLen(header, last),
  };
}

/** 第 index 块的 12 字节 nonce：u32 大端前缀 ‖ u64 大端块号。 */
function blockNonce(prefix: number, index: number): Uint8Array {
  const nonce = new Uint8Array(NONCE_SIZE);
  const view = new DataView(nonce.buffer);
  view.setUint32(0, prefix, false);
  view.setBigUint64(4, BigInt(index), false);
  return nonce;
}

/**
 * 导入内容密钥。
 *
 * 只申请 decrypt 用途：这个密钥在浏览器里永远不该被用来加密任何东西，
 * 收紧用途可以让"误用"在调用时报错而不是悄悄产生数据。
 */
export async function importContentKey(dek: Uint8Array): Promise<CryptoKey> {
  if (dek.byteLength !== KEY_SIZE) {
    throw new XphFormatError(`内容密钥必须为 ${KEY_SIZE} 字节，实得 ${dek.byteLength}`);
  }
  return crypto.subtle.importKey("raw", asBuffer(dek), { name: "AES-GCM" }, false, [
    "decrypt",
  ]);
}

/** 解密单块。cipherBlock 必须恰好是「明文长度 + 16」字节——GCM tag 覆盖整块。 */
export async function decryptBlock(
  key: CryptoKey,
  header: XphHeader,
  index: number,
  cipherBlock: Uint8Array,
): Promise<Uint8Array> {
  const expected = blockCipherLen(header, index);
  if (cipherBlock.byteLength !== expected) {
    throw new XphFormatError(`第 ${index} 块密文长度应为 ${expected}，实得 ${cipherBlock.byteLength}`);
  }
  // GCM 认证失败会抛一个裸的 DOMException("OperationError")，而本文件其它 10 处
  // 失败都是 XphFormatError。那条英文原文既不说明是哪一块、也不说明可能的原因，
  // 正是文件头注释里说的"最难发现"的一类——翻译成可读文案是这里唯一能做对的补救。
  let plain: ArrayBuffer;
  try {
    plain = await crypto.subtle.decrypt(
      {
        name: "AES-GCM",
        iv: asBuffer(blockNonce(header.noncePrefix, index)),
        additionalData: asBuffer(header.aad),
        tagLength: TAG_SIZE * 8,
      },
      key,
      asBuffer(cipherBlock),
    );
  } catch {
    throw new XphFormatError(
      `第 ${index} 块解密认证失败（对象与记录不匹配，或内容被篡改）`,
    );
  }
  return new Uint8Array(plain);
}

/**
 * 从一段连续密文里解出明文区间。
 *
 * 入参 cipher 必须正好对应 chunkRange() 给出的 [start, end)，而 wantStart /
 * wantLength 是最终要保留的明文范围。
 */
export async function decryptCipherRange(
  key: CryptoKey,
  header: XphHeader,
  cipher: Uint8Array,
  span: BlockSpan,
  wantStart: number,
  wantLength: number,
): Promise<Uint8Array> {
  if (span.end <= span.start || wantLength <= 0) {
    return new Uint8Array(0);
  }
  const out = new Uint8Array(wantLength);
  let written = 0;
  let cursor = 0;

  for (let index = span.first; index <= span.last; index++) {
    const len = blockCipherLen(header, index);
    const block = cipher.subarray(cursor, cursor + len);
    cursor += len;
    const plain = await decryptBlock(key, header, index, block);

    // 首块要丢掉 offset 之前的字节；末块只取到 wantLength 为止。
    const blockStart = index * header.blockSize;
    const takeFrom = Math.max(0, wantStart - blockStart);
    const takeTo = Math.min(plain.byteLength, wantStart + wantLength - blockStart);
    if (takeTo <= takeFrom) {
      continue;
    }
    out.set(plain.subarray(takeFrom, takeTo), written);
    written += takeTo - takeFrom;
  }

  if (written !== wantLength) {
    throw new XphFormatError(`解密产出 ${written} 字节，与期望的 ${wantLength} 不一致`);
  }
  return out;
}

/**
 * 逐块解密整份密文并按**块序**交给回调。
 *
 * 顺序保证是这个函数的契约，不是调用方的责任：并发取块会让批次乱序完成，
 * 如果直接把回调按完成顺序抛出去，调用方拼出来的文件就是错位的——而错位在
 * 末尾的 SHA-256 校验里只会表现为"校验失败"，很难联想到是拼装顺序的问题。
 * 代价是需要在内存里暂存已解密但尚未轮到交付的批次，上限与并发数同阶。
 *
 * 不返回缓冲区：大文件上把明文全部驻留内存会直接触发标签页崩溃，
 * 由调用方决定是累积成 Blob 还是流式写盘。
 */
export async function decryptAll(
  key: CryptoKey,
  header: XphHeader,
  readRange: (start: number, endExclusive: number) => Promise<Uint8Array>,
  onBlock: (plain: Uint8Array, index: number) => void | Promise<void>,
  options: { concurrency?: number; batchBlocks?: number; signal?: AbortSignal } = {},
): Promise<void> {
  if (header.blockCount === 0) {
    return;
  }
  const concurrency = Math.max(1, options.concurrency ?? 6);
  const batchBlocks = Math.max(1, options.batchBlocks ?? 8);
  const batchCount = Math.ceil(header.blockCount / batchBlocks);

  /** 已解密但还没轮到交付的批次。 */
  const pending = new Map<number, Uint8Array[]>();
  let deliverIndex = 0;
  let deliverError: unknown = null;
  // 串成一条 promise 链，保证同一时刻只有一处调用 onBlock，且严格按块序。
  let delivery: Promise<void> = Promise.resolve();

  const scheduleDelivery = (): void => {
    delivery = delivery.then(async () => {
      for (;;) {
        const blocks = pending.get(deliverIndex);
        if (!blocks) {
          return;
        }
        pending.delete(deliverIndex);
        const base = deliverIndex * batchBlocks;
        deliverIndex += 1;
        for (let i = 0; i < blocks.length; i++) {
          await onBlock(blocks[i], base + i);
        }
      }
    });
    // 交付侧失败（写盘被拒、Blob 分配失败）要**立刻**让在途 worker 看到。
    // 只在最外层 catch 里赋值是不够的：那个 catch 要等 Promise.all 全部结束
    // 才执行，而 worker 正是靠读 deliverError 来决定是否停手——于是在这段
    // 空窗里它们会把余下整份文件取回、解密，堆进已经没人消费的 pending。
    // 这里另挂一个处理器只做记录：原 promise 仍然会 reject，末尾的
    // `await delivery` 照样把错误抛给调用方。
    void delivery.catch((err: unknown) => {
      deliverError = err;
    });
  };

  let nextBatch = 0;
  const worker = async (): Promise<void> => {
    for (;;) {
      if (options.signal?.aborted) {
        throw new DOMException("已取消", "AbortError");
      }
      if (deliverError) {
        return;
      }
      const batchIndex = nextBatch++;
      if (batchIndex >= batchCount) {
        return;
      }
      const first = batchIndex * batchBlocks;
      const last = Math.min(first + batchBlocks, header.blockCount) - 1;
      const start = blockCipherOffset(header, first);
      const end = blockCipherOffset(header, last) + blockCipherLen(header, last);
      const cipher = await readRange(start, end);

      const blocks: Uint8Array[] = [];
      let cursor = 0;
      for (let index = first; index <= last; index++) {
        const len = blockCipherLen(header, index);
        blocks.push(await decryptBlock(key, header, index, cipher.subarray(cursor, cursor + len)));
        cursor += len;
      }
      pending.set(batchIndex, blocks);
      scheduleDelivery();
    }
  };

  try {
    await Promise.all(Array.from({ length: Math.min(concurrency, batchCount) }, worker));
    await delivery;
  } catch (err) {
    deliverError = err;
    // 没人会再来取这些块了：整批明文继续留在 map 里就是纯粹的内存驻留。
    pending.clear();
    // 交付侧已经失败时，让在途的取数尽快结束，不再继续下载密文。
    throw err;
  }
}

// ---------------------------------------------------------------- 编码工具

export function base64ToBytes(base64: string): Uint8Array {
  const binary = atob(base64.trim());
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

export function bytesToBase64(bytes: Uint8Array): string {
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.byteLength; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

/**
 * 传给 WebCrypto 的字节视图。
 *
 * 需要这个转换是因为 TS 5.7 起 Uint8Array 带了 ArrayBufferLike 泛型参数，
 * 而 WebCrypto 的签名要求 BufferSource（即 ArrayBufferView<ArrayBuffer>）。
 * 两者在运行期行为一致，差异只存在于类型层，因此把断言集中在这里一处，
 * 而不是在几十个调用点上各写一个 as。
 */
export function asBuffer(bytes: Uint8Array): BufferSource {
  return bytes as unknown as BufferSource;
}

function latin1(bytes: Uint8Array): string {
  let out = "";
  for (let i = 0; i < bytes.byteLength; i++) {
    out += String.fromCharCode(bytes[i]);
  }
  return out;
}
