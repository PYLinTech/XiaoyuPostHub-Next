<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import FileKindIcon from "@/components/ui/FileKindIcon.vue";
import PreviewOverlay from "@/components/PreviewOverlay.vue";
import logoUrl from "@/assets/logo.svg";
import { guestApi } from "@/api/endpoints";
import { ApiError } from "@/api/client";
import { type GuestTarget, type GuestShare } from "@/api/types";
import { pickupDeliverySource, shareDeliverySource } from "@/delivery/sources";
import { useDeliveryAction } from "@/delivery/actions";
import { describeError, logError } from "@/lib/async";
import { baseName, formatTime } from "@/lib/format";
import { isPreviewable } from "@/lib/filekind";
import { useSession } from "@/stores/session";

// 取件码入口。
//
// 这个页面不要求登录：取件码本身就是凭据，未登录的访客按访客组配额交付。
// 页面里没有"取件码是否存在"这类信息可挖——后端对一切失败返回同一句话，
// 界面也不能做得更聪明。

const route = useRoute();
const router = useRouter();
const session = useSession();

const CODE_LEN = 6;

const cells = ref<string[]>(Array.from({ length: CODE_LEN }, () => ""));
/** 固定的六个槽位下标：模板按它渲染，长度与真值数组无关。 */
const cellIndices = Array.from({ length: CODE_LEN }, (_, index) => index);
const code = computed(() => cells.value.join(""));
const resolving = ref(false);
const error = ref("");
const share = ref<GuestShare | null>(null);
const target = ref<GuestTarget | null>(null);
const previewOpen = ref(false);

const { busy: downloading, run: runDownload } = useDeliveryAction();

const fileName = computed(() => (target.value ? baseName(target.value.path) : ""));
const isFileShare = computed(() => share.value?.kind === "file");

/** 已登录时嵌在壳的内容区里打开（侧栏入口）；访客保持整屏版式（登录页/外链入口）。 */
const embedded = computed(() => session.state.authenticated);

const remainingVisits = computed(() => {
  const current = share.value;
  if (!current || current.maxVisits <= 0) {
    return "不限";
  }
  return String(Math.max(0, current.maxVisits - current.visits));
});

const expiresLabel = computed(() => {
  const expiresAt = share.value?.expiresAt ?? 0;
  return expiresAt > 0 ? formatTime(expiresAt) : "永久";
});

/**
 * 取件码只有 download 一条交付通道，预览必须借用分享通道，而分享通道会重新
 * 校验访问模式：提取码模式在访客侧必然被拒，"仅自己"只有创建者能过。
 * 因此只在确定能过时才给出预览按钮，避免点了才吃一个 403。
 *
 * 这里以分享自身的 allowPreview 为准，而不是 session.can(Perm.Preview)：
 * 访客拿到的是访客组的权限位（站点级开关），而这一份内容到底让不让预览，
 * 是分享创建者的决定——两者都要满足，但后者才是这里能判断的那个。
 */
const canPreview = computed(() => {
  const current = share.value;
  if (!current || current.kind !== "file" || !current.allowPreview) {
    return false;
  }
  if (!isPreviewable(fileName.value)) {
    return false;
  }
  switch (current.accessMode) {
    case "public":
      return true;
    case "login":
      return session.state.authenticated;
    case "restricted":
      return current.isOwner === true;
    default:
      return false;
  }
});

const canDownload = computed(() => share.value !== null);

const previewSource = computed(() => {
  const current = share.value;
  return current ? shareDeliverySource(current.id, "", "", "preview") : null;
});

function normalizeCode(raw: string): string {
  return raw.replace(/\s+/g, "").toUpperCase();
}

/**
 * 正在编辑的格子下标；-1 表示没有。
 *
 * 光标落进已有内容的格子时，只是"不显示"它——真值仍留在 cells 里。
 * 敲入新字符就替换，直接离开则原样还原：改一位不需要先删一次。
 */
