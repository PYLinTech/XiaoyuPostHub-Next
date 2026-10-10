<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import AppDateTimePicker from "@/components/ui/AppDateTimePicker.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FileKindIcon from "@/components/ui/FileKindIcon.vue";
import FormField from "@/components/ui/FormField.vue";
import { shareApi, type UpdateShareInput } from "@/api/endpoints";
import {
  Perm,
  type AccessMode,
  type AuditLog,
  type PickupCode,
  type Share,
  type ShareKind,
} from "@/api/types";
import { copyText, createRequestGate, describeError, logError, useAsync } from "@/lib/async";
import { dateTimeLocalToUnix, formatTime, unixToDateTimeLocal } from "@/lib/format";
import { useSession } from "@/stores/session";
import { topbarSlot } from "@/stores/shell";
import { useToasts } from "@/stores/toast";

// 分享管理。
//
// 三块内容各自独立取数（列表 / 访问记录 / 取件码），因为它们对应的接口
// 分组不同、失败也互不影响：取件码拉不到不该让整个列表变成错误页。

const route = useRoute();
const router = useRouter();
const session = useSession();
const toasts = useToasts();

const ACCESS_LABELS: Record<AccessMode, string> = {
  public: "公开",
  password: "提取码",
  login: "需登录",
  restricted: "仅自己",
};

const ACCESS_HINTS: Record<AccessMode, string> = {
  public: "拿到链接的任何人都能打开。",
  password: "需要输入提取码才能打开。",
  login: "必须登录本站账号才能打开。",
  restricted: "当前只对创建者本人开放。",
};

const ACTION_LABELS: Record<string, string> = {
  open: "打开",
  list: "浏览目录",
  pickup: "取件码提取",
  download: "下载",
  preview: "预览",
};

/** 表单下拉的选项：文案直接复用上面两张表，避免同一份中文写两遍。 */
const KIND_OPTIONS = (["file", "folder"] as const).map((value) => ({
  value,
  label: kindLabel(value),
}));

const ACCESS_OPTIONS = (Object.keys(ACCESS_LABELS) as AccessMode[]).map((value) => ({
  value,
  label: ACCESS_LABELS[value],
}));

// 行是纯展示映射：状态、次数、有效期都在映射时算好，模板里不再做判断。
interface ShareRow {  share: Share;
  statusLabel: string;
  statusClass: string;
  visitsLabel: string;
  expiresLabel: string;
  createdLabel: string;
}

const canCreate = computed(() => session.state.authenticated && session.can(Perm.Share));
const canPickup = computed(() => session.can(Perm.Pickup));

const limit = ref(50);
const offset = ref(0);

const {
  data: shareData,
  loading: listLoading,
  error: listError,
  run: loadShares,
} = useAsync(() => shareApi.list(limit.value, offset.value), { items: [] as Share[] });

// 后端的列表接口只回 items、不回总数，因此分页只能"有下一页就翻"。
watch([limit, offset], () => {
  void loadShares();
});

/** 不精确分页的"还有下一页"信号：列表接口不回总数，只能拿本页取回的条数推断——
 *  等于 limit 说明后面还有，少于 limit 才能断定已经到末页。不给这个信号，翻到底
 *  之后"下一页"仍然可点，点进去是一页空数据。 */
const hasMore = computed(() => shareData.value.items.length >= limit.value);

/** 改每页条数时必须把 offset 归零：它是按旧 limit 累计的，留在原处会落到新页长
 *  的中段，甚至越过末尾变成一页空数据。 */
function changeLimit(next: number): void {
  limit.value = next;
  offset.value = 0;
}

const rows = computed(
  () => shareData.value.items.map(toRow),
);

function toRow(share: Share): ShareRow {
  // expiresAt 是可选字段：0 与缺失都表示"永久"，先归一再用。
  const expiresAt = share.expiresAt ?? 0;
  const expired = expiresAt > 0 && expiresAt <= Math.floor(Date.now() / 1000);
  return {
    share,
    statusLabel: share.disabled ? "已停用" : expired ? "已过期" : "正常",
    statusClass: share.disabled
      ? "badge--danger"
      : expired
        ? "badge--warn"
        : "badge--success",
    // 有上限时才显示 "已用 / 上限"：0 是"不限"，写成 0/0 只会让人以为配额是零。
    visitsLabel: share.maxVisits > 0 ? `${share.visits} / ${share.maxVisits}` : String(share.visits),
    expiresLabel: expiresAt > 0 ? formatTime(expiresAt) : "永久",
    createdLabel: formatTime(share.createdAt),
  };
}

