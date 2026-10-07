import { onBeforeUnmount, onMounted, shallowRef, watch, type Ref } from "vue";

// 锚点浮层：触发器 + 浮层面板（下拉菜单、日期时间面板……共用一套开合与定位）。
//
// 面板都用 position: fixed 贴着触发器算位置，好处是躲得开表格卡片的裁剪、模态框的
// 边框和 z-index 层级的干扰；代价是位置不再由文档流决定，锚点一动就得自己重算，
// 开合、滚动、缩放都得有人管。规则收在这里，各组件只留自己的内容：选项怎么渲染、
// 选中后发什么、打开时把列表滚到哪一项。
//
// 层级：面板高度得自己写 z-index，fixed 会脱离父级层级。站内模态框 80、提示气泡 90、
// toast 100，浮层取 85——开在模态框正文里的下拉、日历要盖过遮罩，又不能盖过 toast
// 把提示抢掉。

export interface PopoverOptions {
  /** 浮层与触发器的间距。下拉贴得近（4），日历面板大一圈、离得远些（6）。 */
  gap?: number;
  /** 视口四边的最小留白：翻转后仍放不下时的兜底，也是水平方向夹取的范围。 */
  margin?: number;
  /** 浮层是否至少和触发器一样宽。下拉要 true（否则窄触发器会把菜单挤窄），
      定宽面板传 false——表单里的触发器常是整列宽，跟着走会把月历撑得巨大。 */
  matchAnchorWidth?: boolean;
}

/** 默认间距：4px 贴着但不显缝。 */
const GAP = 4;
/** 默认视口留白。 */
const MARGIN = 8;

export function usePopover(
  anchor: Ref<HTMLElement | null>,
  panel: Ref<HTMLElement | null>,
  options: PopoverOptions = {},
): {
  /** 打开态。调用方可以直接写 false（如选中后关闭）。 */
  open: Ref<boolean>;
  /** 绑在浮层面板上的 style：position/top/left/（必要时）min-width。 */
  style: Ref<Record<string, string>>;
  show: () => void;
  hide: () => void;
  toggle: () => void;
} {
  const { gap = GAP, margin = MARGIN, matchAnchorWidth = true } = options;

  // 定位结果整份换掉、从不原地改字段，用浅层 ref 省掉每次重算都套一层代理。
  const open = shallowRef(false);
  const style = shallowRef<Record<string, string>>({});

  /** 固定定位：量的是宿主的视口坐标，下方放不下就翻到上方，水平方向夹在视口内。 */
  function place(): void {
    const trigger = anchor.value;
    const layer = panel.value;
    if (!trigger || !layer) {
      return;
    }
    const rect = trigger.getBoundingClientRect();
    const width = matchAnchorWidth ? Math.max(layer.offsetWidth, rect.width) : layer.offsetWidth;
    let top = rect.bottom + gap;
    if (top + layer.offsetHeight > window.innerHeight - margin) {
      top = rect.top - layer.offsetHeight - gap;
    }
    // 翻上去的高度为负说明浮层比上下空隙还大，此时宁可压边也要保住触发器可见。
    top = Math.max(margin, top);
    const left = Math.min(Math.max(margin, rect.left), Math.max(margin, window.innerWidth - width - margin));
    const next: Record<string, string> = { top: `${top}px`, left: `${left}px` };
    if (matchAnchorWidth) {
      // min-width 而不是 width：选项比触发器长时菜单要能撑开，等宽会截断文字。
      next.minWidth = `${width}px`;
    }
    style.value = next;
  }

  function show(): void {
    open.value = true;
  }

  function hide(): void {
    open.value = false;
  }

  function toggle(): void {
    open.value = !open.value;
  }

  function onDocPointerDown(event: PointerEvent): void {
    const target = event.target as Node | null;
    if (!open.value || !target) {
      return;
    }
    // 组件根节点里只有触发器和面板，所以"在浮层里"就等于"在面板里或按钮里"，
    // 拿这两个引用判包含关系与判根节点等价。
    if (anchor.value?.contains(target) || panel.value?.contains(target)) {
      return;
    }
    open.value = false;
  }

  function onDocKeydown(event: KeyboardEvent): void {
    // 捕获阶段拦截并 stopPropagation：浮层常常开在模态框里，Esc 应当只收起浮层
    // 这一层，放它冒泡会把外层弹窗一起关掉，等于一下按 Esc 连关两层。
    if (open.value && event.key === "Escape") {
      event.stopPropagation();
      open.value = false;
    }
  }

  function onWinResize(): void {
    if (open.value) {
      place();
    }
  }

  /** 滚动即重算：滚动的是触发器所在的任意祖先容器（弹窗正文、主内容区、侧栏
      各自都是 overflow-y: auto），不是 window，只盯 resize 的话浮层会钉在原处。 */
  function onDocScroll(): void {
    place();
  }

  // 捕获阶段挂在 document 上：元素上的 scroll 不冒泡，但捕获会从 document 一路
  // 传到目标，任意容器滚都收得到，也就不必去猜触发器被压在哪个容器里；页面滚动
  // 的目标是 document 自身，同样经过这里。只在打开期间挂着，平时不占全局事件。
  watch(
    open,
    (isOpen) => {
      if (!isOpen) {
        document.removeEventListener("scroll", onDocScroll, { capture: true });
        return;
      }
      // 面板挂在 v-if 下，渲染完才量得到尺寸，所以定位要等 DOM 落地。
      place();
      document.addEventListener("scroll", onDocScroll, { capture: true, passive: true });
    },
    { flush: "post" },
  );

  onMounted(() => {
    document.addEventListener("pointerdown", onDocPointerDown, true);
    document.addEventListener("keydown", onDocKeydown, true);
    window.addEventListener("resize", onWinResize);
  });

  onBeforeUnmount(() => {
    document.removeEventListener("pointerdown", onDocPointerDown, true);
    document.removeEventListener("keydown", onDocKeydown, true);
    window.removeEventListener("resize", onWinResize);
    // 打开状态下直接卸载时，watch 不会再跑一次，这里补一刀。
    document.removeEventListener("scroll", onDocScroll, { capture: true });
  });

  return { open, style, show, hide, toggle };
}
