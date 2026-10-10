import {
  AudioSampleSink, AudioSampleSource, EncodedAudioPacketSource, EncodedPacketSink,
  EncodedVideoPacketSource, InputAudioTrack, InputVideoTrack, Mp4OutputFormat, NullTarget, Output,
  VideoSampleSink, VideoSampleSource, WebMOutputFormat, canEncodeAudio, canEncodeVideo,
} from "mediabunny";
import type { AudioCodec, Input, VideoCodec } from "mediabunny";
import { MediaRangeSource } from "./mediaRangeSource";
import { supportsMediaSourceType } from "./mediaSourceSupport";
import { durationFromTail } from "./isoDuration";

/**
 * 尾部探测的窗口起点序列（字节），命中即止。
 *
 * 末尾 moof 离文件尾多远，只取决于**最后一个分片有多大**，也就是码率 ×
 * 末片时长：这个值没有任何天然上界。16 Mbps 的文件常见末片只有几百 KiB，
 * 但高码率（4K/8K）动辄上百 MiB——实测 25 Mbps 配 6 秒末片就已经超过
 * 16 MiB，100 Mbps 更是差一个数量级。任何写死的上限都只是"刚好没爆"，
 * 换个码率就静默失效，所以这里不设上限，改用几何级数一路放大。
 *
 * 起始档仍取 256 KiB：绝大多数文件在这一档就命中，顺风局不会多付钱。
 * 每档 ×4 保证慢网上档位数少（8 档就到 64 MiB 档），不至于把预算耗在
 * 逐级试探上。
 */
const TAIL_WINDOW_START = 256 * 1024;
const TAIL_WINDOW_GROWTH = 4;

/**
 * 探测尾部窗口最多能放大到多大。
 *
 * 这是防止病态大文件把内存吃掉的兜底，不是"典型范围"。64 MiB 已经覆盖到
 * 约 100 Mbps × 14 秒末片的规模；再往上单个 Uint8Array 本身就逼近浏览器
 * 可承受的连续分配上限（多数引擎单次分配上限约 2 GiB，但 GC 停顿明显）。
 * 越过这里的文件让 duration() 返回 null、进度条留空，比拖垮标签页划算。
 */
const TAIL_WINDOW_MAX = 64 * 1024 * 1024;

/**
 * 时长探测的总预算。
 *
 * 慢网下 8 秒连最小一档都读不完，于是总时长永远拿不到——而总时长对进度条是
 * 必需的，缺了它用户就得从头拖到尾才知道有多长。宁可多等：底层 cipher() 还有
 * 30 秒无数据等待超时兜底，这里放宽到 45 秒。
 *
 * 这个预算同时是尾部搜索的真正终止条件：窗口按几何级数放大，预算耗尽即
 * 放弃（返回 null，进度条留空）。顺风局命中第一档就走完，不会碰到预算。
 */
const PROBE_TIMEOUT = 45_000;

/** 在字节里读 4 字符的 box 类型。 */
function readTypeAt(bytes: Uint8Array, offset: number): string {
  return String.fromCharCode(bytes[offset], bytes[offset + 1], bytes[offset + 2], bytes[offset + 3]);
}

/** 读 box 的 size 字段（支持 64 位扩展）。 */
function readBoxSize(bytes: Uint8Array, offset: number): number {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const size = view.getUint32(offset);
  if (size === 1) return Number(view.getBigUint64(offset + 8));
  return size === 0 ? bytes.byteLength - offset : size;
}

/** 在头部窗口里找到 moov 的起点；找不到返回 -1。 */
function findBox(bytes: Uint8Array, type: string): number {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  for (let offset = 0; offset + 8 <= bytes.byteLength; offset++) {
    if (readTypeAt(bytes, offset + 4) !== type) continue;
    const size = view.getUint32(offset);
    if (size >= 8 && offset + size <= bytes.byteLength) return offset;
  }
  return -1;
}

export type MediaTrackSpec = {
  type: "audio" | "video";
  codec: AudioCodec | VideoCodec;
  container: "mp4" | "webm";
  transcode: boolean;
  mime: string;
};

export type MediaInfo = { tracks: MediaTrackSpec[]; duration: number | null; origin: number };
class MediaCodecError extends Error {
  constructor(message: string) { super(message); this.name = "MediaCodecError"; }
}
let ac3Decoder: Promise<void> | undefined;
let dtsDecoder: Promise<void> | undefined;
let aacEncoder: Promise<void> | undefined;

