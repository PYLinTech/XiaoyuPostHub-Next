import {
  AudioSampleSink, AudioSampleSource, BufferTarget, EncodedAudioPacketSource, EncodedPacketSink,
  EncodedVideoPacketSource, InputAudioTrack, InputVideoTrack, Mp4OutputFormat, Output,
  VideoSampleSink, VideoSampleSource, WebMOutputFormat, canEncodeAudio, canEncodeVideo,
} from "mediabunny";
import type { AudioCodec, Input, VideoCodec } from "mediabunny";
import { MediaRangeSource } from "./mediaRangeSource";
import { supportsMediaSourceType } from "./mediaSourceSupport";

export type MediaTrackSpec = {
  type: "audio" | "video";
  codec: AudioCodec | VideoCodec;
  container: "mp4" | "webm";
  transcode: boolean;
  mime: string;
};

export type MediaInfo = { tracks: MediaTrackSpec[]; duration: number | null; origin: number };
export type MediaSlice = { data: Uint8Array<ArrayBuffer>; mime: string; start: number; end: number; eof: boolean };

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

function containerFor(type: "audio" | "video", codec: AudioCodec | VideoCodec, parameter: string): MediaTrackSpec | null {
  for (const container of ["mp4", "webm"] as const) {
    const format = container === "mp4" ? new Mp4OutputFormat() : new WebMOutputFormat();
    const mime = `${type}/${container}; codecs="${parameter}"`;
    if (format.getSupportedCodecs().includes(codec) && supportsMediaSourceType(mime)) {
      return { type, codec, container, transcode: false, mime };
    }
  }
  return null;
}

async function describeTrack(track: InputAudioTrack | InputVideoTrack): Promise<MediaTrackSpec> {
  const type = track.isVideoTrack() ? "video" : "audio";
  const codec = await track.getCodec();
  const parameter = await track.getCodecParameterString();
  const direct = codec && parameter && containerFor(type, codec, parameter);
  if (direct) return direct;
  if (track.isAudioTrack()) await enableAudioDecoder(codec);
  if (!await track.canDecode()) throw new Error("浏览器无法解码该媒体编码");
  if (track.isAudioTrack()) {
    const options = {
      numberOfChannels: await track.getNumberOfChannels(), sampleRate: await track.getSampleRate(), bitrate: 160_000,
    };
    for (const [target, parameter] of [["aac", "mp4a.40.2"], ["opus", "opus"]] as const) {
      const spec = containerFor("audio", target, parameter);
      if (target === "aac" && spec && !await canEncodeAudio(target, options)) {
        aacEncoder ??= import("@mediabunny/aac-encoder").then(module => module.registerAacEncoder())
          .catch(error => { aacEncoder = undefined; throw error; });
        await aacEncoder;
      }
      if (spec && await canEncodeAudio(target, options)) return { ...spec, transcode: true };
    }
  } else {
    for (const [target, parameter] of [["avc", "avc1.42001f"], ["vp8", "vp8"]] as const) {
      const spec = containerFor("video", target, parameter);
      if (spec && await canEncodeVideo(target, {
        width: await track.getCodedWidth(), height: await track.getCodedHeight(), bitrate: 4_000_000,
      })) return { ...spec, transcode: true };
    }
  }
  throw new Error("浏览器没有可用的媒体转换编码器");
}

async function primaryTracks(input: Input): Promise<Array<InputVideoTrack | InputAudioTrack>> {
  const [video, audio] = await Promise.all([input.getPrimaryVideoTrack(), input.getPrimaryAudioTrack()]);
  return [video, audio].filter((track): track is InputVideoTrack | InputAudioTrack => !!track);
}

export async function inspectMedia(source: MediaRangeSource, signal: AbortSignal): Promise<MediaInfo> {
  const input = source.input(signal);
  try {
    const tracks = await primaryTracks(input);
    if (!tracks.length) throw new Error("文件中没有可播放的音视频轨道");
    const specs = await Promise.all(tracks.map(describeTrack));
    const origin = Math.min(...await Promise.all(tracks.map(track => track.getFirstTimestamp())));
    const duration = await input.getDurationFromMetadata(tracks);
    signal.throwIfAborted();
    return { tracks: specs, duration: duration !== null && duration > origin ? duration - origin : null, origin };
  } finally { input.dispose(); }
}

/** 在首帧之后探测精确时长，限定读取量；无索引格式不扫描完整文件。 */
export async function probeMediaDuration(source: MediaRangeSource, origin: number, signal: AbortSignal): Promise<number | null> {
  const timeout = new AbortController();
  const timer = setTimeout(() => timeout.abort(), 8000);
  const input = source.input(AbortSignal.any([signal, timeout.signal]), 2 * 1024 * 1024);
  try {
    const duration = await input.computeDuration(undefined, { metadataOnly: true });
    return Number.isFinite(duration) && duration > origin ? duration - origin : null;
  } catch { return null; }
  finally { clearTimeout(timer); input.dispose(); }
}

