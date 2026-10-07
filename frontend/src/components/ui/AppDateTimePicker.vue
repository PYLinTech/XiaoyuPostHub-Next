<script setup lang="ts">
import { computed, reactive, ref } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import { usePopover } from "@/lib/popover";
import { pad } from "@/lib/format";

// 自定义日期时间选择器，替代原生的 datetime-local。
//
// 原生控件的外观与交互完全交给浏览器，无法与站内主题一致，不同浏览器的
// 表现差异也很大；这里自己画月历 + 时分下拉，值契约与 datetime-local 相同
// （YYYY-MM-DDTHH:mm 的本地时间字符串，空串 = 未设置），调用方只需把
// <input type="datetime-local"> 换成本组件，其余逻辑不动。
//
// 面板用 fixed 定位（按触发器实时测量，规则见 lib/popover.ts），避免被滚动容器/
// 模态框裁剪。

const props = defineProps<{
  modelValue: string;
  placeholder?: string;
  ariaLabel?: string;
}>();

const emit = defineEmits<{ "update:modelValue": [value: string] }>();

const triggerEl = ref<HTMLElement | null>(null);
const panelEl = ref<HTMLElement | null>(null);

const view = reactive({ year: 0, month: 0 });

const WEEKDAYS = ["一", "二", "三", "四", "五", "六", "日"];
const HOURS = Array.from({ length: 24 }, (_, i) => ({ value: String(i), label: pad(i) }));
const MINUTES = Array.from({ length: 60 }, (_, i) => ({ value: String(i), label: pad(i) }));

function parse(value: string): Date | null {
  if (!value) {
    return null;
  }
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}

const selected = computed(() => parse(props.modelValue));

const display = computed(() => (props.modelValue ? props.modelValue.replace("T", " ") : ""));

/** 一页月历：周一打头，前置空位补齐，末尾是当月天数。 */
const grid = computed<(number | null)[]>(() => {
  const first = new Date(view.year, view.month, 1);
  const lead = (first.getDay() + 6) % 7;
  const days = new Date(view.year, view.month + 1, 0).getDate();
  return [...Array<null>(lead).fill(null), ...Array.from({ length: days }, (_, i) => i + 1)];
});

function sameDay(a: Date | null, year: number, month: number, day: number): boolean {
  return !!a && a.getFullYear() === year && a.getMonth() === month && a.getDate() === day;
}

function isToday(day: number): boolean {
  const now = new Date();
  return sameDay(now, view.year, view.month, day);
}

function isSelected(day: number): boolean {
  return sameDay(selected.value, view.year, view.month, day);
}

function moveMonth(delta: number): void {
  const next = new Date(view.year, view.month + delta, 1);
  view.year = next.getFullYear();
  view.month = next.getMonth();
}

/** 组合日期与时分并提交；没有日期部分时用今天兜底（时分来自已选值或当前时刻）。 */
function commit(date: Date, hours: number, minutes: number): void {
  const d = new Date(date.getFullYear(), date.getMonth(), date.getDate(), hours, minutes);
  emit("update:modelValue", `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`);
}

function pickDay(day: number): void {
  const base = selected.value ?? new Date();
  commit(new Date(view.year, view.month, day), base.getHours(), base.getMinutes());
}

const hourValue = computed(() => (selected.value ? String(selected.value.getHours()) : ""));
const minuteValue = computed(() => (selected.value ? String(selected.value.getMinutes()) : ""));

function setHour(value: string): void {
  const base = selected.value ?? new Date();
  commit(base, Number(value), base.getMinutes());
}

function setMinute(value: string): void {
  const base = selected.value ?? new Date();
  commit(base, base.getHours(), Number(value));
}

function pickToday(): void {
  const base = selected.value ?? new Date();
  commit(new Date(), base.getHours(), base.getMinutes());
  const now = new Date();
  view.year = now.getFullYear();
  view.month = now.getMonth();
}

function clear(): void {
  emit("update:modelValue", "");
}

// ---------------------------------------------------------------- 开合与定位

/** 开合、定位、外部点击、Esc、滚动重排归 usePopover 管；这里只剩开面板时要把
    月历翻到哪个月。matchAnchorWidth: false —— 面板宽度固定（见 .dtp__panel），
    不随触发器拉伸：表单里的触发器有整列宽，跟着它走会把月历撑得巨大。 */
const { open, style: panelStyle, show, hide } = usePopover(triggerEl, panelEl, {
  gap: 6,
  matchAnchorWidth: false,
});

function openPanel(): void {
  const base = selected.value ?? new Date();
  view.year = base.getFullYear();
  view.month = base.getMonth();
  show();
}

function toggle(): void {
  if (open.value) {
    hide();
  } else {
    openPanel();
  }
}
</script>