async function enableAudioDecoder(codec: AudioCodec | VideoCodec | null): Promise<void> {
  if (codec === "ac3" || codec === "eac3") {
    ac3Decoder ??= import("@mediabunny/ac3").then(module => module.registerAc3Decoder())
      .catch(error => { ac3Decoder = undefined; throw error; });
    await ac3Decoder;
  } else if (codec === "dts") {
    dtsDecoder ??= import("@mediabunny/dts").then(module => module.registerDtsDecoder())
      .catch(error => { dtsDecoder = undefined; throw error; });
    await dtsDecoder;
  }
}

let mediaSupportCheck = async (mime: string) => supportsMediaSourceType(mime);
export function setMediaSupportCheck(check: (mime: string) => Promise<boolean>): void { mediaSupportCheck = check; }

async function containerFor(type: "audio" | "video", codec: AudioCodec | VideoCodec, parameter: string): Promise<MediaTrackSpec | null> {
  for (const container of ["mp4", "webm"] as const) {
    const format = container === "mp4" ? new Mp4OutputFormat() : new WebMOutputFormat();
    const mime = `${type}/${container}; codecs="${parameter}"`;
    if (format.getSupportedCodecs().includes(codec) && await mediaSupportCheck(mime)) {
      return { type, codec, container, transcode: false, mime };
    }
  }
  return null;
}

async function describeTrack(track: InputAudioTrack | InputVideoTrack): Promise<MediaTrackSpec> {
  const type = track.isVideoTrack() ? "video" : "audio";
  const codec = await track.getCodec();
  const parameter = await track.getCodecParameterString();
  const direct = codec && parameter && await containerFor(type, codec, parameter);
  if (direct) return direct;
  if (track.isAudioTrack()) await enableAudioDecoder(codec);
  if (!await track.canDecode()) throw new MediaCodecError("浏览器无法解码该媒体编码");
  if (track.isAudioTrack()) {
    const options = {
      numberOfChannels: await track.getNumberOfChannels(), sampleRate: await track.getSampleRate(), bitrate: 160_000,
    };
    for (const [target, parameter] of [["aac", "mp4a.40.2"], ["opus", "opus"]] as const) {
      const spec = await containerFor("audio", target, parameter);
      if (target === "aac" && spec && !await canEncodeAudio(target, options)) {
        aacEncoder ??= import("@mediabunny/aac-encoder").then(module => module.registerAacEncoder())
          .catch(error => { aacEncoder = undefined; throw error; });
        await aacEncoder;
      }
      if (spec && await canEncodeAudio(target, options)) return { ...spec, transcode: true };
    }
  } else {
    for (const [target, parameter] of [["avc", "avc1.42001f"], ["vp8", "vp8"]] as const) {
      const spec = await containerFor("video", target, parameter);
      if (spec && await canEncodeVideo(target, {
        width: await track.getCodedWidth(), height: await track.getCodedHeight(), bitrate: 4_000_000,
      })) return { ...spec, transcode: true };
    }
  }
  throw new MediaCodecError("浏览器没有可用的媒体转换编码器");
}

async function primaryTracks(input: Input): Promise<Array<InputVideoTrack | InputAudioTrack>> {
  const [video, audio] = await Promise.all([input.getPrimaryVideoTrack(), input.getPrimaryAudioTrack()]);
  return [video, audio].filter((track): track is InputVideoTrack | InputAudioTrack => !!track);
}

/** Input、容器索引与认证缓存属于会话；输出封装器只在 seek 时重建。 */
export class MediaDemuxSession {
  private input: Input;
  constructor(private source: MediaRangeSource, private lifetime: AbortSignal) {
    this.input = source.input(lifetime);
    lifetime.addEventListener("abort", () => this.input.dispose(), { once: true });
  }
  dispose() { this.input.dispose(); }
  async inspect(): Promise<MediaInfo> {
    const tracks = await primaryTracks(this.input);
    if (!tracks.length) throw new Error("文件中没有可播放的音视频轨道");
    const specs = await Promise.all(tracks.map(describeTrack));
    const origin = Math.min(...await Promise.all(tracks.map(track => track.getFirstTimestamp())));
    const duration = await this.input.getDurationFromMetadata(tracks);
    this.lifetime.throwIfAborted();
    return { tracks: specs, duration: duration !== null && duration > origin ? duration - origin : null, origin };
  }
  async duration(origin: number): Promise<number | null> {
    // 分片 MP4 把 duration 留在 mvhd 写 0（媒体可无限追加），从头读永远拿不到。
    // 所以先读头部元数据；拿不到再从文件尾部找最后一个 moof 反推——那里的
    // tfdt 带着最后一片的起始时间。两处都失败就返回 null，让播放器留空，
    // 也不要给一个猜出来的值：错的时长会让进度条提前到底然后停住。
    //
    // 探测要有耐心但不能贪心：慢网上固定 8 秒必然读不完最小几档，于是永远
    // 拿不到总时长；而一开始就铺满窗口又会在顺风局白读十几 MiB。所以用
    // 几何级数放大的窗口——命中即止，总预算 PROBE_TIMEOUT 是唯一的终止
    // 条件。底层 cipher() 另有 30 秒无数据等待超时兜底，这里管的是
    // "整体允许花多久"。
    const signal = AbortSignal.any([this.lifetime, AbortSignal.timeout(PROBE_TIMEOUT)]);
    const head = await this.probe(async () => {
      const input = this.source.input(signal, 2 * 1024 * 1024);
      try { return await input.computeDuration(undefined, { metadataOnly: true }); }
      finally { input.dispose(); }
    });
    if (head !== null && head > origin) return head - origin;

    const tail = await this.probe(() => this.tailDuration(signal));
    if (tail !== null && tail > origin) return tail - origin;
    return null;
  }

