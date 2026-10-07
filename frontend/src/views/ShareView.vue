<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import AppBreadcrumb from "@/components/ui/AppBreadcrumb.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppProgress from "@/components/ui/AppProgress.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import FileKindIcon from "@/components/ui/FileKindIcon.vue";
import PreviewOverlay from "@/components/PreviewOverlay.vue";
import { ApiError } from "@/api/client";
import { guestApi } from "@/api/endpoints";
import { FILE_DISABLED, type DeliveryTarget, type ListNode, type Share } from "@/api/types";
import { useDeliveryAction } from "@/delivery/actions";
import { shareDeliverySource } from "@/delivery/sources";
import { createRequestGate, describeError, logError } from "@/lib/async";
import { baseName, formatBytes, formatTime, parentPath, pathSegments } from "@/lib/format";
import { isPreviewable } from "@/lib/filekind";
import { useSession } from "@/stores/session";
import { topbarSlot } from "@/stores/shell";
import { useToasts } from "@/stores/toast";

// 分享访客页。
//
// 登录用户与访客共用同一套界面：后端交付时不为身份改写路径，界面上多一层
// "你已登录所以能看到更多"的说明只会误导人。区别只在配额归属。
//
// 解析失败一律当作"需要提取码"处理——后端对不存在 / 已停用 / 已过期 /
// 达上限 / 提取码错误统一回 403，把它们区分开等于提供了一个可以逐个探测
// 分享 id 是否有效的接口。

const route = useRoute();
const router = useRouter();
const session = useSession();
const toasts = useToasts();

const shareId = computed(() => String(route.params.id ?? ""));
const password = ref("");
const resolving = ref(false);
const failed = ref(false);
const resolveError = ref("");
const share = ref<Share | null>(null);
const target = ref<DeliveryTarget | null>(null);

const { busy: downloading, progress: downloadProgress, run: runDownload } = useDeliveryAction();

// ---------------------------------------------------------------- 目录浏览

const relPath = ref("");
const entries = ref<ListNode[]>([]);
const dirLoading = ref(false);
const dirError = ref("");

const dirColumns: Column[] = [
  { key: "name", label: "名称", mobile: "title" },
  { key: "size", label: "大小", align: "right" },
  { key: "mtime", label: "修改时间" },
  { key: "actions", label: "操作" },
];

const dirRows = computed(
  () =>
    entries.value.map((node) => ({
      node,
      sizeLabel: node.isFolder ? "—" : formatBytes(node.size),
      mtimeLabel: formatTime(node.mtime),
    })),
);

const crumbs = computed(() => pathSegments(relPath.value));
const rootLabel = computed(() => share.value?.rootName ?? "分享根目录");

const displayName = computed(() => {
  if (!share.value) {
    return "";
  }
  const root = share.value.rootName ?? "";
  return share.value.kind === "file"
    ? baseName(target.value?.path ?? root)
    : root;
});

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

const progressRatio = computed(() => {
  const info = downloadProgress.value;
  if (!info || info.bytesTotal <= 0) {
    return null;
  }
  return info.bytesDone / info.bytesTotal;
});

/** 交付接口要的是"相对分享根"的路径。用节点自带的绝对路径做前缀裁剪，
 *  在根为 "/" 或带尾斜杠时容易算错，因此直接按层级拼。 */
function joinRel(base: string, name: string): string {
  return base === "" ? `/${name}` : `${base}/${name}`;
}

function canPreviewName(name: string, allowed: boolean, disabled: boolean): boolean {
  return allowed && !disabled && isPreviewable(name);
}

function canDownloadEntry(allowed: boolean, disabled: boolean): boolean {
  return allowed && !disabled;
}

// 注意：这里刻意不看 session.can()。未登录访客的 permissions 是 0（bootstrap 只在
// 有令牌时才拉 profile），据此判断会把分享页的主要用途——匿名下载——整个关掉。
// 权威开关是分享自身的 allowDownload / allowPreview，站点级的访客开关由后端拒绝。

const canPreviewRoot = computed(() =>
  canPreviewName(displayName.value, share.value?.allowPreview === true, false),
);
const canDownloadRoot = computed(() => canDownloadEntry(share.value?.allowDownload === true, false));

function canPreviewNode(node: ListNode): boolean {
  return canPreviewName(node.name, share.value?.allowPreview === true, isDisabled(node));
}
function canDownloadNode(node: ListNode): boolean {
  return canDownloadEntry(share.value?.allowDownload === true, isDisabled(node));
}

function isDisabled(node: ListNode): boolean {
  return node.status === FILE_DISABLED;
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
    target.value = result.target;
    failed.value = false;
    if (result.share.kind === "folder") {
      relPath.value = "";
      await loadDir();
    }
  } catch (err) {
    if (!resolveGate.isCurrent(token)) return;
    logError("share-resolve", err);
    share.value = null;
    target.value = null;
    failed.value = true;
    // 403 就是"没通过"，用中性文案；其余（限流、网络）才把服务端的话透出来。
    resolveError.value =
      err instanceof ApiError && err.status === 403 ? "" : describeError(err);
  } finally {
    if (!resolveGate.isCurrent(token)) return;
    resolving.value = false;
  }
}

