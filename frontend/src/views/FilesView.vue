<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import AppBreadcrumb from "@/components/ui/AppBreadcrumb.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppModal from "@/components/ui/AppModal.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import FileKindIcon from "@/components/ui/FileKindIcon.vue";
import FolderPicker from "@/components/FolderPicker.vue";
import PreviewOverlay from "@/components/PreviewOverlay.vue";
import { adminApi, fsApi } from "@/api/endpoints";
import { FILE_ARCHIVE, FILE_DISABLED, Perm, type ListNode } from "@/api/types";
import { useDeliveryAction } from "@/delivery/actions";
import { fileDeliverySource } from "@/delivery/sources";
import { copyText, describeError, logError, useAsync } from "@/lib/async";
import { isPreviewable } from "@/lib/filekind";
import { formatBytes, formatRelative, formatTime, pathSegments } from "@/lib/format";
import { useSession } from "@/stores/session";
import { topbarSlot } from "@/stores/shell";
import { enqueueUploads, onUploadComplete, useUploads } from "@/stores/uploads";
import { useToasts } from "@/stores/toast";

// 文件页。
//
// 版面遵循"文件即界面"：页头一行（面包屑 + 过滤 + 操作），下面只有一张列表。
// 统计、容量、进度这些外围信息不再各占一张卡片——容量常驻侧栏，统计降为列表
// 脚注，下载进度只在真的下载时出现。
//
// 交互遵循"高频显形、低频收纳"：
//   · 常态行没有任何按钮；
//   · 悬停显形预览 / 下载 / 分享 / ⋯；
//   · ⋯ 收纳重命名、移动、分享、详情，删除隔离在菜单底部并二次确认；
//   · 勾选后表头原位换成批量条，不插入新卡片把内容推走。
//
// 两条贯穿全页的老原则保持不变：
//   ① 列表刷新只有一条路径（这里的 reload），任何写操作成功后都走它；
//   ② 按名字排序时文件夹优先。

const route = useRoute();
const router = useRouter();
const session = useSession();
const toasts = useToasts();
const downloads = useDeliveryAction();
const uploadState = useUploads();

/** 路由参数可能是 undefined（根目录），一律规范成带前导斜杠的形式。 */
const currentPath = computed(() => {
  const raw = route.params.path;
  const value = Array.isArray(raw) ? raw.join("/") : (raw ?? "");
  const trimmed = String(value).replace(/^\/+|\/+$/g, "");
  return trimmed ? `/${trimmed}` : "/";
});

const segments = computed(() => pathSegments(currentPath.value));
const parentPath = computed(() => (segments.value.length ? segments.value[segments.value.length - 1].path : "/"));

const list = useAsync(() => fsApi.list(currentPath.value), { path: "/", items: [] as ListNode[] });
const stats = useAsync(() => fsApi.stats(currentPath.value), {
  path: "/",
  files: 0,
  folders: 0,
  bytes: 0,
});

const keyword = ref("");
const selection = ref<string[]>([]);

const canUpload = computed(() => session.can(Perm.Upload));
const canManage = computed(() => session.can(Perm.ManageOwnNodes));
const canShare = computed(() => session.can(Perm.Share));

// ---------------------------------------------------------------- 排序

type SortKey = "name" | "size" | "mtime";
const sortKey = ref<SortKey>("name");
const sortAsc = ref(true);

function toggleSort(key: SortKey): void {
  if (sortKey.value === key) {
    sortAsc.value = !sortAsc.value;
    return;
  }
  sortKey.value = key;
  // 时间默认从新到旧，名称与大小默认从低到高——与常见文件管理器一致。
  sortAsc.value = key !== "mtime";
}

/** 状态徽标。随行一起算好，模板只读字段。 */
interface StatusBadge {
  text: string;
  cls: string;
}

/** 列表行 = 接口节点 + 这一行要展示的徽标。 */
interface FileRow extends ListNode {
  badge: StatusBadge | null;
}

/** 文件夹优先，再按所选列排序，并用 numeric 让 v10 排在 v9 之后。 */
const items = computed<FileRow[]>(() => {
  const all = [...list.data.value.items];
  const dir = sortAsc.value ? 1 : -1;
  all.sort((a, b) => {
    if (a.isFolder !== b.isFolder) {
      return a.isFolder ? -1 : 1;
    }
    let delta = 0;
    if (sortKey.value === "size") {
      delta = a.size - b.size;
    } else if (sortKey.value === "mtime") {
      delta = a.mtime - b.mtime;
    } else {
      delta = a.name.localeCompare(b.name, "zh-Hans-CN", { numeric: true });
    }
    if (delta === 0) {
      delta = a.name.localeCompare(b.name, "zh-Hans-CN", { numeric: true });
    }
    return delta * dir;
  });
  const query = keyword.value.trim().toLowerCase();
  const visible = query
    ? all.filter((item) => item.name.toLowerCase().includes(query))
    : all;
  return visible.map((item) => ({ ...item, badge: statusBadge(item) }));
});

