import { createMediaWindowScheduler, MEDIA_WINDOW_SECONDS } from "./mediaWindowScheduler";
import { mediaSourceConstructor, supportsMediaSourceType } from "./mediaSourceSupport";

function sameUrl(left: string, right: string): boolean {
  try { return new URL(left, location.href).href === new URL(right, location.href).href; }
  catch { return false; }
}

/** 控件由预览组件负责，解复用及按时间取片由适配层负责。 */
export function observeXphMsePreview(root: HTMLElement, sourceUrl: string): () => void {
  if (!mediaSourceConstructor()) return () => {};
  const seen = new WeakSet<HTMLMediaElement>();
  const sessions = new Map<HTMLMediaElement, AbortController>();
  let stopped = false;
  const scan = () => {
    if (stopped) return;
    for (const [media, controller] of sessions) {
      if (!root.contains(media)) { controller.abort(); sessions.delete(media); }
    }
    for (const media of root.querySelectorAll<HTMLMediaElement>("video, audio")) {
      const src = media.currentSrc || media.getAttribute("src") || media.querySelector("source")?.src || "";
      if (seen.has(media) || !sameUrl(src, sourceUrl)) continue;
      seen.add(media);
      const controller = new AbortController();
      sessions.set(media, controller);
      void attach(media, root, sourceUrl, controller).catch(error => {
        if (!controller.signal.aborted) {
          media.dispatchEvent(new CustomEvent("xph-preview-error", { bubbles: true, detail: { url: sourceUrl, message: String(error) } }));
          controller.abort();
        }
      });
    }
  };
  const observer = new MutationObserver(scan);
  observer.observe(root, { childList: true, subtree: true, attributes: true, attributeFilter: ["src"] });
  scan();
  return () => {
    stopped = true;
    observer.disconnect();
    for (const controller of sessions.values()) controller.abort();
    sessions.clear();
  };
}