<template>
  <div class="dtp">
    <button
      ref="triggerEl"
      type="button"
      class="input dtp__trigger"
      :class="{ 'dtp__trigger--empty': !modelValue }"
      :aria-label="ariaLabel"
      @click="toggle"
    >
      <span class="truncate">{{ display || placeholder || "未设置" }}</span>
      <AppIcon name="clock" :size="15" />
    </button>

    <div v-if="open" ref="panelEl" class="dtp__panel" :style="panelStyle">
      <div class="dtp__head">
        <button type="button" class="dtp__nav" aria-label="上一月" @click="moveMonth(-1)">
          <AppIcon name="chevronLeft" :size="15" />
        </button>
        <span class="dtp__title">{{ view.year }} 年 {{ view.month + 1 }} 月</span>
        <button type="button" class="dtp__nav" aria-label="下一月" @click="moveMonth(1)">
          <AppIcon name="chevronRight" :size="15" />
        </button>
      </div>

      <div class="dtp__week">
        <span v-for="label in WEEKDAYS" :key="label">{{ label }}</span>
      </div>

      <div class="dtp__grid">
        <span v-for="(day, index) in grid" :key="index" class="dtp__cell">
          <button
            v-if="day"
            type="button"
            class="dtp__day"
            :class="{
              'dtp__day--today': isToday(day) && !isSelected(day),
              'dtp__day--selected': isSelected(day),
            }"
            @click="pickDay(day)"
          >
            {{ day }}
          </button>
        </span>
      </div>

      <!-- 时间行：标签在前，时与分各占剩余宽度的一半，中间不夹冒号。 -->
      <div class="dtp__time">
        <span class="dtp__time-label">时间：</span>
        <AppSelect
          size="sm"
          :model-value="hourValue"
          :options="HOURS"
          placeholder="时"
          aria-label="时"
          @update:model-value="setHour"
        />
        <AppSelect
          size="sm"
          :model-value="minuteValue"
          :options="MINUTES"
          placeholder="分"
          aria-label="分"
          @update:model-value="setMinute"
        />
      </div>

      <div class="dtp__foot">
        <AppButton size="sm" @click="clear">清除</AppButton>
        <AppButton size="sm" variant="primary" @click="pickToday">今天</AppButton>
      </div>
    </div>
  </div>
</template>

<style scoped>
.dtp {
  display: block;
  min-width: 0;
}

/* 触发器复用 .input 的底子，只是排版换成两端对齐。 */
.dtp__trigger {
  display: flex;
  width: 100%;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  text-align: left;
  cursor: pointer;
  color: var(--c-text);
  font-family: inherit;
}

.dtp__trigger--empty {
  color: var(--c-text-faint);
}

.dtp__panel {
  position: fixed;
  z-index: 85; /* 浮层层级约定见 lib/popover.ts */
  width: min(300px, calc(100vw - 24px));
  padding: var(--sp-4);
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  box-shadow: var(--shadow-lg);
}

.dtp__head {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
}

.dtp__title {
  flex: 1;
  text-align: center;
  font-size: var(--fs-md);
  font-weight: 650;
}

.dtp__nav {
  width: 28px;
  height: 28px;
  flex: none;
  border: 0;
  border-radius: var(--r-pill);
  background: transparent;
  color: var(--c-text-muted);
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.dtp__nav:hover {
  background: var(--c-hover);
  color: var(--c-text);
}

.dtp__week,
.dtp__grid {
  display: grid;
  grid-template-columns: repeat(7, 1fr);
  gap: 3px;
}

.dtp__week {
  margin-top: var(--sp-3);
  padding-bottom: var(--sp-1);
  border-bottom: 1px solid var(--c-border);
}

.dtp__week span {
  text-align: center;
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.dtp__grid {
  margin-top: var(--sp-1);
}

.dtp__cell {
  display: block;
}

/* 圆格日期：正圆视觉来自等宽等高 + 全圆角，选中态是一颗实心强调色圆点。 */
.dtp__day {
  width: 100%;
  aspect-ratio: 1;
  height: auto;
  padding: 0;
  border: 0;
  border-radius: var(--r-pill);
  background: transparent;
  color: var(--c-text);
  font-size: var(--fs-sm);
  font-family: inherit;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.dtp__day:hover {
  background: var(--c-hover);
}

/* 今天：强调色描一圈细环，未选中时才标——选中的实心圆本身就是最强的信号。 */
.dtp__day--today {
  box-shadow: inset 0 0 0 1px var(--c-accent);
  color: var(--c-accent);
  font-weight: 620;
}

.dtp__day--selected {
  background: var(--c-accent);
  color: var(--c-text-inverse);
}

/* 时间与快捷操作各成一块：分隔线划层次，月历是主体、它们是附属。 */
.dtp__time {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  margin-top: var(--sp-3);
  padding-top: var(--sp-3);
  border-top: 1px solid var(--c-border);
}

.dtp__time-label {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

/* flex-basis 归零：两个下拉严格平分"时间："之后的剩余宽度，不受各自内容宽影响。 */
.dtp__time > .asel {
  flex: 1 1 0;
  min-width: 0;
}

.dtp__foot {
  display: flex;
  justify-content: space-between;
  gap: var(--sp-2);
  margin-top: var(--sp-2);
  padding-top: var(--sp-3);
  border-top: 1px solid var(--c-border);
}
</style>