const allSelected = computed(
  () => items.value.length > 0 && selection.value.length === items.value.length,
);

function isSelected(path: string): boolean {
  return selection.value.includes(path);
}

/**
 * 批量动作的对象来源。
 *
 * 一律按 selection 取，而不是按 items：items 已经被 keyword 过滤过，而勾选集合
 * 不受搜索词影响（改关键词不会取消勾选，表头写的也始终是"已选 N 项"）。移动与
 * 删除本来就按 selection 执行，下载若从 items 筛，就会只下当前可见的那几项，
 * 还会在完成后 toast 出一个对不上的数量。三个批量动作共用这一份来源，范围才一致。
 */
const selectedItems = computed<ListNode[]>(() => {
  if (selection.value.length === 0) {
    return [];
  }
  const byPath = new Map(list.data.value.items.map((item) => [item.path, item]));
  const picked: ListNode[] = [];
  for (const path of selection.value) {
    const item = byPath.get(path);
    // 目录内容可能在勾选之后被别处改动，查不到就跳过，不让已删的路径混进去。
    if (item) {
      picked.push(item);
    }
  }
  return picked;
});

/** 移动与删除要的是路径，顺序与勾选一致。 */
const selectedPaths = computed(() => selectedItems.value.map((item) => item.path));

async function reload(): Promise<void> {
  await Promise.all([list.run(), stats.run()]);
  // 目录内容变了，之前选中的项可能已经不存在。
  selection.value = selection.value.filter((path) =>
    list.data.value.items.some((item) => item.path === path),
  );
}

// 上传完成后只刷新"当前目录"：后台可能正在上传别的目录的文件，
// 那些目录的列表会在用户切过去时自然刷新。
const offUploadComplete = onUploadComplete((_node, parent) => {
  if (parent === currentPath.value) {
    void reload();
  }
});

onMounted(() => {
  document.addEventListener("click", onDocumentClick);
  document.addEventListener("keydown", onDocumentKeydown);
  void reload();
});

onBeforeUnmount(() => {
  document.removeEventListener("click", onDocumentClick);
  document.removeEventListener("keydown", onDocumentKeydown);
  offUploadComplete();
});

watch(currentPath, () => {
  closeMenu();
  void reload();
});

function toggleAll(): void {
  selection.value = allSelected.value ? [] : items.value.map((item) => item.path);
}

function toggleOne(path: string): void {
  const index = selection.value.indexOf(path);
  if (index >= 0) {
    selection.value.splice(index, 1);
  } else {
    selection.value.push(path);
  }
}

function statusBadge(item: ListNode): StatusBadge | null {
  if (item.status === FILE_DISABLED) {
    return { text: "已停用", cls: "badge--danger" };
  }
  if (item.status === FILE_ARCHIVE) {
    return { text: "待回收", cls: "badge--warn" };
  }
  return null;
}

// ---------------------------------------------------------------- 行内菜单

const menuFor = ref<string | null>(null);

function toggleMenu(path: string): void {
  menuFor.value = menuFor.value === path ? null : path;
}

function closeMenu(): void {
  menuFor.value = null;
}

/** 点击别处关掉菜单：事件绑在 document 上，因此要自己判断落点。 */
function onDocumentClick(event: MouseEvent): void {
  if (!menuFor.value) {
    return;
  }
  const target = event.target as HTMLElement | null;
  if (!target?.closest(".frow__menu-root") && !target?.closest(".fmenu")) {
    closeMenu();
  }
}

/** 键盘也要有出口：只认点击的话，菜单打开后键盘用户没有任何办法关掉它。 */
function onDocumentKeydown(event: KeyboardEvent): void {
  if (event.key === "Escape") {
    closeMenu();
  }
}

// ---------------------------------------------------------------- 导航

function openNode(item: ListNode): void {
  if (item.isFolder) {
    void router.push(`/files${item.path}`);
    return;
  }
  if (isPreviewable(item.name)) {
    previewPath.value = item.path;
    previewName.value = item.name;
    previewOpen.value = true;
    return;
  }
  void downloadOne(item);
}

