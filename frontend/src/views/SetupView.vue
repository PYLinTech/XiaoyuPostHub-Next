<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import { RouterLink, useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import FormField from "@/components/ui/FormField.vue";
import logoUrl from "@/assets/logo.svg";
import { setupApi } from "@/api/endpoints";
import type { SetupResult, SetupState, SetupValidateRequest, SystemMode } from "@/api/types";
import { copyText, describeError, logError } from "@/lib/async";
import { useOverlayScroll } from "@/lib/overlayScroll";
import {
  ACCOUNT_PLACEHOLDER,
  PASSWORD_PLACEHOLDER,
  validateAccount,
  validatePassword,
} from "@/lib/credentials";
import { useSession } from "@/stores/session";
import { useToasts } from "@/stores/toast";

// 首次初始化引导页 —— 分步向导。
//
// 版面学 Windows 11 的首次开机流程：一步只问一件事，标题大、说明短、下一步在
// 右下角，左上角保留品牌（logo + 名称）。这样做的实际收益不只是好看：初始化
// 表单原先十几个字段一次铺开，最容易被忽略的恰恰是最要紧的那一步——加密密钥
// 留空时生成的那一份只显示一次，错过就再也拿不到。
//
// 账号的长度约束来自接口返回的 SetupState；密码字符集和长度是全站固定规则，
// 由 credentials.ts 与服务端 auth 包同时执行，避免初始化绕过其它入口的校验。

const router = useRouter();
const session = useSession();
const toasts = useToasts();

const state = ref<SetupState | null>(null);
const loading = ref(true);
const submitting = ref(false);
/** 分步校验在途：这段时间里禁用"下一步"，避免连点重复发请求。 */
const checking = ref(false);
/** 按钮的忙碌态：提交与分步校验共用，避免两种在途状态各写一遍。 */
const busy = computed(() => submitting.value || checking.value);
const error = ref("");
const result = ref<SetupResult | null>(null);
const localError = ref("");

const form = reactive({
  account: "",
  password: "",
  confirm: "",
  siteName: "",
  registerMode: "invite",
  systemMode: "both" as SystemMode,
  encryptionKey: "",
  token: "",
  clientId: "",
  clientSecret: "",
  rootDirId: "",
  privateKey: "",
  authCallback: false,
  directLink: false,
});

/** 存储凭据可选填，但默认按"现在就接"展开：多数场景希望初始化完就能传文件。 */
const storageEnabled = ref(true);
const showKey = ref(false);
const showPassword = ref(false);
const showClientSecret = ref(false);

type SecretField = "password" | "clientSecret";
const composing = reactive<Record<SecretField, boolean>>({
  password: false,
  clientSecret: false,
});

// 管理员密码使用原生 password 类型，显示时切换为 text；Client Secret 仍使用
// 普通文本控件配合自绘圆点，避免把第三方凭据交给浏览器密码管理器。
function isSecretVisible(field: SecretField): boolean {
  if (field === "password") return showPassword.value;
  return showClientSecret.value;
}

function maskedSecret(value: string): string {
  return Array.from(value, () => "●").join("");
}

function secretDisplay(field: SecretField): string {
  const value = form[field];
  return isSecretVisible(field) ? value : maskedSecret(value);
}

/** 把遮罩字符串中的字符位置换算成原文的 UTF-16 位置，兼容 emoji 等字符。 */
function maskedOffsetToRaw(value: string, offset: number): number {
  let rawOffset = 0;
  let count = 0;
  for (const char of value) {
    if (count >= offset) break;
    rawOffset += char.length;
    count += 1;
  }
  return rawOffset;
}

/** 从输入框当前值计算一次编辑，并把结果写回原文状态与遮罩值。 */
function applySecretInput(field: SecretField, input: HTMLInputElement): void {
  const raw = form[field];
  const before = secretDisplay(field);
  const after = input.value;
  let start = 0;
  while (start < before.length && start < after.length && before[start] === after[start]) {
    start += 1;
  }
  let beforeEnd = before.length;
  let afterEnd = after.length;
  while (beforeEnd > start && afterEnd > start && before[beforeEnd - 1] === after[afterEnd - 1]) {
    beforeEnd -= 1;
    afterEnd -= 1;
  }

  const inserted = after.slice(start, afterEnd);
  const visible = isSecretVisible(field);
  const rawStart = visible ? start : maskedOffsetToRaw(raw, start);
  const rawEnd = visible ? beforeEnd : maskedOffsetToRaw(raw, beforeEnd);
  form[field] = raw.slice(0, rawStart) + inserted + raw.slice(rawEnd);

  const display = secretDisplay(field);
  const caret = visible ? start + inserted.length : start + Array.from(inserted).length;
  input.value = display;
  input.setSelectionRange(caret, caret);
}

function handleSecretInput(field: SecretField, event: Event): void {
  if (composing[field] || (event as InputEvent).isComposing) return;
  applySecretInput(field, event.currentTarget as HTMLInputElement);
}

function handleSecretCompositionStart(field: SecretField, event: CompositionEvent): void {
  composing[field] = true;
  if (isSecretVisible(field)) return;

  const input = event.currentTarget as HTMLInputElement;
  const raw = form[field];
  const start = maskedOffsetToRaw(raw, input.selectionStart ?? 0);
  const end = maskedOffsetToRaw(raw, input.selectionEnd ?? start);
  // 输入法需要看到原文来维护组合串；组合期间仍使用同一个普通文本框。
  input.value = raw;
  input.setSelectionRange(start, end);
}

function handleSecretCompositionEnd(field: SecretField, event: CompositionEvent): void {
  const input = event.currentTarget as HTMLInputElement;
  const raw = input.value;
  const rawCaret = input.selectionStart ?? raw.length;
  composing[field] = false;
  form[field] = raw;

  const display = secretDisplay(field);
  const caret = isSecretVisible(field) ? rawCaret : Array.from(raw.slice(0, rawCaret)).length;
  input.value = display;
  input.setSelectionRange(caret, caret);
}

const brandName = computed(() => state.value?.siteName?.trim() || session.state.siteName || "XiaoyuPostHub-Next");

/** 回源鉴权地址：后端固定挂在 /api/cdn/auth，用当前访问源拼出可直接粘贴的完整地址。 */
const cdnAuthURL = computed(() => `${window.location.origin}/api/cdn/auth`);
/** 判断访问源是否是"上游 CDN 能访问到"的公网地址。 */
function isPublicHost(host: string): boolean {
  const h = host.replace(/^\[|\]$/g, "").toLowerCase();
  if (!h || h === "localhost" || h.endsWith(".localhost") || h.endsWith(".local")) {
    return false;
  }
  const isIPv4 = /^\d{1,3}(\.\d{1,3}){3}$/.test(h);
  if (isIPv4) {
    const [a, b] = h.split(".").map(Number);
    if (a === 0 || a === 10 || a === 127) return false;
    if (a === 169 && b === 254) return false;
    if (a === 172 && b >= 16 && b <= 31) return false;
    if (a === 192 && b === 168) return false;
    if (a === 100 && b >= 64 && b <= 127) return false; // 运营商级 NAT 段
    if (a >= 224) return false; // 组播与保留段
    return true;
  }
  if (h.includes(":")) {
    return false; // IPv6 字面量一律按"非公网 IPv4"处理
  }
  // 域名：解析结果在浏览器里看不到，按可公网访问处理。
  return true;
}

/** 上游 CDN 只能访问公网地址，本机/内网地址配了也不会生效。 */
const cdnAuthNotPublic = computed(() => !isPublicHost(window.location.hostname));

async function copyCdnAuthURL(): Promise<void> {
  const ok = await copyText(cdnAuthURL.value);
  if (ok) {
    toasts.success("已复制鉴权地址");
  } else {
    toasts.error("复制失败，请手动选中后复制");
  }
}

// 打开开关会撑高这一段，而步骤区是固定高度的滚动容器：
// 展开后把这一块滚到可视位置，否则用户只看到开关亮起、看不到下面的地址。
const cdnBox = ref<HTMLElement | null>(null);
watch(
  () => form.authCallback,
  async (on) => {
    if (!on) {
      return;
    }
    await nextTick();
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    cdnBox.value?.scrollIntoView({ block: "start", behavior: reduced ? "auto" : "smooth" });
    // 指示条不在这里补算：它的位置由下面的 ResizeObserver 跟着正文盒子的高度走，
    // 展开与收起都会自己跟上。
  },
);

// ---------------------------------------------------------------- 步骤

type StepId =
  | "welcome"
  | "token"
  | "site"
  | "admin"
  | "mode"
  | "register"
  | "encryption"
  | "storage";

interface Step {
  id: StepId;
  title: string;
  icon: string;
}

// 八步固定流程：令牌对任何来源都必填，不再按访问来源增删步骤。
//
// 系统模式排在管理员账号之后：先把"谁在管"定下来，再决定"管什么"。
// 放最后会让已经看过前六步的人以为自己了解了这套系统，实际上
// 最影响后续使用形态的选择被压在最后。
const STEPS: Step[] = [
  { id: "welcome", title: "欢迎", icon: "hardDrive" },
  { id: "token", title: "先验证这次初始化", icon: "key" },
  { id: "site", title: "给站点起个名字", icon: "hardDrive" },
  { id: "admin", title: "创建管理员账号", icon: "user" },
  { id: "mode", title: "这个站点提供什么", icon: "ri-apps-line" },
  { id: "register", title: "新用户如何加入", icon: "tag" },
  { id: "encryption", title: "加密密钥怎么来", icon: "lock" },
  { id: "storage", title: "现在就接入存储吗", icon: "cloud" },
];

const index = ref(0);
const step = computed(() => STEPS[Math.min(index.value, STEPS.length - 1)]);
const isLast = computed(() => index.value >= STEPS.length - 1);
const isFirst = computed(() => index.value === 0);
const progress = computed(() => ((index.value + 1) / STEPS.length) * 100);

/** 前进 / 后退的方向，用来决定切换动画从哪一侧进来。 */
const dir = ref<"fwd" | "back">("fwd");
const stepBody = ref<HTMLElement | null>(null);
const stepActions = ref<HTMLElement | null>(null);

// ---------------------------------------------------------------- 自绘滚动指示条
//
// 引导页把浏览器原生滚动条全部隐藏（见样式），滚动位置改由这条浮动指示条表达。
// 它绝对定位、pointer-events: none，完全不参与布局，因此不会像原生滚动条那样
// 把正文挤窄——这也是"不占位"的含义。
//
// 滑块的长度、位移与显隐交给 lib 的 useOverlayScroll（AppModal、表格用的是同一套）：
// 那里面已经盯上了滚动、容器尺寸与内容增删。手写一份就只能靠 watch 手工列举"哪些
// 状态会改高度"，漏一项就留下一根不更新的指示条。
// 本页自己只留定位：指示条浮在正文右侧的卡片留白里，top / right 取决于正文与根
// 容器的相对位置，这是本页版面的事。

/** 挂在 <html> 上的类名：只在引导页存续期间隐藏页面级滚动条。 */
const HIDE_SCROLLBAR_CLASS = "xph-setup-no-scrollbar";

const rootBox = ref<HTMLElement | null>(null);

/** 指示条宽度，与样式里 .oobe__bar 的 width 保持一致。 */
const BAR_WIDTH = 5;
/** 指示条左缘与正文右缘的距离：落在正文之外的卡片留白里。 */
const BAR_GUTTER = 6;

const { visible: barVisible, style: barThumbStyle } = useOverlayScroll(stepBody, "y");

/** 指示条轨道的位置：横向浮在正文右侧的留白中，纵向与正文可视区等长。 */
const barBox = ref<Record<string, string>>({ top: "0px", right: "0px", height: "0px" });

/** 重新测量正文矩形与根容器，写回指示条轨道的定位。 */
function syncBarBox(): void {
  const body = stepBody.value;
  const root = rootBox.value;
  if (!body || !root) {
    return;
  }
  const bodyRect = body.getBoundingClientRect();
  const rootRect = root.getBoundingClientRect();
  barBox.value = {
    // 指示条浮在正文右缘之外的留白里——正文自身的左右边距仍算在距离内，因此
    // 它与内容隔得更开；同时它不占正文宽度，也不改动正文的任何尺寸。
    top: `${bodyRect.top - rootRect.top}px`,
    right: `${rootRect.right - bodyRect.right - BAR_GUTTER - BAR_WIDTH}px`,
    height: `${bodyRect.height}px`,
  };
}

// 轨道位置只跟版面走（换步会换掉正文元素、窗口尺寸会挪动卡片），所以盯引用重建
// 观察器，而不是在 onMounted 里接一次——那一刻正文元素还不存在。
let barBoxObserver: ResizeObserver | null = null;

watch(
  [rootBox, stepBody],
  ([root, body]) => {
    barBoxObserver?.disconnect();
    barBoxObserver = null;
    if (!root || !body) {
      return;
    }
    barBoxObserver = new ResizeObserver(syncBarBox);
    barBoxObserver.observe(root);
    barBoxObserver.observe(body);
    syncBarBox();
  },
  { immediate: true, flush: "post" },
);

/** 每一步的前置校验：只校验这一步问到的字段，错误就地显示。 */
function validateStep(id: StepId): string {
  switch (id) {
    case "token":
      return form.token.trim() ? "" : "请填写运行日志中的初始化令牌";
    case "admin": {
      const accountError = validateAccount(form.account);
      if (accountError) {
        return accountError;
      }
      const passwordError = validatePassword(form.password);
      if (passwordError) {
        return passwordError;
      }
      if (form.password !== form.confirm) {
        return "两次输入的密码不一致";
      }
      return "";
    }
    case "storage":
      if (!storageEnabled.value) {
        return "";
      }
      if (!form.clientId.trim() || !form.clientSecret.trim()) {
        return "请填写 Client ID 与 Client Secret";
      }
      if (!form.rootDirId.trim()) {
        return "请填写根目录 ID";
      }
      if (form.directLink && form.rootDirId.trim() === "0") {
        return "网盘根目录（ID 为 0）不能启用直链空间，请填写具体的根目录编号";
      }
      return "";
    default:
      return "";
  }
}

/**
 * 把当前这一步的字段发给后端校验。
 *
 * 只有四步需要：令牌、管理员账号、主密钥、存储凭据。站点名、系统模式与注册
 * 模式都是纯配置，没有服务端可校验的语义，留在本地就够了。
 *
 * 返回错误文案，空串表示通过。
 */
async function validateStepOnServer(id: StepId): Promise<string> {
  let body: SetupValidateRequest | null = null;
  switch (id) {
    case "token":
      body = { step: "token", token: form.token.trim() };
      break;
    case "admin":
      body = { step: "admin", account: form.account.trim(), password: form.password };
      break;
    case "encryption":
      body = { step: "encryption", encryptionKey: form.encryptionKey.trim() };
      break;
    case "storage":
      if (!storageEnabled.value) {
        return "";
      }
      body = {
        step: "storage",
        storage: {
          clientId: form.clientId.trim(),
          clientSecret: form.clientSecret.trim(),
          rootDirId: form.rootDirId.trim(),
          privateKey: form.privateKey.trim(),
          authCallback: form.authCallback,
          directLink: form.directLink,
        },
      };
      break;
    default:
      return "";
  }

  checking.value = true;
  try {
    await setupApi.validate(body);
    return "";
  } catch (err) {
    logError("setup-validate", err);
    return describeError(err);
  } finally {
    checking.value = false;
  }
}

/** 需要发到后端校验的步骤集合。 */
const SERVER_VALIDATED: ReadonlySet<StepId> = new Set<StepId>([
  "token",
  "admin",
  "encryption",
  "storage",
]);

async function next(): Promise<void> {
  // 重入保护：站点名与确认密码两个输入框上都绑了 @keyup.enter="next"，回车不受
  // 「下一步」按钮 disabled 的约束。少了这一句，服务端校验往返期间再按一次回车，
  // 就会 index += 1 两次——一次跨过两步，「这个站点提供什么」被静默跳过。
  if (busy.value) {
    return;
  }
  localError.value = "";
  const current = step.value.id;
  // 本地校验先跑：字段级规则（必填、两次密码一致、勾选互斥）留在前端，
  // 报错能就地指出是哪个输入框，不必等一次往返。
  const message = validateStep(current);
  if (message) {
    void showError(message);
    return;
  }
  if (isLast.value) {
    void submit();
    return;
  }
  // 服务端校验决定"能不能往下走"：令牌填错、账号被占用、密钥格式不对
  // 都在这一步当场暴露，而不是让人填完剩下的步骤到提交时才知道。
  if (SERVER_VALIDATED.has(current)) {
    const serverMessage = await validateStepOnServer(current);
    if (serverMessage) {
      await showError(serverMessage);
      return;
    }
  }
  dir.value = "fwd";
  index.value += 1;
}

function back(): void {
  localError.value = "";
  if (isFirst.value) {
    return;
  }
  dir.value = "back";
  index.value -= 1;
}

// 每步进入后把焦点落到第一个输入框；没有输入框的步骤（欢迎页）落到主按钮，
// 这样键盘用户一路回车就能走完。
//
// 选择器里不再写 select：模式选择早就换成 .choice 按钮，本文件里没有 select 了。
watch(step, async () => {
  await nextTick();
  const field = stepBody.value?.querySelector<HTMLElement>("input, textarea");
  if (field) {
    field.focus();
    return;
  }
  const buttons = stepActions.value?.querySelectorAll<HTMLButtonElement>("button");
  buttons?.[buttons.length - 1]?.focus();
});

// 校验失败与接口报错都汇总到这里：写进提示区，并把它整个滚进视野。
//
// 不用 watch(localError)：next() 会先把提示清空再写回同一句话，值等于上一轮，
// watch 判定"没变"就不触发——而用户恰恰是在又点了一次下一步之后，期待被带回
// 提示处。滚动必须由"给出错误"这个动作触发，不能挂在值的变化上。
//
// 也不用 scrollIntoView：它按元素的当前渲染盒计算，而提示带 fields-in 入场动画
// （起点比落位高 4px），照它滚会刚好差这 4px，框底被裁掉一条。这里按当前盒算完
// 再留出 ERROR_GAP，动画的下落量被这点余量吃掉；滚动范围不够时由浏览器夹紧，
// 框仍是完整可见。
const ERROR_GAP = 12;
const errorBox = ref<HTMLElement | null>(null);

async function showError(message: string): Promise<void> {
  localError.value = message;
  await nextTick();
  const body = stepBody.value;
  const box = errorBox.value;
  if (!body || !box) {
    return;
  }
  const bodyRect = body.getBoundingClientRect();
  const boxRect = box.getBoundingClientRect();
  const below = boxRect.bottom + ERROR_GAP - bodyRect.bottom;
  const above = bodyRect.top + ERROR_GAP - boxRect.top;
  const delta = below > 0 ? below : above > 0 ? -above : 0;
  if (delta === 0) {
    return;
  }
  const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  body.scrollTo({ top: body.scrollTop + delta, behavior: reduced ? "auto" : "smooth" });
}

onMounted(async () => {
  document.documentElement.classList.add(HIDE_SCROLLBAR_CLASS);
  try {
    const info = await setupApi.state();
    state.value = info;
    form.siteName = info.siteName;
    form.registerMode = info.registerMode;
    if (info.initialized) {
      // 已完成初始化时这个页面没有意义：直接送去登录，避免用户对着一个
      // 注定返回 409 的表单填半天。
      await router.replace("/login");
    }
  } catch (err) {
    logError("setup-state", err);
    error.value = describeError(err);
  } finally {
    loading.value = false;
  }
});

onBeforeUnmount(() => {
  document.documentElement.classList.remove(HIDE_SCROLLBAR_CLASS);
  barBoxObserver?.disconnect();
});

async function submit(): Promise<void> {
  localError.value = "";
  for (const item of STEPS) {
    const message = validateStep(item.id);
    if (message) {
      void showError(message);
      dir.value = "back";
      index.value = STEPS.indexOf(item);
      return;
    }
  }

  submitting.value = true;
  try {
    result.value = await setupApi.init({
      account: form.account.trim(),
      password: form.password,
      siteName: form.siteName.trim(),
      registerMode: form.registerMode,
      systemMode: form.systemMode,
      encryptionKey: form.encryptionKey.trim(),
      token: form.token.trim(),
      storage:
        storageEnabled.value && form.clientId.trim()
          ? {
              clientId: form.clientId.trim(),
              clientSecret: form.clientSecret.trim(),
              rootDirId: form.rootDirId.trim(),
              privateKey: form.privateKey.trim(),
              authCallback: form.authCallback,
              directLink: form.directLink,
            }
          : undefined,
    });
    // 刷新站点信息是初始化之后的收尾，不属于初始化本身：它失败时 result 已填、
    // 页面也已切到完成页，落进下面的 catch 只会把错误写到一个已经不在屏幕上的
    // 步骤区，被静默吞掉。因此单独处理，只提示，不改结果。
    try {
      await session.reloadSiteInfo();
    } catch (err) {
      logError("setup-site-info", err);
      toasts.info("站点信息刷新失败，刷新页面后会重新读取");
    }
  } catch (err) {
    logError("setup-init", err);
    void showError(describeError(err));
  } finally {
    submitting.value = false;
  }
}

async function copyKey(): Promise<void> {
  if (!result.value?.encryptionKey) {
    return;
  }
  const ok = await copyText(result.value.encryptionKey);
  if (ok) {
    toasts.success("已复制到剪贴板");
  } else {
    toasts.error("复制失败，请手动选中后复制");
  }
}

const REGISTER_MODES: Array<{ value: string; title: string; desc: string; icon: string }> = [
  { value: "invite", title: "需要邀请码", desc: "只有拿到邀请码的人能注册", icon: "tag" },
  { value: "open", title: "开放注册", desc: "任何人都可以自行注册账号", icon: "users" },
  { value: "closed", title: "关闭注册", desc: "只保留现有账号，不再新增", icon: "lock" },
];

/** 系统模式的两种取值。desc 说清"关掉的那侧会怎样"，而不只是名字。 */
const SYSTEM_MODES: Array<{ value: SystemMode; title: string; desc: string; icon: string }> = [
  {
    value: "both",
    title: "文件与邮件（推荐）",
    desc: "站点将同时提供文件和邮件功能，以文件为主",
    icon: "ri-apps-line",
  },
  {
    value: "files_only",
    title: "仅文件",
    desc: "只提供文件，并关闭邮件功能",
    icon: "folder",
  },
];

/** 完成页里的存储状态：一句话说明，长原因另外用提示块展开。 */
const storageState = computed(() => {
  const info = result.value;
  if (!info) {
    return "";
  }
  if (info.storageWarning) {
    return "未启用（原因见下方）";
  }
  if (!info.storageConfigured) {
    return "未配置";
  }
  const probe = info.storageProbe;
  if (!probe) {
    return "已就绪";
  }
  const mark = (ok: boolean) => (ok ? "对" : "错");
  return `已就绪：上传${mark(probe.uploaded)} 下传${mark(probe.downloaded)} 校验${mark(probe.verified)} 清理${mark(probe.deleted)}`;
});
</script>

<template>
  <div ref="rootBox" class="oobe">
    <RouterLink class="oobe__brand" to="/" :title="brandName">
      <img class="oobe__logo" :src="logoUrl" alt="" width="33" height="33" />
      <span class="oobe__name">{{ brandName }}</span>
    </RouterLink>

    <h1 class="sr-only">初始化站点</h1>

    <main class="oobe__stage">
      <p v-if="loading" class="oobe__loading">
        <span class="spinner" />
        <span class="muted">正在读取站点状态</span>
      </p>

      <p v-else-if="error" class="notice notice--danger">{{ error }}</p>

      <!-- 完成页：沿用向导的同一个卡片骨架（同一套图标行、间距、底部操作条），
           只把"字段"换成结果清单，避免从表单跳到另一个视觉体系。 -->
      <section v-else-if="result" class="oobe__card oobe__card--done">
        <div ref="stepBody" class="oobe__body">
          <div class="oobe__toprow">
            <span class="oobe__tile oobe__tile--ok">
              <AppIcon name="shield" :size="24" />
            </span>
            <h2 class="oobe__title">初始化完成</h2>
          </div>

          <dl class="oobe__spec">
            <dt>管理员账号</dt>
            <dd>{{ result.adminAccount }}</dd>
            <dt>站点名称</dt>
            <dd>{{ result.siteName }}</dd>
            <dt>存储后端</dt>
            <dd>{{ storageState }}</dd>
          </dl>

          <div v-if="result.encryptionGenerated && result.encryptionKey" class="oobe__fields">
            <FormField label="加密密钥">
              <template #action>
                <span class="oobe__field-note">只显示这一次，请立刻离线保存</span>
              </template>
              <span class="oobe__keybox">
                <textarea
                  class="textarea mono oobe__keyring"
                  readonly
                  rows="1"
                  wrap="off"
                  :value="result.encryptionKey"
                />
                <button
                  type="button"
                  class="oobe__copy oobe__copy--inset"
                  title="复制加密密钥"
                  aria-label="复制加密密钥"
                  @click="copyKey"
                >
                  <AppIcon name="copy" :size="16" />
                </button>
              </span>
            </FormField>
          </div>

          <p v-if="result.storageWarning" class="notice notice--warn">
            存储凭据已保存，但未能启用：{{ result.storageWarning }}
            <br />
            这不算初始化失败——凭据可能填错或上游暂时不可达，稍后可在「管理 → 存储」重试。
          </p>
        </div>

        <footer class="oobe__actions">
          <span class="spacer" />
          <AppButton variant="primary" icon="logout" @click="router.push('/login')">去登录</AppButton>
        </footer>
      </section>

      <!-- 向导 -->
      <section v-else class="oobe__card">
        <div class="oobe__progress" role="progressbar" :aria-valuenow="index + 1" :aria-valuemin="1" :aria-valuemax="STEPS.length">
          <span class="oobe__progress-fill" :style="{ width: `${progress}%` }" />
        </div>

        <Transition
          :name="dir === 'back' ? 'step-back' : 'step-fwd'"
          mode="out-in"
          @after-enter="syncBarBox"
        >
          <div
            :key="step.id"
            ref="stepBody"
            class="oobe__body"
            :class="{ 'oobe__body--center': step.id === 'welcome' }"
          >
            <!-- 欢迎页：只有 logo 与欢迎语，居中显示，没有任何可填项 -->
            <div v-if="step.id === 'welcome'" class="oobe__welcome">
              <span class="logo-hop">
                <img class="logo-hop__icon" :src="logoUrl" alt="" width="56" height="56" />
              </span>
              <h2 class="oobe__welcome-title">欢迎</h2>
            </div>

            <template v-else>
              <div class="oobe__toprow">
                <span class="oobe__tile">
                  <AppIcon :name="step.icon" :size="24" />
                </span>
                <h2 class="oobe__title">{{ step.title }}</h2>
                <span class="oobe__counter faint">{{ index + 1 }} / {{ STEPS.length }}</span>
              </div>

              <!-- 2 · 令牌 -->
              <template v-if="step.id === 'token'">
                <FormField label="初始化令牌" required hint="请在后端程序运行日志的 SETUP TOKEN 一行查看">
                  <input v-model="form.token" class="input mono oobe__input" autocomplete="off" />
                </FormField>
              </template>

              <!-- 3 · 站点名称 -->
              <template v-else-if="step.id === 'site'">
                <FormField label="站点名称">
                  <input
                    v-model="form.siteName"
                    class="input oobe__input"
                    :placeholder="state?.siteName"
                    @keyup.enter="next"
                  />
                </FormField>
              </template>

              <!-- 4 · 管理员账号 -->
              <template v-else-if="step.id === 'admin'">
                <div class="oobe__fields">
                  <FormField label="管理员账号" required>
                    <input
                      v-model="form.account"
                      class="input oobe__input"
                      autocomplete="username"
                      :placeholder="ACCOUNT_PLACEHOLDER"
                    />
                  </FormField>
                  <FormField label="管理员密码" required>
                    <span class="oobe__reveal-wrap">
                      <input
                        :value="secretDisplay('password')"
                        class="input oobe__input"
                        :class="{ mono: showPassword }"
                        :type="showPassword ? 'text' : 'password'"
                        lang="zh-CN"
                        spellcheck="false"
                        autocomplete="new-password"
                        :placeholder="PASSWORD_PLACEHOLDER"
                        @input="handleSecretInput('password', $event)"
                        @compositionstart="handleSecretCompositionStart('password', $event)"
                        @compositionend="handleSecretCompositionEnd('password', $event)"
                      />
                      <button
                        type="button"
                        class="oobe__reveal"
                        :aria-label="showPassword ? '隐藏密码' : '显示密码'"
                        :aria-pressed="showPassword"
                        :title="showPassword ? '隐藏密码' : '显示密码'"
                        @click="showPassword = !showPassword"
                      >
                        <AppIcon :name="showPassword ? 'ri-eye-off-line' : 'eye'" :size="17" />
                      </button>
                    </span>
                  </FormField>
                  <FormField label="确认密码" required>
                    <input
                      v-model="form.confirm"
                      class="input oobe__input"
                      type="password"
                      autocomplete="new-password"
                      :placeholder="PASSWORD_PLACEHOLDER"
                      @keyup.enter="next"
                    />
                  </FormField>
                </div>
              </template>

              <!-- 5 · 系统模式 -->
              <template v-else-if="step.id === 'mode'">
                <div class="choices">
                  <button
                    v-for="mode in SYSTEM_MODES"
                    :key="mode.value"
                    type="button"
                    class="choice"
                    :class="{ 'choice--on': form.systemMode === mode.value }"
                    @click="form.systemMode = mode.value"
                  >
                    <span class="choice__icon"><AppIcon :name="mode.icon" :size="20" /></span>
                    <span class="choice__text">
                      <span class="choice__title">{{ mode.title }}</span>
                      <span class="choice__desc">{{ mode.desc }}</span>
                    </span>
                    <span class="choice__check"><AppIcon name="check" :size="16" /></span>
                  </button>
                </div>
              </template>

              <!-- 6 · 注册模式 -->
              <template v-else-if="step.id === 'register'">
                <div class="choices">
                  <button
                    v-for="mode in REGISTER_MODES"
                    :key="mode.value"
                    type="button"
                    class="choice"
                    :class="{ 'choice--on': form.registerMode === mode.value }"
                    @click="form.registerMode = mode.value"
                  >
                    <span class="choice__icon"><AppIcon :name="mode.icon" :size="20" /></span>
                    <span class="choice__text">
                      <span class="choice__title">{{ mode.title }}</span>
                      <span class="choice__desc">{{ mode.desc }}</span>
                    </span>
                    <span class="choice__check"><AppIcon name="check" :size="16" /></span>
                  </button>
                </div>
              </template>

              <!-- 7 · 加密密钥 -->
              <template v-else-if="step.id === 'encryption'">
                <div class="choices">
                  <button
                    type="button"
                    class="choice"
                    :class="{ 'choice--on': !showKey }"
                    @click="showKey = false"
                  >
                    <span class="choice__icon"><AppIcon name="shield" :size="20" /></span>
                    <span class="choice__text">
                      <span class="choice__title">由服务端生成（推荐）</span>
                      <span class="choice__desc">密钥只在完成后显示一次，请离线保存</span>
                    </span>
                    <span class="choice__check"><AppIcon name="check" :size="16" /></span>
                  </button>
                  <button
                    type="button"
                    class="choice"
                    :class="{ 'choice--on': showKey }"
                    @click="showKey = true"
                  >
                    <span class="choice__icon"><AppIcon name="key" :size="20" /></span>
                    <span class="choice__text">
                      <span class="choice__title">使用自己的主密钥</span>
                      <span class="choice__desc">
                        请使用<a
                          class="choice__link"
                          href="https://www.bing.com/search?q=%e5%a6%82%e4%bd%95%e7%94%9f%e6%88%9032%e5%ad%97%e8%8a%82%e7%9a%84URL%e5%ae%89%e5%85%a8Base64%e5%af%86%e9%92%a5"
                          target="_blank"
                          rel="noopener noreferrer"
                        >32字节的URL安全Base64密钥</a>，后续不可更改
                      </span>
                    </span>
                    <span class="choice__check"><AppIcon name="check" :size="16" /></span>
                  </button>
                </div>

                <FormField v-if="showKey" label="主密钥">
                  <input
                    v-model="form.encryptionKey"
                    class="input mono"
                    type="text"
                    autocomplete="off"
                    spellcheck="false"
                    placeholder="aXrHeyGXVIJ3ZqroPgJpOJ413adLPd4Ck8rraCrxwxs"
                  />
                </FormField>
              </template>

              <!-- 8 · 存储 -->
              <template v-else>
                <div class="choices">
                  <button
                    type="button"
                    class="choice"
                    :class="{ 'choice--on': !storageEnabled }"
                    @click="storageEnabled = false"
                  >
                    <span class="choice__icon"><AppIcon name="clock" :size="20" /></span>
                    <span class="choice__text">
                      <span class="choice__title">先跳过</span>
                      <span class="choice__desc">跳过后可在管理后台里配置</span>
                    </span>
                    <span class="choice__check"><AppIcon name="check" :size="16" /></span>
                  </button>
                  <button
                    type="button"
                    class="choice"
                    :class="{ 'choice--on': storageEnabled }"
                    @click="storageEnabled = true"
                  >
                    <span class="choice__icon"><AppIcon name="cloud" :size="20" /></span>
                    <span class="choice__text">
                      <span class="choice__title">现在就填存储凭据</span>
                      <span class="choice__desc">目前仅支持123云盘开放平台</span>
                    </span>
                    <span class="choice__check"><AppIcon name="check" :size="16" /></span>
                  </button>
                </div>

                <div v-if="storageEnabled" class="oobe__fields">
                  <FormField label="Client ID" required>
                    <input
                      v-model="form.clientId"
                      class="input mono oobe__input"
                      placeholder="请先购买123云盘开发者权益包"
                    />
                  </FormField>
                  <FormField label="Client Secret" required>
                    <span class="oobe__reveal-wrap">
                      <input
                        :value="secretDisplay('clientSecret')"
                        class="input mono oobe__input"
                        type="text"
                        lang="zh-CN"
                        spellcheck="false"
                        autocomplete="off"
                        placeholder="查看站内信获取"
                        @input="handleSecretInput('clientSecret', $event)"
                        @compositionstart="handleSecretCompositionStart('clientSecret', $event)"
                        @compositionend="handleSecretCompositionEnd('clientSecret', $event)"
                      />
                      <button
                        type="button"
                        class="oobe__reveal"
                        :aria-label="showClientSecret ? '隐藏 Client Secret' : '显示 Client Secret'"
                        :aria-pressed="showClientSecret"
                        :title="showClientSecret ? '隐藏 Client Secret' : '显示 Client Secret'"
                        @click="showClientSecret = !showClientSecret"
                      >
                        <AppIcon :name="showClientSecret ? 'ri-eye-off-line' : 'eye'" :size="17" />
                      </button>
                    </span>
                  </FormField>
                  <FormField label="根目录 ID" required>
                    <input
                      v-model="form.rootDirId"
                      class="input mono oobe__input"
                      placeholder="进入云盘文件夹后在浏览器链接末尾获得"
                    />
                  </FormField>
                  <FormField label="直链URL鉴权密钥">
                    <input v-model="form.privateKey" class="input mono oobe__input" />
                  </FormField>

                  <FormField hint="开启本项是下载使用直链加密流量的前提">
                    <label class="switch">
                      <input v-model="form.directLink" class="switch__input" type="checkbox" />
                      <span class="switch__track"><span class="switch__knob" /></span>
                      <span class="switch__label">启用直链空间</span>
                    </label>
                  </FormField>

                  <div ref="cdnBox" class="oobe__cdn">
                    <label class="switch">
                      <input v-model="form.authCallback" class="switch__input" type="checkbox" />
                      <span class="switch__track"><span class="switch__knob" /></span>
                      <span class="switch__label">直链启用远程鉴权</span>
                    </label>

                    <template v-if="form.authCallback">
                      <div class="oobe__cdn-row">
                        <span class="oobe__cdn-label">鉴权服务器地址</span>
                        <span class="oobe__cdn-value mono truncate">{{ cdnAuthURL }}</span>
                        <button
                          type="button"
                          class="oobe__copy"
                          title="复制地址"
                          aria-label="复制鉴权服务器地址"
                          @click="copyCdnAuthURL"
                        >
                          <AppIcon name="copy" :size="16" />
                        </button>
                      </div>

                      <dl class="oobe__spec">
                        <dt>请求方法</dt>
                        <dd class="mono">GET</dd>
                        <dt>自定义参数</dt>
                        <dd class="mono">需要配置URL参数：①选择参数 → remote_addr → $remote_addr   ②选择参数 → request_uri → $request_uri</dd>
                        <dt>鉴权失败状态码</dt>
                        <dd class="mono">403</dd>
                        <dt>鉴权超时时长</dt>
                        <dd>3000 毫秒</dd>
                        <dt>鉴权超时之后的动作</dt>
                        <dd>拒绝</dd>
                      </dl>

                      <p v-if="cdnAuthNotPublic" class="notice notice--warn">
                        检测到当前不是 IPV4 公网地址，请从公网域名访问后再配置，或直接使用
                        <span class="mono">https://你的域名/api/cdn/auth</span>。
                      </p>
                    </template>
                  </div>
                </div>

              </template>

              <p v-if="localError" ref="errorBox" class="notice notice--danger oobe__error">{{ localError }}</p>
            </template>
          </div>
        </Transition>

        <footer ref="stepActions" class="oobe__actions">
          <AppButton v-if="!isFirst" :disabled="busy" @click="back">上一步</AppButton>
          <span class="spacer" />
          <!-- 校验在途时按钮也进入 loading：请求发出到返回之间再次点击，
               会把同一份输入重复打给后端，既没有意义也会让错误提示闪烁。 -->
          <AppButton variant="primary" :loading="busy" @click="next">
            {{ isLast ? "完成初始化" : step.id === "welcome" ? "开始" : "下一步" }}
          </AppButton>
        </footer>
      </section>
    </main>

    <!-- 自绘的浮动滚动指示条：绝对定位 + pointer-events: none，不参与布局，
         因此不会像原生滚动条那样挤窄正文。位置由本页算（top / right / height），
         滑块的长度、位移与显隐由 useOverlayScroll 给。 -->
    <div
      class="oobe__bar"
      :class="{ 'oobe__bar--on': barVisible }"
      :style="barBox"
      aria-hidden="true"
    >
      <span class="oobe__bar-thumb" :style="barThumbStyle" />
    </div>
  </div>
</template>

<style scoped>
.oobe {
  position: relative;
  min-height: 100dvh;
  display: flex;
  flex-direction: column;
  align-items: center;
  /* 顶部留出品牌的固定位置，避免窄屏时和卡片贴在一起。 */
  padding: 76px var(--sp-4) var(--sp-10);
}

/* 完全隐藏本页所有元素的浏览器原生滚动条：滚动位置改由下面那条自绘的浮动
   指示条表达。用通配后代覆盖到全部子元素，正文里的 textarea 等内部滚动容器
   也一并包括在内。 */
.oobe,
.oobe * {
  scrollbar-width: none;
  -ms-overflow-style: none;
}

.oobe::-webkit-scrollbar,
.oobe *::-webkit-scrollbar {
  width: 0;
  height: 0;
}

/* 品牌以左上角为锚点：整块可点，回到根页面。 */
.oobe__brand {
  position: absolute;
  top: var(--sp-5);
  left: var(--sp-5);
  display: inline-flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-md);
  color: var(--c-text);
  text-decoration: none;
  transition: background 0.14s ease;
}

