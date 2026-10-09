import { fetchCipherRange, fsApi, streamUrlWithToken } from "@/api/endpoints";
import type { DeliveryPlan } from "@/api/types";
import {
  createClientKeyPair,
  encryptionSupported,
  resolveContentKey,
  type ClientKeyPair,
} from "@/crypto/clientkey";
import { decryptAll, importContentKey, parseXphHeader, HEADER_SIZE, type XphHeader } from "@/crypto/xph";
import { Sha256, bytesToHex } from "@/crypto/sha256";

// 交付编排：把"取密文 → 解密 → 校验 → 交付"串成一条路径。
//
// 下载与预览共用这里的前半段，只在消费端分岔。合一条路径是为了避免出现
// "预览能看、下载报错"这类不一致——那类问题的排查成本极高，因为两边的差异
// 藏在分岔点之后，而分岔点会随版本漂移。

/** 获取交付计划的策略。文件与分享/取件码走不同接口，但下游完全一致。 */
export interface DeliverySource {
  plan(pair: ClientKeyPair | null): Promise<DeliveryPlan>;
}

export interface DeliveryProgress {
  phase: "preparing" | "fetching" | "decrypting" | "verifying" | "delivering" | "done";
  /** 已产出的明文字节数。 */
  bytesDone: number;
  /** 明文总长度；未知时为 0。 */
  bytesTotal: number;
  message: string;
}

interface DeliveryResult {
  fileName: string;
  mimeType: string;
  checksum: string;
  bytes: number;
  /** 累积在内存中的产物；走流式落盘时为 null。 */
  blob: Blob | null;
  /** 流式落盘时的文件名；未走流式时为 null。 */
  savedAs: string | null;
}

interface DeliveryOptions {
  onProgress?: (progress: DeliveryProgress) => void;
  signal?: AbortSignal;
  /** 超过该大小时优先流式落盘，避免整份文件驻留内存。 */
  streamToDiskAbove?: number;
}

const DEFAULT_STREAM_THRESHOLD = 256 * 1024 * 1024;

/**
 * 执行一次交付。
 *
 * 无论成功还是失败都会结算票据：不结算会让预扣的额度被永久占住，
 * 而客户端中断恰恰是最常见的失败形态。
 */
export async function runDelivery(
  source: DeliverySource,
  options: DeliveryOptions = {},
): Promise<DeliveryResult> {
  const report = options.onProgress ?? (() => {});
  const signal = options.signal;

  report({ phase: "preparing", bytesDone: 0, bytesTotal: 0, message: "正在准备" });

  const pair = encryptionSupported() ? await createClientKeyPair() : null;
  const plan = await source.plan(pair);

  let result: DeliveryResult;
  try {
    result =
      plan.contentForm === "ciphertext"
        ? await receiveCiphertext(plan, pair, report, signal, options)
        : await receivePlaintext(plan, report, signal, options);
  } catch (err) {
    await settleQuietly(plan);
    throw err;
  }

  // 密文通道按预扣全额由后端结算，上报字节数没有意义；这里只负责确认结账。
  await settleQuietly(plan);
  report({ phase: "done", bytesDone: result.bytes, bytesTotal: result.bytes, message: "已完成" });
  return result;
}

/**
 * 结算失败不阻断交付：文件已经交给用户，此时报错只会变成"下完了但界面报错"。
 *
 * 只有密文通道（direct/proxy）需要客户端调用：服务端无法实测用量，后端按
 * 预扣全额结算；proxy_decrypt 在中转流输出时已由服务端实测结算，不再上报。
 * 下载与预览（含 SW/Blob 兜底）共用这一个结账入口。
 */
export async function settleQuietly(plan: DeliveryPlan): Promise<void> {
  if (!plan.ticketId || plan.mode === "proxy_decrypt") {
    return;
  }
  try {
    await fsApi.settle(plan.ticketId);
  } catch {
    // 忽略：额度会在维护任务里被对账修正。
  }
}