  /**
   * 从文件尾部反推容器时长：渐进放大窗口，命中即止。
   *
   * 末尾 moof 离文件尾的距离取决于最后一个分片有多大（码率 × 末片时长），
   * 这个值没有天然上界——本项目实测过的极端案例是 11.9 MiB，但高码率片源
   * 轻松超过。所以从 256 KiB 起步逐级放大到 TAIL_WINDOW_MAX，而不是每次读满，
   * 顺风局只付最小那一档的钱。
   */
  private async tailDuration(signal: AbortSignal): Promise<number | null> {
    const head = await this.source.read(0, Math.min(this.source.size, 1024 * 1024), signal);
    const moovStart = findBox(head, "moov");
    // moov 可能超过 1 MiB（长索引表），按需再取一次。
    const moov = moovStart < 0 ? null
      : moovStart + readBoxSize(head, moovStart) <= head.byteLength ? head.subarray(moovStart)
        : await this.source.read(moovStart, Math.min(this.source.size, moovStart + 4 * 1024 * 1024), signal)
          .then(bytes => bytes.subarray(0, readBoxSize(bytes, 0)));
    if (!moov || readTypeAt(moov, 4) !== "moov") return null;

    // 窗口按 256 KiB → 1 MiB → 4 MiB … 一路放大到 TAIL_WINDOW_MAX，命中即止。
    // 放大到上限仍未命中就返回 null：让进度条留空，也不给猜出来的值——错的
    // 时长会让进度条提前到底然后停住，危害大于不显示。外层 PROBE_TIMEOUT
    // 还会在慢网上更早叫停，所以这里不会真的读到上限那么远。
    const total = this.source.size;
    for (let window = Math.min(TAIL_WINDOW_START, total); ; window *= TAIL_WINDOW_GROWTH) {
      signal.throwIfAborted();
      // 请求量收敛到上限后不再增长，避免同一段尾部被反复读取。
      const reach = Math.min(window, TAIL_WINDOW_MAX, total);
      const tail = await this.source.tail(reach, signal);
      const duration = durationFromTail(moov, tail);
      // 找到就用，不继续放大：再往后读到的 moof 只会更早，tfdt 更小。
      if (duration !== null) return duration;
      // 已经读满整个文件（或上限）还没找到，没有下一档可试。
      if (reach >= total || window >= TAIL_WINDOW_MAX) return null;
    }
  }

  /** 探测失败（超时、读预算耗尽、上游不支持）一律当作"拿不到"，不让它打断播放。 */
  private async probe(operation: () => Promise<number | null>): Promise<number | null> {
    try { const value = await operation(); return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : null; }
    catch { return null; }
  }