const editing = ref(-1);

/** 格子里该显示什么：编辑中的格子显示为空，其余按真值。 */
function cellValue(index: number): string {
  return editing.value === index ? "" : (cells.value[index] ?? "");
}

function beginEdit(index: number): void {
  editing.value = cells.value[index] ? index : -1;
}

function endEdit(): void {
  editing.value = -1;
}

/** 把整串写回六个格子。整串输入只有两个入口：粘贴、以及链接里的 ?code= 回填。 */
function fill(raw: string): void {
  const value = normalizeCode(raw).slice(0, CODE_LEN);
  cells.value = Array.from({ length: CODE_LEN }, (_, index) => value[index] ?? "");
  editing.value = -1;
}

const cellEls = ref<Array<HTMLInputElement | null>>([]);

function setCellEl(element: unknown, index: number): void {
  cellEls.value[index] = (element as HTMLInputElement | null) ?? null;
}

function focusCell(index: number): void {
  cellEls.value[index]?.focus();
}

/** 在范围内就聚焦目标格子，否则什么也不做。 */
function jump(to: number, event: KeyboardEvent): void {
  if (to < 0 || to >= CODE_LEN) {
    return;
  }
  event.preventDefault();
  focusCell(to);
}

/** 六格填满就自动提交，省掉"再点一次提取"；出错后可用按钮手动重试。 */
function submitIfComplete(): void {
  if (resolving.value || cells.value.some((char) => char === "")) {
    return;
  }
  void resolve();
}

function onCellInput(index: number, event: Event): void {
  const element = event.target as HTMLInputElement;
  // 一次输入只可能是单个字符（多字符走 fill()），取最后一个即可；
  // 删空也走这条路径：真删除了，离开时不再还原。
  const char = normalizeCode(element.value).slice(-1);
  element.value = char;
  cells.value[index] = char;
  editing.value = -1;
  if (char && index < CODE_LEN - 1) {
    focusCell(index + 1);
  }
  submitIfComplete();
}

function onCellKeydown(index: number, event: KeyboardEvent): void {
  if (event.key === "Backspace") {
    // 自己处理删除，不依赖输入框的默认行为：编辑中的格子 DOM 里本来就是空的，
    // 默认行为不会触发任何事件，表现就是"按了没反应"。
    event.preventDefault();
    editing.value = -1;
    const target = lastFilledAtOrBefore(index);
    if (target >= 0) {
      cells.value[target] = "";
      focusCell(target);
    }
    return;
  }
  if (event.key === "ArrowLeft") {
    jump(index - 1, event);
    return;
  }
  if (event.key === "ArrowRight") {
    jump(index + 1, event);
  }
}

/** 从 index 往前找最近一个有内容的格子；一个都没有则返回 -1。 */
function lastFilledAtOrBefore(index: number): number {
  for (let cursor = index; cursor >= 0; cursor -= 1) {
    if (cells.value[cursor]) {
      return cursor;
    }
  }
  return -1;
}

function onPaste(event: ClipboardEvent): void {
  const text = normalizeCode(event.clipboardData?.getData("text") ?? "");
  if (!text) {
    return;
  }
  event.preventDefault();
  fill(text);
  focusCell(Math.min(text.length, CODE_LEN - 1));
  submitIfComplete();
}

async function resolve(input?: string): Promise<void> {
  // 只有"从链接带 ?code= 进来"会传 input；其余情况格子里已经是规范化的码。
  if (input !== undefined) {
    fill(input);
  }
  const value = code.value;
  if (value === "") {
    error.value = "请输入取件码";
    return;
  }
  resolving.value = true;
  error.value = "";
  try {
    const result = await guestApi.resolvePickup(value);
    share.value = result.share;
    target.value = result.target;
    // 把码写进地址栏，方便把"取件"做成一枚链接直接发出去。
    if (route.query.code !== value) {
      void router.replace({ query: { code: value } });
    }
  } catch (err) {
    logError("pickup-resolve", err);
    share.value = null;
    target.value = null;
    // 后端对"码不存在 / 已停用 / 已过期 / 已用尽 / 分享侧失效"统一回 403 与一句
    // 中性文案，这里也不去区分：能区分就等于给出一个可以逐个枚举取件码的入口。
    error.value =
      err instanceof ApiError && err.status === 403 ? "取件码无效或已失效" : describeError(err);
  } finally {
    resolving.value = false;
  }
}