/**
 * 按交付模式解析密文来源 URL。
 *
 * direct 用后端签发的 123 直链（跨域，票据已在 query 中，不能再带自定义头）；
 * proxy 用本机中转地址，服务器只做纯反向代理，密文原样返回，因此同样走
 * Range 取密文 + 本地解密，唯一差别是同源地址需要补上访问令牌。
 */
export function cipherSourceUrl(plan: DeliveryPlan): string {
  if (plan.mode === "direct") {
    if (!plan.url && !plan.parts?.length) {
      throw new Error("交付计划声明为直链，但没有提供直链地址");
    }
    // 多卷直链没有单一 URL；调用方应通过 fetchCipherPlanRange 按偏移取数。
    return plan.url ?? "";
  }
  if (plan.mode === "proxy") {
    if (!plan.streamUrl) {
      throw new Error("交付计划声明为中转加密，但没有提供中转地址");
    }
    return streamUrlWithToken(plan.streamUrl);
  }
  throw new Error(`交付模式 ${plan.mode} 不应走密文接收路径`);
}

/** 按逻辑密文区间读取交付计划，自动跨越一个或多个物理卷。 */
export function fetchCipherPlanRange(
  plan: DeliveryPlan,
  start: number,
  endInclusive: number,
  signal?: AbortSignal,
): Promise<Uint8Array> {
  if (plan.mode === "direct" && plan.parts?.length) {
    return fetchCipherPartsRange(plan.parts, start, endInclusive, signal);
  }
  return fetchCipherRange(cipherSourceUrl(plan), start, endInclusive, signal);
}

async function receiveCiphertext(
  plan: DeliveryPlan,
  pair: ClientKeyPair | null,
  report: (p: DeliveryProgress) => void,
  signal: AbortSignal | undefined,
  options: DeliveryOptions,
): Promise<DeliveryResult> {
  const meta = plan.encryption;
  if (!meta) {
    throw new Error("交付计划声明为密文，但没有提供解密元数据");
  }
  // 密文有两个来源，处理路径完全一致——区别只在 URL：
  //   direct：123 直链绝对地址，跨域、只带 Range 头；
  //   proxy ：本机中转地址（纯反向代理），同源、票据与令牌走查询串。
  const fetchRange = (start: number, endExclusive: number, rangeSignal = signal) => {
    return fetchCipherPlanRange(plan, start, endExclusive - 1, rangeSignal);
  };

  // 先取文件头：块大小、明文长度、nonce 前缀都在里面，而它们必须与交付元数据
  // 一致。不一致说明对象与记录已经错配，此时继续解密只会得到认证失败。
  report({ phase: "fetching", bytesDone: 0, bytesTotal: meta.plainSize, message: "正在读取文件头" });
  const headerBytes = await fetchRange(0, HEADER_SIZE);
  const header = parseXphHeader(headerBytes);
  assertHeaderMatchesMeta(header, meta);

  const dek = await resolveContentKey(meta, pair);
  const key = await importContentKey(dek);

  const sink =
    meta.plainSize >= (options.streamToDiskAbove ?? DEFAULT_STREAM_THRESHOLD)
      ? await tryOpenDiskSink(plan.fileName)
      : null;

  const parts: Uint8Array[] = [];
  const hasher = new Sha256();
  let done = 0;
  const total = header.plainSize;

  try {
    await decryptAll(
      key,
      header,
      fetchRange,
      async (plain) => {
        hasher.update(plain);
        if (sink) {
          await sink.write(plain);
        } else {
          parts.push(plain);
        }
        done += plain.byteLength;
        report({ phase: "decrypting", bytesDone: done, bytesTotal: total, message: "正在解密" });
      },
      { concurrency: 6, batchBlocks: 8, signal },
    );

    report({ phase: "verifying", bytesDone: done, bytesTotal: total, message: "正在校验完整性" });
    const digest = bytesToHex(hasher.digest());
    if (digest !== plan.checksum) {
      throw new Error(`内容校验失败：期望 ${plan.checksum}，实得 ${digest}`);
    }

    if (sink) {
      await sink.close();
      return {
        fileName: plan.fileName,
        mimeType: plan.mimeType,
        checksum: digest,
        bytes: done,
        blob: null,
        savedAs: sink.name,
      };
    }

    report({ phase: "delivering", bytesDone: done, bytesTotal: total, message: "正在交付" });
    return {
      fileName: plan.fileName,
      mimeType: plan.mimeType,
      checksum: digest,
      bytes: done,
      blob: new Blob(parts as BlobPart[], { type: plan.mimeType || "application/octet-stream" }),
      savedAs: null,
    };
  } catch (err) {
    // 网络失败、用户点取消、GCM 认证失败都会走到这里。原先只在"摘要不符"
    // 一条分支上 abort，异常路径下 sink 一直开着：浏览器会继续锁住用户刚选
    // 定的目标文件，再下载同一路径直接失败，必须重启标签页才解锁。
    // abort() 内部对已关闭是幂等的，成功路径上重复调用也安全。
    await sink?.abort().catch(() => {});
    throw err;
  }
}