.oobe__brand:hover {
  background: var(--c-hover);
  text-decoration: none;
}

.oobe__logo {
  width: 33px;
  height: 33px;
  flex: none;
  display: block;
}

.oobe__name {
  font-size: var(--fs-lg);
  font-weight: 650;
  letter-spacing: -0.01em;
}

.oobe__stage {
  width: min(620px, 100%);
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
}

.oobe__loading {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--sp-3);
}

.oobe__card {
  position: relative;
  display: flex;
  flex-direction: column;
  /* 固定高度：步骤之间的内容量差别很大，卡片跟着内容长高矮下去会让整页跳动。
     超高时由卡片内部的步骤区滚动，而不是把按钮推走。 */
  height: min(540px, calc(100dvh - 190px));
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  box-shadow: var(--shadow-md);
  padding: var(--sp-5) var(--sp-6) var(--sp-5);
  overflow: hidden;
}

.oobe__card--done {
  /* 与向导共用同一个卡片骨架与固定高度，只多一个人场动画。 */
  animation: card-in 0.32s cubic-bezier(0.2, 0.9, 0.25, 1);
}

.oobe__progress {
  position: absolute;
  inset: 0 0 auto 0;
  height: 3px;
  background: var(--c-surface-sunken);
}

.oobe__progress-fill {
  display: block;
  height: 100%;
  background: var(--c-accent);
  border-radius: 0 var(--r-pill) var(--r-pill) 0;
  transition: width 0.36s cubic-bezier(0.2, 0.9, 0.25, 1);
}

