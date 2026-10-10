export const MEDIA_WINDOW_SECONDS = 4;

type SchedulerOptions = {
  signal: AbortSignal;
  duration(): number;
  loaded(index: number): boolean;
  load(index: number, signal: AbortSignal, target: number | null): Promise<void>;
  error(error: unknown): void;
};

/** 目标窗口独占优先级，随后才开放最多四个前后窗口任务。 */
export function createMediaWindowScheduler(options: SchedulerOptions) {
  let controller = new AbortController();
  let epoch = 0;
  let center = -1;
  let running = false;
  let stopped = false;
  const run = async (time: number, seek = false): Promise<void> => {
    if (stopped || options.signal.aborted || !Number.isFinite(time)) return;
    const index = Math.max(0, Math.floor(time / MEDIA_WINDOW_SECONDS));
    if (!seek && running && index === center) return;
    if (seek || index !== center) {
      controller.abort();
      controller = new AbortController();
      epoch++;
    } else if (running) return;
    center = index;
    running = true;
    const token = epoch;
    const signal = AbortSignal.any([options.signal, controller.signal]);
    try {
      if (!options.loaded(index)) await options.load(index, signal, time);
      signal.throwIfAborted();
      const ids = options.loaded(index) ? [] : [index];
      for (let distance = 1; distance <= 4; distance++) {
        if ((index + distance) * MEDIA_WINDOW_SECONDS < options.duration()) ids.push(index + distance);
        if (distance <= 2 && index >= distance) ids.push(index - distance);
      }
      let cursor = 0;
      await Promise.all(Array.from({ length: Math.min(4, ids.length) }, async () => {
        while (cursor < ids.length) {
          signal.throwIfAborted();
          const next = ids[cursor++];
          if (!options.loaded(next)) await options.load(next, signal, null);
        }
      }));
    } catch (error) {
      if (!signal.aborted) {
        controller.abort();
        options.error(error);
      }
    } finally { if (token === epoch) running = false; }
  };
  return { run, stop: () => { stopped = true; running = false; controller.abort(); epoch++; } };
}