/** 把跨物理卷的一个逻辑密文区间拼成连续字节，供既有解密器使用。 */
async function fetchCipherPartsRange(
  parts: NonNullable<DeliveryPlan["parts"]>,
  start: number,
  endInclusive: number,
  signal?: AbortSignal,
): Promise<Uint8Array> {
  const endExclusive = endInclusive + 1;
  const hits = parts.filter((part) => part.offset < endExclusive && part.offset + part.size > start);
  if (!hits.length || hits[0].offset > start || hits[hits.length - 1].offset + hits[hits.length - 1].size < endExclusive) {
    throw new Error("分卷清单无法覆盖所请求的密文区间");
  }
  if (hits.length === 1) {
    const part = hits[0];
    return fetchCipherRange(part.url, start - part.offset, endInclusive - part.offset, signal);
  }
  const blocks = await Promise.all(hits.map((part) => {
    const from = Math.max(start, part.offset);
    const to = Math.min(endExclusive, part.offset + part.size);
    return fetchCipherRange(part.url, from - part.offset, to - part.offset - 1, signal);
  }));
  const out = new Uint8Array(endExclusive - start);
  let offset = 0;
  for (const block of blocks) {
    out.set(block, offset);
    offset += block.byteLength;
  }
  if (offset !== out.byteLength) {
    throw new Error("分卷响应长度与逻辑密文区间不一致");
  }
  return out;
}

async function receivePlaintext(
  plan: DeliveryPlan,
  report: (p: DeliveryProgress) => void,
  signal: AbortSignal | undefined,
  options: DeliveryOptions,
): Promise<DeliveryResult> {
  if (!plan.streamUrl) {
    throw new Error("服务端未提供可用内容：既没有直链也没有中转地址");
  }
  const url = streamUrlWithToken(plan.streamUrl);
  report({
    phase: "fetching",
    bytesDone: 0,
    bytesTotal: plan.plainSize,
    message: "正在接收",
  });

  const response = await fetch(url, { signal });
  if (!response.ok) {
    throw new Error(`接收失败：HTTP ${response.status}`);
  }

  // 这条路径下服务端推过来的已经是明文，字节必须原样保存。
  // 这里唯一可信的对照是内容摘要——本地没有任何可独立校验的素材。
  let sink: DiskSink | null = null;
  let reader: ReadableStreamDefaultReader<Uint8Array> | null = null;
  const parts: Uint8Array[] = [];
  const hasher = new Sha256();
  let done = 0;
  try {
    reader = response.body?.getReader() ?? null;
    sink =
      reader && plan.plainSize >= (options.streamToDiskAbove ?? DEFAULT_STREAM_THRESHOLD)
        ? await tryOpenDiskSink(plan.fileName)
        : null;

    if (reader) {
      for (;;) {
        const { done: finished, value } = await reader.read();
        if (finished) {
          break;
        }
        if (!value) {
          continue;
        }
        hasher.update(value);
        if (sink) {
          await sink.write(value);
        } else {
          parts.push(value);
        }
        done += value.byteLength;
        report({ phase: "fetching", bytesDone: done, bytesTotal: plan.plainSize, message: "正在接收" });
      }
    }

    report({ phase: "verifying", bytesDone: done, bytesTotal: plan.plainSize, message: "正在校验完整性" });
    // 长度核对与摘要缺一不可：摘要防篡改/截断，但摘要缺失时长度核对是唯一
    // 能发现"流被截断却正常结束"的关卡。
    if (plan.plainSize && done !== plan.plainSize) {
      throw new Error(`内容长度不一致：期望 ${plan.plainSize} 字节，实得 ${done} 字节`);
    }
    const digest = bytesToHex(hasher.digest());
    if (plan.checksum && digest !== plan.checksum) {
      throw new Error(`内容校验失败：期望 ${plan.checksum}，实得 ${digest}`);
    }

    if (sink) {
      await sink.close();
      return {
        fileName: plan.fileName,
        mimeType: plan.mimeType,
        checksum: digest,
        bytes: done,
        blob: null,
        savedAs: sink.name,
      };
    }
    return {
      fileName: plan.fileName,
      mimeType: plan.mimeType,
      checksum: digest,
      bytes: done,
      blob: new Blob(parts as BlobPart[], { type: plan.mimeType || "application/octet-stream" }),
      savedAs: null,
    };
  } catch (err) {
    await reader?.cancel(err).catch(() => {});
    await sink?.abort().catch(() => {});
    throw err;
  } finally {
    reader?.releaseLock();
  }
}