function kindLabel(kind: ShareKind): string {
  return kind === "folder" ? "文件夹" : "文件";
}

async function copyLink(share: Share): Promise<void> {
  const ok = await copyText(`${window.location.origin}/s/${share.id}`);
  if (ok) {
    toasts.success("分享链接已复制");
  } else {
    toasts.error("复制失败", "浏览器不允许自动复制，请手动选中地址栏中的链接");
  }
}

// ---------------------------------------------------------------- 新建 / 编辑

const formOpen = ref(false);
const editing = ref<Share | null>(null);
const saving = ref(false);
const formError = ref("");

const form = reactive({
  path: "",
  kind: "file" as ShareKind,
  accessMode: "public" as AccessMode,
  password: "",
  allowDownload: true,
  allowPreview: true,
  allowSubpath: true,
  showSharerName: true,
  expiresAtLocal: "",
  maxVisits: "0",
  disabled: false,
});

function openCreate(path: string, kind: ShareKind): void {
  editing.value = null;
  Object.assign(form, {
    path,
    kind,
    accessMode: "public" as AccessMode,
    password: "",
    allowDownload: true,
    allowPreview: true,
    allowSubpath: true,
    showSharerName: true,
    expiresAtLocal: "",
    maxVisits: "0",
    disabled: false,
  });
  formError.value = "";
  formOpen.value = true;
}

function openEdit(share: Share): void {
  editing.value = share;
  Object.assign(form, {
    path: share.rootPath,
    kind: share.kind,
    accessMode: share.accessMode,
    // 后端只回 hasPassword，不回明文；留空即"不改提取码"。
    password: "",
    allowDownload: share.allowDownload,
    allowPreview: share.allowPreview,
    allowSubpath: share.allowSubpath,
    showSharerName: share.showSharerName,
    expiresAtLocal: unixToDateTimeLocal(share.expiresAt),
    maxVisits: String(share.maxVisits),
    disabled: share.disabled,
  });
  formError.value = "";
  formOpen.value = true;
}

/** 表单里选的过期时间是否已经过去：0 表示留空（永久），不参与判断。 */
const expiresInPast = computed(() => {
  const expiresAt = dateTimeLocalToUnix(form.expiresAtLocal);
  return expiresAt > 0 && expiresAt <= Math.floor(Date.now() / 1000);
});

/** 编辑时只提交真正改动过的字段：后端用指针区分"改"与"不改"，整体回写会把
 *  maxVisits、expiresAt 这类字段用零值覆盖掉。 */
function buildPatch(original: Share, expiresAt: number, maxVisits: number): UpdateShareInput {
  const patch: UpdateShareInput = {};
  if (form.showSharerName !== original.showSharerName) {
    patch.showSharerName = form.showSharerName;
  }
  // expiresAt 是可选字段，缺失即"永久"，而表单留空读出来是 0。直接拿它和
  // original.expiresAt 比，0 !== undefined 恒真，于是每次保存都会多带一个
  // expiresAt: 0，与这个函数声明的"不回写零值"相悖。先归一，缺失与 0 在
  // 比较时才是同一个意思。
  const originalExpiresAt = original.expiresAt ?? 0;
  if (form.accessMode !== original.accessMode) {
    patch.accessMode = form.accessMode;
    if (form.accessMode !== "password") {
      // 从提取码模式切走时顺手清掉散列：否则 hasPassword 会一直是真，
      // 界面显示"提取码：无"却还留着一条谁也说不出的旧码。
      patch.password = "";
    }
  }
  if (form.accessMode === "password" && form.password.trim() !== "") {
    patch.password = form.password.trim();
  }
  if (form.allowDownload !== original.allowDownload) {
    patch.allowDownload = form.allowDownload;
  }
  if (form.allowPreview !== original.allowPreview) {
    patch.allowPreview = form.allowPreview;
  }
  if (form.kind === "folder" && form.allowSubpath !== original.allowSubpath) {
    patch.allowSubpath = form.allowSubpath;
  }
  if (expiresAt !== originalExpiresAt) {
    patch.expiresAt = expiresAt;
  }
  if (maxVisits !== original.maxVisits) {
    patch.maxVisits = maxVisits;
  }
  if (form.disabled !== original.disabled) {
    patch.disabled = form.disabled;
  }
  return patch;
}

