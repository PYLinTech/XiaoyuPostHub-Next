/**
 * 完整下载兜底的能力判定。
 *
 * 兜底不是万能药：它只是把"流式读一段"换成"整个文件拿下来再播"。如果失败原因
 * 根本与取数无关（浏览器解不了这个编码、容器解析不了、磁盘缓存不可用），那么
 * 下载完整文件也救不回来，只会让用户在等待几十秒之后收到同一个错误。
 *
 * 所以执行兜底之前先分诊：
 *
 *  - **解码层失败**（MediaCodecError / UnsupportedInputFormatError）→ 兜底无效。
 *    完整文件喂给浏览器，解码器照样不认。唯一还有救的是"转码方案可用但流式
 *    封装失败"，那属于封装层，不是解码层。
 *  - **封装/管线失败**（MSE 追加失败、重封装崩溃、Range 不受支持）→ 兜底有效。
 *    数据本身没问题，只是流式组装那条路走不通，改成整份给播放器即可。
 *  - **资源能力不足**（浏览器不支持 OPFS、配额不够）→ 兜底注定失败。
 *
 * 判定宁可保守：判成"不该兜底"只是少试一次，判成"该兜底"却失败才是白等。
 */

export type FallbackVerdict =
  /** 兜底能解决问题，值得花时间下载整个文件。 */
  | { attempt: true; reason: string }
  /** 兜底注定无效，立即报错，不做无谓的下载。 */
  | { attempt: false; reason: string };

/** 这些错误说明的是"数据没问题但流式组装走不通"，整份下载有意义。 */
const ASSEMBLY_FAILURES = new Set([
  "MediaRemuxError",       // 重封装过程崩溃
  "MediaAppendError",      // SourceBuffer 追加失败（含 QuotaExceededError 包装）
  "RangeUnsupportedError", // 上游不支持 Range，无法流式
  "ContainerAppendError",  // MSE 不接受这段字节
]);

/** 这些错误说明浏览器根本解不了这份内容，兜底也白搭。 */
const DECODING_FAILURES = new Set([
  "MediaCodecError",
  "UnsupportedInputFormatError",
  "NotEncodableError",
]);

export interface FallbackContext {
  /** 文件明文大小。 */
  plainSize: number;
  /** 是否已超过 Blob 路径上限（即会走 OPFS）。 */
  exceedsBlobLimit: boolean;
  /** 浏览器是否具备 OPFS 能力。 */
  hasDiskCache: boolean;
  /** 是否具备 MediaSource / ManagedMediaSource。 */
  hasMediaSource: boolean;
}

export function resolveFallbackCapability(reason: unknown, context: FallbackContext): FallbackVerdict {
  const name = reason instanceof Error ? reason.name : "";
  const message = reason instanceof Error ? reason.message : String(reason ?? "");

  // 先问"兜底有没有物理条件"，再问"这次失败值不值得兜底"。
  // 顺序反了会给出误导性的提示：比如解码失败时，用户真正需要的提示是编解码问题，
  // 而不是"请下载后播放"。
  if (DECODING_FAILURES.has(name)) {
    // 但如果浏览器压根不支持流式管线，那这份文件到现在根本还没被尝试解码过——
    // 完整下载后交给原生 <video> 反而可能能播。这一条要区别对待。
    if (!context.hasMediaSource) return { attempt: true, reason: "浏览器不支持流式管线，改由原生播放器处理完整文件" };
    return { attempt: false, reason: message || "浏览器无法解码该文件的音视频编码，完整下载也无法解决" };
  }
  if (context.exceedsBlobLimit && !context.hasDiskCache) {
    return { attempt: false, reason: "文件过大且浏览器不支持本地缓存，请下载后播放" };
  }

  if (ASSEMBLY_FAILURES.has(name)) {
    return { attempt: true, reason: "流式封装路径失败，改用完整文件播放" };
  }
  // DOMException 家族：QuotaExceededError 是 SourceBuffer 塞不下，
  // 属于封装层，兜底（走 OPFS 或内存）反而可能缓解。
  if (name === "QuotaExceededError") {
    return { attempt: context.exceedsBlobLimit ? context.hasDiskCache : true,
      reason: "浏览器缓冲配额不足，改用完整文件播放" };
  }
  if (name === "DataCloneError" || name === "InvalidStateError") {
    return { attempt: true, reason: "媒体元素状态异常，改用完整文件重试" };
  }
  // MediaSourceError 的 range 分支：上游不给正确的区间。此时 complete() 走的
  // 也是同一条 cipher()，但它读的是 0..cipherSize 的连续区间，上游常常能正常
  // 响应（不少服务端只对"从0 开始"的全量请求友好），所以仍值得一试。
  const code = reason && typeof reason === "object" && "code" in reason ? reason.code : undefined;
  if (code === "range") {
    return { attempt: true, reason: "上游不支持区间读取，改用完整读取" };
  }
  // 未知错误：允许尝试一次。兜底是最后一道保险，宁可多试也不要直接放弃。
  return { attempt: true, reason: message || "流式播放失败，改用完整文件重试" };
}

/** OPFS 路径的三个阶段，让长时间等待有可见的进度而不是一个卡住的百分比。 */
export type CompletePhase = "download" | "verify" | "prepare";

export function describePhase(phase: CompletePhase, ratio: number): string {
  const percent = Math.floor(ratio * 100);
  switch (phase) {
    case "download": return `正在下载文件 ${percent}%`;
    case "verify": return `正在校验完整性 ${percent}%`;
    case "prepare": return "正在准备本地播放";
  }
}