export function assertHeaderMatchesMeta(header: XphHeader, meta: DeliveryPlan["encryption"]): void {
  if (!meta) {
    return;
  }
  const problems: string[] = [];
  if (header.blockLog2 !== meta.chunkLog2) {
    problems.push(`块大小指数 头=${header.blockLog2} 记录=${meta.chunkLog2}`);
  }
  if (header.plainSize !== meta.plainSize) {
    problems.push(`明文长度 头=${header.plainSize} 记录=${meta.plainSize}`);
  }
  if (header.cipherSize !== meta.cipherSize) {
    problems.push(`密文长度 头=${header.cipherSize} 记录=${meta.cipherSize}`);
  }
  if (header.noncePrefix !== meta.noncePrefix) {
    problems.push(`nonce 前缀 头=${header.noncePrefix} 记录=${meta.noncePrefix}`);
  }
  if (problems.length > 0) {
    throw new Error(`对象与记录不一致，已中止（${problems.join("；")}）`);
  }
}

interface DiskSink {
  name: string;
  write(chunk: Uint8Array): Promise<void>;
  close(): Promise<void>;
  abort(): Promise<void>;
}

interface SaveFilePickerWindow {
  showSaveFilePicker?: (options: { suggestedName?: string }) => Promise<FileSystemFileHandle>;
}

/**
 * 尝试打开流式落盘通道。
 *
 * 只在支持 File System Access 的浏览器上可用；不支持时返回 null，由调用方
 * 回落到内存累积。"不能流式"不是失败，只是另一种交付方式，因此这里不报错。
 */
async function tryOpenDiskSink(fileName: string): Promise<DiskSink | null> {
  const picker = (window as unknown as SaveFilePickerWindow).showSaveFilePicker;
  if (typeof picker !== "function") {
    return null;
  }
  try {
    const handle = await picker({ suggestedName: fileName });
    const writable = await handle.createWritable();
    let closed = false;
    return {
      name: handle.name,
      async write(chunk) {
        await writable.write(chunk as unknown as BufferSource);
      },
      async close() {
        if (!closed) {
          await writable.close();
          closed = true;
        }
      },
      async abort() {
        if (!closed) {
          closed = true;
          await writable.abort();
        }
      },
    };
  } catch (err) {
    // 用户取消保存选择就应取消下载；把它当作"不走流式"会继续把超大文件
    // 全部堆进内存，既违背用户意图，也会表现为下载耗时很久甚至页面卡死。
    if (err instanceof DOMException && err.name === "AbortError") {
      throw err;
    }
    // 其它文件系统错误仍回落到浏览器内存下载。
    return null;
  }
}

/** 触发浏览器下载。 */
export function saveBlob(blob: Blob, fileName: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = fileName;
  anchor.rel = "noopener";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  // 立刻回收会让部分浏览器来不及读取，延迟释放是必要的。
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}
