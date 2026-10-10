export interface CipherPart { url: string; offset: number; size: number }

/** 在逻辑密文区间上校验连续覆盖，并转换为每卷内的物理区间。 */
export async function readCipherParts(
  parts: readonly CipherPart[], start: number, endInclusive: number,
  read: (url: string, start: number, end: number, signal?: AbortSignal) => Promise<Uint8Array>,
  signal?: AbortSignal,
): Promise<Uint8Array> {
  signal?.throwIfAborted();
  const end = endInclusive + 1;
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end <= start) throw new Error("密文区间不正确");
  const ordered = [...parts].sort((a, b) => a.offset - b.offset);
  let cursor = start;
  const ranges: { url: string; start: number; end: number; offset: number }[] = [];
  for (const part of ordered) {
    if (!part.url || !Number.isSafeInteger(part.offset) || part.offset < 0 || !Number.isSafeInteger(part.size)
      || part.size <= 0 || !Number.isSafeInteger(part.offset + part.size)) throw new Error("分卷清单不正确");
    if (part.offset >= end || part.offset + part.size <= start) continue;
    const from = Math.max(start, part.offset), to = Math.min(end, part.offset + part.size);
    if (from !== cursor) throw new Error("分卷清单存在缺口或重叠");
    ranges.push({ url: part.url, start: from - part.offset, end: to - part.offset - 1, offset: from - start });
    cursor = to;
  }
  if (cursor !== end) throw new Error("分卷清单无法覆盖所请求的密文区间");
  const controller = new AbortController();
  const combined = signal ? AbortSignal.any([signal, controller.signal]) : controller.signal;
  const output = new Uint8Array(end - start);
  let index = 0;
  const workers = Array.from({ length: Math.min(4, ranges.length) }, async () => {
    while (index < ranges.length) {
      combined.throwIfAborted();
      const range = ranges[index++];
      const bytes = await read(range.url, range.start, range.end, combined);
      combined.throwIfAborted();
      if (bytes.byteLength !== range.end - range.start + 1) throw new Error("分卷响应长度与请求区间不一致");
      output.set(bytes, range.offset);
    }
  });
  try { await Promise.all(workers); }
  catch (error) { controller.abort(); await Promise.allSettled(workers); throw error; }
  return output;
}
