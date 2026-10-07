<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { AdminNodeItem, AdminNodeStats } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import FilterBar from "@/components/admin/FilterBar.vue";
import Panel from "@/components/admin/Panel.vue";
import StatStrip, { type StatItem } from "@/components/admin/StatStrip.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { topbarSlot } from "@/stores/shell";
import { createRequestGate, describeError, isAbortError, logError } from "@/lib/async";
import { formatBytes, formatTime } from "@/lib/format";
import { useToasts } from "@/stores/toast";

// 文件管理：全站文件检索 + 内容池对象处置。
//
// 这一页原来只有两块：对象状态计数，和一个"按校验码处置"的弹窗。问题是
// 管理员根本无从知道该填哪个校验码——对象按内容寻址，文件名在 user_nodes
// 里，属主在 users 里，三者都不在 files 表上。于是这一页等于只能盲查。
//
// 现在检索与处置接上了：先在表里按名称/路径/属主搜到东西，点某一行的
// 「停用/恢复」直接带上该行的校验码与原因，不再需要手抄 64 位十六进制。

const toasts = useToasts();

const loading = ref(false);
const error = ref("");

// ---- 顶部统计 ----

const STATUS_META: Record<string, { label: string; tone: string }> = {
  uploading: { label: "上传中", tone: "" },
  normal: { label: "正常", tone: "badge--success" },
  disabled: { label: "已停用", tone: "badge--danger" },
  archive: { label: "待回收", tone: "badge--warn" },
  purged: { label: "存储删除", tone: "badge--danger" },
  // 状态未知：文件节点在内容池里找不到对应行。LEFT JOIN 的 NULL 走的是
  // FileUnknown 哨兵，界面上必须说清"查不到"，不能显示成裸的 unknown。
  unknown: { label: "状态未知", tone: "" },
};
const STATUS_ORDER = ["uploading", "normal", "disabled", "archive", "purged"];

/**
 * 空站也要给出结构完整的统计：map 为空时前端若拿到 null，
 * Object.entries 会直接抛错，整页白屏比显示 0 糟糕得多。
 */
const EMPTY_STATS: AdminNodeStats = {
  nodeTotal: 0,
  fileNodes: 0,
  folderNodes: 0,
  fileObjectTotal: 0,
  mailObjectTotal: 0,
  objectsByStatus: {},
};

const stats = ref<AdminNodeStats>(EMPTY_STATS);

/**
 * 统计条刻意分两段：前两格数「节点」（与下面的表格同一批东西，行数能对上），
 * 后一段数「内容池对象」（同一份内容被多人存只算一份）。
 * 早先这两批混在一排里，管理员数着表格永远对不上顶上的总数。
 */
const statItems = computed<StatItem[]>(() => [
  {
    label: "站点文件与文件夹",
    value: stats.value.nodeTotal,
    hint: `与下方表格同一批：${stats.value.fileNodes} 个文件、${stats.value.folderNodes} 个文件夹`,
  },
  {
    label: "文件对象",
    value: stats.value.fileObjectTotal,
    hint: "用户上传去重后的份数",
  },
  {
    label: "邮件对象",
    value: stats.value.mailObjectTotal,
    hint: "邮件内容入库去重后的份数",
  },
  ...STATUS_ORDER.filter((key) => stats.value.objectsByStatus[key] !== undefined).map((key) => ({
    label: STATUS_META[key].label,
    value: stats.value.objectsByStatus[key] ?? 0,
    tone: STATUS_META[key].tone,
  })),
]);

// ---- 全站检索 ----

const NODE_TYPES = [
  { value: "", label: "全部类型" },
  { value: "file", label: "仅文件" },
  { value: "folder", label: "仅文件夹" },
] as const;

const FILE_STATES = [
  { value: "", label: "全部状态" },
  { value: "uploading", label: "上传中" },
  { value: "normal", label: "正常" },
  { value: "disabled", label: "已停用" },
  { value: "archive", label: "待回收" },
  { value: "purged", label: "存储删除" },
] as const;