async function attach(media: HTMLMediaElement, root: HTMLElement, url: string, controller: AbortController): Promise<void> {
  const signal = controller.signal;
  const [{ MediaRangeSource }, { inspectMedia, remuxMediaSlice, probeMediaDuration }] = await Promise.all([
    import("./mediaRangeSource"), import("./mediaRemux"),
  ]);
  signal.throwIfAborted();
  const source = new MediaRangeSource(url);
  const initialTime = media.currentTime || 0;
  const resume = !media.paused;
  let blob = "";
  let timer: ReturnType<typeof setInterval> | undefined;
  let update = () => {};
  let seeking = () => {};
  let stopScheduler = () => {};
  let first = true;
  let attached = false;
  const previousRemotePlayback = media.disableRemotePlayback;
  let managed: (MediaSource & { streaming?: boolean }) | null = null;
  const fail = (error: unknown) => {
    if (signal.aborted) return;
    media.dispatchEvent(new CustomEvent("xph-preview-error", { bubbles: true, detail: { url, message: String(error) } }));
    controller.abort();
  };
  // MSE 的 currentSrc 是 blob URL，须转换回会话 URL 才能触发组件的兼容兜底。
  const mediaFailed = () => { if (attached && media.error) fail(media.error.message || "媒体解码失败"); };
  const markReady = () => root.classList.add("xph-mse-frame-ready");
  const cleanup = () => {
    stopScheduler();
    clearInterval(timer);
    source.clear();
    media.removeEventListener("timeupdate", update);
    media.removeEventListener("waiting", update);
    media.removeEventListener("seeking", seeking);
    media.removeEventListener("loadeddata", markReady);
    media.removeEventListener("error", mediaFailed);
    managed?.removeEventListener("startstreaming", update);
    media.disableRemotePlayback = previousRemotePlayback;
    root.classList.remove("xph-mse-frame-ready");
    if (attached) media.pause();
    if (blob) URL.revokeObjectURL(blob);
  };
  signal.addEventListener("abort", cleanup, { once: true });
  try {
    await source.open(signal);
    const info = await inspectMedia(source, signal);
    signal.throwIfAborted();
    const Constructor = mediaSourceConstructor()!;
    const mse = new Constructor();
    if ("streaming" in mse && typeof mse.streaming === "boolean") {
      managed = mse as MediaSource & { streaming: boolean };
      media.disableRemotePlayback = true;
    }
    blob = URL.createObjectURL(mse);
    media.pause();
    media.addEventListener("error", mediaFailed);
    attached = true;
    media.src = blob;
    media.load();
    await waitEvent(mse, "sourceopen", signal);
    let duration = info.duration || Infinity;
    mse.duration = duration;
    // 无索引裸流在精确时长补齐前也允许按时间跳转，避免 Infinity 时被限制在已缓冲区。
    if (!Number.isFinite(duration) && "setLiveSeekableRange" in mse) mse.setLiveSeekableRange(0, 365 * 86400);
    // 所有轨道须在第一个 init segment 之前创建；Chromium 初始化后拒绝增加轨道。
    const buffers = info.tracks.map(spec => {
      const buffer = mse.addSourceBuffer(spec.mime);
      buffer.mode = "segments";
      return buffer;
    });
    const bufferTypes = info.tracks.map(spec => spec.mime);
    let mutations = Promise.resolve();
    const completed = new Set<number>();
    const trackEnds = new Map<number, number>();
    let lastEviction = 0;
    const serialize = (work: () => Promise<void>) => {
      mutations = mutations.then(work, work);
      return mutations;
    };
    const append = (track: number, bytes: Uint8Array<ArrayBuffer>, mime: string, requestSignal: AbortSignal) => serialize(async () => {
      requestSignal.throwIfAborted();
      if (!supportsMediaSourceType(mime)) throw new Error("浏览器不支持转换后的媒体编码");
      if (bufferTypes[track] !== mime && "changeType" in buffers[track]) {
        buffers[track].changeType(mime);
        bufferTypes[track] = mime;
      }
      buffers[track].appendBuffer(bytes);
      await waitEvent(buffers[track], "updateend", signal);
    });
    const scheduler = createMediaWindowScheduler({
      signal, duration: () => duration,
      loaded: index => completed.has(index) && buffers.every((buffer, track) => {
        const start = index * MEDIA_WINDOW_SECONDS;
        const end = Math.min(start + MEDIA_WINDOW_SECONDS, duration, trackEnds.get(track) ?? Infinity);
        if (end <= start + 0.15) return true;
        for (let i = 0; i < buffer.buffered.length; i++) {
          if (buffer.buffered.start(i) <= start + 0.15 && buffer.buffered.end(i) >= end - 0.15) return true;
        }
        return false;
      }),
      async load(index, requestSignal, target) {
        const start = index * MEDIA_WINDOW_SECONDS;
        if (start >= duration) return;
        const fullEnd = Math.min(start + MEDIA_WINDOW_SECONDS, duration);
        const end = target === null ? fullEnd : Math.min(fullEnd, Math.max(start + 1, target + 1));
        await Promise.all(info.tracks.map(async (spec, track) => {
          const slice = await remuxMediaSlice(source, spec, start, end, info.origin, requestSignal, target === null);
          requestSignal.throwIfAborted();
          if (slice) await append(track, slice.data, slice.mime, requestSignal);
          if (slice?.eof) {
            trackEnds.set(track, slice.end);
          }
        }));
        requestSignal.throwIfAborted();
        if (end === fullEnd) completed.add(index);
        if (first) {
          first = false;
          if (initialTime > 0 && media.currentTime !== initialTime) media.currentTime = initialTime;
          if (resume) void media.play().catch(() => {});
          if (!info.duration) void probeMediaDuration(source, info.origin, signal).then(value => {
            if (value && !signal.aborted) {
              duration = value;
              void serialize(async () => { signal.throwIfAborted(); mse.duration = value; }).catch(() => {});
            }
          });
        }
        if (!Number.isFinite(duration) && trackEnds.size === info.tracks.length) {
          duration = Math.max(...trackEnds.values());
          if (duration > 0) await serialize(async () => { signal.throwIfAborted(); mse.duration = duration; });
        }
      },
      error: fail,
    });
    stopScheduler = scheduler.stop;
    let evictionEpoch = 0;
    const evict = (time: number, includeFuture = false) => {
      const epoch = ++evictionEpoch;
      const cutoff = Math.floor(Math.max(0, time - 30) / MEDIA_WINDOW_SECONDS) * MEDIA_WINDOW_SECONDS;
      const future = Math.ceil((time + 30) / MEDIA_WINDOW_SECONDS) * MEDIA_WINDOW_SECONDS;
      for (const buffer of buffers) {
        void serialize(async () => {
          signal.throwIfAborted();
          // 连续跳转时，排队中的旧清理不能删除最新目标附近的缓冲。
          if (epoch !== evictionEpoch) return;
          if (!buffer.buffered.length) return;
          if (cutoff > buffer.buffered.start(0)) {
            buffer.remove(0, cutoff);
            await waitEvent(buffer, "updateend", signal);
          }
          const last = buffer.buffered.length ? buffer.buffered.end(buffer.buffered.length - 1) : 0;
          if (epoch === evictionEpoch && includeFuture && last > future) {
            buffer.remove(future, last);
            await waitEvent(buffer, "updateend", signal);
          }
        }).catch(() => {});
      }
      for (const index of completed) {
        if ((index + 1) * MEDIA_WINDOW_SECONDS <= cutoff || (includeFuture && index * MEDIA_WINDOW_SECONDS >= future)) completed.delete(index);
      }
    };
    seeking = () => {
      // 在目标片追加前释放远处缓冲，连续跳转不会积累整份视频。
      evict(media.currentTime, true);
      void scheduler.run(media.currentTime, true);
    };
    update = () => {
      if (managed?.streaming === false && !first) return;
      void scheduler.run(media.currentTime);
      if (Number.isFinite(duration) && media.currentTime >= duration - MEDIA_WINDOW_SECONDS && trackEnds.size === info.tracks.length) {
        void serialize(async () => {
          signal.throwIfAborted();
          // 轨道曾到达末尾并不代表末尾仍在缓冲中，跳转和驱逐可能已将其移除。
          const hasTail = buffers.every((buffer, track) => {
            const end = trackEnds.get(track);
            if (end === undefined) return false;
            for (let i = 0; i < buffer.buffered.length; i++) {
              if (buffer.buffered.start(i) <= Math.max(0, end - 0.15) && buffer.buffered.end(i) >= end - 0.15) return true;
            }
            return false;
          });
          if (mse.readyState === "open" && hasTail) mse.endOfStream();
        }).catch(() => {});
      }
      if (media.currentTime - lastEviction < 30) return;
      lastEviction = media.currentTime;
      evict(media.currentTime);
    };
    media.addEventListener("seeking", seeking);
    media.addEventListener("timeupdate", update);
    media.addEventListener("waiting", update);
    media.addEventListener("loadeddata", markReady);
    managed?.addEventListener("startstreaming", update);
    timer = setInterval(update, 1000);
    await scheduler.run(initialTime, true);
  } catch (error) {
    cleanup();
    signal.removeEventListener("abort", cleanup);
    throw error;
  }
}

function waitEvent(target: EventTarget, event: string, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted();
  return new Promise((resolve, reject) => {
    const clean = () => {
      clearTimeout(timer);
      target.removeEventListener(event, success);
      target.removeEventListener("error", failure);
      signal.removeEventListener("abort", aborted);
    };
    const success = () => { clean(); resolve(); };
    const failure = () => { clean(); reject(new Error("媒体缓冲初始化或追加失败")); };
    const aborted = () => { clean(); reject(signal.reason); };
    const timer = setTimeout(failure, 15_000);
    target.addEventListener(event, success, { once: true });
    target.addEventListener("error", failure, { once: true });
    signal.addEventListener("abort", aborted, { once: true });
  });
}
