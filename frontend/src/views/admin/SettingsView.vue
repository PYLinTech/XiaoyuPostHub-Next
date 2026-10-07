<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi, valuesToJsonBlob } from "@/api/endpoints";
import type { SettingsEntry, SettingsListResult } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import Panel from "@/components/admin/Panel.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { topbarSlot } from "@/stores/shell";
import {
  copyText,
  createRequestGate,
  describeError,
  logError,
  toastApiError,
} from "@/lib/async";
import { useOverlayScroll } from "@/lib/overlayScroll";
import { refreshAfterAdminChange } from "@/stores/session";
import { useToasts } from "@/stores/toast";

// 系统配置。
//
// 三项硬约束决定了这一页的形态：
// 1. 只展示管理员真正需要决定的核心项，按分区 + 搜索组织；上传并发、轮询、
//    加密块大小等内部调优已由后端固定为安全默认值；
// 2. 保存只提交改动过的键——后端把"空串"解释为"清除覆盖回到默认"，整体回写
//    会把所有未改动的项一起抹成默认值；
// 3. 敏感项的展示值是掩码，绝不能回填进输入框再提交，否则掩码本身会被当成密钥存下来。

const toasts = useToasts();

const sections = ref<SettingsListResult["sections"]>([]);
const entries = ref<SettingsEntry[]>([]);
const loading = ref(false);
const error = ref("");
const activeSection = ref("");
const keyword = ref("");

/** 编辑草稿。敏感项初始为空串，表示"不修改"。 */
const drafts = ref<Record<string, string>>({});

const saving = ref(false);

function syncDrafts(items: SettingsEntry[]): void {
  const next: Record<string, string> = {};
  for (const entry of items) {
    next[entry.key] = entry.secret ? "" : entry.value;
  }
  drafts.value = next;
}

const gate = createRequestGate();