// ---------------------------------------------------------------- 下载

async function downloadOne(item: ListNode): Promise<void> {
  closeMenu();
  if (item.isFolder) {
    // 后端只按单个对象交付，目录没有可交付的内容。界面上也不该给出这个入口，
    // 这里再挡一次是为了防止将来某处漏判。
    toasts.info("目录不能整体下载，请进入目录逐个下载文件");
    return;
  }
  await downloads.run(fileDeliverySource(item.path, "download"), { fileName: item.name });
}

/**
 * 批量下载。
 *
 * 串行执行而不是并发：每个文件都会走一次完整的准备—取数—结算，
 * 并发会让票据与额度在短时间内成倍占用，也会让"下到第几个了"无法回答。
 *
 * 取源是 selectedItems 而不是 items：带搜索词时 items 只是当前可见的那几项，
 * 而用户勾选的是全部选中项（见 selectedItems 的说明）。
 */
async function downloadSelected(): Promise<void> {
  const targets = selectedItems.value.filter((item) => !item.isFolder);
  if (targets.length === 0) {
    toasts.info("选中的项里没有文件");
    return;
  }
  for (const item of targets) {
    const ok = await downloads.run(fileDeliverySource(item.path, "download"), { silent: true, fileName: item.name });
    if (!ok) {
      toasts.error(`批量下载中断于 ${item.name}`);
      return;
    }
  }
  toasts.success(`已下载 ${targets.length} 个文件`);
}

// ---------------------------------------------------------------- 新建目录

const mkdirOpen = ref(false);
const mkdirName = ref("");
const mkdirBusy = ref(false);

async function doMkdir(): Promise<void> {
  if (!mkdirName.value.trim()) {
    return;
  }
  mkdirBusy.value = true;
  try {
    await fsApi.mkdir(currentPath.value, mkdirName.value.trim());
    toasts.success("已创建");
    mkdirOpen.value = false;
    mkdirName.value = "";
    await reload();
  } catch (err) {
    logError("mkdir", err);
    toasts.error("创建失败", describeError(err));
  } finally {
    mkdirBusy.value = false;
  }
}

// ---------------------------------------------------------------- 重命名

const renameOpen = ref(false);
const renameTarget = ref<ListNode | null>(null);
const renameName = ref("");
const renameBusy = ref(false);

function openRename(item: ListNode): void {
  closeMenu();
  renameTarget.value = item;
  renameName.value = item.name;
  renameOpen.value = true;
}

async function doRename(): Promise<void> {
  const target = renameTarget.value;
  if (!target || !renameName.value.trim()) {
    return;
  }
  renameBusy.value = true;
  try {
    await fsApi.rename(target.path, renameName.value.trim());
    toasts.success("已重命名");
    renameOpen.value = false;
    await reload();
  } catch (err) {
    logError("rename", err);
    toasts.error("重命名失败", describeError(err));
  } finally {
    renameBusy.value = false;
  }
}

// ---------------------------------------------------------------- 移动

const moveOpen = ref(false);
const moveTargets = ref<string[]>([]);

function openMove(paths: string[]): void {
  closeMenu();
  moveTargets.value = paths;
  moveOpen.value = true;
}

async function doMove(dest: string): Promise<void> {
  const targets = moveTargets.value;
  moveOpen.value = false;
  let moved = 0;
  for (const path of targets) {
    try {
      await fsApi.move(path, dest);
      moved += 1;
    } catch (err) {
      logError("move", err);
      toasts.error(`${path} 移动失败`, describeError(err));
    }
  }
  if (moved > 0) {
    toasts.success(`已移动 ${moved} 项`);
    await reload();
  }
}

// ---------------------------------------------------------------- 删除

const deleteOpen = ref(false);
const deleteTargets = ref<string[]>([]);
const deleteBusy = ref(false);

function openDelete(paths: string[]): void {
  closeMenu();
  deleteTargets.value = paths;
  deleteOpen.value = true;
}

async function doDelete(): Promise<void> {
  const targets = deleteTargets.value;
  deleteBusy.value = true;
  let removed = 0;
  try {
    for (const path of targets) {
      try {
        await fsApi.remove(path);
        removed += 1;
      } catch (err) {
        logError("delete", err);
        toasts.error(`${path} 删除失败`, describeError(err));
      }
    }
    if (removed > 0) {
      toasts.success(`已删除 ${removed} 项`);
      selection.value = [];
      await reload();
    }
    deleteOpen.value = false;
  } finally {
    deleteBusy.value = false;
  }
}