async function submit(): Promise<void> {
  formError.value = "";
  const expiresAt = dateTimeLocalToUnix(form.expiresAtLocal);
  const parsedVisits = Number.parseInt(form.maxVisits, 10);
  const maxVisits = Number.isFinite(parsedVisits) && parsedVisits > 0 ? parsedVisits : 0;
  const original = editing.value;

  if (!original && form.path.trim() === "") {
    formError.value = "请填写要分享的路径";
    return;
  }
  // 密码模式必须有提取码，否则这条分享建出来谁都打不开。
  if (form.accessMode === "password" && form.password.trim() === "") {
    const keepExisting = original?.hasPassword === true && original.accessMode === "password";
    if (!keepExisting) {
      formError.value = "提取码模式必须填写提取码";
      return;
    }
  }

  saving.value = true;
  try {
    if (!original) {
      await shareApi.create({
        path: form.path.trim(),
        kind: form.kind,
        accessMode: form.accessMode,
        password: form.accessMode === "password" ? form.password.trim() : undefined,
        allowDownload: form.allowDownload,
        allowPreview: form.allowPreview,
        allowSubpath: form.kind === "folder" && form.allowSubpath,
        showSharerName: form.showSharerName,
        expiresAt,
        maxVisits,
      });
      toasts.success("已创建分享");
    } else {
      const patch = buildPatch(original, expiresAt, maxVisits);
      if (Object.keys(patch).length > 0) {
        await shareApi.update(original.id, patch);
        toasts.success("已保存修改");
      } else {
        // 改了又改回原值时 patch 是空的：直接关窗会让人以为已经保存过。
        toasts.info("没有需要保存的改动");
      }
    }
    formOpen.value = false;
    if (!original && offset.value > 0) {
      // 新建的分享排在最前，留在后面的页上会找不到它；改 offset 会触发重载。
      offset.value = 0;
    } else {
      await loadShares();
    }
  } catch (err) {
    logError("share-save", err);
    formError.value = describeError(err);
  } finally {
    saving.value = false;
  }
}

// ---------------------------------------------------------------- 删除

const deleting = ref<Share | null>(null);
const deleteBusy = ref(false);

async function confirmDelete(): Promise<void> {
  const target = deleting.value;
  if (!target) {
    return;
  }
  deleteBusy.value = true;
  try {
    await shareApi.remove(target.id);
    toasts.success("已删除分享");
    deleting.value = null;
    if (shareData.value.items.length <= 1 && offset.value > 0) {
      // 删掉本页最后一条时往回退一页，否则会停在一个空页上。
      offset.value = Math.max(0, offset.value - limit.value);
    } else {
      await loadShares();
    }
  } catch (err) {
    logError("share-delete", err);
    toasts.error("删除失败", describeError(err));
  } finally {
    deleteBusy.value = false;
  }
}

// ---------------------------------------------------------------- 访问记录

const accessesOpen = ref(false);
const accessesBusy = ref(false);
const accesses = ref<AuditLog[]>([]);
const accessesTitle = ref("");

const accessColumns: Column[] = [
  { key: "action", label: "动作", mobile: "title" },
  { key: "target", label: "目标" },
  { key: "actor", label: "访问者" },
  { key: "clientIp", label: "来源 IP" },
  { key: "occurredAt", label: "时间" },
];

// 弹窗的标题先落、数据后到，而连点两条分享时两次请求会交叉：后返回的那次会把
// 记录挂到另一条分享的标题下。用序号守卫保证只有最后一次请求能写回界面。
const accessesGate = createRequestGate();

