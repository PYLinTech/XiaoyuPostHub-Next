<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import FileKindIcon from "@/components/ui/FileKindIcon.vue";
import { openFilePreview } from "@/stores/preview";
import { ApiError } from "@/api/client";
import { guestApi } from "@/api/endpoints";
import type { ListNode, GuestShare } from "@/api/types";
import { useDeliveryAction } from "@/delivery/actions";
import { shareDeliverySource } from "@/delivery/sources";
import { createRequestGate, describeError, logError } from "@/lib/async";
import { formatBytes, formatTime, pathSegments } from "@/lib/format";
import { topbarSlot } from "@/stores/shell";

const route = useRoute();
const router = useRouter();

const shareId = computed(() => String(route.params.id ?? ""));
const password = ref("");
const resolving = ref(false);
const failed = ref(false);
const resolveError = ref("");
const share = ref<GuestShare | null>(null);

const { busy: downloading, run: runDownload } = useDeliveryAction();

// ---------------------------------------------------------------- 目录浏览

const relPath = ref("");
const entries = ref<ListNode[]>([]);
const dirLoading = ref(false);
const dirError = ref("");

const dirRows = computed(
  () =>
    entries.value.map((node) => ({
      node,
      sizeLabel: node.isFolder ? "—" : formatBytes(node.size),
      mtimeLabel: formatTime(node.mtime),
    })),
);

const crumbs = computed(() => pathSegments(relPath.value));

const displayName = computed(() => share.value?.rootName ?? "");

const remainingVisits = computed(() => {
  const current = share.value;
  if (!current || current.maxVisits <= 0) {
    return "不限";
  }
  return String(Math.max(0, current.maxVisits - current.visits));
});

const expiresLabel = computed(() => {
  const expiresAt = share.value?.expiresAt ?? 0;
  return expiresAt > 0 ? `有效期至 ${formatTime(expiresAt)}` : "永久有效";
});

/** 交付接口要的是"相对分享根"的路径。用节点自带的绝对路径做前缀裁剪，
 *  在根为 "/" 或带尾斜杠时容易算错，因此直接按层级拼。 */
function joinRel(base: string, name: string): string {
  return base === "" ? `/${name}` : `${base}/${name}`;
}

const canPreviewRoot = computed(() => share.value?.allowPreview === true);
const canDownloadRoot = computed(() => share.value?.allowDownload === true);

function shareErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError) || err.status !== 403) return describeError(err);
  const detail = err.detail ?? "";
  if (detail.includes("停用")) return "该分享已停用，请联系分享者确认。";
  if (detail.includes("过期") && !detail.includes("或")) return "该分享已过期，请联系分享者重新分享。";
  if (detail.includes("访问上限") && !detail.includes("或")) return "该分享的访问次数已用完，请联系分享者。";
  if (detail.includes("需要登录")) return "请登录后查看此分享。";
  if (detail.includes("仅创建者")) return "该分享仅分享者本人可查看。";
  if (detail.includes("提取码不正确")) return "请输入正确的提取码后重试。";
  if (detail.includes("内容已不存在")) return "分享内容已移除，请联系分享者。";
  return "暂时无法访问此分享，请联系分享者确认链接和访问权限。";
}

// 解析与列目录各自要一份守卫：它们是两个请求，可能同时在飞。
const resolveGate = createRequestGate();
const dirGate = createRequestGate();

async function resolve(): Promise<void> {
  const token = resolveGate.next();
  resolving.value = true;
  resolveError.value = "";
  try {
    const result = await guestApi.resolveShare(shareId.value, { password: password.value });
    if (!resolveGate.isCurrent(token)) return;
    share.value = result.share;
    failed.value = false;
    if (result.share.kind === "folder") {
      relPath.value = "";
      await loadDir();
    }
  } catch (err) {
    if (!resolveGate.isCurrent(token)) return;
    logError("share-resolve", err);
    share.value = null;
    failed.value = true;
    resolveError.value = shareErrorMessage(err);
  } finally {
    if (!resolveGate.isCurrent(token)) return;
    resolving.value = false;
  }
}