/* 自绘滚动指示条。top / right / height 由脚本按正文矩形写入，滑块长度与位移
   按滚动比例换算；默认透明，只有当正文确实溢出时才淡入。 */
.oobe__bar {
  position: absolute;
  width: 5px;
  border-radius: var(--r-pill);
  pointer-events: none;
  opacity: 0;
  transition: opacity 0.18s ease;
}

.oobe__bar--on {
  opacity: 1;
}

.oobe__bar-thumb {
  display: block;
  width: 100%;
  border-radius: inherit;
  background: var(--c-text-muted);
  opacity: 0.42;
  will-change: transform;
}

/* 图标与步骤计数同一行：图标在左，计数推到最右。 */
.oobe__toprow {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  flex: none;
}

/* 间距节奏：同一组内的控件 8px，同一组内的字段 16px，标题行与内容、内容与
   错误提示这类"不同组"的间隔 24px。三档拉开层次，才不会到处一样松或一样紧。 */
.oobe__body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  /* 左右对称边距。指示条并不落在这条边距里，而是浮在更外侧的卡片留白中，
     与内容隔开一段明显距离；这条边距只负责正文自身的留白。 */
  padding-inline: var(--sp-3);
  display: flex;
  flex-direction: column;
  gap: var(--sp-6);
  padding-bottom: var(--sp-1);
}