// ---------------------------------------------------------------- 上传

const fileInput = ref<HTMLInputElement | null>(null);
const dragActive = ref(false);

function pickFiles(): void {
  fileInput.value?.click();
}

function onFilesPicked(event: Event): void {
  const input = event.target as HTMLInputElement;
  const files = Array.from(input.files ?? []);
  input.value = "";
  if (files.length > 0) {
    enqueueUploads(files, currentPath.value, "rename");
    toasts.info(`已加入上传队列（${files.length} 个文件）`);
  }
}

function onDrop(event: DragEvent): void {
  dragActive.value = false;
  if (!canUpload.value) {
    toasts.error("当前账号没有上传权限");
    return;
  }
  const files = Array.from(event.dataTransfer?.files ?? []);
  if (files.length > 0) {
    enqueueUploads(files, currentPath.value, "rename");
    toasts.info(`已加入上传队列（${files.length} 个文件）`);
  }
}

/** 没有上传权限就不亮遮罩：否则用户会一直看到"松开即可上传"，松手才被告知没权限。 */
function onDragOver(): void {
  if (!canUpload.value) {
    return;
  }
  dragActive.value = true;
}

/**
 * dragleave 在指针从容器移向任一子元素时也会冒泡上来（事件目标是容器）。
 * 只有真正离开整个容器才撤遮罩，否则遮罩会在掠过行、按钮时闪一下。
 */
function onDragLeave(event: DragEvent): void {
  const container = event.currentTarget as HTMLElement | null;
  const next = event.relatedTarget as Node | null;
  // relatedTarget 为空表示指针离开了窗口，按离开处理。
  if (container && next && container.contains(next)) {
    return;
  }
  dragActive.value = false;
}

// ---------------------------------------------------------------- 详情与拉黑

const detailOpen = ref(false);
const detailItem = ref<FileRow | null>(null);
const blacklistOpen = ref(false);
const blacklistReason = ref("");
const blacklistBusy = ref(false);

function openDetail(item: FileRow): void {
  closeMenu();
  detailItem.value = item;
  detailOpen.value = true;
}

/**
 * 停用确认从详情弹窗里发起，因此必须先把详情关掉。
 *
 * 两个 AppModal 会各自在 document 上监听 Esc，同时打开时一次 Esc 会把两个一起
 * 关掉——用户在确认框上想"取消"，结果连详情也一起没了。
 */
function openBlacklist(): void {
  detailOpen.value = false;
  blacklistOpen.value = true;
}

async function copyChecksum(): Promise<void> {
  const checksum = detailItem.value?.checksum;
  if (!checksum) {
    return;
  }
  const ok = await copyText(checksum);
  if (ok) {
    toasts.success("已复制校验码");
  } else {
    toasts.error("复制失败");
  }
}

async function doBlacklist(): Promise<void> {
  const checksum = detailItem.value?.checksum;
  if (!checksum) {
    return;
  }
  blacklistBusy.value = true;
  try {
    await adminApi.setFileStatus(checksum, true, blacklistReason.value.trim());
    toasts.success("该内容对象已停用");
    blacklistOpen.value = false;
    blacklistReason.value = "";
    detailOpen.value = false;
    await reload();
  } catch (err) {
    logError("blacklist", err);
    toasts.error("停用失败", describeError(err));
  } finally {
    blacklistBusy.value = false;
  }
}

// ---------------------------------------------------------------- 预览与分享

const previewOpen = ref(false);
const previewPath = ref("");
const previewName = ref("");

const previewSource = computed(() =>
  previewPath.value ? fileDeliverySource(previewPath.value, "preview") : null,
);

async function shareItem(item: ListNode): Promise<void> {
  closeMenu();
  await router.push({
    path: "/shares",
    query: { create: item.path, kind: item.isFolder ? "folder" : "file" },
  });
}
</script>