const filters = reactive({ q: "", owner: "", nodeType: "", state: "" });
const items = ref<AdminNodeItem[]>([]);
const total = ref(0);
const offset = ref(0);
/** 必须是 ref：分页组件的「每页条数」是用户可改的，绑成常量就改不动。 */
const limit = ref(50);
const searching = ref(false);

/** 状态名 → store.FileStatus 的数值。-1 是"不适用"，只对文件夹出现。 */
const STATE_VALUE: Record<string, number> = {
  uploading: 0,
  normal: 1,
  disabled: 2,
  archive: 3,
  purged: 4,
  "-1": -1,
};

const columns: Column[] = [
  { key: "name", label: "名称", mobile: "title" },
  { key: "owner", label: "属主" },
  { key: "sizePlain", label: "大小", align: "right" },
  { key: "mtime", label: "修改时间" },
  { key: "fileStatus", label: "对象状态" },
  { key: "actions", label: "操作", align: "right" },
];

function statusOf(item: AdminNodeItem): { label: string; tone: string } {
  if (item.nodeType === "folder") {
    return { label: "文件夹", tone: "" };
  }
  // find 返回的是键名，找不到时是 undefined——原先那句 `name === "-1"`
  // 比较的是值，而键不可能长成 "-1"，于是那一支永远不成立，"状态未知"
  // 这条注释描述的路径其实靠 ?? 兜着才走到。
  const name = Object.keys(STATE_VALUE).find((k) => STATE_VALUE[k] === item.fileStatus);
  const key = name ?? "unknown";
  return { label: STATUS_META[key]?.label ?? key, tone: STATUS_META[key]?.tone ?? "" };
}

// 状态在映射时就定好：模板里原来一次渲染调两次 statusOf，每次都重做一遍
// Object.keys 反查，50 行的表格就是 100 次无用功。
const rows = computed<Record<string, unknown>[]>(() =>
  items.value.map((it) => {
    const status = statusOf(it);
    return {
      name: it.name,
      path: it.path,
      owner: it.account,
      group: it.groupName,
      sizePlain: it.sizePlain,
      mtime: it.mtime,
      statusLabel: status.label,
      statusTone: status.tone,
      isFolder: it.nodeType === "folder",
      ref: it,
    };
  }),
);

const gate = createRequestGate();
/** 上一轮检索的取消句柄。新一轮开始前把它掐掉，服务端那次全表扫就不用跑完。 */
let inflight: AbortController | null = null;