async function load(): Promise<void> {
  // 序号守卫：save / import / reset 成功后都会重新 load()，顶部「刷新」又可以和
  // 它们并发。少了它，先发后回的那次会用旧配置整表重建 drafts，把管理员刚填的
  // 草稿连同所在分区一起抹掉——而 drafts 只存在于内存，界面上无法察觉。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await adminApi.listSettings();
    if (!gate.isCurrent(token)) return;
    sections.value = result.sections ?? [];
    entries.value = result.items ?? [];
    if (!activeSection.value || !sections.value.some((item) => item.id === activeSection.value)) {
      activeSection.value = sections.value[0]?.id ?? "";
    }
    syncDrafts(entries.value);
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.settings", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

onMounted(load);

// 分区图标：竖排导航比横向页签更适合九个分区——一眼能看完所有分区并
// 直接跳过去，而横向页签在窄屏会被挤进横向滚动条，多一个分区就少一个可见。
const SECTION_ICONS: Record<string, string> = {
  site: "ri-home-smile-line",
  share: "share",
  auth: "user",
  storage: "hardDrive",
  pan123: "cloud",
  delivery: "download",
  upload: "upload",
  archive: "trash",
  mail: "ri-mail-line",
  ops: "settings",
};

function sectionIcon(id: string): string {
  return SECTION_ICONS[id] ?? "settings";
}

// 分区导航在窄屏折成横向滚动条：换成与页签、侧栏同一套自绘指示器
// （见 lib/overlayScroll），否则系统"始终显示滚动条"时会占掉一条高度。
const navScroller = ref<HTMLElement | null>(null);
const {
  visible: navThumbVisible,
  style: navThumbStyle,
  onThumbPointerDown: onNavThumbDown,
  onThumbPointerMove: onNavThumbMove,
  onThumbPointerUp: onNavThumbUp,
} = useOverlayScroll(navScroller, "x");

/** 竖排导航的条目：图标 + 标题。 */
const navItems = computed(() =>
  sections.value.map((item) => ({
    key: item.id,
    label: item.title,
    icon: sectionIcon(item.id),
  })),
);

const matched = computed<SettingsEntry[]>(() => {
  const needle = keyword.value.trim().toLowerCase();
  if (!needle) {
    return entries.value.filter((entry) => entry.section === activeSection.value);
  }
  // 搜索跨全部分区：管理员记得键名却不记得它落在哪个分区是常态。
  return entries.value.filter(
    (entry) =>
      entry.key.toLowerCase().includes(needle) ||
      entry.title.toLowerCase().includes(needle),
  );
});

const visibleSections = computed(() =>
  sections.value
    .map((section) => ({
      id: section.id,
      title: section.title,
      items: matched.value.filter((entry) => entry.section === section.id),
    }))
    .filter((section) => section.items.length > 0),
);

const searching = computed(() => keyword.value.trim().length > 0);

function isDirty(entry: SettingsEntry): boolean {
  const draft = drafts.value[entry.key] ?? "";
  // 敏感项只在填了新值时才提交：留空就是"不修改"。
  return entry.secret ? draft !== "" : draft !== entry.value;
}

const pending = computed(() => entries.value.filter(isDirty));

function resetDraft(key: string): void {
  const entry = entries.value.find((item) => item.key === key);
  if (entry) {
    drafts.value[key] = entry.secret ? "" : entry.value;
  }
}

async function save(): Promise<void> {
  const values: Record<string, string> = {};
  for (const entry of pending.value) {
    values[entry.key] = drafts.value[entry.key] ?? "";
  }
  if (Object.keys(values).length === 0) {
    return;
  }
  saving.value = true;
  try {
    const result = await adminApi.updateSettings(values);
    toasts.success(`已更新 ${result.updated?.length ?? 0} 项配置`);
    await load();
    await refreshAfterAdminChange();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.settings.save", err);
  } finally {
    saving.value = false;
  }
}

// 重置是"整表重建草稿"型操作：load() 里的 syncDrafts 会按最新配置覆盖 drafts 整张表，
// 因此在 A 分区改了 3 项、又到 B 分区改了 2 项，点任意一行的重置图标，另外 4 项
// 未保存的改动会一起消失。这类"顺手丢东西"页面上只剩底部"N 项待保存"一个事后痕迹，
// 而"放弃修改"按钮已经专职承担丢弃改动这件事——所以重置必须自己问一次。
const resetTarget = ref<SettingsEntry | null>(null);
const resetBusy = ref(false);

function askReset(entry: SettingsEntry): void {
  resetTarget.value = entry;
}

async function confirmReset(): Promise<void> {
  const target = resetTarget.value;
  if (!target) {
    return;
  }
  resetBusy.value = true;
  try {
    await adminApi.resetSetting(target.key);
    toasts.success(`已把 ${target.key} 重置为内置默认值`);
    resetTarget.value = null;
    await load();
    await refreshAfterAdminChange();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.settings.reset", err);
  } finally {
    resetBusy.value = false;
  }
}

// ---------------------------------------------------------------- 123 云盘分区

// 该分区完全复刻初始化页存储步骤的布局与控件（显隐密钥、开关 + 回源鉴权地址），
// 让"初始化时怎么填、后来就怎么改"。敏感项语义与通用渲染一致：草稿从空串开始，
// 留空表示不修改。

const PAN_KEYS = {
  clientId: "pan123.client_id",
  clientSecret: "pan123.client_secret",
  rootDirId: "pan123.root_dir_id",
  privateKey: "pan123.private_key",
  authCallback: "pan123.auth_callback",
  directLink: "pan123.direct_link",
} as const;

type PanSecretField = "clientSecret" | "privateKey";
const panSecretVisible = reactive<Record<PanSecretField, boolean>>({
  clientSecret: false,
  privateKey: false,
});

function setPanBool(key: string, checked: boolean): void {
  drafts.value[key] = checked ? "true" : "false";
}

/** 回源鉴权地址：后端固定挂在 /api/cdn/auth，用当前访问源拼出可直接粘贴的完整地址。 */
const cdnAuthURL = computed(() => `${window.location.origin}/api/cdn/auth`);

/** 上游 CDN 只能访问公网地址，本机/内网地址配了也不会生效。 */
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
  }
  return true;
}

const cdnAuthNotPublic = computed(() => !isPublicHost(window.location.hostname));

async function copyCdnAuthURL(): Promise<void> {
  const ok = await copyText(cdnAuthURL.value);
  if (ok) {
    toasts.success("已复制鉴权地址");
  } else {
    toasts.error("复制失败，请手动选中后复制");
  }
}

// ---------------------------------------------------------------- 导出 / 导入

const exportBusy = ref(false);

