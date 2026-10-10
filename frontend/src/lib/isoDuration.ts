// 从 ISO BMFF（MP4/MOV）字节流中解析容器时长。
//
// 为什么单独成文件：fMP4 把 duration 留在 mvhd 写 0，真实时长只在 moof 的
// tfdt 里，所以"读头部拿时长"对这类文件必然失败。解析需要按轨道分别取
// timescale，混在 mediaRemux 里会把那已经很长的函数继续撑大。

const MAX_BOX_SIZE = 64 * 1024 * 1024;

/** 从 moof 里解析出的单轨时间信息（尚未除以 timescale）。 */
export interface TrackTiming {
  /** 文件内唯一的轨道标识。 */
  trackId: number;
  /** 最后一个 tfdt 的解码时间（以该轨 timescale 为单位）。 */
  baseTime: number;
  /** 最后一个 trun 里各样本时长之和（以该轨 timescale 为单位）。 */
  sampleDuration: number;
}

/** 轨道在 moov 中的声明信息。 */
export interface TrackDeclaration {
  trackId: number;
  timescale: number;
}

function readType(view: DataView, offset: number): string | null {
  if (offset + 4 > view.byteLength) return null;
  return String.fromCharCode(
    view.getUint8(offset), view.getUint8(offset + 1),
    view.getUint8(offset + 2), view.getUint8(offset + 3),
  );
}

/**
 * 从 moov 里读出每条轨的 timescale 与 trackId。
 *
 * timescale 存在 mdhd 里，而 tfdt 的计数以各自轨道的时间基为单位——两条轨
 * 通常不同（本项目实测视频 30000、音频 44100），混用会算出完全错误的时长。
 */
export function parseMoovTimings(moov: Uint8Array): TrackDeclaration[] {
  // 传入的 moov 切片含 moov 自身的头，从它的内容起点开始遍历子 box。
  const view = new DataView(moov.buffer, moov.byteOffset, moov.byteLength);
  const tracks: TrackDeclaration[] = [];

  const walk = (start: number, end: number, onBox: (type: string, from: number, to: number) => void): void => {
    let offset = start;
    while (offset + 8 <= end) {
      const size = view.getUint32(offset);
      const type = readType(view, offset + 4);
      let header = 8;
      let actual = size;
      if (size === 1) {
        if (offset + 16 > end) return;
        actual = Number(view.getBigUint64(offset + 8));
        header = 16;
      } else if (size === 0) {
        actual = end - offset; // 延伸到父 box 末尾
      }
      if (actual < header || offset + actual > end) return;
      onBox(type!, offset + header, offset + actual);
      offset += actual;
    }
  };

  // moov 自身的头是 8 字节（size + type），子 box 从偏移 8 开始。
  walk(8, moov.byteLength, (type, from, to) => {
    if (type !== "trak") return;
    let trackId = 0, timescale = 0;
    // walk 回调的 from 已经是 box 内容起点（size+type 之后），不要重复偏移。
    walk(from, to, (inner, innerFrom) => {
      if (inner === "tkhd") {
        const version = view.getUint8(innerFrom);
        // FullBox 头 version(1)+flags(3)=4，随后创建时间与修改时间各 4(v0)/8(v1)，
        // 再往后 track_ID 才是 4 字节。v0 合计 4+4+4=12。
        const base = innerFrom + 4 + (version === 1 ? 8 : 4) * 2;
        if (base + 4 <= to) trackId = view.getUint32(base);
      } else if (inner === "mdia") {
        walk(innerFrom, to, (leaf, leafFrom) => {
          if (leaf !== "mdhd") return;
          const version = view.getUint8(leafFrom);
          // version(1)+flags(3)+创建(4|8)+修改(4|8)，之后才是 timescale。v0 合计 4+4+4=12。
          const base = leafFrom + 4 + (version === 1 ? 8 : 4) * 2;
          if (base + 4 <= to) timescale = view.getUint32(base);
        });
      }
    });
    // timescale 必须非零，否则后面换算会除零；track_ID 0 在规范里是合法值，
    // 所以判据只能落在 timescale 上，不能顺带把 trackId 一起排掉。
    if (timescale) tracks.push({ trackId, timescale });
  });

  return tracks;
}

/**
 * 扫描尾部窗口，找出最后一个 moof 并取出各轨的 tfdt/trun。
 *
 * 窗口起点通常落在某个 mdat 的载荷中间，所以不能顺序解析——必须逐字节
 * 试探 box 边界，并用"类型合法 + 尺寸合理 + 内部结构自洽"三重校验排掉
 * 载荷里恰好出现 moof 字样的巧合。
 */
