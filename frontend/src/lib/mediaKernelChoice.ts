/**
 * 播放内核选型：原生直放 vs 兼容转换。
 *
 * 两套内核的成本差在整条链路的长度上：
 *
 *  - 原生直放：`<video src="/__xph/{id}">` —— 浏览器自己发 Range，SW 换算成密文
 *    区间取回、解密、按 206 返回。没有 Worker、没有解封装/重封装、没有跨线程
 *    传输、没有 SourceBuffer 追加，字节从存储直��进解码器。
 *  - 兼容转换：Mediabunny 解封装 → 重封装 → postMessage 传回 → appendBuffer。
 *    多出来的每一步都要占主线程预算和内存。
 *
 * 所以常见组合（H.264 + AAC 的 MP4 / MOV）走原生，复杂格式仍交给 Mediabunny——
 * 它继续承担 MKV、特殊编码与需要重封装的兼容工作，不是被绕过。
 *
 * **判定必须基于实际编码，不能只看扩展名**：`.mp4` 里可以是 HEVC、AV1、
 * ProRes 甚至 MKV 改的后缀；`.mkv` 里也可能只是 H.264。所以这里要读容器，
 * 见 `probeNativePlayable`。
 */

/** 采样容器与编码判定需要读到的字节数；faststart MP4 的 moov 通常在这个范围内。 */
const PROBE_BYTES = 128 * 1024;

/** 原生直放白名单：这些 codec 字符串在各主流浏览器上都能直接解码。 */
const NATIVE_VIDEO_CODECS = /^(avc1|avc3|vp8|vp9|av01)[.\s]/i;
const NATIVE_AUDIO_CODECS = /^(mp4a|opus|vorbis|flac|alac)/i;
/** 明确排除：这些即使写着 avc1 也常带浏览器解不了的 profile/level 或扩展数据。 */
const RISKY_VIDEO_CODECS = /^(hev1|hvc1|av01)/i;

/** 容器判定结果。unknown 表示读到的字节不足以定论，交给兼容内核。 */
export type ContainerVerdict = "mp4" | "matroska" | "webm" | "unknown";

export interface NativeProbe {
  /** 容器族。 */
  container: ContainerVerdict;
  /** 文件头附近的轨道编码串（如 avc1.64001f、mp4a.40.2），没有则为null。 */
  codecs: string[];
  /** 是否适合原生直放。false 时应回退到兼容内核。 */
  native: boolean;
  /** 不能直放的原因，用于日志与提示。 */
  reason: string;
}

/** 从文件头读容器族。 */
function containerFromBytes(bytes: Uint8Array): ContainerVerdict {
  if (bytes.byteLength >= 12) {
    const type = String.fromCharCode(bytes[4], bytes[5], bytes[6], bytes[7]);
    if (type === "ftyp") return "mp4";
  }
  // Matroska 与 WebM 共用 EBML 头（0x1A45DFA3）。两者都交给兼容内核，
  // 原生直放对 MKV 的支持完全取决于浏览器，不值得赌——见 containerForcesCompat。
  if (bytes.byteLength >= 4 && bytes[0] === 0x1a && bytes[1] === 0x45 && bytes[2] === 0xdf && bytes[3] === 0xa3) {
    return "matroska";
  }
  return "unknown";
}

/**
 * 在字节流里找codec 字符串。
 *
 * 做法是扫描 ISO BMFF 里的 `stsd` 盒区域：`stsd` 之后每个 sample entry 的
 * 前四字节就是 fourcc（如 avc1、mp4a）。这比解析完整盒子树便宜得多，也不用
 * 维护一个 MP4 解析器——我们要的只是"有哪些编码"，不是精确的轨道布局。
 */
function codecsFromBytes(bytes: Uint8Array): string[] {
  const found = new Set<string>();
  // stsd 全量匹配（sample description），出现在任何轨道描述里都有效。
  for (let i = 0; i + 4 <= bytes.byteLength; i++) {
    if (bytes[i] !== 0x73 || bytes[i + 1] !== 0x74 || bytes[i + 2] !== 0x73 || bytes[i + 3] !== 0x64) continue;
    // stsd 的 sample entry 从偏移 +16 开始（version+flags 4 + entryCount 4 + 8 保留）。
    for (let j = i + 16; j + 4 <= bytes.byteLength && j < i + 256; j++) {
      const code = String.fromCharCode(bytes[j], bytes[j + 1], bytes[j + 2], bytes[j + 3]);
      if (/^[a-z0-9]{4}$/i.test(code)) found.add(code.toLowerCase());
    }
  }
  return [...found];
}

