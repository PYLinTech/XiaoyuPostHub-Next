<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from "vue";
import AppIcon from "./AppIcon.vue";
import { useOverlayScroll } from "@/lib/overlayScroll";

// 弹窗。
//
// 三个必要的细节：Esc 关闭、打开时锁滚动、所有视口都居中。
// 少任何一个都会让人觉得"这个网页怪怪的"，而它们很难在事后补——
// 因为每个调用点都会各自以为"弹窗自己会处理"。

const props = withDefaults(
  defineProps<{
    open: boolean;
    title?: string;
    wide?: boolean;
    /** 关键操作（例如删除确认）不允许点击遮罩关闭，避免误触丢失输入。 */
    dismissible?: boolean;
    /** 小窗：始终浮在页面正中的紧凑面板，
        正文区定高（见 .modal__panel--compact .modal__body）——尺寸与内容
        无关，切换空态/加载态/列表时窗口不会跟着伸缩。
        随手看一眼的内容（公告、消息）用这个，别把整个下半屏占掉。 */
    compact?: boolean;
  }>(),
  { wide: false, dismissible: true, compact: false },
);

const emit = defineEmits<{ close: [] }>();

// 标题 id 必须每实例唯一：弹窗是可以嵌套的（ConfirmDialog 内部就是另一个
// AppModal），共用一个 id 会让两个 dialog 的 aria-labelledby 指到同一个标题。
let modalSeq = 0;
const titleId = `modal-title-${++modalSeq}`;

const panelEl = ref<HTMLElement | null>(null);
/** 打开弹窗时焦点所在的那个元素，关闭后要还回去。 */
let opener: HTMLElement | null = null;

/** 面板内参与 Tab 循环的元素。排除 [tabindex="-1"]：面板自身就是 -1，
    它只作兜底落点，不该混进用户按 Tab 走过的那一串里。 */
const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])';

// 已打开的面板，按打开顺序排。弹窗是会嵌套的（ConfirmDialog 套在页面自己的
// AppModal 里），两个实例会同时收到 document 上的 keydown；外层若也跟着拦 Tab，
// 它发现焦点不在自己面板里（焦点其实在内层面板）就会把焦点抢回自己——于是确认框里
// 每按一次 Tab 都被弹回第一个按钮。所以只有最上面那层弹窗管焦点。
const openPanels: HTMLElement[] = [];

/** 把面板从嵌套栈里摘掉。已经在栈外（还没入栈就被关掉）就什么都不做。 */
function dropPanel(): void {
  const panel = panelEl.value;
  if (!panel) {
    return;
  }
  const index = openPanels.indexOf(panel);
  if (index >= 0) {
    openPanels.splice(index, 1);
  }
}

/**
 * 把 Tab 圈在面板内。
 *
 * 模板上写了 aria-modal="true"，屏幕阅读器因此认为页面上只剩这个弹窗；可 Tab
 * 照样能走到背后那些"已经隐藏"的控件上，焦点落到看不见的地方后就再也回不来。
 * 只拦首尾两端，中间那几步照常走浏览器默认顺序。
 */
function trapTab(event: KeyboardEvent): void {
  const panel = panelEl.value;
  if (!panel) {
    return;
  }
  // 不是最上面那层就完全不插手：嵌套弹窗里焦点属于内层，外层抢走等于禁用 Tab。
  if (openPanels[openPanels.length - 1] !== panel) {
    return;
  }
  const items = Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE));
  // 一个可聚焦元素都没有时不要拦 Tab：拦下来焦点原地不动，键盘用户会被彻底卡住。
  if (items.length === 0) {
    return;
  }
  const first = items[0];
  const last = items[items.length - 1];
  const active = document.activeElement;
  // 焦点已经跑到弹窗外（被代码移走、或在还焦点之前就点到了别处）时也拉回第一个，
  // 否则用户按多少次 Tab 都回不来。
  const outside = !active || !panel.contains(active);
  if (event.shiftKey && (outside || active === first)) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && (outside || active === last)) {
    event.preventDefault();
    first.focus();
  }
}

/** 把焦点还给打开弹窗的那个元素。拿不到就跳过，绝不因此打断关闭流程。 */
function restoreFocus(): void {
  const target = opener;
  opener = null;
  // 元素可能已经被卸载或替换掉了（例如它所在的行刚被删），此时还焦点没有意义。
  if (!target || !target.isConnected) {
    return;
  }
  target.focus();
}

function onKeydown(event: KeyboardEvent): void {
  // 只有最上面那层弹窗响应键盘。Esc 同理：所有实例都监听 document，
  // 不挡住非栈顶的话，在确认框里按一次 Esc 会把确认框和它底下的编辑弹窗
  // 一起关掉——上下文和已填的内容一起没了。
  if (openPanels[openPanels.length - 1] !== panelEl.value) {
    return;
  }
  if (event.key === "Tab") {
    trapTab(event);
    return;
  }
  if (event.key === "Escape" && props.dismissible) {
    emit("close");
  }
}