/* 标签与输入框之间给到 6px：40px 高的输入框配 4px 会显得贴在一起。 */
.oobe__body :deep(.field) {
  gap: 6px;
}

.oobe__tile {
  width: 44px;
  height: 44px;
  border-radius: 12px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: var(--c-accent-weak);
  color: var(--c-accent);
  flex: none;
  animation: tile-in 0.34s cubic-bezier(0.2, 0.9, 0.25, 1);
}

.oobe__tile--ok {
  background: var(--c-success-weak);
  color: var(--c-success);
}

/* 欢迎页：内容在卡片里上下左右居中，且没有任何可操作项。 */
.oobe__body--center {
  justify-content: center;
  align-items: center;
  gap: 0;
}

.oobe__welcome {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--sp-5);
  animation: tile-in 0.34s cubic-bezier(0.2, 0.9, 0.25, 1);
}

.oobe__welcome-title {
  font-size: var(--fs-2xl);
  font-weight: 650;
  letter-spacing: -0.02em;
}

.oobe__title {
  font-size: var(--fs-xl);
  font-weight: 650;
  letter-spacing: -0.02em;
  line-height: 1.25;
}

/* 与图标同行时让标题吃掉剩余宽度，把计数挤到最右。 */
.oobe__toprow .oobe__title {
  flex: 1;
  min-width: 0;
}