/**
 * 判断一个明文媒体文件能否原生直放。
 *
 * 读到的字节来自 SW 虚拟地址（`/session-id`）的 Range 0 请求——那本来就是明文，
 * 与播放器将要看到的完全一致，所以这里探测的就是真实内容，不是猜测。
 *
 * 容器与编码判定只是第一关。第二关是浏览器自己说能不能播：`canPlayType` 用的
 * 是浏览器真实的解码器能力表，比任何 codec 字符串白名单都准。所以顺序是——
 * 先看容器/编码排除明显不能直放的，再让浏览器对自己的解码表点头。
 */
export async function probeNativePlayable(url: string, mime: string, signal: AbortSignal): Promise<NativeProbe> {
  let bytes: Uint8Array;
  try {
    const response = await fetch(url, { headers: { Range: `bytes=0-${PROBE_BYTES - 1}` }, signal, cache: "no-store" });
    if (response.status !== 206 && response.status !== 200) throw new Error(`探测响应 ${response.status}`);
    // 某些代理会忽略 Range 并返回整份媒体。探测只需要头部，必须限量读取，
    // 否则一次编码判定就可能把数 GiB 文件整体装进内存。
    const reader = response.body?.getReader();
    if (!reader) throw new Error("探测响应没有可读内容");
    const chunks: Uint8Array[] = [];
    let received = 0;
    try {
      while (received < PROBE_BYTES) {
        signal.throwIfAborted();
        const { value, done } = await reader.read();
        if (done) break;
        const take = Math.min(value.byteLength, PROBE_BYTES - received);
        chunks.push(value.subarray(0, take)); received += take;
        if (take < value.byteLength) break;
      }
    } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
    bytes = new Uint8Array(received);
    let offset = 0;
    for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  } catch (error) {
    signal.throwIfAborted();
    return { container: "unknown", codecs: [], native: false, reason: "无法读取文件头以判定编码" };
  }
  const container = containerFromBytes(bytes);
  if (containerForcesCompat(container)) {
    return { container, codecs: [], native: false, reason: "Matroska 容器统一走兼容内核" };
  }
  if (container === "unknown") {
    return { container, codecs: [], native: false, reason: "非 ISO BMFF 容器，需重新封装" };
  }
  const codecs = codecsFromBytes(bytes);
  if (!codecs.length) {
    // faststart 的 MP4 里 stsd 在文件头；没 faststart 的 moov 在尾部，探不到很正常。
    // 对常见 MP4 而言"探不到 codec"不等于"不能播"——浏览器自己能解析完整文件。
    return { container, codecs, native: false, reason: "未能在头部定位音轨描述" };
  }
  const video = codecs.filter(c => NATIVE_VIDEO_CODECS.test(c) || RISKY_VIDEO_CODECS.test(c));
  const risky = video.some(c => RISKY_VIDEO_CODECS.test(c));
  const audio = codecs.filter(c => NATIVE_AUDIO_CODECS.test(c));
  // 有明确的非原生编码（HEVC/AV1）就直接否掉；有白名单里的视频与音频才肯定。
  if (risky) return { container, codecs, native: false, reason: `含浏览器未必支持的编码 ${video.join(",")}` };
  if (video.length && !video.every(c => NATIVE_VIDEO_CODECS.test(c))) {
    return { container, codecs, native: false, reason: `视频编码 ${video.join(",")} 不在原生直放范围` };
  }
  if (audio.length && !audio.every(c => NATIVE_AUDIO_CODECS.test(c))) {
    return { container, codecs, native: false, reason: `音频编码 ${audio.join(",")} 不在原生直放范围` };
  }
  // 有 codec 但一条都没归类（如 ProRes、LPCM）：宁可走兼容内核，也不要赌。
  if (!video.length && !audio.length) {
    return { container, codecs, native: false, reason: "未能识别任何常见音视频编码" };
  }
  // 白名单过了，最后交给浏览器自己的解码器表裁决——它比我们的字符串判断权威。
  // Safari 与移动端对H.264 的 profile/level 限制各不相同，这一步正是为它们准备的。
  const probe = document.createElement(video.length ? "video" : "audio");
  const verdict = probe.canPlayType(mime);
  if (!verdict) {
    return { container, codecs, native: false, reason: `浏览器不认 ${mime}` };
  }
  return { container, codecs, native: true, reason: "" };
}

/**
 * 已知一定不要走原生直放的容器。
 *
 * Matroska 家族整体交给兼容内核：原生路径对 MKV 的支持全看浏览器，而
 * mediabunny 对它的处理是确定的。宁可多走一步，也不要在 MKV 上碰到"能播但卡"
 * 的灰色地带。
 */
export function containerForcesCompat(container: ContainerVerdict): boolean {
  return container === "matroska";
}