// 注意：列目录在后端也要走一次"解析分享"，因此每进一层目录都会消耗一次访问
// 次数（与下载、预览共用同一个计数器）。
async function loadDir(): Promise<void> {
  const current = share.value;
  if (!current || current.kind !== "folder") {
    return;
  }
  // 连点面包屑、或在深层目录里连点"返回上级"，请求会交叉：后返回的那一次会把
  // 上一层的内容留在当前路径下，目录和内容对不上。序号守卫只让最后一次写回。
  const token = dirGate.next();
  dirLoading.value = true;
  dirError.value = "";
  try {
    const result = await guestApi.listShare(current.id, {
      password: password.value,
      relPath: relPath.value,
    });
    if (!dirGate.isCurrent(token)) return;
    entries.value = result.items ?? [];
  } catch (err) {
    if (!dirGate.isCurrent(token)) return;
    logError("share-list", err);
    entries.value = [];
    dirError.value = shareErrorMessage(err);
  } finally {
    if (!dirGate.isCurrent(token)) return;
    dirLoading.value = false;
  }
}

function goTo(path: string): void {
  relPath.value = path === "/" ? "" : path;
  void loadDir();
}

function enterFolder(node: ListNode): void {
  relPath.value = joinRel(relPath.value, node.name);
  void loadDir();
}

// ---------------------------------------------------------------- 预览与下载

function openPreview(rel: string, name: string): void {
  const current = share.value;
  if (!current) return;
  openFilePreview({ fileName: name,
    source: shareDeliverySource(current.id, rel, password.value, "preview"),
    downloadSource: shareDeliverySource(current.id, rel, password.value, "download"),
    previewAllowed: current.allowPreview, downloadAllowed: current.allowDownload });
}

async function download(rel: string): Promise<void> {
  const current = share.value;
  if (!current) {
    return;
  }
  await runDownload(shareDeliverySource(current.id, rel, password.value, "download"), { fileName: rel.split("/").filter(Boolean).pop() ?? current.rootName });
}

/** 文件名统一打开预览弹窗；权限决定内容和下载入口。 */
function openEntry(node: ListNode): void {
  if (node.isFolder) { enterFolder(node); return; }
  openPreview(joinRel(relPath.value, node.name), node.name);
}

// 同路由换分享 id（/s/A → /s/B，浏览器前进/后退）不会重建这个组件，只在
// onMounted 解析一次会让页面停在上一条分享上。盯住 id 重新解析；换 id 等价于
// 换一条分享，因此先把上一条的解析结果与提取码清掉。
watch(
  shareId,
  () => {
    dirGate.next();
    dirLoading.value = false;
    relPath.value = "";
    share.value = null;
    entries.value = [];
    dirError.value = "";
    password.value = "";
    void resolve();
  },
  { immediate: true },
);
</script>