async function openAccesses(share: Share): Promise<void> {
  const token = accessesGate.next();
  accessesTitle.value = `访问记录 · ${share.rootPath}`;
  accesses.value = [];
  accessesOpen.value = true;
  accessesBusy.value = true;
  try {
    const result = await shareApi.accesses(share.id, 100);
    if (!accessesGate.isCurrent(token)) return;
    accesses.value = result.items ?? [];
  } catch (err) {
    if (!accessesGate.isCurrent(token)) return;
    logError("share-accesses", err);
    toasts.error("访问记录加载失败", describeError(err));
  } finally {
    if (!accessesGate.isCurrent(token)) return;
    accessesBusy.value = false;
  }
}

function actorLabel(log: AuditLog): string {
  if (log.actorType === "guest") {
    return "访客";
  }
  return log.actorId > 0 ? `用户 #${log.actorId}` : "已注销账号";
}

function actionLabel(action: string): string {
  // action 是 string，查不到就是没收录的新动作：原样显示比一个假的占位符有用。
  return ACTION_LABELS[action] ?? action;
}

// ---------------------------------------------------------------- 取件码

const pickupOpen = ref(false);
const pickupBusy = ref(false);
const pickupShare = ref<Share | null>(null);
const pickups = ref<PickupCode[]>([]);
const pickupError = ref("");
const pickupCreating = ref(false);
// 新建后要醒目展示的明文码。列表接口其实也会回明文（pickup_codes 的 code 就是
// 主键），但刚拿到手时最需要的是"立刻复制"，所以仍然单独突出一次。
const freshCode = ref("");
// 新码的到期点由后端按管理员统一配置的取件码有效期算出，用户不能自定义。
const freshExpiresAt = ref(0);
const pickupForm = reactive({ maxUses: "1" });

const pickupColumns: Column[] = [
  { key: "code", label: "取件码", mobile: "title" },
  { key: "used", label: "已用次数", align: "right" },
  { key: "expires", label: "有效期" },
  { key: "status", label: "状态" },
  { key: "actions", label: "操作" },
];

interface PickupRow {
  pickup: PickupCode;
  usedLabel: string;
  expiresLabel: string;
  statusLabel: string;
  statusClass: string;
}

const pickupRows = computed(() => pickups.value.map(toPickupRow));

function toPickupRow(pickup: PickupCode): PickupRow {
  const expiresAt = pickup.expiresAt ?? 0;
  const expired = expiresAt > 0 && expiresAt <= Math.floor(Date.now() / 1000);
  const exhausted = pickup.usedCount >= pickup.maxUses;
  return {
    pickup,
    usedLabel: `${pickup.usedCount} / ${pickup.maxUses}`,
    expiresLabel: expiresAt > 0 ? formatTime(expiresAt) : "永久",
    statusLabel: pickup.disabled ? "已停用" : expired ? "已过期" : exhausted ? "已用尽" : "可用",
    statusClass: pickup.disabled || exhausted
      ? "badge--danger"
      : expired
        ? "badge--warn"
        : "badge--success",
  };
}

async function openPickup(share: Share): Promise<void> {
  pickupShare.value = share;
  pickupError.value = "";
  freshCode.value = "";
  freshExpiresAt.value = 0;
  pickupForm.maxUses = "1";
  pickups.value = [];
  pickupOpen.value = true;
  await loadPickups();
}

// 与访问记录同一个道理：生成、删除之后都要重拉一次，而重拉之前用户可能已经
// 切到别的分享上，序号守卫避免旧列表盖掉新分享的取件码。
const pickupGate = createRequestGate();

async function loadPickups(): Promise<void> {
  const share = pickupShare.value;
  if (!share) {
    return;
  }
  const token = pickupGate.next();
  pickupBusy.value = true;
  try {
    const result = await shareApi.listPickup(share.id);
    if (!pickupGate.isCurrent(token)) return;
    pickups.value = result.items ?? [];
  } catch (err) {
    if (!pickupGate.isCurrent(token)) return;
    logError("pickup-list", err);
    pickupError.value = describeError(err);
  } finally {
    if (!pickupGate.isCurrent(token)) return;
    pickupBusy.value = false;
  }
}