export function findLastMoof(window: Uint8Array): Map<number, TrackTiming> {
  const view = new DataView(window.buffer, window.byteOffset, window.byteLength);
  const best = new Map<number, Omit<TrackTiming, "trackId">>();

  for (let offset = 0; offset + 8 <= window.byteLength; offset++) {
    if (readType(view, offset + 4) !== "moof") continue;
    const size = view.getUint32(offset);
    if (size < 16 || size > MAX_BOX_SIZE || offset + size > window.byteLength) continue;
    // moof 的第一个子 box 必须是 mfhd 或 traf，否则多半是载荷里的巧合。
    // 子 box 的 type 在它自身 size(4) 之后，即 offset+8 是 size、offset+12 是 type。
    const first = readType(view, offset + 12);
    if (first !== "mfhd" && first !== "traf") continue;

    const timing = parseMoof(view, offset, offset + size);
    for (const [trackId, value] of timing) {
      const previous = best.get(trackId);
      // 严格取时间最大的那个。窗口内的 moof 通常按时间递增排列，但不必依赖这一点：
      // 载荷里的假阳性可能乱序出现，用 >= 会让它们覆盖掉真正的末尾片段。
      if (!previous || value.baseTime > previous.baseTime) best.set(trackId, value);
    }
  }

  const merged: Array<[number, TrackTiming]> = [];
  for (const [id, value] of best) merged.push([id, { ...value, trackId: id }]);
  return new Map(merged);
}

/** 解析单个 moof，返回 trackId -> tfdt/trun。 */
function parseMoof(view: DataView, start: number, end: number): Map<number, Omit<TrackTiming, "trackId">> {
  const result = new Map<number, { baseTime: number; sampleDuration: number }>();

  const walk = (from: number, to: number, onBox: (type: string, from: number, to: number) => void): void => {
    let offset = from;
    while (offset + 8 <= to) {
      const size = view.getUint32(offset);
      const type = readType(view, offset + 4);
      let header = 8, actual = size;
      if (size === 1) {
        if (offset + 16 > to) return;
        actual = Number(view.getBigUint64(offset + 8));
        header = 16;
      } else if (size === 0) {
        actual = to - offset;
      }
      if (actual < header || offset + actual > to) return;
      onBox(type!, offset + header, offset + actual);
      offset += actual;
    }
  };

  walk(start + 8, end, (type, from, to) => {
    if (type !== "traf") return;
    let trackId = 0, baseTime: number | null = null, sampleDuration = 0;
    // to 是 traf 自己的末尾，子 box 必须在这个范围内遍历——用整个 moof 的 end 会
    // 让最后一条 traf 把下一条 traf 的字段也读进来。
    walk(from, to, (inner, innerFrom, innerTo) => {
      if (inner === "tfhd") {
        // FullBox 头 4 字节之后是 track_ID。
        const boxStart = innerFrom - 8;
        trackId = view.getUint32(boxStart + 12);
      } else if (inner === "tfdt") {
        const version = view.getUint8(innerFrom);
        baseTime = version === 1
          ? Number(view.getBigUint64(innerFrom + 4))
          : view.getUint32(innerFrom + 4);
      } else if (inner === "trun") {
        sampleDuration += parseTrun(view, innerFrom - 8, innerTo);
      }
    });
    if (trackId && baseTime !== null) result.set(trackId, { baseTime, sampleDuration });
  });

  return result;
}

/** 累加 trun 里各样本的时长。data_offset / first_sample_flags 会在偏移上前置。 */
function parseTrun(view: DataView, start: number, end: number): number {
  const flags = view.getUint32(start + 8) & 0xffffff;
  const count = view.getUint32(start + 12);
  if (!count || count > 1 << 20) return 0;
  let offset = start + 16;
  if (flags & 0x1) offset += 4;  // data_offset
  if (flags & 0x4) offset += 4;  // first_sample_flags
  let total = 0;
  for (let i = 0; i < count; i++) {
    if (offset + 4 > end) break;
    if (flags & 0x100) { total += view.getUint32(offset); offset += 4; }
    if (flags & 0x200) offset += 4;  // sample_flags
    if (flags & 0x400) offset += 4;  // sample_composition_time_offset
    if (flags & 0x800) offset += 4;  // sample_degradation_priority
  }
  return total;
}

/**
 * 合并 moov 的 timescale 与尾部 moof 的 tfdt，算出容器总时长（秒）。
 *
 * 返回 null 表示尾部窗口不足以判定（例如最后一个 moof 在窗口之外），
 * 调用方应据此放弃尾部探测，而不是回退到某个猜测值——错误的时长比没有
 * 时长危害大得多：播放器会显示"3 分 20 秒"然后提前结束。
 */
export function durationFromTail(moov: Uint8Array, window: Uint8Array): number | null {
  const declarations = new Map(parseMoovTimings(moov).map(t => [t.trackId, t.timescale]));
  const timings = findLastMoof(window);
  if (!timings.size) return null;

  let best: number | null = null;
  for (const [trackId, timing] of timings) {
    const timescale = declarations.get(trackId);
    // timescale 缺失就无从换算；这条轨的时长只能作废。
    if (!timescale) continue;
    const end = (timing.baseTime + timing.sampleDuration) / timescale;
    if (!Number.isFinite(end) || end <= 0) continue;
    // 取各轨的最大值：容器时长由最长的那条轨决定。
    if (best === null || end > best) best = end;
  }
  return best;
}