function reset(): void {
  fill("");
  share.value = null;
  target.value = null;
  error.value = "";
  void router.replace({ query: {} });
}

async function download(): Promise<void> {
  await runDownload(pickupDeliverySource(code.value), { fileName: fileName.value || share.value?.rootName });
}

onMounted(() => {
  const preset = route.query.code;
  if (typeof preset === "string" && preset !== "") {
    void resolve(preset);
  }
});
</script>

<template>
  <div class="pickup" :class="{ 'pickup--result': share && target, 'pickup--embedded': embedded }">
    <div class="pickup__panel card">
      <header class="pickup__head">
        <img class="pickup__logo" :src="logoUrl" alt="" width="36" height="36" />
        <h1 class="page-head__title">取件码</h1>
        <!-- 「返回登录」只对访客有意义：壳内打开时侧栏就是导航。 -->
        <AppButton v-if="!embedded" variant="ghost" size="sm" @click="router.push('/login')">
          返回登录
        </AppButton>
      </header>

      <div class="card__body stack">
        <form class="stack" @submit.prevent="resolve()">
          <div class="code" @paste="onPaste">
            <input
              v-for="index in cellIndices"
              :key="index"
              class="code__cell"
              type="text"
              inputmode="text"
              autocomplete="off"
              autocapitalize="characters"
              spellcheck="false"
              :ref="(el) => setCellEl(el, index)"
              :value="cellValue(index)"
              :aria-label="`取件码第 ${index + 1} 位`"
              @focus="beginEdit(index)"
              @blur="endEdit"
              @input="onCellInput(index, $event)"
              @keydown="onCellKeydown(index, $event)"
            />
          </div>
          <AppButton type="submit" variant="primary" block :loading="resolving">提取</AppButton>
        </form>

        <p v-if="error" class="notice notice--danger">{{ error }}</p>
      </div>
    </div>

    <div v-if="share && target" class="card">
      <div class="card__body stack">
        <div class="row row--between">
          <div class="row" style="min-width: 0">
            <FileKindIcon
              :name="fileName || share.rootName || ''"
              :is-folder="share.kind === 'folder'"
              :size="20"
            />
            <strong class="truncate">{{ fileName || share.rootName }}</strong>
          </div>
          <span class="badge badge--success">已提取</span>
        </div>

        <dl class="kv">
          <dt>内容名称</dt>
          <dd class="mono">{{ share.rootName }}</dd>
          <dt>类型</dt>
          <dd>{{ share.kind === "folder" ? "文件夹" : "文件" }}</dd>
          <dt>有效期</dt>
          <dd>{{ expiresLabel }}</dd>
          <dt>剩余访问次数</dt>
          <dd>{{ remainingVisits }}</dd>
        </dl>

        <template v-if="isFileShare">
          <div class="row">
            <AppButton v-if="canPreview" variant="primary" icon="eye" @click="previewOpen = true">
              预览
            </AppButton>
            <AppButton
              v-if="canDownload"
              :variant="canPreview ? 'default' : 'primary'"
              icon="download"
              :loading="downloading"
              @click="download()"
            >
              下载
            </AppButton>
          </div>
          <p class="faint" style="font-size: var(--fs-xs)">
            预览走的是分享通道，会再消耗一次分享的访问次数，但不消耗取件码的使用次数。
          </p>
        </template>

        <template v-else>
          <p class="notice notice--warn">
            这是一条文件夹分享。取件码通道一次只交付一个对象，文件夹本身没有可交付的内容，
            后端也没有打包下载接口，因此这里取不到文件。
          </p>
          <div class="row">
            <AppButton icon="external" @click="router.push(`/s/${share.id}`)">
              打开分享页逐个下载
            </AppButton>
          </div>
          <p class="faint" style="font-size: var(--fs-xs)">
            分享页会按原分享的访问方式要求凭据；如果它需要提取码，请向分享者一并索取。
          </p>
        </template>

        <p class="faint" style="font-size: var(--fs-xs)">
          查看内容不消耗使用次数，只有真正取数（下载）才会消耗一次。
        </p>

        <div class="row">
          <AppButton size="sm" icon="refresh" @click="reset()">换一个取件码</AppButton>
        </div>
      </div>
    </div>

    <PreviewOverlay
      :open="previewOpen"
      :source="previewSource"
      :file-name="fileName"
      @close="previewOpen = false"
    />
  </div>