// 注意：列目录在后端也要走一次"解析分享"，因此每进一层目录都会消耗一次访问
// 次数（与下载、预览共用同一个计数器）。界面不隐藏刷新，但把它标成有代价的操作。
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
    dirError.value = describeError(err);
  } finally {
    if (!dirGate.isCurrent(token)) return;
    dirLoading.value = false;
  }
}

function goTo(path: string): void {
  relPath.value = path === "/" ? "" : path;
  void loadDir();
}

function goUp(): void {
  const parent = parentPath(relPath.value);
  goTo(parent === "/" ? "/" : parent);
}

function enterFolder(node: ListNode): void {
  if (!share.value?.allowSubpath) {
    toasts.info("这条分享不允许浏览子目录，只能查看根目录这一层");
    return;
  }
  relPath.value = joinRel(relPath.value, node.name);
  void loadDir();
}

// ---------------------------------------------------------------- 预览与下载

const previewOpen = ref(false);
const previewRelPath = ref("");
const previewName = ref("");

const previewSource = computed(() => {
  const current = share.value;
  return current
    ? shareDeliverySource(current.id, previewRelPath.value, password.value, "preview")
    : null;
});

function openPreview(rel: string, name: string): void {
  previewRelPath.value = rel;
  previewName.value = name;
  previewOpen.value = true;
}

async function download(rel: string): Promise<void> {
  const current = share.value;
  if (!current) {
    return;
  }
  await runDownload(shareDeliverySource(current.id, rel, password.value, "download"));
}

/** 点击条目：目录就进目录，文件优先预览、其次下载——与文件页的手感一致。 */
function openEntry(node: ListNode): void {
  if (node.isFolder) {
    enterFolder(node);
    return;
  }
  const rel = joinRel(relPath.value, node.name);
  if (canPreviewNode(node)) {
    openPreview(rel, node.name);
    return;
  }
  if (canDownloadNode(node)) {
    void download(rel);
    return;
  }
  // "已停用"与"没开放预览/下载"是两回事：表格里这条节点已经标了停用，再提示
  // "没有开放预览或下载"会让人以为是分享者的权限设置，改错地方。
  if (isDisabled(node)) {
    toasts.info("这个文件已停用，无法访问");
    return;
  }
  toasts.info("这条分享没有开放预览或下载");
}