<template>
  <div class="stack">
    <div v-if="resolving" class="card">
      <div class="card__body row">
        <span class="spinner" />
        <span class="muted">正在加载分享</span>
      </div>
    </div>

    <!-- 无法访问时保留提取码和登录入口，方便重新验证。 -->
    <div v-else-if="failed" class="card share__gate">
      <div class="card__body stack">
        <div class="row">
          <AppIcon name="lock" :size="22" />
          <h1 class="page-head__title">打开分享</h1>
        </div>

        <p v-if="resolveError" class="notice notice--danger">{{ resolveError }}</p>

        <form class="stack" @submit.prevent="resolve()">
          <input
            v-model="password"
            class="input"
            type="password"
            placeholder="提取码"
            autocomplete="off"
            aria-label="提取码"
          />
          <AppButton type="submit" variant="primary" block :loading="resolving">打开分享</AppButton>
        </form>

        <div class="row">
          <AppButton size="sm" icon="key" @click="router.push('/pickup')">使用取件码</AppButton>
          <AppButton
            size="sm"
            icon="user"
            @click="router.push({ path: '/login', query: { next: route.fullPath } })"
          >
            登录
          </AppButton>
        </div>
      </div>
    </div>

    <template v-else-if="share">
      <Teleport v-if="topbarSlot" :to="topbarSlot">
        <AppButton size="sm" icon="key" @click="router.push('/pickup')">取件码</AppButton>
        <button v-if="share.kind === 'folder'" class="btn btn--sm share-refresh" type="button"
          :disabled="dirLoading" :aria-busy="dirLoading" title="刷新文件列表" @click="loadDir()">
          <AppIcon name="refresh" :size="14" :class="{ 'share-refresh__spin': dirLoading }" /><span>刷新</span>
        </button>
      </Teleport>
      <section class="share-card" :class="{ 'share-card--file': share.kind === 'file' }">
        <header class="share-heading">
          <h1 class="share-title"><button v-if="share.kind === 'file'" type="button" class="share-file-name" @click="openPreview('', displayName)">{{ displayName }}</button><template v-else>{{ displayName }}</template></h1>
          <div class="share-meta">
            <span v-if="share.kind === 'file'">{{ formatBytes(share.size) }}</span>
            <span v-if="share.sharerName"><AppIcon name="ri-account-circle-line" :size="15" />{{ share.sharerName }} 分享</span>
            <span><AppIcon name="ri-time-line" :size="15" />{{ expiresLabel }}</span>
            <span v-if="share.maxVisits > 0">剩余 {{ remainingVisits }} 次访问</span>
          </div>
        </header>
        <div v-if="share.kind === 'file'" class="share-file stack">
          <div class="row share-file__actions">
            <AppButton v-if="canDownloadRoot" variant="primary" icon="download" :loading="downloading" @click="download('')">下载</AppButton>
            <AppButton v-if="canPreviewRoot" :variant="canDownloadRoot ? 'default' : 'primary'" icon="eye" @click="openPreview('', displayName)">预览</AppButton>
          </div>
          <p v-if="!canPreviewRoot && !canDownloadRoot" class="notice notice--warn">因分享者设置，此分享不可预览或下载。</p>
        </div>
        <template v-else>
          <div class="share-browser-head">
            <nav class="share-crumbs" aria-label="分享目录">
              <span v-if="!relPath">全部文件</span>
              <template v-else>
                <button type="button" @click="goTo('/')">{{ displayName }}</button>
                <template v-for="(crumb, index) in crumbs" :key="crumb.path">
                  <span class="share-crumbs__sep">/</span>
                  <span v-if="index === crumbs.length - 1" aria-current="page">{{ crumb.name }}</span>
                  <button v-else type="button" @click="goTo(crumb.path)">{{ crumb.name }}</button>
                </template>
              </template>
            </nav>
            <span v-if="!dirLoading && !dirError" class="share-count">{{ entries.length }} 项</span>
          </div>
          <p v-if="dirError" class="notice notice--danger share-notice">{{ dirError }}</p>
          <div v-if="dirLoading" class="share-state row" role="status"><span class="spinner" /><span class="muted">正在加载文件</span></div>
          <div v-else-if="!dirError && !entries.length" class="share-state muted">暂无可访问的文件</div>
          <table v-else-if="!dirError" class="share-table">
            <thead><tr><th scope="col">名称</th><th scope="col" class="num">大小</th><th scope="col">修改时间</th><th scope="col"><span class="sr-only">文件操作</span></th></tr></thead>
            <tbody>
              <tr v-for="row in dirRows" :key="row.node.path">
                <td class="share-table__name">
                  <button type="button" class="share-entry" @click="openEntry(row.node)">
                    <FileKindIcon :name="row.node.name" :is-folder="row.node.isFolder" :size="20" /><span>{{ row.node.name }}</span>
                  </button>
                </td>
                <td class="share-table__size num">{{ row.sizeLabel }}</td><td class="share-table__time">{{ row.mtimeLabel }}</td>
                <td class="share-table__actions">
                  <div v-if="!row.node.isFolder" class="actions">
                    <AppButton v-if="canPreviewRoot" size="sm" icon="eye" @click="openPreview(joinRel(relPath, row.node.name), row.node.name)">预览</AppButton>
                    <AppButton v-if="canDownloadRoot" size="sm" icon="download" :disabled="downloading" @click="download(joinRel(relPath, row.node.name))">下载</AppButton>
                    <span v-if="!canPreviewRoot && !canDownloadRoot" class="muted">未开放预览和下载</span>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </template>
      </section>
    </template>

  </div>
</template>