<template>
  <div
    class="files"
    :class="{ 'files--drop': dragActive }"
    @dragover.prevent="onDragOver"
    @dragleave="onDragLeave"
    @drop.prevent="onDrop"
  >
    <h1 class="sr-only">文件</h1>

    <div class="files__head">
      <AppBreadcrumb
        :segments="segments"
        @navigate="router.push(`/files${$event === '/' ? '' : $event}`)"
      />
      <input v-model="keyword" class="input files__filter" placeholder="搜索当前目录" />
    </div>

    <!-- 操作项投递到顶栏：不在页内再占一行，位置与其他页面统一 -->
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <div class="files__head-actions">
        <input ref="fileInput" type="file" multiple class="sr-only" @change="onFilesPicked" />
        <AppButton v-if="canUpload" size="sm" variant="primary" icon="upload" @click="pickFiles">
          上传
        </AppButton>
        <AppButton v-if="canManage" size="sm" icon="plus" @click="mkdirOpen = true">新建文件夹</AppButton>
        <AppButton size="sm" icon="refresh" :loading="list.loading.value" title="刷新" @click="reload()" />
      </div>
    </Teleport>

    <p v-if="list.error.value" class="notice notice--danger">{{ list.error.value }}</p>
    <!-- 统计走另一个接口，它挂掉时列表本身照常显示，页脚却会退回初始值写成
         "0 个文件 · 0 个文件夹 · 共 0 B"——那是一条明确的错误信息，不能当统计读。
         因此在这里单独提示一次，而不是让页脚沉默。 -->
    <p v-if="stats.error.value" class="notice notice--danger">读取统计失败：{{ stats.error.value }}</p>

    <div class="card files__list">
      <!-- 表头：未选中时是列名，选中后原位换成批量条 -->
      <div v-if="selection.length === 0" class="fhead fgrid">
        <label class="fcell fcell--check">
          <input type="checkbox" :checked="allSelected" aria-label="全选" @change="toggleAll" />
        </label>
        <span class="fcell">
          <button
            type="button"
            class="fsort"
            :class="{ 'fsort--active': sortKey === 'name' }"
            @click="toggleSort('name')"
          >
            名称
            <AppIcon
              v-if="sortKey === 'name'"
              :name="sortAsc ? 'ri-arrow-up-s-line' : 'ri-arrow-down-s-line'"
              :size="14"
            />
          </button>
        </span>
        <span class="fcell fcell--right">
          <button
            type="button"
            class="fsort"
            :class="{ 'fsort--active': sortKey === 'size' }"
            @click="toggleSort('size')"
          >
            大小
            <AppIcon
              v-if="sortKey === 'size'"
              :name="sortAsc ? 'ri-arrow-up-s-line' : 'ri-arrow-down-s-line'"
              :size="14"
            />
          </button>
        </span>
        <span class="fcell fcell--time">
          <button
            type="button"
            class="fsort"
            :class="{ 'fsort--active': sortKey === 'mtime' }"
            @click="toggleSort('mtime')"
          >
            修改时间
            <AppIcon
              v-if="sortKey === 'mtime'"
              :name="sortAsc ? 'ri-arrow-up-s-line' : 'ri-arrow-down-s-line'"
              :size="14"
            />
          </button>
        </span>
        <span class="fcell" />
      </div>

      <div v-else class="fhead fhead--bulk">
        <span class="fbulk__count">已选 {{ selection.length }} 项</span>
        <div class="fbulk__actions">
          <AppButton size="sm" variant="ghost" icon="download" @click="downloadSelected">下载文件</AppButton>
          <AppButton v-if="canManage" size="sm" variant="ghost" icon="move" @click="openMove(selectedPaths)">
            移动
          </AppButton>
          <AppButton
            v-if="canManage"
            size="sm"
            variant="ghost"
            icon="trash"
            @click="openDelete(selectedPaths)"
          >
            删除
          </AppButton>
          <span class="fbulk__sep" />
          <AppButton size="sm" variant="ghost" @click="selection = []">取消</AppButton>
        </div>
      </div>

      <div v-if="list.loading.value && items.length === 0" class="fempty">
        <span class="spinner" />
        <p class="faint">正在加载</p>
      </div>

      <AppEmpty
        v-else-if="items.length === 0"
        :icon="keyword ? 'search' : 'folder'"
        :title="keyword ? '没有匹配的项目' : '这个目录是空的'"
        :hint="
          keyword
            ? '换个关键词试试'
            : canUpload
              ? '把文件拖到这里，或点右上角的「上传」'
              : undefined
        "
      />

      <template v-else>
        <div
          v-for="row in items"
          :key="row.path"
          class="frow fgrid"
          :class="{ 'frow--selected': isSelected(row.path), 'frow--menu': menuFor === row.path }"
        >
          <label class="fcell fcell--check">
            <input
              type="checkbox"
              :checked="isSelected(row.path)"
              :aria-label="`选择 ${row.name}`"
              @change="toggleOne(row.path)"
            />
          </label>

          <div class="fcell frow__name">
            <button type="button" class="frow__name-btn" @click="openNode(row)">
              <FileKindIcon :name="row.name" :is-folder="row.isFolder" />
              <span class="truncate">{{ row.name }}</span>
            </button>
            <span
              v-if="row.badge"
              class="badge"
              :class="row.badge.cls"
              :title="row.disableReason"
            >
              {{ row.badge.text }}
            </span>
          </div>

          <div class="fcell fcell--right frow__meta mono">
            {{ row.isFolder ? "—" : formatBytes(row.size) }}
          </div>
          <div class="fcell fcell--time frow__meta nowrap" :title="formatTime(row.mtime)">
            {{ formatRelative(row.mtime) }}
          </div>

          <div class="fcell frow__actions">
            <button
              v-if="!row.isFolder && isPreviewable(row.name)"
              type="button"
              class="iconbtn"
              title="预览"
              @click="openNode(row)"
            >
              <AppIcon name="eye" :size="17" />
            </button>
            <button v-if="!row.isFolder" type="button" class="iconbtn" title="下载" @click="downloadOne(row)">
              <AppIcon name="download" :size="17" />
            </button>
            <button v-if="canShare" type="button" class="iconbtn" title="创建分享" @click="shareItem(row)">
              <AppIcon name="share" :size="17" />
            </button>
            <span v-if="canManage || canShare" class="frow__menu-root">
              <button
                type="button"
                class="iconbtn"
                title="更多"
                aria-haspopup="menu"
                :aria-expanded="menuFor === row.path"
                @click.stop="toggleMenu(row.path)"
              >
                <AppIcon name="ri-more-fill" :size="18" />
              </button>
              <div v-if="menuFor === row.path" class="fmenu">
                <button v-if="canManage" type="button" class="fmenu__item" @click="openRename(row)">
                  <AppIcon name="edit" :size="16" />
                  重命名
                </button>
                <button v-if="canManage" type="button" class="fmenu__item" @click="openMove([row.path])">
                  <AppIcon name="move" :size="16" />
                  移动到…
                </button>
                <button v-if="canShare" type="button" class="fmenu__item" @click="shareItem(row)">
                  <AppIcon name="share" :size="16" />
                  创建分享
                </button>
                <button type="button" class="fmenu__item" @click="openDetail(row)">
                  <AppIcon name="info" :size="16" />
                  详情
                </button>
                <template v-if="canManage">
                  <hr class="fmenu__sep" />
                  <button
                    type="button"
                    class="fmenu__item fmenu__item--danger"
                    @click="openDelete([row.path])"
                  >
                    <AppIcon name="trash" :size="16" />
                    删除
                  </button>
                </template>
              </div>
            </span>
          </div>
        </div>
      </template>

      <div class="ffoot">
        <span>
          {{ stats.data.value.files }} 个文件 · {{ stats.data.value.folders }} 个文件夹 · 共
          {{ formatBytes(stats.data.value.bytes) }}
        </span>
        <span class="spacer" />
        <span v-if="keyword">匹配 {{ items.length }} 项</span>
        <span v-if="uploadState.activeCount.value > 0" class="badge badge--accent">
          上传中 {{ uploadState.activeCount.value }}
        </span>
      </div>
    </div>

    <!-- 新建文件夹 -->
    <AppModal :open="mkdirOpen" title="新建文件夹" @close="mkdirOpen = false">
      <FormField label="名称" hint="不能包含斜杠或控制字符">
        <input v-model="mkdirName" class="input" autofocus @keyup.enter="doMkdir" />
      </FormField>
      <template #footer>
        <AppButton @click="mkdirOpen = false">取消</AppButton>
        <AppButton variant="primary" :loading="mkdirBusy" @click="doMkdir">创建</AppButton>
      </template>
    </AppModal>

    <!-- 重命名 -->
    <AppModal :open="renameOpen" title="重命名" @close="renameOpen = false">
      <FormField label="新名称">
        <input v-model="renameName" class="input" autofocus @keyup.enter="doRename" />
      </FormField>
      <template #footer>
        <AppButton @click="renameOpen = false">取消</AppButton>
        <AppButton variant="primary" :loading="renameBusy" @click="doRename">保存</AppButton>
      </template>
    </AppModal>

    <!-- 移动 -->
    <FolderPicker
      :open="moveOpen"
      title="移动到"
      :initial-path="parentPath"
      :exclude-path="moveTargets[0]"
      @close="moveOpen = false"
      @select="doMove"
    />

    <!-- 删除确认 -->
    <ConfirmDialog
      :open="deleteOpen"
      danger
      :message="`将删除 ${deleteTargets.length} 项，文件夹会连同其子项一起删除。`"
      detail="删除后可在「归档」中恢复。共享内容不受影响：仍被他人引用的对象不会被回收。"
      confirm-text="删除"
      :loading="deleteBusy"
      @cancel="deleteOpen = false"
      @confirm="doDelete"
    />

    <!-- 详情 -->
    <AppModal :open="detailOpen" title="文件详情" @close="detailOpen = false">
      <dl v-if="detailItem" class="kv">
        <dt>名称</dt>
        <dd>{{ detailItem.name }}</dd>
        <dt>路径</dt>
        <dd class="mono">{{ detailItem.path }}</dd>
        <dt>类型</dt>
        <dd>{{ detailItem.isFolder ? "文件夹" : "文件" }}</dd>
        <dt>大小</dt>
        <dd>{{ detailItem.isFolder ? "—" : formatBytes(detailItem.size) }}</dd>
        <dt>修改时间</dt>
        <dd>{{ formatTime(detailItem.mtime) }}</dd>
        <template v-if="detailItem.checksum">
          <dt>内容校验码</dt>
          <dd class="mono" style="word-break: break-all">
            {{ detailItem.checksum }}
            <AppButton size="sm" variant="ghost" icon="copy" title="复制" @click="copyChecksum" />
          </dd>
          <dt>对象状态</dt>
          <dd>
            <span v-if="detailItem.badge" class="badge" :class="detailItem.badge.cls">
              {{ detailItem.badge.text }}
            </span>
            <span v-else class="badge badge--success">正常</span>
          </dd>
        </template>
      </dl>
      <template #footer>
        <AppButton
          v-if="session.can(Perm.AdminFiles) && detailItem?.checksum"
          variant="danger"
          icon="shield"
          @click="openBlacklist()"
        >
          停用该内容对象
        </AppButton>
        <AppButton variant="primary" @click="detailOpen = false">关闭</AppButton>
      </template>
    </AppModal>

    <!-- 拉黑对象 -->
    <ConfirmDialog
      :open="blacklistOpen"
      danger
      title="停用内容对象"
      :message="`将停用校验码为 ${detailItem?.checksum?.slice(0, 16)}… 的内容对象。`"
      detail="全局生效：所有引用该对象的账号都会受影响，未过期的下载票据同时吊销。"
      confirm-text="停用"
      :loading="blacklistBusy"
      @cancel="blacklistOpen = false"
      @confirm="doBlacklist"
    />

    <PreviewOverlay
      :open="previewOpen"
      :source="previewSource"
      :file-name="previewName"
      @close="previewOpen = false"
    />
  </div>