// 同路由换分享 id（/s/A → /s/B，浏览器前进/后退）不会重建这个组件，只在
// onMounted 解析一次会让页面停在上一条分享上。盯住 id 重新解析；换 id 等价于
// 换一条分享，因此先把上一条的解析结果与提取码清掉。
watch(
  shareId,
  () => {
    share.value = null;
    target.value = null;
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
        <span class="muted">正在解析分享</span>
      </div>
    </div>

    <!-- 解析失败：给提取码输入框，而不是"分享不存在"这种结论。 -->
    <div v-else-if="failed" class="card share__gate">
      <div class="card__body stack">
        <div class="row">
          <AppIcon name="lock" :size="22" />
          <h1 class="page-head__title">需要提取码</h1>
        </div>
        <p class="muted">该分享需要提取码，或已不可用。请向分享者确认后重试。</p>
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
          <AppButton size="sm" icon="key" @click="router.push('/pickup')">用取件码提取</AppButton>
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
      <div class="page-head">
        <div style="min-width: 0">
          <h1 class="page-head__title row" style="gap: var(--sp-2)">
            <FileKindIcon
              :name="displayName"
              :is-folder="share.kind === 'folder'"
              :size="22"
            />
            <span class="truncate">{{ displayName }}</span>
          </h1>
          <p class="page-head__sub">
            {{ session.state.siteName }} 的{{ share.kind === "folder" ? "文件夹" : "文件" }}分享
          </p>
        </div>
        <Teleport v-if="topbarSlot" :to="topbarSlot">
          <AppButton size="sm" icon="key" @click="router.push('/pickup')">取件码</AppButton>
        </Teleport>
      </div>

      <div class="card">
        <div class="card__body stack">
          <dl class="kv">
            <dt>有效期</dt>
            <dd>{{ expiresLabel }}</dd>
            <dt>剩余访问次数</dt>
            <dd>{{ remainingVisits }}</dd>
          </dl>
          <div class="row">
            <span class="badge" :class="share.allowDownload ? 'badge--success' : 'badge--danger'">
              下载：{{ share.allowDownload ? "允许" : "关闭" }}
            </span>
            <span class="badge" :class="share.allowPreview ? 'badge--success' : 'badge--danger'">
              预览：{{ share.allowPreview ? "允许" : "关闭" }}
            </span>
            <span v-if="share.kind === 'folder'" class="badge">
              子目录：{{ share.allowSubpath ? "允许" : "关闭" }}
            </span>
          </div>
        </div>
      </div>

      <!-- 单文件分享 -->
      <div v-if="share.kind === 'file'" class="card">
        <div class="card__body stack">
          <div class="row" style="min-width: 0">
            <FileKindIcon :name="displayName" :size="22" />
            <strong class="truncate">{{ displayName }}</strong>
          </div>

          <div class="row">
            <AppButton
              v-if="canPreviewRoot"
              variant="primary"
              icon="eye"
              @click="openPreview('', displayName)"
            >
              预览
            </AppButton>
            <AppButton
              v-if="canDownloadRoot"
              :variant="canPreviewRoot ? 'default' : 'primary'"
              icon="download"
              :loading="downloading"
              @click="download('')"
            >
              下载
            </AppButton>
          </div>

          <AppProgress
            v-if="downloading"
            :ratio="progressRatio"
            :bytes-done="downloadProgress?.bytesDone"
            :bytes-total="downloadProgress?.bytesTotal"
            :label="downloadProgress?.message"
          />

          <p v-if="!canPreviewRoot && !canDownloadRoot" class="notice notice--warn">
            这条分享既没有开放预览也没有开放下载，请联系分享者调整设置。
          </p>
          <p
            v-else-if="!share.allowPreview && share.allowDownload"
            class="faint"
            style="font-size: var(--fs-xs)"
          >
            分享者关闭了在线预览，只能下载后查看。
          </p>
        </div>
      </div>

      <!-- 文件夹分享 -->
      <div v-else class="card">
        <div class="card__head">
          <AppBreadcrumb :segments="crumbs" :root-label="rootLabel" @navigate="goTo" />
          <div class="row">
            <AppButton v-if="relPath" size="sm" icon="chevronLeft" @click="goUp()">
              返回上级
            </AppButton>
            <AppButton
              size="sm"
              icon="refresh"
              title="刷新会再消耗一次访问次数"
              :loading="dirLoading"
              @click="loadDir()"
            >
              刷新
            </AppButton>
          </div>
        </div>

        <div v-if="dirError" class="card__body">
          <p class="notice notice--danger">{{ dirError }}</p>
        </div>
        <div v-else-if="!share.allowSubpath" class="card__body">
          <p class="notice notice--info">这条分享只开放根目录，子目录无法进入。</p>
        </div>

        <div class="card__body card__body--flush">
          <AppTable
            :columns="dirColumns"
            :rows="dirRows"
            :loading="dirLoading"
            empty-title="这里是空的"
            empty-hint="分享者在根目录下没有放东西。"
          >
            <template #name="{ row }">
              <div class="row" style="min-width: 0">
                <button type="button" class="entry" @click="openEntry(row.node)">
                  <FileKindIcon
                    :name="row.node.name"
                    :is-folder="row.node.isFolder"
                  />
                  <span class="truncate">{{ row.node.name }}</span>
                </button>
                <span v-if="isDisabled(row.node)" class="badge badge--danger">已停用</span>
              </div>
            </template>

            <template #size="{ row }">{{ row.sizeLabel }}</template>
            <template #mtime="{ row }">{{ row.mtimeLabel }}</template>

            <template #actions="{ row }">
              <div class="actions">
                <AppButton
                  v-if="row.node.isFolder"
                  size="sm"
                  icon="chevronRight"
                  :disabled="!share.allowSubpath"
                  @click="enterFolder(row.node)"
                >
                  进入
                </AppButton>
                <template v-else>
                  <AppButton
                    v-if="canPreviewNode(row.node)"
                    size="sm"
                    icon="eye"
                    @click="
                      openPreview(
                        joinRel(relPath, row.node.name),
                        row.node.name,
                      )
                    "
                  >
                    预览
                  </AppButton>
                  <AppButton
                    v-if="canDownloadNode(row.node)"
                    size="sm"
                    icon="download"
                    @click="download(joinRel(relPath, row.node.name))"
                  >
                    下载
                  </AppButton>
                  <span
                    v-if="!canPreviewNode(row.node) && !canDownloadNode(row.node)"
                    class="faint"
                    style="font-size: var(--fs-xs)"
                  >
                    不可用
                  </span>
                </template>
              </div>
            </template>
          </AppTable>
        </div>
      </div>
    </template>

    <PreviewOverlay
      :open="previewOpen"
      :source="previewSource"
      :file-name="previewName"
      @close="previewOpen = false"
    />
  </div>
</template>

<style scoped>
.share__gate {
  max-width: 480px;
  margin: 0 auto;
}

.entry {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  border: 0;
  background: transparent;
  padding: 0;
  cursor: pointer;
  color: var(--c-text);
  font: inherit;
  min-width: 0;
  text-align: left;
}

.entry:hover {
  color: var(--c-accent);
}

@media (max-width: 720px) {
  .actions {
    justify-content: flex-start;
  }
}
</style>