async function createPickup(): Promise<void> {
  const share = pickupShare.value;
  if (!share) {
    return;
  }
  pickupError.value = "";
  const parsed = Number.parseInt(pickupForm.maxUses, 10);
  if (!Number.isFinite(parsed) || parsed < 1) {
    // 后端会把 <=0 静默改成 1，这里先拦下来，免得用户以为设置了"不限次数"。
    pickupError.value = "使用次数至少为 1";
    return;
  }
  pickupCreating.value = true;
  try {
    const result = await shareApi.createPickup(share.id, parsed);
    freshCode.value = result.pickup.code;
    freshExpiresAt.value = result.pickup.expiresAt ?? 0;
    pickupForm.maxUses = "1";
    await loadPickups();
    toasts.success("取件码已生成");
  } catch (err) {
    logError("pickup-create", err);
    pickupError.value = describeError(err);
  } finally {
    pickupCreating.value = false;
  }
}

async function copyCode(code: string): Promise<void> {
  const ok = await copyText(code);
  if (ok) {
    toasts.success("取件码已复制");
  } else {
    toasts.error("复制失败", "请手动抄写取件码");
  }
}

const pickupDeleting = ref<PickupCode | null>(null);
const pickupDeleteBusy = ref(false);

async function confirmDeletePickup(): Promise<void> {
  const target = pickupDeleting.value;
  if (!target) {
    return;
  }
  pickupDeleteBusy.value = true;
  try {
    await shareApi.deletePickup(target.code);
    toasts.success("已删除取件码");
    if (freshCode.value === target.code) {
      freshCode.value = "";
    }
    pickupDeleting.value = null;
    await loadPickups();
  } catch (err) {
    logError("pickup-delete", err);
    toasts.error("删除失败", describeError(err));
  } finally {
    pickupDeleteBusy.value = false;
  }
}

// ---------------------------------------------------------------- 进入时的参数

onMounted(() => {
  void loadShares();
  // 文件页可以带着待分享的路径跳过来，弹窗用完就把参数清掉，
  // 否则刷新一次会再弹一次。
  const path = route.query.create;
  if (typeof path === "string" && path !== "") {
    openCreate(path, route.query.kind === "folder" ? "folder" : "file");
    void router.replace({ query: {} });
  }
});
</script>