</template>

<style scoped>
/* 与登录/注册页同一套小表单版式：居中的窄卡片 + 顶部标识。 */
.pickup {
  min-height: 100dvh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--sp-4);
  padding: var(--sp-6) var(--sp-4);
}

/* 提取出结果后内容变高，改从顶部开始排，避免超出视口被裁。 */
.pickup--result {
  justify-content: flex-start;
  padding-top: var(--sp-8);
}

/* 壳内版式：同一张卡片，但不再整屏接管。垂直居中范围 = 视口 - 顶栏
   - 内容区自身的上下内边距（sp-5 / sp-10，见 app.css .shell__content）。 */
.pickup--embedded {
  min-height: calc(100dvh - var(--topbar-h) - var(--sp-5) - var(--sp-10));
  padding: 0;
}

.pickup--embedded.pickup--result {
  padding-top: var(--sp-8);
}

.pickup__panel,
.pickup > .card:not(.pickup__panel) {
  width: min(440px, 100%);
  /* 入场：卡片浮入 → 标识放大 → 取件码格与按钮依次落位；
     提取出结果后，结果卡也用同一套浮入动画接上。 */
  animation: enter-page 0.44s cubic-bezier(0.2, 0.9, 0.25, 1) both;
}

.pickup__head {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-5) var(--sp-5) 0;
}

/* 标题吃掉中间空间，把「返回登录」推到右上角。 */
.pickup__head .page-head__title {
  flex: 1;
  min-width: 0;
}

.pickup__logo {
  width: 36px;
  height: 36px;
  flex: none;
  display: block;
  animation: enter-mark 0.5s cubic-bezier(0.34, 1.32, 0.64, 1) both;
  animation-delay: 0.1s;
}

.pickup__panel form > * {
  animation: enter-item 0.42s cubic-bezier(0.2, 0.9, 0.25, 1) both;
}

.pickup__panel form > *:nth-child(1) {
  animation-delay: 0.18s;
}

.pickup__panel form > *:nth-child(2) {
  animation-delay: 0.26s;
}

@media (prefers-reduced-motion: reduce) {
  .pickup__panel,
  .pickup > .card:not(.pickup__panel),
  .pickup__logo,
  .pickup__panel form > * {
    animation: none;
  }
}

/* 六个字符格：一格里一个字符，粘整整串也能一次填满。 */
.code {
  display: flex;
  gap: var(--sp-2);
}

.code__cell {
  flex: 1;
  min-width: 0;
  height: 54px;
  padding: 0;
  text-align: center;
  font-family: var(--font-mono);
  font-size: var(--fs-xl);
  text-transform: uppercase;
  color: var(--c-text);
  background: var(--c-surface);
  border: 1px solid var(--c-border-strong);
  border-radius: var(--r-sm);
  outline: none;
  transition: border-color 0.12s ease, box-shadow 0.12s ease;
}

.code__cell:focus {
  border-color: var(--c-accent);
  box-shadow: 0 0 0 3px var(--c-accent-weak);
}
</style>
