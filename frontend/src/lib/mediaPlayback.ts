import { mediaSourceConstructor, supportsMediaSourceType } from "./mediaSourceSupport";
import type { MediaPreviewClient } from "./mediaPreviewClient";
import { MediaBufferController } from "./mediaBufferController";
import type { PlaybackStateMachine } from "./playbackStateMachine";

// 本文件只负责 MSE / SourceBuffer / 播放事件与媒体缓冲。"该缓冲多少秒"的策略
// 在 MediaBufferController 里，页面状态机在 MediaPreview.vue 里——都不在这里。

/**
 * 直接绑定专用播放器。Worker 只在片段已追加后继续，最多每轨一个片段在途。
 *
 * `machine` 是可选的：传入时播放器事件会驱动状态机，上层据此决定重建还是保留。
 * 不传则只做播放，行为与从前一致——状态机是观察者，不改变播放语义。
 */
export async function attachMediaPlayback(media: HTMLMediaElement, client: MediaPreviewClient, signal: AbortSignal,
  onError: (error: Error) => void, machine?: PlaybackStateMachine): Promise<void> {
  const Constructor = mediaSourceConstructor();
  if (!Constructor) throw new Error("浏览器不支持流式媒体缓冲");
  /** 缓冲目标由 MediaBufferController 统一决策，这里只读它的结论。 */
  const bufferControl = new MediaBufferController();
  // 吞吐是健康度 R 的分子，必须在首次 retune 之前接上，否则策略只能靠中性先验。
  // 取数在 worker 线程，这里收到的是它回传的"已认证交付字节"。
  // 可选调用：契约之外的实现（测试替身、旧版 worker）没有这个方法时只是拿不到
  // 吞吐，退回中性先验继续跑，不该因此整个预览起不来。
  client.onDelivered?.((bytes, at) => bufferControl.noteDelivered(bytes, at));
  const info = await client.inspect(); signal.throwIfAborted();
  // 媒体消费速率的先验：多轨道码率求和。拿不到时控制器按保守值起算。
  bufferControl.setBitrate(info.bitrates ?? []);
  const mse = new Constructor();
  const managed = mse as MediaSource & { streaming?: boolean };
  const blob = URL.createObjectURL(mse);
  let token = client.stop(), stopped = false, started = false, lastEviction = 0;
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
    const horizon = () => media.currentTime + (managed.streaming === false && started ? 0 : bufferControl.target * bufferControl.scaleByRate(media.playbackRate));
    /**
     * 把播放状态打包成一条指令推给 Worker。
     *
     * 一次带齐代际、缓冲目标、暂停与预测开关：worker 拿到的是自洽的一组参数，
     * 而不是可能互相矛盾的多次更新（先说"缓冲 30 秒"再说"暂停"这类）。
     */
    const directive = () => ({
      generation: token, currentTime: media.currentTime, horizon: horizon(),
      predict: bufferControl.predictEnabled,
    });
    /**
     * 把当前播放状态交给控制器重新计算缓冲目标，再把结果推给读取调度。
     *
     * 这一步是"缓冲 ↔ 调度"的接缝：控制器算出该留多少秒与要不要预测，
     * scheduler.updatePolicy 负责落实到并发与预取视野。
     */
    const retune = () => {
      bufferControl.retune(media.buffered, {
        started, paused: media.paused, playbackRate: media.playbackRate, currentTime: media.currentTime,
      }, performance.now());
      // 可选调用：没有它只是缓冲与调度脱钩，退回固定预取深度继续播。
      client.updatePolicy?.(bufferControl.policy());
    };
    const remove = async (buffer: SourceBuffer, from: number, to: number) => {
      if (to <= from || !buffer.buffered.length) return;
      await waitEvent(buffer, "updateend", signal, () => buffer.remove(from, to));
    };
    const contains = (time: number) => {
      for (let i = 0; i < media.buffered.length; i++) if (media.buffered.start(i) <= time && media.buffered.end(i) > time + 0.1) return true;
      return false;
    };
    client.listen((track, bytes, mime, generation, keys = [], mediaSeconds = 0) => {
      bufferControl.noteMediaRate(track, bytes.byteLength, mediaSeconds);
      return serialize(async () => {
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
        // 配额异常即内存压力：目标压到最小，后向也只留回退必需的量，再试一次追加。
        bufferControl.applyMemoryPressure(); client.directive(directive());
        // 不删除尚未消费的前向片段，否则连续游标不会再次生成被删除的数据。
        for (let i = 0; i < buffers.length; i++) await remove(buffers[i], 0, safeCutoff(i, media.currentTime - bufferControl.retention));
        signal.throwIfAborted(); if (generation !== token) throw new DOMException("已取消", "AbortError");
        buffer.appendBuffer(bytes);
      }
      await waitEvent(buffer, "updateend", signal);
      keyframes[track].push(...keys);
      started = true;
      // 数据开始进了MediaSource，但还不等于能播——canplay/playing 才推进到 ready。
      machine?.observeEvent("canplay");
      });
    }, (end, generation) => {
      void serialize(async () => {
        signal.throwIfAborted(); if (generation !== token || mse.readyState !== "open") return;
        if (end > 0) mse.duration = end;
        mse.endOfStream();
      }).catch(fail);
    }, fail);
    seek = () => {
      if (contains(media.currentTime)) { update(); return; }
      const time = media.currentTime;
      bufferControl.noteSeek(performance.now());
      const generation = token = client.stop();
      void serialize(async () => {
        signal.throwIfAborted(); if (generation !== token) return;
        // 排队中的旧追加已完成，之后才能清除缓冲并启动新代际。
        for (const buffer of buffers) {
          if (buffer.updating) await waitEvent(buffer, "updateend", signal);
          if (buffer.buffered.length) await remove(buffer, 0, buffer.buffered.end(buffer.buffered.length - 1));
        }
        if (generation === token) { for (const keys of keyframes) keys.length = 0; client.start(time, directive()); }
      }).catch(fail);
    };
    update = () => {
      if (stopped || signal.aborted) return;
      retune();
      client.directive(directive());
      const time = media.currentTime;
      if (time - lastEviction < 10) return; lastEviction = time;
      const generation = token;
      void serialize(async () => {
        signal.throwIfAborted(); if (generation !== token) return;
        for (let i = 0; i < buffers.length; i++) {
          const cutoff = safeCutoff(i, time - bufferControl.retention);
          await remove(buffers[i], 0, cutoff);
          keyframes[i] = keyframes[i].filter(key => key >= cutoff);
        }
      }).catch(fail);
    };
    media.addEventListener("timeupdate", update); media.addEventListener("waiting", update);
    media.addEventListener("ratechange", update); media.addEventListener("seeking", seek);
    media.addEventListener("error", nativeError);
    mse.addEventListener("startstreaming", update); mse.addEventListener("endstreaming", update);
    // 把真实媒体事件喂给状态机。信号一律取自媒体元素本身，不另造判定标准——
    // 自己猜"是不是就绪"迟早会和浏览器的真实行为对不上。
    if (machine) {
      const forward = (name: Parameters<PlaybackStateMachine["observeEvent"]>[0]) => () => machine.observeEvent(name);
      media.addEventListener("loadedmetadata", forward("loadedmetadata"), { signal });
      media.addEventListener("canplay", forward("canplay"), { signal });
      media.addEventListener("playing", forward("playing"), { signal });
      media.addEventListener("waiting", forward("waiting"), { signal });
      media.addEventListener("pause", forward("pause"), { signal });
      media.addEventListener("emptied", forward("emptied"), { signal });
      machine.to("buffering");
    }
    // 在启动解复用之前先压下预取上限。启动时通常还处于暂停且缓冲为空，若等第一
    // 次 timeupdate 才发送策略，Worker 会先按默认窗口并发读取数 MiB。
    retune();
    client.directive(directive());
    client.start(media.currentTime || 0, directive());
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