<template>
  <div class="stack">
    <!-- 操作项投递到顶栏（页面名在顶栏左侧）。 -->
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="listLoading" @click="loadShares()">刷新</AppButton>
      <AppButton v-if="canCreate" size="sm" variant="primary" icon="plus" @click="openCreate('', 'file')">
        新建分享
      </AppButton>
    </Teleport>

    <p v-if="listError" class="notice notice--danger">
      {{ listError }}
    </p>
    <p
      v-else-if="session.state.authenticated && !session.can(Perm.Share)"
      class="notice notice--info"
    >
      当前用户组没有创建分享的权限，已有分享仍可管理。
    </p>

    <div v-if="listLoading" class="card share-slot">
      <div class="share-loading">
        <span class="spinner" /> 正在加载
      </div>
    </div>
    <div v-else-if="rows.length === 0" class="card share-slot">
      <!-- offset > 0 说明是翻过了末页：这不是"一条都没有"，必须给出去路。 -->
      <AppEmpty
        v-if="offset > 0"
        title="没有更多分享了"
        hint="已经翻到最后一页，把「每页条数」调小或用「上一页」往回翻。"
      />
      <AppEmpty v-else title="还没有分享" hint="用右上角的「新建分享」创建第一个。" />
    </div>
    <template v-else>
      <div class="share-grid">
        <article v-for="row in rows" :key="row.share.id" class="card share-card">
          <header class="share-card__head">
            <div class="share-card__id">
              <span class="share-card__name">
                <FileKindIcon
                  :name="row.share.rootPath"
                  :is-folder="row.share.kind === 'folder'"
                />
                <span class="truncate mono">{{ row.share.rootPath }}</span>
                <span class="badge" :class="row.statusClass">{{ row.statusLabel }}</span>
              </span>
              <span class="share-card__meta">创建于 {{ row.createdLabel }}</span>
            </div>
            <div class="actions">
              <AppButton
                size="sm"
                icon="link"
                data-tip="复制链接"
                aria-label="复制链接"
                @click="copyLink(row.share)"
              />
              <AppButton
                size="sm"
                icon="activity"
                data-tip="访问记录"
                aria-label="访问记录"
                @click="openAccesses(row.share)"
              />
              <AppButton
                size="sm"
                icon="key"
                data-tip="取件码"
                aria-label="取件码"
                @click="openPickup(row.share)"
              />
              <AppButton
                size="sm"
                icon="edit"
                data-tip="编辑"
                aria-label="编辑"
                @click="openEdit(row.share)"
              />
              <AppButton
                size="sm"
                variant="danger"
                icon="trash"
                data-tip="删除"
                aria-label="删除"
                @click="deleting = row.share"
              />
            </div>
          </header>

          <dl class="share-card__facts">
            <div class="share-card__fact">
              <dt>类型</dt>
              <dd>{{ kindLabel(row.share.kind) }}</dd>
            </div>
            <div class="share-card__fact">
              <dt>访问方式</dt>
              <dd class="row" style="gap: 4px">
                <span>{{ ACCESS_LABELS[row.share.accessMode] }}</span>
                <AppIcon
                  v-if="row.share.hasPassword"
                  name="lock"
                  :size="12"
                  data-tip="已设置提取码"
                  aria-label="已设置提取码"
                />
              </dd>
            </div>
            <div class="share-card__fact">
              <dt>访问次数</dt>
              <dd class="mono">{{ row.visitsLabel }}</dd>
            </div>
            <div class="share-card__fact">
              <dt>有效期</dt>
              <dd class="nowrap">{{ row.expiresLabel }}</dd>
            </div>
          </dl>
        </article>
      </div>
    </template>

    <!-- 分页条放在空态分支之外：翻过末页拿到一页空数据时，它仍要留在页面上，
         否则用户既翻不回来、也看不到"没有更多"。
         exact=false（后端不回总数）时必须自己给 has-more，否则"下一页"永远可点。 -->
    <div v-if="!listLoading" class="share-foot">
      <AppPagination
        :total="offset + shareData.items.length"
        :limit="limit"
        :offset="offset"
        :exact="false"
        :has-more="hasMore"
        @update:limit="changeLimit"
        @update:offset="offset = $event"
      />
    </div>

    <!-- 新建 / 编辑 -->
    <AppModal
      :open="formOpen"
      :title="editing ? '编辑分享' : '新建分享'"
      :dismissible="!saving"
      @close="formOpen = false"
    >
      <div class="stack">
        <p v-if="formError" class="notice notice--danger">{{ formError }}</p>

        <FormField v-if="!editing" label="路径" required hint="必须是自己的文件或目录，以 / 开头。">
          <input v-model="form.path" class="input mono" placeholder="/目录/文件.txt" />
        </FormField>

        <div v-else class="kv">
          <dt>路径</dt>
          <dd class="mono">{{ form.path }}</dd>
          <dt>类型</dt>
          <dd>{{ kindLabel(form.kind) }}</dd>
        </div>

        <div class="form-grid">
          <FormField v-if="!editing" label="类型" hint="必须与实际路径的类型一致。">
            <AppSelect v-model="form.kind" aria-label="类型" :options="KIND_OPTIONS" />
          </FormField>

          <FormField label="访问方式" :hint="ACCESS_HINTS[form.accessMode]">
            <AppSelect v-model="form.accessMode" aria-label="访问方式" :options="ACCESS_OPTIONS" />
          </FormField>

          <FormField
            v-if="form.accessMode === 'password'"
            label="提取码"
            :hint="editing && editing.hasPassword ? '留空表示沿用原提取码。' : '访客需要输入它才能打开。'"
          >
            <input v-model="form.password" class="input" autocomplete="off" />
          </FormField>

          <FormField label="过期时间" hint="留空表示永久有效。">
            <div class="stack stack--tight">
              <AppDateTimePicker v-model="form.expiresAtLocal" />
              <p v-if="expiresInPast" class="field__error">该时间已经过了，保存后分享立即失效。</p>
            </div>
          </FormField>

          <FormField label="访问上限" hint="0 表示不限次数。">
            <input v-model="form.maxVisits" class="input" type="number" min="0" step="1" />
          </FormField>
        </div>

        <div class="stack stack--tight">
          <label class="check">
            <input v-model="form.allowDownload" type="checkbox" />
            <span class="check__text"><span>允许下载</span></span>
          </label>
          <label class="check">
            <input v-model="form.allowPreview" type="checkbox" />
            <span class="check__text"><span>允许在线预览</span></span>
          </label>
          <label v-if="form.kind === 'folder'" class="check">
            <input v-model="form.allowSubpath" type="checkbox" />
            <span class="check__text">
              <span>允许浏览子目录</span>
              <span class="faint">关闭后访客只能看到分享根目录这一层。</span>
            </span>
          </label>
          <label class="check">
            <input v-model="form.showSharerName" type="checkbox" />
            <span class="check__text"><span>展示分享者名称</span><span class="faint">在分享页展示你的名称。</span></span>
          </label>
          <label v-if="editing" class="check">
            <input v-model="form.disabled" type="checkbox" />
            <span class="check__text">
              <span>停用</span>
              <span class="faint">停用后链接立即失效，随时可以再打开。</span>
            </span>
          </label>
        </div>
      </div>

      <template #footer>
        <AppButton :disabled="saving" @click="formOpen = false">取消</AppButton>
        <AppButton variant="primary" :loading="saving" @click="submit()">
          {{ editing ? "保存" : "创建" }}
        </AppButton>
      </template>
    </AppModal>

    <ConfirmDialog
      :open="deleting !== null"
      danger
      title="删除分享"
      confirm-text="删除"
      :loading="deleteBusy"
      message="删除后该分享的取件码与访问记录会一并删除，已发出的链接立即失效。"
      detail="此操作不可撤销。如果只是想暂时收回，改用「停用」。"
      @cancel="deleting = null"
      @confirm="confirmDelete()"
    />

    <!-- 访问记录 -->
    <AppModal
      :open="accessesOpen"
      :title="accessesTitle"
      wide
      @close="accessesOpen = false"
    >
      <AppTable
        :columns="accessColumns"
        :rows="accesses"
        :loading="accessesBusy"
        empty-title="还没有访问记录"
        empty-hint="对方打开、浏览或提取之后才会出现明细。"
      >
        <template #action="{ row }">{{ actionLabel(row.action) }}</template>
        <template #target="{ row }">
          <span class="mono">{{ row.target || "—" }}</span>
        </template>
        <template #actor="{ row }">
          <span class="badge">{{ actorLabel(row) }}</span>
        </template>
        <template #occurredAt="{ row }">
          {{ formatTime(row.occurredAt) }}
        </template>
      </AppTable>
    </AppModal>

    <!-- 取件码 -->
    <AppModal :open="pickupOpen" title="取件码" wide @close="pickupOpen = false">
      <div class="stack">
        <p v-if="pickupError" class="notice notice--danger">{{ pickupError }}</p>

        <p v-if="pickupShare" class="muted" style="font-size: var(--fs-sm)">
          分享 <span class="mono">{{ pickupShare.rootPath }}</span> · 对方在「取件码」页输入即可提取，无需提取码。
        </p>

        <div v-if="freshCode" class="fresh">
          <p class="fresh__label">新取件码，请立即复制发给对方</p>
          <div class="row">
            <code class="fresh__code">{{ freshCode }}</code>
            <AppButton variant="primary" icon="copy" @click="copyCode(freshCode)">复制</AppButton>
          </div>
          <p class="faint" style="font-size: var(--fs-xs)">
            有效期至 {{ freshExpiresAt > 0 ? formatTime(freshExpiresAt) : "永久" }}
            ；谁拿到都能提取，发错了删掉这条即可。
          </p>
        </div>

        <div v-if="canPickup" class="form-grid">
          <FormField label="使用次数" hint="至少 1 次，独立于分享的访问上限。">
            <input v-model="pickupForm.maxUses" class="input" type="number" min="1" step="1" />
          </FormField>
          <div class="form-grid--full">
            <p class="faint" style="font-size: var(--fs-xs); margin: 0 0 var(--sp-2)">
              取件码有效期由管理员统一设置，无法为单个取件码单独指定；具体到期时间见下方列表。
            </p>
            <div class="row row--end">
              <AppButton variant="primary" icon="plus" :loading="pickupCreating" @click="createPickup()">
                生成取件码
              </AppButton>
            </div>
          </div>
        </div>
        <p v-else class="notice notice--info">当前用户组没有创建取件码的权限，只能查看与删除已有的码。</p>

        <AppTable
          :columns="pickupColumns"
          :rows="pickupRows"
          :loading="pickupBusy"
          empty-title="还没有取件码"
          empty-hint="生成后把码发给对方，对方在取件码页输入即可提取。"
        >
          <template #code="{ row }">
            <code class="mono">{{ row.pickup.code }}</code>
          </template>
          <template #used="{ row }">{{ row.usedLabel }}</template>
          <template #expires="{ row }">{{ row.expiresLabel }}</template>
          <template #status="{ row }">
            <span class="badge" :class="row.statusClass">
              {{ row.statusLabel }}
            </span>
          </template>
          <template #actions="{ row }">
            <div class="actions">
              <AppButton size="sm" icon="copy" @click="copyCode(row.pickup.code)">
                复制
              </AppButton>
              <AppButton
                size="sm"
                variant="danger"
                icon="trash"
                @click="pickupDeleting = row.pickup"
              >
                删除
              </AppButton>
            </div>
          </template>
        </AppTable>
      </div>
    </AppModal>

    <ConfirmDialog
      :open="pickupDeleting !== null"
      danger
      title="删除取件码"
      confirm-text="删除"
      :loading="pickupDeleteBusy"
      message="删除后这个取件码立即失效，已经拿到它的人无法再提取。"
      @cancel="pickupDeleting = null"
      @confirm="confirmDeletePickup()"
    />
  </div>
