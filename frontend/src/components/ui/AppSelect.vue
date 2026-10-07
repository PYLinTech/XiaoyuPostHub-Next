<script setup lang="ts" generic="T extends string | number">
import { computed, ref, watch } from "vue";
import { useOverlayScroll } from "@/lib/overlayScroll";
import { usePopover } from "@/lib/popover";

// 自绘下拉：站内所有下拉都走这里。
//
// 原生 <select> 折叠时还能靠 CSS 伪装（app.css 的 .select 就去掉了系统外观），
// 展开的选项列表却由操作系统画，观感和站内其它控件对不上，所以整条自己画。

const props = withDefaults(
  defineProps<{
    modelValue: T;
    options: readonly { value: T; label: string }[];
    ariaLabel?: string;
    /** 没有匹配到选项时触发器显示的文案。 */
    placeholder?: string;
    /** "sm" 与 .btn--sm 同高，用于分页这种一行里全是小控件的地方。 */
    size?: "sm" | "md";
    disabled?: boolean;
  }>(),
  { size: "md" },
);

const emit = defineEmits<{ "update:modelValue": [T] }>();

const triggerEl = ref<HTMLElement | null>(null);
const menuEl = ref<HTMLElement | null>(null);
/** 定位的宿主层：样式绑在它身上，量的是它（含边框），里面滚的 ul 不是。 */
const menuWrapEl = ref<HTMLElement | null>(null);

// 菜单自绘滚动条：原生条在选项列表里占一条宽度、观感也跟系统走，与站内
// 其它滚动区统一换成浮动滑块。轨道挂在菜单外层的宿主上（宿主不滚）。
const {
  visible: barVisible,
  style: barStyle,
  onThumbPointerDown,
  onThumbPointerMove,
  onThumbPointerUp,
} = useOverlayScroll(menuEl, "y");

const currentLabel = computed(
  () => props.options.find((option) => option.value === props.modelValue)?.label ?? props.placeholder ?? "",
);

/** 固定定位、外部点击、Esc、滚动重排都归 usePopover 管；这里只留下拉自己的事：
    什么时候能开（disabled）、以及打开后把选中项滚到哪里。 */
const { open, style: menuStyle, toggle: toggleMenu } = usePopover(triggerEl, menuWrapEl, { gap: 4 });

function toggle(): void {
  if (props.disabled) {
    return;
  }
  toggleMenu();
}

function scrollToSelected(): void {
  const menu = menuEl.value;
  const active = menu?.querySelector<HTMLElement>(".asel__option--on");
  if (!menu || !active) {
    return;
  }
  const delta =
    active.getBoundingClientRect().top -
    menu.getBoundingClientRect().top -
    (menu.clientHeight - active.offsetHeight) / 2;
  const max = menu.scrollHeight - menu.clientHeight;
  menu.scrollTop = Math.max(0, Math.min(menu.scrollTop + delta, max));
}

/** 打开时把选中项滚到可视区中部：小时/分钟这种几十项的列表默认停在顶部，
    要用户自己翻才看得到当前值。用 rect 差值算而不是 offsetTop——菜单的
    offsetParent 是固定定位的宿主层，不是滚动容器本身。
    post 是为了等菜单挂上来：宿主层在 v-if 下，watch 默认时机跑的时候它还不存在。 */
watch(
  open,
  (isOpen) => {
    if (isOpen) {
      scrollToSelected();
    }
  },
  { flush: "post" },
);

function pick(value: T): void {
  open.value = false;
  if (value !== props.modelValue) {
    emit("update:modelValue", value);
  }
}
</script>

<template>
  <div class="asel" :class="{ 'asel--sm': size === 'sm' }">
    <button
      ref="triggerEl"
      type="button"
      class="select asel__trigger"
      :class="{ 'select--sm': size === 'sm' }"
      :aria-label="ariaLabel"
      aria-haspopup="listbox"
      :aria-expanded="open"
      :disabled="disabled"
      @click="toggle"
    >
      <span class="truncate">{{ currentLabel }}</span>
    </button>

    <!-- 宿主层不滚，给滑块轨道提供定位上下文；真正的滚动容器是里面的 ul。 -->
    <div
      v-if="open"
      ref="menuWrapEl"
      class="asel__menuwrap ovscroll-layer ovscroll-layer--y"
      :style="menuStyle"
    >
      <ul ref="menuEl" class="asel__menu ovscroll" role="listbox">
        <li v-for="option in options" :key="option.value">
          <button
            type="button"
            class="asel__option"
            :class="{ 'asel__option--on': option.value === modelValue }"
            role="option"
            :aria-selected="option.value === modelValue"
            @click="pick(option.value)"
          >
            {{ option.label }}
          </button>
        </li>
      </ul>
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
  </div>
</template>

<style scoped>
/* 宽度与 .select 一致：默认撑满，sm 由 .select--sm 收成内容宽。 */
.asel {
  display: inline-flex;
  width: 100%;
}

.asel--sm {
  width: auto;
}

/* 触发器沿用 .select/.select--sm 的观感（箭头也是它画的），只把按钮默认的居中
   改成左对齐，跟原生 select 的文字位置一致。
   flex:1 让它撑满 .asel：.select--sm 是 width:auto（按内容收宽），不撑的话外面
   给了多宽都没用，可见框只有内容那么宽。 */
.asel__trigger {
  flex: 1;
  min-width: 0;
  align-items: center;
  text-align: left;
}

/* 浮层定位与观感在宿主层；真正的滚动容器是里面的 ul，原生条已由
   .ovscroll 藏掉，滑块轨道贴宿主右缘。
   overflow: hidden 让滑块被外框的圆角裁住——否则它是一根直角的长条，
   顶部/底部会戳出圆角之外。 */
.asel__menuwrap {
  position: fixed;
  z-index: 85; /* 浮层层级约定见 lib/popover.ts */
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  box-shadow: var(--shadow-lg);
  max-height: 282px;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.asel__menu {
  margin: 0;
  padding: var(--sp-1);
  list-style: none;
  min-width: 0;
  overflow-y: auto;
}

.asel__option {
  display: block;
  width: 100%;
  padding: var(--sp-2) var(--sp-3);
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--c-text);
  font-size: var(--fs-sm);
  text-align: left;
  white-space: nowrap;
}

.asel__option:hover {
  background: var(--c-hover);
}

.asel__option--on {
  color: var(--c-accent);
  font-weight: 640;
}

/* 选项字号跟着触发器走，别出现"小号控件配大号菜单"。 */
.asel--sm .asel__option {
  font-size: var(--fs-xs);
}

/* 窄屏下 .select 用 16px（防 iOS 聚焦放大），菜单跟着一起放大。 */
@media (max-width: 640px) {
  .asel__option {
    font-size: 16px;
  }
}
</style>