function downloadJson(values: Record<string, string>, name: string): void {
  const url = URL.createObjectURL(valuesToJsonBlob(values));
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = name;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  // 立刻 revoke 会在部分浏览器里打断正在进行的下载，所以推到下一个任务。
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

async function runExport(): Promise<void> {
  exportBusy.value = true;
  try {
    const result = await adminApi.exportSettings();
    downloadJson(result.values, "xph-settings.json");
    toasts.success("已导出配置（不含敏感项）", `${Object.keys(result.values ?? {}).length} 项`);
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.settings.export", err);
  } finally {
    exportBusy.value = false;
  }
}

const importOpen = ref(false);
const importText = ref("");
const importOverwrite = ref(false);
const importBusy = ref(false);
const importError = ref("");
const importApplied = ref<number | null>(null);

function openImport(): void {
  importText.value = "";
  importOverwrite.value = false;
  importError.value = "";
  importApplied.value = null;
  importOpen.value = true;
}

/** 返回解析结果而不是抛异常：错误要显示在弹窗里，而不是吞掉。 */
function parseImport(raw: string): { values: Record<string, string>; error: string } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    return { values: {}, error: `JSON 解析失败：${err instanceof Error ? err.message : String(err)}` };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { values: {}, error: "顶层必须是对象，形如 {\"values\": {…}} 或直接是键值对" };
  }
  const record = parsed as Record<string, unknown>;
  const keys = Object.keys(record);
  const source =
    keys.length === 1 && keys[0] === "values" && record.values !== null && typeof record.values === "object"
      ? (record.values as Record<string, unknown>)
      : record;

  const values: Record<string, string> = {};
  for (const [key, value] of Object.entries(source)) {
    if (typeof value === "string") {
      values[key] = value;
    } else if (typeof value === "number" || typeof value === "boolean") {
      values[key] = String(value);
    } else {
      return { values: {}, error: `配置项 ${key} 的值必须是字符串、数字或布尔值` };
    }
  }
  if (Object.keys(values).length === 0) {
    return { values: {}, error: "没有可导入的配置项" };
  }
  return { values, error: "" };
}

async function runImport(): Promise<void> {
  const parsed = parseImport(importText.value);
  if (parsed.error) {
    importError.value = parsed.error;
    return;
  }
  importBusy.value = true;
  importError.value = "";
  try {
    const result = await adminApi.importSettings(parsed.values, importOverwrite.value);
    importApplied.value = result.applied ?? 0;
    toasts.success(`已导入 ${importApplied.value} 项配置`);
    await load();
    // 导入的影响面最大（可能一次改掉站点名、注册模式、访客开关），
    // 刷新会话快照这一步在这里最不能省。
    await refreshAfterAdminChange();
  } catch (err) {
    importError.value = describeError(err);
    logError("admin.settings.import", err);
  } finally {
    importBusy.value = false;
  }
}

// ---------------------------------------------------------------- 控件辅助

function kindPlaceholder(entry: SettingsEntry): string {
  if (entry.placeholder) {
    return entry.placeholder;
  }
  switch (entry.kind) {
    case "duration":
      return "例如 15m、24h";
    case "size":
      return "例如 8M、1G";
    case "csv":
      return "多个值用逗号分隔";
    default:
      return "";
  }
}

function enumLabel(entry: SettingsEntry, option: string): string {
  if (entry.key === "auth.register_mode") {
    return { open: "开放注册", invite: "仅邀请码", closed: "关闭注册" }[option] ?? option;
  }
  if (entry.key === "storage.dedup_scope") {
    return { group: "同组共享", global: "全站共享", off: "关闭秒传" }[option] ?? option;
  }
  return option;
}