</template>

<style scoped>
/* 加载与空态占的是"分享卡片本来会在的那个格子"：套上和 .share-card 同一个
   .card 容器，页面才不会一半是卡片、一半悬着一段没有落点的文字。 */
.share-slot {
  width: 100%;
}

.share-loading {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--sp-2);
  padding: var(--sp-6);
  color: var(--c-text-muted);
}

/* 始终一行一个：卡片纵向堆叠，每张铺满容器整行宽度。 */
.share-grid {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
}

.share-card {
  width: 100%;
}

.share-card__head {
  padding: var(--sp-4);
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sp-3);
  flex-wrap: wrap;
}

.share-card__id {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.share-card__name {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-sm);
  font-weight: 620;
  color: var(--c-text);
  min-width: 0;
}

.share-card__meta {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
  padding-left: 22px;
}

/* 全局的 .actions 样式挂在 .table 下，卡片布局里要自己排（同账号/用户组页）。 */
.share-card .actions {
  display: flex;
  gap: var(--sp-2);
  flex: none;
}

/* 事实条按 2×2 收纳：4 项硬挤一行在窄卡里放不下，折行后带出来的竖分隔线
   会挂在行首。格子间画线，容器边缘不画。 */
.share-card__facts {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  margin: 0;
  border-top: 1px solid var(--c-border);
}