async function search(nextOffset = 0): Promise<void> {
  // 序号守卫 + 主动取消双管：序号保证结果不会串（取消也可能慢半拍才生效），
  // 取消保证真正昂贵的全表扫不会白跑。
  const token = gate.next();
  inflight?.abort();
  inflight = new AbortController();
  const signal = inflight.signal;
  searching.value = true;
  try {
    const res = await adminApi.listNodes(
      {
        q: filters.q.trim() || undefined,
        owner: filters.owner.trim() || undefined,
        nodeType: filters.nodeType || undefined,
        state: filters.state || undefined,
        limit: limit.value,
        offset: nextOffset,
      },
      signal,
    );
    if (!gate.isCurrent(token)) return;
    items.value = res.items;
    total.value = res.total;
    offset.value = res.offset;
    // 统计条跟着列表一起回来，省掉一次额外请求；它不受筛选影响，
    // 所以翻页与筛选都不需要重新统计。
    stats.value = res.stats ?? EMPTY_STATS;
  } catch (err) {
    // 取消是预期行为：不提示、不写错误区。
    if (isAbortError(err) || !gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.files.search", err);
  } finally {
    if (gate.isCurrent(token)) searching.value = false;
  }
}

function resetFilters(): void {
  filters.q = "";
  filters.owner = "";
  filters.nodeType = "";
  filters.state = "";
  void search(0);
}

/**
 * 分页翻页。
 *
 * AppPagination 只 emit `update:limit` / `update:offset`，没有 `change`
 * 事件——用 @change 监听会静默失效，点多少次都没反应，而类型检查抓不到。
 * 因此这里按与其余六个列表页一致的口径接两个事件。
 */
function changePage(nextOffset: number, nextLimit: number): void {
  limit.value = nextLimit;
  offset.value = nextOffset;
  void search(nextOffset);
}

async function load(): Promise<void> {
  loading.value = true;
  error.value = "";
  try {
    // 统计条随列表一起取，不再单独调概览接口：概览要 AdminAudit，
    // 而这一页只要 AdminFiles，只有文件管理权的管理员会被 403 掉整页。
    //
    // 这里不套 try/catch：search() 自己捕获了所有异常并写进 error，
    // 外面这层 catch 永远不会执行，却让人以为首屏加载失败被兜住了。
    await search(0);
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// ---- 处置 ----

const open = ref(false);
const busy = ref(false);
const formError = ref("");
/** 从表格行进来时带上校验码与原因；手抄模式则留空让用户填。 */
const form = reactive({ checksum: "", reason: "", locked: false });

const confirmTarget = ref(false);

function openFromRow(row: AdminNodeItem): void {
  if (row.nodeType === "folder") {
    return;
  }
  form.checksum = row.checksum;
  form.reason = "";
  form.locked = true;
  formError.value = "";
  open.value = true;
}

function openManual(): void {
  form.checksum = "";
  form.reason = "";
  form.locked = false;
  formError.value = "";
  open.value = true;
}

function validate(): string {
  if (!form.checksum.trim()) {
    return "请填写文件对象的校验码";
  }
  if (!/^[0-9a-fA-F]{64}$/.test(form.checksum.trim())) {
    return "校验码必须是 64 位十六进制字符";
  }
  if (!form.reason.trim()) {
    return "请填写处置原因";
  }
  return "";
}

function ask(action: "disable" | "restore"): void {
  const invalid = validate();
  if (invalid) {
    formError.value = invalid;
    return;
  }
  formError.value = "";
  if (action === "disable") {
    confirmTarget.value = true;
    return;
  }
  void submit(false);
}

async function submit(disabled: boolean): Promise<void> {
  busy.value = true;
  formError.value = "";
  try {
    const normalized = form.checksum.trim().toLowerCase();
    await adminApi.setFileStatus(normalized, disabled, form.reason.trim());
    toasts.success(`已${disabled ? "停用" : "恢复"}对象 ${normalized.slice(0, 12)}…`);
    open.value = false;
    confirmTarget.value = false;
    // 状态变了，计数和列表都要跟着刷新，否则会出现"表里还写着正常"。
    await load();
  } catch (err) {
    formError.value = describeError(err);
    logError("admin.files.status", err);
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load">刷新</AppButton>
      <AppButton size="sm" icon="lock" @click="openManual">按校验码处置</AppButton>
    </Teleport>

    <StatStrip :items="statItems" />

    <!-- 「对象状态」常显而不是折进更多筛选：这一页的主要动作就是按状态
         找东西并处置它，把它藏起来等于让核心操作多点一次。
         「节点类型」是低频条件，留在折叠区。 -->
    <!-- flat：筛选控件直接落在页面上，紧贴下面的 flush 表格卡片。
         与邮件管理页同一形态——两页问的是同类问题，骨架不该让人重新学一遍。 -->
    <FilterBar flat :busy="searching" @search="search(0)" @reset="resetFilters">
      <input
        v-model="filters.q"
        class="input"
        type="search"
        placeholder="按文件名或路径搜索全站文件"
      />
      <AppSelect v-model="filters.state" :options="FILE_STATES" aria-label="对象状态" size="sm" />
      <input
        v-model="filters.owner"
        class="input"
        type="search"
        placeholder="属主账号"
      />
      <template #extra>
        <AppSelect v-model="filters.nodeType" :options="NODE_TYPES" aria-label="节点类型" size="sm" />
        <p class="filterbar__hint">
          检索范围是所有用户目录里的文件与文件夹；「对象状态」是内容池层面的状态，
          停用会影响引用它的每一个人。文件夹不占用对象，没有对象状态。
        </p>
      </template>
    </FilterBar>

    <Panel :title="filters.q || filters.owner ? '检索结果' : '全站文件'" :count="`${total} 条`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="searching"
        empty-title="没有匹配的文件"
        empty-hint="换个关键词，或清空筛选条件看看站点里现有的文件。"
      >
        <template #name="{ row }">
          <div class="cell">
            <span class="cell__name truncate">{{ String(row.name) }}</span>
            <span class="cell__path mono truncate" :title="String(row.path)">
              {{ String(row.path) }}
            </span>
          </div>
        </template>

        <template #owner="{ row }">
          <div class="cell">
            <span>{{ String(row.owner) }}</span>
            <span class="faint">{{ String(row.group) }}</span>
          </div>
        </template>

        <template #sizePlain="{ row }">
          <span :class="{ muted: row.isFolder }">
            {{ row.isFolder ? "—" : formatBytes(Number(row.sizePlain)) }}
          </span>
        </template>

        <template #mtime="{ row }">
          <span class="muted">{{ Number(row.mtime) ? formatTime(Number(row.mtime)) : "—" }}</span>
        </template>

        <template #fileStatus="{ row }">
          <span class="badge" :class="String(row.statusTone)">{{ row.statusLabel }}
          </span>
          <span
            v-if="!row.isFolder && Number((row.ref as AdminNodeItem).refCount) > 1"
            class="refs"
            :title="`该对象被全站 ${(row.ref as AdminNodeItem).refCount} 处引用，停用会全部受影响`"
          >
            ×{{ (row.ref as AdminNodeItem).refCount }}
          </span>
        </template>

        <template #actions="{ row }">
          <AppButton
            v-if="!row.isFolder"
            size="sm"
            variant="ghost"
            icon="lock"
            @click="openFromRow(row.ref as AdminNodeItem)"
          >
            处置
          </AppButton>
          <span v-else class="muted">—</span>
        </template>
      </AppTable>
      <!-- 分页收进 Panel 底栏：与邮件管理页同一形态，悬在卡片外面会像
           一个没有落点的浮条。 -->
      <div class="panel-foot">
        <AppPagination
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:limit="(value: number) => changePage(0, value)"
          @update:offset="(value: number) => changePage(value, limit)"
        />
      </div>
    </Panel>

    <AppModal :open="open" title="处置内容对象" @close="open = false">
      <div class="stack">
        <FormField
          label="文件对象校验码"
          required
          :hint="
            form.locked
              ? '由检索结果带入。停用是对内容池对象的全局操作，会影响所有引用者。'
              : '64 位十六进制；可在用户文件列表的「详情」中复制。'
          "
        >
          <input
            v-model="form.checksum"
            class="input mono"
            placeholder="64 位校验码（SHA-256）"
            spellcheck="false"
            autocomplete="off"
            :readonly="form.locked"
          />
        </FormField>

        <FormField label="原因" required hint="写入对象记录与审计日志。">
          <input v-model="form.reason" class="input" placeholder="例如：版权投诉 / 误传测试数据" />
        </FormField>

        <p v-if="formError" class="field__error">{{ formError }}</p>
      </div>

      <template #footer>
        <AppButton :disabled="busy" @click="open = false">取消</AppButton>
        <AppButton icon="refresh" :disabled="busy" @click="ask('restore')">恢复</AppButton>
        <AppButton variant="danger" icon="lock" :loading="busy" @click="ask('disable')">停用</AppButton>
      </template>
    </AppModal>

    <ConfirmDialog
      :open="confirmTarget"
      danger
      confirm-text="停用对象"
      :loading="busy"
      message="停用这个文件对象？"
      detail="全局生效：所有引用该对象的账号都会受影响，未过期的下载票据同时吊销。"
      @confirm="submit(true)"
      @cancel="confirmTarget = false"
    />
  </AdminPage>
</template>

<style scoped>
.cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.cell__name {
  font-weight: 550;
}

.cell__path,
.cell .faint {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.refs {
  margin-left: var(--sp-1);
  padding: 0 5px;
  border-radius: var(--r-pill);
  background: var(--c-warn-weak);
  color: var(--c-warn);
  font-size: 11px;
  line-height: 16px;
}
</style>