.oobe__fields {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
  animation: fields-in 0.22s ease;
}

.oobe__input {
  min-height: 40px;
}

/* 主密钥是定长的机器串：单行展示，超宽横向滚动，不可换行也不可拖拽改高。 */
.oobe__keyring {
  resize: none;
  min-height: 40px;
  line-height: 1.5;
}

/* 密码框右侧的显示/隐藏按钮：贴在输入框内，不改变字段高度。 */
.oobe__reveal-wrap {
  position: relative;
  display: block;
}

.oobe__reveal-wrap .input {
  /* 输入框默认是 inline-block，行盒会在其下方多出基线间隙；
     按钮绝对定位按"框内右下角"算，必须先去掉这层间隙。 */
  display: block;
  padding-right: 44px;
}

.oobe__reveal {
  position: absolute;
  z-index: 1;
  top: 50%;
  right: 6px;
  transform: translateY(-50%);
  width: 30px;
  height: 30px;
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--c-text-muted);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  transition: background 0.14s ease, color 0.14s ease;
}

.oobe__reveal:hover {
  background: var(--c-hover);
  color: var(--c-text);
}

/* 开关样式（.switch 系列）已提升到 app.css 全局，供设置页复用。 */

/* 回源鉴权：勾选后才展开，地址是拼好的只读文本 + 复制按钮。 */
.oobe__cdn {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  padding: var(--sp-3);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface-2);
  /* 展开后自动滚到可视位置时，留一点上边距，不贴着卡片顶。 */
  scroll-margin-top: var(--sp-3);
}