.share-card__fact {
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-4);
  border-left: 1px solid var(--c-border);
}

/* 每行第一格不画左边线：挂在行首的竖线看起来像排版错位。 */
.share-card__fact:nth-child(2n + 1) {
  border-left: 0;
}

.share-card__fact:nth-child(n + 3) {
  border-top: 1px solid var(--c-border);
}

@media (min-width: 720px) {
  .share-card__facts {
    grid-template-columns: repeat(4, 1fr);
  }

  .share-card__fact:nth-child(n) {
    border-left: 1px solid var(--c-border);
    border-top: 0;
  }

  .share-card__fact:first-child {
    border-left: 0;
  }
}

.share-card__fact dt {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.share-card__fact dd {
  margin: 0;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--c-text);
}

.share-foot {
  margin-top: var(--sp-4);
}

.fresh {
  border: 1px solid var(--c-accent);
  background: var(--c-accent-weak);
  border-radius: var(--r-md);
  padding: var(--sp-3) var(--sp-4);
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.fresh__label {
  font-weight: 600;
  color: var(--c-accent);
}

.fresh__code {
  font-family: var(--font-mono);
  font-size: var(--fs-xl);
  letter-spacing: 0.24em;
  font-weight: 650;
  padding: var(--sp-1) var(--sp-3);
  border-radius: var(--r-sm);
  background: var(--c-surface);
  border: 1px solid var(--c-border-strong);
}
</style>