  /** 顺序遍历编码包，不按固定时间窗口查终点或反复输出初始化段。 */
  async stream(spec: MediaTrackSpec, start: number, origin: number, signal: AbortSignal,
    onChunk: (data: Uint8Array<ArrayBuffer>, mime: string, keyframes: number[]) => Promise<void>,
    permit: (time: number) => Promise<void>): Promise<number> {
    signal.throwIfAborted();
    const track = spec.type === "video" ? await this.input.getPrimaryVideoTrack() : await this.input.getPrimaryAudioTrack();
    if (!track) throw new Error("媒体轨道发生变化");
    let boxes: Uint8Array[] = [], chunks: Uint8Array<ArrayBuffer>[] = [];
    const join = (parts: Uint8Array[]) => {
      const data = new Uint8Array(parts.reduce((sum, part) => sum + part.length, 0)); let offset = 0;
      for (const part of parts) { data.set(part, offset); offset += part.length; } return data;
    };
    const output = new Output({ target: new NullTarget(), format: spec.container === "mp4"
      ? new Mp4OutputFormat({ fastStart: "fragmented", minimumFragmentDuration: 0.5,
        onFtyp: data => { boxes = [data]; }, onMoov: data => { chunks.push(join([...boxes, data])); boxes = []; },
        onMoof: data => { boxes = [data]; }, onMdat: data => { chunks.push(join([...boxes, data])); boxes = []; },
      })
      : new WebMOutputFormat({ appendOnly: true, minimumClusterDuration: 0.5,
        onEbmlHeader: data => { boxes = [data]; }, onSegmentHeader: data => { chunks.push(join([...boxes, data])); boxes = []; },
        onCluster: data => { chunks.push(join([data])); },
      }),
    });
    let actualEnd = start, pendingBytes = 0;
    const keyframes: number[] = [];
    const flush = async (throttle = true) => {
      if (!chunks.length) return;
      const ready = chunks; chunks = [];
      const mime = (await output.getMimeType()).replace(/^video\//, spec.type + "/");
      for (const bytes of ready) { signal.throwIfAborted(); await onChunk(bytes, mime, keyframes.splice(0)); }
      pendingBytes = 0;
      // 完整片段交给 MSE 后才等待，长 GOP 不会在片段尚未封口时死锁。
      if (throttle) await permit(actualEnd);
      signal.throwIfAborted();
    };
    try {
      if (spec.transcode) {
        if (track.isAudioTrack()) {
          const writer = new AudioSampleSource({ codec: spec.codec as AudioCodec, bitrate: 160_000 });
          output.addAudioTrack(writer); await output.start();
          for await (const sample of new AudioSampleSink(track).samples(start + origin)) {
            try { signal.throwIfAborted(); sample.setTimestamp(sample.timestamp - origin);
              actualEnd = Math.max(actualEnd, sample.timestamp + sample.duration); await writer.add(sample); await flush();
            } finally { sample.close(); }
          }
        } else {
          const writer = new VideoSampleSource({ codec: spec.codec as VideoCodec, bitrate: 4_000_000, keyFrameInterval: 1,
            onEncodedPacket: packet => { if (packet.type === "key") keyframes.push(packet.timestamp); },
          });
          output.addVideoTrack(writer, { transformationMatrix: await track.getTransformationMatrix() }); await output.start();
          for await (const sample of new VideoSampleSink(track).samples(start + origin)) {
            try { signal.throwIfAborted(); sample.setTimestamp(sample.timestamp - origin);
              actualEnd = Math.max(actualEnd, sample.timestamp + sample.duration); await writer.add(sample); await flush();
            } finally { sample.close(); }
          }
        }
      } else {
        const sink = new EncodedPacketSink(track);
        const first = start > 0 ? await sink.getKeyPacket(start + origin) || await sink.getFirstKeyPacket() : await sink.getFirstKeyPacket();
        signal.throwIfAborted();
        if (!first) return start;
        const videoWriter = track.isVideoTrack() ? new EncodedVideoPacketSource(spec.codec as VideoCodec) : null;
        const audioWriter = track.isAudioTrack() ? new EncodedAudioPacketSource(spec.codec as AudioCodec) : null;
        if (videoWriter && track.isVideoTrack()) output.addVideoTrack(videoWriter, { transformationMatrix: await track.getTransformationMatrix() });
        if (audioWriter) output.addAudioTrack(audioWriter);
        const config = await track.getDecoderConfig();
        if (!config) throw new Error("媒体缺少解码配置");
        await output.start();
        for await (const packet of sink.packets(first)) {
          signal.throwIfAborted();
          pendingBytes += packet.byteLength;
          if (pendingBytes > 64 * 1024 * 1024) throw new Error("单个媒体片段超过内存限制");
          const normalized = packet.clone({ timestamp: packet.timestamp - origin });
          actualEnd = Math.max(actualEnd, normalized.timestamp + normalized.duration);
          if (videoWriter) {
            if (normalized.type === "key") keyframes.push(normalized.timestamp);
            await videoWriter.add(normalized, { decoderConfig: config as VideoDecoderConfig });
          }
          if (audioWriter) await audioWriter.add(normalized, { decoderConfig: config as AudioDecoderConfig });
          await flush();
        }
      }
      signal.throwIfAborted(); await output.finalize(); await flush(false); return actualEnd;
    } finally { if (output.state !== "finalized" && output.state !== "canceled") await output.cancel(); }
  }
}
