import {
  AudioSampleSink, AudioSampleSource, EncodedAudioPacketSource, EncodedPacketSink,
  EncodedVideoPacketSource, InputAudioTrack, InputVideoTrack, Mp4OutputFormat, NullTarget, Output,
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
    const input = this.source.input(AbortSignal.any([this.lifetime, AbortSignal.timeout(8000)]), 2 * 1024 * 1024);
    try { const duration = await input.computeDuration(undefined, { metadataOnly: true }); return Number.isFinite(duration) && duration > origin ? duration - origin : null; }
    catch { return null; } finally { input.dispose(); }
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
