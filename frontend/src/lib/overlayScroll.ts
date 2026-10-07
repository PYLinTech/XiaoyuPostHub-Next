import { computed, onBeforeUnmount, ref, watch, type Ref } from "vue";

// 自绘滚动条。
//
// 系统开启"始终显示滚动条"时，原生条会占掉一条宽度（内容跟着窄十几 px），粗细和
// 配色也由系统决定。这里把原生条藏掉（样式见 app.css 的 .ovscroll），换一条浮在
// 内容之上的滑块：不占布局、滚动时出现、停下后淡出、可以拖。
//
// 内容是原生长条时用它，样式见 app.css 的 .ovscroll / .ovscroll-layer。

/** 滑块最短长度：内容很长时也要留出能抓住的一截。 */
const MIN_SIZE = 32;
/** 停止滚动后多久淡出。 */
const FADE_DELAY = 800;

export interface OverlayScrollOptions {
  /**
   * 滑块能否拖动。默认 true。
   *
   * 有些场合的滑块只是"这里还有多少内容"的指示件：侧栏那条竖向指示条
   * 浮在品牌行之上、宽度不够抓，拖它反而会跟点击冲突。只表达位置就够了。
   */
  draggable?: boolean;
  /** 停止滚动后多久淡出（毫秒）。默认 FADE_DELAY。 */
  fadeDelay?: number;
}

export function useOverlayScroll(
  target: Ref<HTMLElement | null>,
  axis: "x" | "y",
  options: OverlayScrollOptions = {},
) {
  const draggable = options.draggable ?? true;
  const fadeDelay = options.fadeDelay ?? FADE_DELAY;
  const visible = ref(false);
  /** 指针正悬在滚动容器上。悬停期间滑块不淡出——否则它会在用户伸手去抓它时消失。 */
  const hovering = ref(false);
  const size = ref(0);
  const offset = ref(0);

  let fadeTimer = 0;
  let frame = 0;
  let dragging: { from: number; origin: number } | null = null;

  function limits(): { viewport: number; content: number; pos: number } | null {
    const el = target.value;
    if (!el) {
      return null;
    }
    if (axis === "x") {
      return { viewport: el.clientWidth, content: el.scrollWidth, pos: el.scrollLeft };
    }
    return { viewport: el.clientHeight, content: el.scrollHeight, pos: el.scrollTop };
  }

  function measure(): void {
    const limit = limits();
    if (!limit || limit.content <= limit.viewport + 1) {
      size.value = 0;
      visible.value = false;
      return;
    }
    const length = Math.max(MIN_SIZE, (limit.viewport * limit.viewport) / limit.content);
    const travel = limit.viewport - length;
    const scrollable = limit.content - limit.viewport;
    size.value = length;
    offset.value = scrollable > 0 ? (limit.pos / scrollable) * travel : 0;
  }

  function schedule(): void {
    if (frame) {
      return;
    }
    frame = requestAnimationFrame(() => {
      frame = 0;
      measure();
    });
  }

  /** 滚动、拖动时亮一下，随后淡出。 */
  function flash(): void {
    measure();
    if (size.value === 0) {
      return;
    }
    visible.value = true;
    window.clearTimeout(fadeTimer);
    fadeTimer = window.setTimeout(() => {
      if (!dragging && !hovering.value) {
        visible.value = false;
      }
    }, fadeDelay);
  }

  const style = computed<Record<string, string>>((): Record<string, string> =>
    axis === "x"
      ? { width: `${size.value}px`, transform: `translateX(${offset.value}px)` }
      : { height: `${size.value}px`, transform: `translateY(${offset.value}px)` },
  );

  function onThumbPointerDown(event: PointerEvent): void {
    const el = target.value;
    if (!el || !draggable) {
      return;
    }
    event.preventDefault();
    (event.currentTarget as HTMLElement).setPointerCapture(event.pointerId);
    dragging = { from: axis === "x" ? event.clientX : event.clientY, origin: axis === "x" ? el.scrollLeft : el.scrollTop };
    window.clearTimeout(fadeTimer);
    visible.value = true;
  }

  function onThumbPointerMove(event: PointerEvent): void {
    const el = target.value;
    const limit = limits();
    if (!dragging || !el || !limit) {
      return;
    }
    const travel = limit.viewport - size.value;
    if (travel <= 0) {
      return;
    }
    const delta = (axis === "x" ? event.clientX : event.clientY) - dragging.from;
    const next = dragging.origin + (delta * (limit.content - limit.viewport)) / travel;
    if (axis === "x") {
      el.scrollLeft = next;
    } else {
      el.scrollTop = next;
    }
    measure();
  }

  function onThumbPointerUp(event: PointerEvent): void {
    if (!dragging) {
      return;
    }
    dragging = null;
    (event.currentTarget as HTMLElement).releasePointerCapture(event.pointerId);
    flash();
  }

  let stop: (() => void) | null = null;

  // 盯住引用而不是在 onMounted 里接一次：目标常常挂在 v-if 下（外壳要等会话
  // 就绪才渲染），挂载那一刻还是 null，一锤子买卖会永远接不上。
  watch(
    target,
    (el) => {
      stop?.();
      stop = null;
      // 目标没了（比如切到整屏版面）：状态复位，免得旧滑块停在那儿。
      size.value = 0;
      visible.value = false;
      hovering.value = false;
      if (!el) {
        return;
      }
      el.addEventListener("scroll", flash, { passive: true });
      // 悬停期间不让滑块淡出：它淡出的时机正好是用户伸手去抓它的时候。
      const onEnter = (): void => {
        hovering.value = true;
        measure();
        if (size.value > 0) {
          visible.value = true;
        }
      };
      const onLeave = (): void => {
        hovering.value = false;
        flash();
      };
      el.addEventListener("pointerenter", onEnter, { passive: true });
      el.addEventListener("pointerleave", onLeave, { passive: true });
      const resize = new ResizeObserver(schedule);
      resize.observe(el);
      // 内容长度变化（翻页、切路由）既不改容器尺寸也不发 scroll，得单独盯着。
      const content = new MutationObserver(schedule);
      content.observe(el, { childList: true, subtree: true });
      measure();
      stop = () => {
        el.removeEventListener("scroll", flash);
        el.removeEventListener("pointerenter", onEnter);
        el.removeEventListener("pointerleave", onLeave);
        resize.disconnect();
        content.disconnect();
      };
    },
    { immediate: true, flush: "post" },
  );

  onBeforeUnmount(() => {
    stop?.();
    window.clearTimeout(fadeTimer);
    if (frame) {
      cancelAnimationFrame(frame);
    }
  });

  return { visible, hovering, style, onThumbPointerDown, onThumbPointerMove, onThumbPointerUp };
}