/** 原生容器 -> 单轨 MSE 片段。保留编码及时间戳；只有必要时才在本地转换。 */
export async function remuxMediaSlice(
  source: MediaRangeSource, spec: MediaTrackSpec, start: number, end: number, origin: number, signal: AbortSignal, completeGop = true,
): Promise<MediaSlice | null> {
  const input = source.input(signal);
  let output: Output<Mp4OutputFormat | WebMOutputFormat, BufferTarget> | null = null;
  try {
    const track = spec.type === "video" ? await input.getPrimaryVideoTrack() : await input.getPrimaryAudioTrack();
    if (!track) throw new Error("媒体轨道发生变化");
    output = new Output({
      target: new BufferTarget(),
      format: spec.container === "mp4"
        ? new Mp4OutputFormat({ fastStart: "fragmented", minimumFragmentDuration: 1 })
        : new WebMOutputFormat({ appendOnly: true, minimumClusterDuration: 1 }),
    });
    const sink = new EncodedPacketSink(track);
    const final = await sink.getPacket(end + origin - 1e-6, { metadataOnly: true });
    // 时间窗口可能位于延迟起始轨道之前。没有终点时不可把 undefined 传给 packets，
    // 否则迭代器会从首包读到文件末尾。
    if (!final) return null;
    const first = await sink.getKeyPacket(start + origin) || await sink.getFirstKeyPacket();
    if (!first) return null;
    if (first.sequenceNumber > final.sequenceNumber) return null;
    const actualStart = first.timestamp - origin;
    let actualEnd = actualStart;
    const after = track.isVideoTrack() && !spec.transcode && completeGop
      ? await sink.getNextKeyPacket(final, { metadataOnly: true })
      : await sink.getNextPacket(final, { metadataOnly: true });
    if (spec.transcode) {
      if (track.isAudioTrack()) {
        const writer = new AudioSampleSource({ codec: spec.codec as AudioCodec, bitrate: 160_000 });
        output.addAudioTrack(writer);
        await output.start();
        for await (const sample of new AudioSampleSink(track).samples(start + origin, end + origin)) {
          try {
            signal.throwIfAborted();
            sample.setTimestamp(sample.timestamp - origin);
            actualEnd = Math.max(actualEnd, sample.timestamp + sample.duration);
            await writer.add(sample);
          } finally { sample.close(); }
        }
      } else {
        const writer = new VideoSampleSource({ codec: spec.codec as VideoCodec, bitrate: 4_000_000, keyFrameInterval: 1 });
        output.addVideoTrack(writer);
        await output.start();
        for await (const sample of new VideoSampleSink(track).samples(start + origin, end + origin)) {
          try {
            signal.throwIfAborted();
            sample.setTimestamp(sample.timestamp - origin);
            actualEnd = Math.max(actualEnd, sample.timestamp + sample.duration);
            await writer.add(sample);
          } finally { sample.close(); }
        }
      }
    } else {
      const videoWriter = track.isVideoTrack() ? new EncodedVideoPacketSource(spec.codec as VideoCodec) : null;
      const audioWriter = track.isAudioTrack() ? new EncodedAudioPacketSource(spec.codec as AudioCodec) : null;
      if (videoWriter && track.isVideoTrack()) output.addVideoTrack(videoWriter, { transformationMatrix: await track.getTransformationMatrix() });
      if (audioWriter) output.addAudioTrack(audioWriter);
      const config = await track.getDecoderConfig();
      if (!config) throw new Error("媒体缺少解码配置");
      await output.start();
      // 以解码顺序边界切片，保留 B 帧所依赖的前向参考帧。
      let bytes = 0;
      for await (const packet of sink.packets(first, after || undefined)) {
        signal.throwIfAborted();
        bytes += packet.byteLength;
        if (bytes > 64 * 1024 * 1024) throw new Error("单个媒体窗口超过内存限制");
        const normalized = packet.clone({ timestamp: packet.timestamp - origin });
        actualEnd = Math.max(actualEnd, normalized.timestamp + normalized.duration);
        if (videoWriter) await videoWriter.add(normalized, { decoderConfig: config as VideoDecoderConfig });
        if (audioWriter) await audioWriter.add(normalized, { decoderConfig: config as AudioDecoderConfig });
      }
    }
    signal.throwIfAborted();
    if (actualEnd <= actualStart) return null;
    await output.finalize();
    signal.throwIfAborted();
    if (!output.target.buffer) throw new Error("媒体切片输出为空");
    // 输出库的 MP4 基础 MIME 默认是 video/mp4；纯音频缓冲明确声明 audio/mp4。
    const mime = (await output.getMimeType()).replace(/^video\//, spec.type + "/");
    return { data: new Uint8Array(output.target.buffer), mime, start: actualStart, end: actualEnd, eof: !after };
  } finally {
    input.dispose();
    if (output && output.state !== "finalized" && output.state !== "canceled") await output.cancel();
  }
}