.oobe__cdn-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  animation: fields-in 0.22s ease;
}

.oobe__cdn-label {
  flex: none;
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
}

.oobe__cdn-value {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-sm);
  color: var(--c-text);
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-sm);
  padding: 6px var(--sp-2);
}

.oobe__copy {
  width: 30px;
  height: 30px;
  flex: none;
  border: 1px solid var(--c-border);
  border-radius: var(--r-sm);
  background: var(--c-surface);
  color: var(--c-text-muted);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
}

.oobe__copy:hover {
  color: var(--c-accent);
  border-color: var(--c-accent);
}

/* 密钥框：复制按钮嵌在框内右侧垂直居中，不额外占一行。 */
.oobe__keybox {
  position: relative;
  display: block;
}

.oobe__keybox .oobe__keyring {
  /* 同上：块级化，否则按钮会落到框外压住边框。 */
  display: block;
  padding-right: 44px;
}

.oobe__copy--inset {
  position: absolute;
  right: 6px;
  top: 50%;
  transform: translateY(-50%);
}

/* 字段标签右侧的补充说明（例如加密密钥"只显示这一次"）。 */
.oobe__field-note {
  font-size: var(--fs-xs);
  font-weight: 400;
  color: var(--c-warn);
}

/* 只读规格清单：回源鉴权参数与完成页的结果都用它，保证两处观感一致。 */
.oobe__spec {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: 6px var(--sp-4);
  font-size: var(--fs-xs);
  animation: fields-in 0.22s ease;
}