function enumOptions(entry: SettingsEntry): { value: string; label: string }[] {
  return (entry.enum ?? []).map((option) => ({ value: option, label: enumLabel(entry, option) }));
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <input
        v-model="keyword"
        class="input topbar-search"
        type="search"
        placeholder="搜索"
      />
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load">刷新</AppButton>
      <AppButton size="sm" icon="upload" @click="openImport">导入</AppButton>
      <AppButton size="sm" icon="download" :loading="exportBusy" @click="runExport()">导出</AppButton>
    </Teleport>

    <!-- 搜索态铺满全宽；正常态左导航 + 右内容 -->
    <div class="settings" :class="{ 'settings--searching': searching }">
      <div v-if="!searching" class="ovscroll-layer ovscroll-layer--x settings__nav-layer">
        <nav ref="navScroller" class="ovscroll settings__nav" aria-label="配置分区">
          <button
            v-for="item in navItems"
            :key="item.key"
            type="button"
            class="settings__nav-item"
            :class="{ 'settings__nav-item--on': item.key === activeSection }"
            :aria-current="item.key === activeSection ? 'page' : undefined"
            @click="activeSection = item.key"
          >
            <AppIcon :name="item.icon" :size="16" />
            <span class="truncate">{{ item.label }}</span>
          </button>
        </nav>

        <!-- 轨道挂在不滚动的宿主上而不是滚动容器里：绝对定位的盒子跟着
             内容一起滚，滚出可视区后就再也对不上位置了。 -->
        <div class="ovscroll__track" :class="{ 'ovscroll__track--on': navThumbVisible }">
          <div
            class="ovscroll__thumb"
            :style="navThumbStyle"
            @pointerdown="onNavThumbDown"
            @pointermove="onNavThumbMove"
            @pointerup="onNavThumbUp"
          />
        </div>
      </div>

      <div class="settings__main">
    <section v-for="section in visibleSections" :key="section.id">
      <!-- 123 云盘：完全复刻初始化页存储步骤的布局与控件，搜索态退回通用渲染。 -->
      <Panel v-if="section.id === 'pan123' && !searching" :title="section.title">
        <div class="pan123-form">
          <FormField label="Client ID">
            <input
              v-model="drafts[PAN_KEYS.clientId]"
              class="input mono"
              placeholder="请先购买123云盘开发者权益包"
            />
          </FormField>

          <FormField label="Client Secret">
            <span class="oobe__reveal-wrap">
              <input
                v-model="drafts[PAN_KEYS.clientSecret]"
                class="input mono"
                :type="panSecretVisible.clientSecret ? 'text' : 'password'"
                autocomplete="new-password"
                placeholder="查看站内信获取"
              />
              <button
                type="button"
                class="oobe__reveal"
                :aria-label="panSecretVisible.clientSecret ? '隐藏 Client Secret' : '显示 Client Secret'"
                @click="panSecretVisible.clientSecret = !panSecretVisible.clientSecret"
              >
                <AppIcon :name="panSecretVisible.clientSecret ? 'ri-eye-off-line' : 'eye'" :size="17" />
              </button>
            </span>
          </FormField>

          <FormField label="根目录 ID">
            <input
              v-model="drafts[PAN_KEYS.rootDirId]"
              class="input mono"
              placeholder="进入云盘文件夹后在浏览器链接末尾获得"
            />
          </FormField>

          <FormField label="直链URL鉴权私钥">
            <input v-model="drafts[PAN_KEYS.privateKey]" class="input mono" />
          </FormField>

          <FormField
            label="直链空间"
            hint="勾选并保存后对上方根目录启用直链空间；取消勾选并保存则关闭。启用后才能签发直链加密流量下载地址，关闭后统一走服务器中转通道。"
          >
            <label class="switch">
              <input
                class="switch__input"
                type="checkbox"
                :checked="drafts[PAN_KEYS.directLink] === 'true'"
                @change="setPanBool(PAN_KEYS.directLink, ($event.target as HTMLInputElement).checked)"
              />
              <span class="switch__track"><span class="switch__knob" /></span>
              <span class="switch__label">启用直链空间</span>
            </label>
            <p
              v-if="
                drafts[PAN_KEYS.directLink] === 'true' &&
                (!drafts[PAN_KEYS.rootDirId] || drafts[PAN_KEYS.rootDirId].trim() === '0')
              "
              class="notice notice--warn field-warn"
            >
              网盘根目录（ID 为 0 或留空）不能启用直链空间，请先填写具体的根目录编号。
            </p>
          </FormField>

          <FormField label="回源鉴权">
            <label class="switch">
              <input
                class="switch__input"
                type="checkbox"
                :checked="drafts[PAN_KEYS.authCallback] === 'true'"
                @change="setPanBool(PAN_KEYS.authCallback, ($event.target as HTMLInputElement).checked)"
              />
              <span class="switch__track"><span class="switch__knob" /></span>
              <span class="switch__label">直链启用远程鉴权</span>
            </label>
            <template v-if="drafts[PAN_KEYS.authCallback] === 'true'">
              <div class="oobe__cdn">
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
              </div>
            </template>
          </FormField>
        </div>
      </Panel>

      <!--
        通用分区：每项一行，标题与说明在左、控件在右。
        这条布局取代了原先"每项一张竖排卡片"：配置项之间彼此独立、且大多数
        是一行能读完的短说明，横排能让一屏看到的项数翻几倍，左右两列的对应
        关系也更好扫——竖排堆叠时"哪个说明属于哪个输入框"要靠缩进猜。
      -->
      <Panel v-else :title="section.title">
        <div class="rows">
          <div
            v-for="entry in section.items"
            :key="entry.key"
            class="row"
            :class="{ 'row--dirty': isDirty(entry) }"
          >
            <div class="row__text">
              <div class="row__head">
                <span class="row__label">{{ entry.title }}</span>
                <span v-if="entry.overridden" class="badge badge--accent">已自定义</span>
                <span v-else-if="entry.secret && entry.hasValue" class="badge">已设置</span>
                <code class="row__key mono">{{ entry.key }}</code>
              </div>
              <p v-if="entry.help" class="row__help">{{ entry.help }}</p>
              <p v-if="entry.overridden && entry.defaultValue" class="row__default">
                默认 <span class="mono">{{ entry.defaultValue }}</span>
              </p>
            </div>

            <div class="row__control">
              <!-- string / csv -->
              <input
                v-if="entry.kind === 'string' || entry.kind === 'csv'"
                v-model="drafts[entry.key]"
                class="input"
                :aria-label="entry.title"
                :placeholder="kindPlaceholder(entry)"
              />

              <!-- secret：展示值是掩码，因此草稿从空串开始，留空即不修改。 -->
              <input
                v-else-if="entry.kind === 'secret'"
                v-model="drafts[entry.key]"
                class="input"
                type="password"
                autocomplete="new-password"
                :aria-label="entry.title"
                :placeholder="entry.hasValue ? '留空表示不修改' : '尚未设置'"
              />

              <input
                v-else-if="entry.kind === 'int'"
                v-model="drafts[entry.key]"
                class="input"
                type="number"
                step="1"
                :aria-label="entry.title"
                :min="entry.min || undefined"
                :max="entry.max || undefined"
              />

              <input
                v-else-if="entry.kind === 'duration' || entry.kind === 'size'"
                v-model="drafts[entry.key]"
                class="input"
                :aria-label="entry.title"
                :placeholder="kindPlaceholder(entry)"
              />

              <AppSelect
                v-else-if="entry.kind === 'enum'"
                v-model="drafts[entry.key]"
                :aria-label="entry.title"
                :options="enumOptions(entry)"
              />

              <!-- 布尔项用开关而不是复选框：开关把"开/关"这件事本身画出来了，
                   不用读旁边的「启用」两个字。 -->
              <label v-else-if="entry.kind === 'bool'" class="switch">
                <input
                  class="switch__input"
                  type="checkbox"
                  :checked="drafts[entry.key] === 'true'"
                  @change="drafts[entry.key] = ($event.target as HTMLInputElement).checked ? 'true' : 'false'"
                />
                <span class="switch__track"><span class="switch__knob" /></span>
                <span class="switch__label">{{ drafts[entry.key] === "true" ? "已开启" : "已关闭" }}</span>
              </label>

              <div v-if="entry.warn" class="notice notice--warn row__warn">{{ entry.warn }}</div>
            </div>

            <div class="row__ops">
              <!-- 重置进行期间把行内两个按钮一起锁住：它们都会间接改 drafts
                   （一个重建整表、一个就地还原），与在途的重置并发结果不可预期。 -->
              <AppButton
                v-if="entry.overridden"
                size="sm"
                variant="ghost"
                icon="ri-arrow-go-back-line"
                title="重置为内置默认值"
                :disabled="resetBusy"
                @click="askReset(entry)"
              />
              <AppButton
                v-if="isDirty(entry)"
                size="sm"
                variant="ghost"
                icon="ri-close-line"
                title="撤销本次修改"
                :disabled="resetBusy"
                @click="resetDraft(entry.key)"
              />
            </div>
          </div>
        </div>
      </Panel>
    </section>

    <Panel v-if="visibleSections.length === 0">
      <p class="muted">没有匹配「{{ keyword }}」的配置项。</p>
    </Panel>

      </div>
    </div>

    <div v-if="pending.length > 0" class="admin-sticky-actions">
      <span>{{ pending.length }} 项待保存</span>
      <span class="spacer" />
      <!-- 两个按钮在重置期间都锁掉：「放弃修改」与保存一样会把草稿整体重建，
           管理员刚敲下的内容不该在一次确认还悬着的时候被另一条路径冲掉。 -->
      <AppButton :disabled="saving || resetBusy" @click="load">放弃修改</AppButton>
      <AppButton variant="primary" :loading="saving" :disabled="resetBusy" @click="save">
        保存改动
      </AppButton>
    </div>

    <ConfirmDialog
      :open="!!resetTarget"
      danger
      confirm-text="重置为默认值"
      :loading="resetBusy"
      :message="`把「${resetTarget?.title ?? ''}」重置为内置默认值？`"
      :detail="`只重置 ${resetTarget?.key ?? ''} 这一项，但重置后草稿会按最新配置整体重建，其余分区未保存的改动会一起丢弃。若只是想丢弃改动，请用底部的「放弃修改」。`"
      @confirm="confirmReset"
      @cancel="resetTarget = null"
    />

    <AppModal :open="importOpen" wide title="导入配置" @close="importOpen = false">
      <div class="stack">
        <FormField
          label="配置 JSON"
          hint="可以直接粘贴导出件，也可以粘贴形如 {&quot;site.name&quot;: &quot;示例&quot;} 的键值对。"
        >
          <textarea v-model="importText" class="textarea mono" rows="10" spellcheck="false" />
        </FormField>

        <label class="check">
          <input v-model="importOverwrite" type="checkbox" />
          <span class="check__text">
            <span>已存在的不覆盖</span>
            <span class="faint">只补齐当前未设置的项。</span>
          </span>
        </label>

        <p v-if="importError" class="notice notice--danger">{{ importError }}</p>
        <p v-if="importApplied !== null" class="notice notice--success">
          已应用 {{ importApplied }} 项配置。
        </p>
      </div>

      <template #footer>
        <AppButton :disabled="importBusy" @click="importOpen = false">关闭</AppButton>
        <AppButton variant="primary" :loading="importBusy" @click="runImport">导入</AppButton>
      </template>
    </AppModal>
  </AdminPage>
</template>

<style scoped>
.topbar-search {
  /* 占位只有"搜索"两个字，230px 的空框显得空；收到 140px 仍看得清输入内容。
     字号取 sm 按钮的同款（--fs-xs）：输入框默认的 14/16px 比按钮文字大一截，
     排在一行里像两套体系。高度也与 sm 按钮共用变量——输入框默认按触摸目标
     给到 36/40px，比按钮高一截。padding 归零，文字自己居中。
     flex-basis 取 88px 而不是 140px：折行是按各件的 flex-basis 判定的，若按
     140 算，这一排会为了剩不下的那一个按钮多折一行，留下一条几乎空的行；
     按 88 算就能挤进一行，再由 flex-grow 长回 140（上限锁在 140）。文本框是
     这一排里唯一的弹性项，宽度由它来让最合适。 */
  width: 140px;
  max-width: 140px;
  min-width: 88px;
  flex: 1 1 88px;
  font-size: var(--fs-xs);
  height: var(--control-h-sm);
  min-height: var(--control-h-sm);
  padding-block: 0;
}

/* ---- 两栏骨架：左分区导航 + 右内容 ---- */

.settings {
  display: grid;
  grid-template-columns: 208px minmax(0, 1fr);
  gap: var(--sp-4);
  align-items: start;
}

/* 搜索时铺满：搜索是跨分区的，此时再挂一个只作用于单分区的导航只会误导。 */
.settings--searching {
  grid-template-columns: minmax(0, 1fr);
}

/* 宿主只负责提供定位上下文（.ovscroll-layer 是 position: relative）。
   桌面态本身不滚动，sticky 挂在宿主上而不是 .settings__nav 上：
   窄屏时 .settings__nav 变成横向滚动容器，sticky 必须跟着宿主走。 */
.settings__nav-layer {
  position: sticky;
  top: var(--sp-4);
  min-width: 0;
  /* 圆角裁剪：轨道是方的，不裁就会从导航盒的圆角外侧探出来一截。
     宿主与导航同宽同圆角，裁剪线正好落在边框外沿。 */
  border-radius: var(--r-lg);
  overflow: hidden;
}

.settings__nav {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: var(--sp-2);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  background: var(--c-surface);
}

.settings__nav-item {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  width: 100%;
  padding: 8px 10px;
  border: 0;
  border-radius: var(--r-md);
  background: transparent;
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
  text-align: left;
  cursor: pointer;
  transition: background-color 0.12s ease, color 0.12s ease;
}

.settings__nav-item:hover {
  background: var(--c-hover);
  color: var(--c-text);
}

.settings__nav-item--on {
  background: var(--c-accent-weak);
  color: var(--c-accent);
  font-weight: 600;
}

.settings__main {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
}

/* ---- 行式配置项 ---- */

.rows {
  display: flex;
  flex-direction: column;
}

/* 用分隔线而不是卡片间距分组：配置项之间是并列关系，不是卡片关系，
   一条线既省高度又让"这一屏属于同一个分区"这件事更明确。 */
.row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 260px auto;
  gap: var(--sp-4);
  align-items: start;
  padding: var(--sp-3) 0;
  border-top: 1px solid var(--c-border-line);
}

