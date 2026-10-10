import { mediaSourceConstructor, supportsMediaSourceType } from "./mediaSourceSupport";
import type { MediaPreviewClient } from "./mediaPreviewClient";

/** 直接绑定专用播放器。Worker 只在片段已追加后继续，最多每轨一个片段在途。 */
export async function attachMediaPlayback(media: HTMLMediaElement, client: MediaPreviewClient, signal: AbortSignal,
  onError: (error: Error) => void): Promise<void> {
  const Constructor = mediaSourceConstructor();
  if (!Constructor) throw new Error("浏览器不支持流式媒体缓冲");
  const info = await client.inspect(); signal.throwIfAborted();
  const mse = new Constructor();
  const managed = mse as MediaSource & { streaming?: boolean };
  const blob = URL.createObjectURL(mse);
  let token = client.stop(), stopped = false, started = false, ahead = 30, lastEviction = 0;
  let mutations = Promise.resolve();
  const previousRemote = media.disableRemotePlayback;
  if ("streaming" in mse) media.disableRemotePlayback = true;
  const serialize = (work: () => Promise<void>) => { mutations = mutations.then(work, work); return mutations; };
  const fail = (error: unknown) => { if (!stopped && !signal.aborted) onError(error instanceof Error ? error : new Error(String(error))); };
  const cleanup = () => {
    if (stopped) return; stopped = true; client.stop(); clearInterval(timer);
    media.removeEventListener("timeupdate", update); media.removeEventListener("waiting", update);
    media.removeEventListener("ratechange", update); media.removeEventListener("seeking", seek);
    media.removeEventListener("error", nativeError);
    mse.removeEventListener("startstreaming", update); mse.removeEventListener("endstreaming", update);
    media.pause(); media.removeAttribute("src"); media.load();
    media.disableRemotePlayback = previousRemote; URL.revokeObjectURL(blob);
  };
  let update = () => {}, seek = () => {};
  const nativeError = () => { if (media.error) fail(new Error(media.error.message || "媒体解码失败")); };
  const timer = setInterval(() => update(), 1000);
  signal.addEventListener("abort", cleanup, { once: true });
  try {
    const opened = waitEvent(mse, "sourceopen", signal);
    media.src = blob; media.load(); await opened;
    mse.duration = info.duration ?? Infinity;
    const buffers = info.tracks.map(spec => { const buffer = mse.addSourceBuffer(spec.mime); buffer.mode = "segments"; return buffer; });
    const types = info.tracks.map(spec => spec.mime);
    const keyframes: number[][] = info.tracks.map(() => []);
    const safeCutoff = (track: number, desired: number) => {
      if (info.tracks[track].type !== "video") return Math.max(0, desired);
      // remove 会延伸到下一个随机访问点，必须保留当前播放所依赖的整个 GOP。
      const keys = keyframes[track].filter(time => time <= desired);
      return Math.max(0, (keys.length ? keys[keys.length - 1] : 0) - 0.001);
    };
    const horizon = () => media.currentTime + (managed.streaming === false && started ? 0 : ahead * Math.max(1, Math.min(4, Math.abs(media.playbackRate))));
    const remove = async (buffer: SourceBuffer, from: number, to: number) => {
      if (to <= from || !buffer.buffered.length) return;
      await waitEvent(buffer, "updateend", signal, () => buffer.remove(from, to));
    };
    const contains = (time: number) => {
      for (let i = 0; i < media.buffered.length; i++) if (media.buffered.start(i) <= time && media.buffered.end(i) > time + 0.1) return true;
      return false;
    };
    client.listen((track, bytes, mime, generation, keys = []) => serialize(async () => {
      signal.throwIfAborted();
      if (generation !== token) throw new DOMException("已取消", "AbortError");
      const buffer = buffers[track];
      if (mime !== types[track]) {
        if (!supportsMediaSourceType(mime) || !buffer.changeType) throw new Error("浏览器不支持转换后的媒体编码");
        buffer.changeType(mime); types[track] = mime;
      }
      try { buffer.appendBuffer(bytes); }
      catch (error) {
        if (!(error instanceof DOMException) || error.name !== "QuotaExceededError") throw error;
        ahead = 10; client.demand(horizon());
        // 不删除尚未消费的前向片段，否则连续游标不会再次生成被删除的数据。
        for (let i = 0; i < buffers.length; i++) await remove(buffers[i], 0, safeCutoff(i, media.currentTime - 8));
        signal.throwIfAborted(); if (generation !== token) throw new DOMException("已取消", "AbortError");
        buffer.appendBuffer(bytes);
      }
      await waitEvent(buffer, "updateend", signal);
      keyframes[track].push(...keys);
      started = true;
    }), (end, generation) => {
      void serialize(async () => {
        signal.throwIfAborted(); if (generation !== token || mse.readyState !== "open") return;
        if (end > 0) mse.duration = end;
        mse.endOfStream();
      }).catch(fail);
    }, fail);
    seek = () => {
      if (contains(media.currentTime)) { update(); return; }
      const time = media.currentTime;
      const generation = token = client.stop();
      void serialize(async () => {
        signal.throwIfAborted(); if (generation !== token) return;
        // 排队中的旧追加已完成，之后才能清除缓冲并启动新代际。
        for (const buffer of buffers) {
          if (buffer.updating) await waitEvent(buffer, "updateend", signal);
          if (buffer.buffered.length) await remove(buffer, 0, buffer.buffered.end(buffer.buffered.length - 1));
        }
        if (generation === token) { for (const keys of keyframes) keys.length = 0; client.start(time, horizon()); }
      }).catch(fail);
    };
    update = () => {
      if (stopped || signal.aborted) return;
      client.demand(horizon());
      const time = media.currentTime;
      if (time - lastEviction < 10) return; lastEviction = time;
      const generation = token;
      void serialize(async () => {
        signal.throwIfAborted(); if (generation !== token) return;
        for (let i = 0; i < buffers.length; i++) {
          const cutoff = safeCutoff(i, time - 20);
          await remove(buffers[i], 0, cutoff);
          keyframes[i] = keyframes[i].filter(key => key >= cutoff);
        }
      }).catch(fail);
    };
    media.addEventListener("timeupdate", update); media.addEventListener("waiting", update);
    media.addEventListener("ratechange", update); media.addEventListener("seeking", seek);
    media.addEventListener("error", nativeError);
    mse.addEventListener("startstreaming", update); mse.addEventListener("endstreaming", update);
    client.start(media.currentTime || 0, horizon());
    if (!info.duration) media.addEventListener("loadeddata", () => {
      void client.duration(info.origin).then(duration => {
        if (duration && !signal.aborted) void serialize(async () => { signal.throwIfAborted(); if (mse.readyState === "open") mse.duration = duration; }).catch(fail);
      }).catch(() => {});
    }, { once: true, signal });
  } catch (error) { cleanup(); signal.removeEventListener("abort", cleanup); throw error; }
}

function waitEvent(target: EventTarget, event: string, signal: AbortSignal, action?: () => void): Promise<void> {
  signal.throwIfAborted();
  return new Promise((resolve, reject) => {
    const clean = () => { clearTimeout(timer); target.removeEventListener(event, success); target.removeEventListener("error", failure); signal.removeEventListener("abort", abort); };
    const success = () => { clean(); resolve(); };
    const failure = () => { clean(); reject(new Error("媒体缓冲初始化或追加失败")); };
    const abort = () => { clean(); reject(signal.reason); };
    const timer = setTimeout(failure, 15_000);
    target.addEventListener(event, success, { once: true }); target.addEventListener("error", failure, { once: true });
    signal.addEventListener("abort", abort, { once: true });
    try { action?.(); } catch (error) { clean(); reject(error); }
  });
}