.oobe__spec dt {
  color: var(--c-text-muted);
}

.oobe__spec dd {
  margin: 0;
  color: var(--c-text);
  min-width: 0;
  word-break: break-all;
}

.oobe__error {
  animation: fields-in 0.18s ease;
}

/* 选择卡片：整行可点，选中时描边 + 底纹 + 勾选，和系统设置里的观感一致 */
.choices {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.choice {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  width: 100%;
  padding: var(--sp-3) var(--sp-4);
  border: 1px solid var(--c-border-strong);
  border-radius: var(--r-md);
  background: var(--c-surface);
  text-align: left;
  cursor: pointer;
  transition: border-color 0.14s ease, background 0.14s ease, transform 0.14s ease;
}

.choice:hover {
  border-color: var(--c-accent);
  transform: translateY(-1px);
}

.choice--on {
  border-color: var(--c-accent);
  background: var(--c-accent-weak);
}

.choice__icon {
  width: 36px;
  height: 36px;
  border-radius: var(--r-md);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: var(--c-surface-sunken);
  color: var(--c-text-muted);
  flex: none;
  transition: color 0.14s ease, background 0.14s ease;
}

.choice--on .choice__icon {
  background: var(--c-surface);
  color: var(--c-accent);
}

.choice__text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  flex: 1;
}