.row:first-child {
  border-top: 0;
  padding-top: 0;
}

.row:last-child {
  padding-bottom: 0;
}

/* 待保存的行左侧压一道竖线：整页都要滚动才看全，靠底部"待保存 N 项"提示
   才知道改了什么，在当前行就地给一个标记更省事。 */
.row--dirty {
  box-shadow: inset 3px 0 0 var(--c-accent);
  padding-left: var(--sp-3);
  margin-left: calc(var(--sp-3) * -1);
}

.row__text {
  min-width: 0;
}

.row__head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
}

.row__label {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--c-text);
}

/* 键名放在标题旁边：管理员几乎总是拿着键名或从导出的 JSON 里来查，
   让它紧挨着标题比藏进 tooltip 里有用得多。 */
.row__key {
  font-size: 11px;
  color: var(--c-text-faint);
  background: var(--c-surface-2);
  border-radius: var(--r-sm);
  padding: 1px 5px;
}

.row__help {
  margin: var(--sp-1) 0 0;
  color: var(--c-text-muted);
  font-size: var(--fs-xs);
  line-height: 1.55;
}

.row__default {
  margin: var(--sp-1) 0 0;
  color: var(--c-text-faint);
  font-size: 11px;
}

.row__control {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  min-width: 0;
}

.row__control .input {
  width: 100%;
}