<style scoped>
.share__gate { max-width: 480px; margin: 0 auto; }
.share-card { background: var(--c-surface); border: 1px solid var(--c-border); border-radius: var(--r-md); overflow: hidden; }
.share-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 20px; padding: 24px; }
.share-title { flex: 1; min-width: 0; font-size: 24px; font-weight: 650; line-height: 1.45; overflow-wrap: anywhere; }
.share-meta { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 12px 16px; max-width: 48%; margin-top: 5px; color: var(--c-text-muted); font-size: 12px; }
.share-meta > span { display: inline-flex; align-items: center; gap: 6px; overflow-wrap: anywhere; }
.share-card--file { padding: 32px; }
.share-card--file .share-heading { padding: 0; }
.share-file { margin-top: 28px; }
.share-file__actions { gap: 12px; }
.share-file__actions .btn { min-width: 104px; min-height: 38px; justify-content: center; }
.share-browser-head { display: flex; align-items: center; gap: 16px; padding: 16px 20px; border-top: 1px solid var(--c-border); border-bottom: 1px solid var(--c-border); }
.share-crumbs { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; min-width: 0; font-size: 14px; font-weight: 600; overflow-wrap: anywhere; }
.share-crumbs button { border: 0; background: transparent; padding: 0; font: inherit; color: var(--c-text); cursor: pointer; overflow-wrap: anywhere; }
.share-crumbs button:hover { color: var(--c-accent); }
.share-crumbs__sep { color: var(--c-text-muted); font-weight: 400; }
.share-count { margin-left: auto; flex-shrink: 0; font-size: 12px; color: var(--c-text-muted); }
.share-notice { margin: 16px 20px; }
.share-state { padding: 32px 20px; justify-content: center; text-align: center; }
.share-table { width: 100%; border-collapse: collapse; font-size: 14px; }
.share-table th { padding: 10px 20px; text-align: left; background: var(--c-surface-2); color: var(--c-text-muted); font-size: 12px; font-weight: 600; }
.share-table th:first-child { width: 44%; }
.share-table th:last-child { width: 20%; }
.share-table td { padding: 18px 20px; border-top: 1px solid var(--c-border); }
.share-table .num { text-align: right; }
.share-table__size, .share-table__time { font-size: 13px; white-space: nowrap; color: var(--c-text-muted); font-variant-numeric: tabular-nums; }
.share-table tbody tr:hover { background: var(--c-surface-2); }
.share-entry { display: inline-flex; align-items: center; gap: 12px; max-width: 100%; min-width: 0; border: 0; background: transparent; padding: 7px 8px; margin: -7px -8px; border-radius: 5px; font: inherit; text-align: left; cursor: pointer; transition: background .15s, color .15s; }
.share-entry > span { overflow-wrap: anywhere; }
.share-entry :deep(.file-icon) { flex-shrink: 0; }
.share-entry:hover { background: var(--c-hover); color: var(--c-accent); }
.share-entry:hover :deep(.file-icon) { color: var(--c-accent); }
.share-table__actions .actions { display: flex; align-items: center; justify-content: flex-end; flex-wrap: nowrap; gap: 8px; }
.share-refresh { display: inline-flex; align-items: center; gap: 6px; }
.share-refresh__spin { animation: share-refresh-spin .7s linear infinite; }
@keyframes share-refresh-spin { to { transform: rotate(360deg); } }
@media (max-width: 720px) {
  .share-heading { flex-wrap: wrap; padding: 20px 16px; gap: 12px; }
  .share-title { flex-basis: 100%; font-size: 21px; }
  .share-meta { max-width: 100%; margin: 0 0 0 auto; }
  .share-card--file { padding: 24px 20px; }
  .share-file__actions { flex-wrap: nowrap; }
  .share-file__actions .btn { flex: 1; min-width: 0; }
  .share-browser-head { padding: 14px 16px; }
  .share-table, .share-table tbody { display: block; width: 100%; }
  .share-table thead { display: none; }
  .share-table tbody tr { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 8px 12px; align-items: center; padding: 16px; border-top: 1px solid var(--c-border); }
  .share-table tbody tr:first-child { border-top: 0; }
  .share-table td { display: block; padding: 0; border: 0; min-width: 0; text-align: left; }
  .share-table__name { grid-column: 1 / -1; }
  .share-table__size { grid-column: 1; }
  .share-table__time { grid-column: 2; white-space: normal; font-size: 12px; }
  .share-table__actions { grid-column: 1 / -1; }
  .share-table__actions .actions { margin-top: 6px; width: 100%; }
  .share-table__actions .btn { flex: 1; min-width: 0; min-height: 36px; justify-content: center; }
  .share-table__actions:empty { display: none; }
}
@media (prefers-reduced-motion: reduce) {
  .share-refresh__spin { animation: none; }
  .share-entry { transition: none; }
}

.share-file-name { font: inherit; color: inherit; text-align: left; background: none; border: 0; padding: 0; cursor: pointer; overflow-wrap: anywhere; }
.share-file-name:hover { color: var(--c-accent); }
</style>