.choice__title {
  font-size: var(--fs-sm);
  font-weight: 600;
}

.choice__desc {
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
}

.choice__link {
  color: var(--c-accent);
  text-decoration: underline;
  text-underline-offset: 2px;
}

.choice__link:hover {
  color: var(--c-accent-hover);
}

.choice__check {
  width: 22px;
  height: 22px;
  border-radius: 50%;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: 1px solid var(--c-border-strong);
  color: transparent;
  flex: none;
  transition: all 0.14s ease;
}

.choice--on .choice__check {
  background: var(--c-accent);
  border-color: var(--c-accent);
  color: #fff;
}

.oobe__actions {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  margin-top: var(--sp-4);
  padding-top: var(--sp-4);
  border-top: 1px solid var(--c-border);
  flex: none;
}

.oobe__counter {
  margin-left: auto;
  font-size: var(--fs-xs);
  flex: none;
}

/* 步骤切换：前进从右侧滑入，后退从左侧滑入 */
.step-fwd-enter-active,
.step-back-enter-active {
  transition: opacity 0.2s ease, transform 0.24s cubic-bezier(0.2, 0.9, 0.25, 1);
}

.step-fwd-leave-active,
.step-back-leave-active {
  transition: opacity 0.12s ease, transform 0.12s ease;
}

.step-fwd-enter-from {
  opacity: 0;
  transform: translateX(24px);
}

.step-fwd-leave-to {
  opacity: 0;
  transform: translateX(-14px);
}

.step-back-enter-from {
  opacity: 0;
  transform: translateX(-24px);
}

.step-back-leave-to {
  opacity: 0;
  transform: translateX(14px);
}

@keyframes tile-in {
  from {
    opacity: 0;
    transform: scale(0.86);
  }
}

@keyframes fields-in {
  from {
    opacity: 0;
    transform: translateY(-4px);
  }
}

@keyframes card-in {
  from {
    opacity: 0;
    transform: translateY(8px) scale(0.99);
  }
}

@media (prefers-reduced-motion: reduce) {
  .oobe__tile,
  .oobe__fields,
  .oobe__card--done,
  .oobe__error {
    animation: none;
  }

  .oobe__bar {
    transition: none;
  }

  .step-fwd-enter-active,
  .step-fwd-leave-active,
  .step-back-enter-active,
  .step-back-leave-active {
    transition: opacity 0.12s ease;
  }

  .step-fwd-enter-from,
  .step-fwd-leave-to,
  .step-back-enter-from,
  .step-back-leave-to {
    transform: none;
  }

  .choice:hover {
    transform: none;
  }
}

@media (max-width: 640px) {
  .oobe {
    padding: 68px var(--sp-3) var(--sp-8);
  }

  .oobe__brand {
    top: var(--sp-4);
    left: var(--sp-4);
  }

  .oobe__card {
    padding: var(--sp-5) var(--sp-4) var(--sp-4);
  }

  .oobe__title {
    font-size: var(--fs-lg);
  }
}
</style>

<!-- 页面级滚动条（html / body）不在 scoped 的作用域内，改用挂载期开关的类名控制：
     类名随组件卸载移除，因此不会把"隐藏滚动条"带到其它页面上。 -->
<style>
html.xph-setup-no-scrollbar,
html.xph-setup-no-scrollbar body {
  scrollbar-width: none;
  -ms-overflow-style: none;
}

html.xph-setup-no-scrollbar::-webkit-scrollbar,
html.xph-setup-no-scrollbar body::-webkit-scrollbar {
  width: 0;
  height: 0;
}
</style>