.row__warn {
  font-size: var(--fs-xs);
}

.row__ops {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  flex: none;
  min-height: var(--control-h-sm);
}

@media (max-width: 1023px) {
  .settings {
    grid-template-columns: minmax(0, 1fr);
    /* 竖排时两个都是整块的白底描边卡片，16px 的列间距拿来当行间距会显得
       糊在一起（实测底部几乎顶死），上下堆叠需要比左右并排更大的留白。 */
    row-gap: var(--sp-6);
  }

  .settings__nav-layer {
    /* 撤掉 sticky，但必须回 relative 而不是 static：static 会连宿主自带的
       定位上下文一起取消，轨道就找不到近祖先、飘到页面底部去了。 */
    position: relative;
  }

  .settings__nav {
    flex-direction: row;
    overflow-x: auto;
    /* 底部留出指示器浮过的高度：原生条被 .ovscroll 藏掉了，但自绘滑块
       是浮在内容之上的，不留位会压在最后一项的文字上。 */
    padding-bottom: var(--sp-3);
  }

  .settings__nav-item {
    width: auto;
    flex: none;
  }

  /* 窄屏放不下三列，退回"标题在上、控件在下"的竖排，但保留左侧对齐，
     这样输入框仍是一条左边界，不会随着说明文字长短来回跳。 */
  .row {
    grid-template-columns: minmax(0, 1fr) auto;
    grid-template-areas:
      "text ops"
      "control control";
    row-gap: var(--sp-2);
  }

  .row__text { grid-area: text; }
  .row__control { grid-area: control; }
  .row__ops { grid-area: ops; }
}

.field-warn {
  margin-top: var(--sp-2);
}

/* ---- 123 云盘分区：与初始化页存储步骤同款的布局与控件 ---- */

.pan123-form {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
}

.oobe__reveal-wrap {
  position: relative;
  display: block;
}

.oobe__reveal-wrap .input {
  /* 输入框默认是 inline-block，行盒会在其下方多出基线间隙；
     按钮绝对定位按"框内右缘居中"算，必须先去掉这层间隙。 */
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

/* 回源鉴权：开启后才展开，地址是拼好的只读文本 + 复制按钮。 */
.oobe__cdn {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  padding: var(--sp-3);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface-2);
}

.oobe__cdn-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
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

.oobe__spec {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: 6px var(--sp-4);
  margin: 0;
  font-size: var(--fs-xs);
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
</style>