</template>

<style scoped>
.files {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

.files--drop::after {
  content: "松开即可上传到当前目录";
  position: fixed;
  inset: var(--sp-4);
  border: 2px dashed var(--c-accent);
  border-radius: var(--r-lg);
  background: color-mix(in srgb, var(--c-accent-weak) 70%, transparent);
  color: var(--c-accent);
  display: flex;
  align-items: center;
  justify-content: center;
  font-weight: 650;
  font-size: var(--fs-lg);
  z-index: 30;
  pointer-events: none;
}

/* ---------------------------------------------------------------- 页头 */

/* 面包屑与搜索同一行，都不换行：面包屑让出宽度（它自己会优先保住最深的目录），
   搜索框固定宽度不吃伸缩。 */
.files__head {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  min-height: 40px;
  min-width: 0;
}

.files__head :deep(.breadcrumb) {
  flex: 1 1 auto;
  min-width: 0;
}

.files__head-actions {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex-wrap: wrap;
}

/* 尺寸贴着面包屑走：比正文输入框矮一档，才是"同一行的搜索"。 */
.files__filter {
  flex: none;
  width: 160px;
  min-height: 28px;
  padding: 0 10px;
  font-size: var(--fs-sm);
}

/* ---------------------------------------------------------------- 列表 */

.files__list {
  overflow: visible;
}

/* 一份栅格定义同时给表头与行用，列宽不会走偏。 */
.fgrid {
  display: grid;
  grid-template-columns: 40px minmax(0, 1fr) 96px 124px 116px;
  align-items: center;
}

.fcell {
  padding: 0 var(--sp-3);
  min-width: 0;
}

.fcell--check {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 0;
}

.fcell--check input {
  width: 16px;
  height: 16px;
  margin: 0;
  accent-color: var(--c-accent);
  cursor: pointer;
}

.fcell--right {
  text-align: right;
}

.fhead {
  height: 38px;
  border-bottom: 1px solid var(--c-border);
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
}

.fsort {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  border: 0;
  background: transparent;
  padding: 2px 4px;
  margin-left: -4px;
  border-radius: var(--r-sm);
  font: inherit;
  color: inherit;
  cursor: pointer;
}

.fsort:hover {
  background: var(--c-hover);
  color: var(--c-text);
}

.fsort--active {
  color: var(--c-text);
  font-weight: 600;
}

.fcell--right .fsort {
  margin: 0 -4px 0 0;
}

/* 批量模式：表头原位换掉，不插入新卡片把内容推走。 */
.fhead--bulk {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
  padding-right: var(--sp-3);
  background: var(--c-accent-weak);
  color: var(--c-accent);
}

.fbulk__count {
  padding-left: var(--sp-4);
  font-weight: 600;
}

.fbulk__actions {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  flex-wrap: wrap;
}

.fbulk__sep {
  width: 1px;
  height: 18px;
  background: var(--c-border-strong);
  margin: 0 var(--sp-1);
}

.frow {
  height: 46px;
  border-bottom: 1px solid var(--c-border-line);
  position: relative;
}

.frow:last-of-type {
  border-bottom: 0;
}

.frow:hover {
  background: var(--c-surface-2);
}

.frow--selected {
  background: var(--c-accent-weak);
}

.frow--selected .frow__name-btn {
  color: var(--c-accent);
}

.frow__name {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.frow__name-btn {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  border: 0;
  background: transparent;
  cursor: pointer;
  padding: 2px 4px;
  margin-left: -4px;
  border-radius: var(--r-sm);
  color: var(--c-text);
  font-size: var(--fs-sm);
  font-weight: 550;
  min-width: 0;
  text-align: left;
}

.frow__name-btn:hover {
  background: var(--c-hover);
}

.frow__meta {
  font-size: var(--fs-sm);
  color: var(--c-text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
}

.frow__actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 2px;
  opacity: 0;
  pointer-events: none;
  transition: opacity 0.12s ease;
}

.frow:hover .frow__actions,
.frow--menu .frow__actions {
  opacity: 1;
  pointer-events: auto;
}

/* 触摸设备没有悬停：按钮常显，否则操作入口等于不存在。 */
@media (hover: none) {
  .frow__actions {
    opacity: 1;
    pointer-events: auto;
  }
}

.iconbtn {
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
}

.iconbtn:hover {
  background: var(--c-hover);
  color: var(--c-text);
}

.frow__menu-root {
  position: relative;
  display: inline-flex;
}

.fmenu {
  position: absolute;
  right: 0;
  top: calc(100% + 2px);
  z-index: 40;
  min-width: 176px;
  padding: var(--sp-1);
  display: flex;
  flex-direction: column;
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  box-shadow: var(--shadow-lg);
}

.fmenu__item {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  width: 100%;
  border: 0;
  background: transparent;
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-sm);
  font-size: var(--fs-sm);
  text-align: left;
  color: var(--c-text);
  cursor: pointer;
}

.fmenu__item:hover {
  background: var(--c-hover);
}

.fmenu__item--danger {
  color: var(--c-danger);
}

.fmenu__item--danger:hover {
  background: var(--c-danger-weak);
}

.fmenu__sep {
  border: 0;
  border-top: 1px solid var(--c-border);
  margin: var(--sp-1) 0;
}

/* 只剩"正在加载"在用：空态已换成全站统一的 AppEmpty，
   这里不再手写一份图标 + 标题 + 说明的组合。 */
.fempty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-10) var(--sp-4);
  color: var(--c-text-faint);
}

.ffoot {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-3) var(--sp-4);
  border-top: 1px solid var(--c-border);
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
  flex-wrap: wrap;
}

/* 窄屏：先收时间列，再收大小列；操作列永远保留。 */
@media (max-width: 880px) {
  .fgrid {
    grid-template-columns: 40px minmax(0, 1fr) 88px 108px;
  }

  .fcell--time {
    display: none;
  }
}

@media (max-width: 640px) {
  .fgrid {
    grid-template-columns: 40px minmax(0, 1fr) 96px;
  }

  .fcell--right {
    display: none;
  }

  .files__filter {
    width: 120px;
  }

  .fhead--bulk {
    height: auto;
    padding: var(--sp-2) var(--sp-3);
    flex-wrap: wrap;
  }

  .fbulk__count {
    padding-left: 0;
  }
}
</style>