// 模块级计数：AppModal 是会嵌套的——ConfirmDialog 自己就渲染一个 AppModal，
// 而引用它的页面几乎全都同时渲染着另一个 AppModal。原来每次关闭都无脑解锁，
// 于是"关掉确认框"会顺手把还开着的编辑弹窗的背景也解锁，从弹窗边缘露出来开始滚。
let lockCount = 0;

function lockScroll(locked: boolean): void {
  lockCount = locked ? lockCount + 1 : Math.max(0, lockCount - 1);
  // 用类名而不是内联 style：内联样式优先级最高，会盖掉其它来源的 overflow
  // 并在关闭时无条件抹掉它；类名则能和已有规则叠加。
  //
  // 依赖：对应的 CSS 规则不在本文件，而在 styles/app.css
  // （body.modal-open { overflow: hidden; }）。本组件只负责开关这个类名。
  document.body.classList.toggle("modal-open", lockCount > 0);
}

watch(
  () => props.open,
  (open) => {
    if (open) {
      // 先记下"是谁打开了弹窗"，关闭时才知道要把焦点还给谁。
      // 挂载即打开时 activeElement 是 body，那不是可还焦点的目标，跳过。
      const active = document.activeElement;
      opener = active instanceof HTMLElement && active !== document.body ? active : null;
      document.addEventListener("keydown", onKeydown);
      lockScroll(true);
      // 等面板真的插进 DOM（Teleport + v-if）再聚焦：提前调 focus() 会落在 body 上，
      // 键盘用户仍然不知道自己在弹窗里。
      void nextTick(() => {
        // 拿不到面板说明这次打开已经被关掉了，别把它加进栈。
        if (!panelEl.value) {
          return;
        }
        panelEl.value.focus();
        openPanels.push(panelEl.value);
      });
    } else {
      document.removeEventListener("keydown", onKeydown);
      lockScroll(false);
      // 先出栈再还焦点：焦点要回到外层弹窗里，那一层得重新接管 Tab。
      dropPanel();
      restoreFocus();
    }
  },
  // immediate：挂载时就处于打开态（HMR 重挂载、编程式打开）也要绑上监听，
  // 否则那个弹窗永远关不掉（Esc 无效）。
  { immediate: true },
);

onBeforeUnmount(() => {
  document.removeEventListener("keydown", onKeydown);
  lockScroll(false);
  dropPanel();
  // 卸载同样要还焦点：路由直接切走时，上面的 watch 不会跑到关闭分支。
  restoreFocus();
});

// 正文自绘滚动条：弹窗内容超高时原生条会占掉一条宽度、样式也跟系统走，
// 与站内其它滚动区（内容区、页签）统一换成浮动滑块。轨道挂在面板上
// （不滚的宿主），滑块贴着正文右缘。
const bodyEl = ref<HTMLElement | null>(null);
const {
  visible: barVisible,
  style: barStyle,
  onThumbPointerDown,
  onThumbPointerMove,
  onThumbPointerUp,
} = useOverlayScroll(bodyEl, "y");
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="open"
        class="modal"
        :class="{ 'modal--compact': compact }"
        role="dialog"
        aria-modal="true"
        :aria-labelledby="title ? titleId : undefined"
      >
        <div class="modal__backdrop" @click="dismissible && emit('close')" />
        <div
          ref="panelEl"
          class="modal__panel"
          :class="{ 'modal__panel--wide': wide, 'modal__panel--compact': compact }"
          tabindex="-1"
        >
          <header class="modal__head">
            <h2 :id="titleId" class="modal__title">{{ title }}</h2>
            <!-- 头部操作与关闭同属右上角那一组，一起右对齐；操作按钮由调用方
                 从 #actions 传进来，于是常驻操作不必再占一条底部操作栏。 -->
            <div class="modal__head-actions">
              <slot name="actions" />
              <!-- 关闭是红底白叉（全站统一）：没有底部操作栏的弹窗里它是唯一
                 出口，给点颜色更好找。 -->
            <button class="btn btn--sm btn--danger" type="button" aria-label="关闭" @click="emit('close')">
                <AppIcon name="close" :size="14" />
              </button>
            </div>
          </header>
          <!-- 正文与滑块轨道同住一层：宿主被头部与底栏夹在中间、自己不滚，
               轨道因此恰好只覆盖正文高度。 -->
          <div class="modal__bodywrap ovscroll-layer ovscroll-layer--y">
            <div ref="bodyEl" class="modal__body ovscroll">
              <slot />
            </div>
            <div class="ovscroll__track" :class="{ 'ovscroll__track--on': barVisible }">
              <div
                class="ovscroll__thumb"
                :style="barStyle"
                @pointerdown="onThumbPointerDown"
                @pointermove="onThumbPointerMove"
                @pointerup="onThumbPointerUp"
              />
            </div>
          </div>
          <footer v-if="$slots.footer" class="modal__foot">
            <slot name="footer" />
          </footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
