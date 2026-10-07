<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { MailUnbindRequest, MailUnbindStats } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import FilterBar from "@/components/admin/FilterBar.vue";
import Panel from "@/components/admin/Panel.vue";
import StatStrip, { type StatItem } from "@/components/admin/StatStrip.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import { createRequestGate, describeError, logError, toastApiError } from "@/lib/async";
import { formatTime } from "@/lib/format";
import { useToasts } from "@/stores/toast";

// 邮箱解绑审核。
//
// 批准是**真删除**：地址消失后发往它的邮件会在 SMTP 阶段被拒（550），外部
// 发信人立刻收到退信，而用户已收到的邮件全部保留——归属记在 mailboxes.user_id
// 上，不在地址上。这个代价由不在场的人承担，所以每一单都要一个人明确点头，
// 不能批量自动通过。

const toasts = useToasts();
const items = ref<MailUnbindRequest[]>([]);
const stats = ref<MailUnbindStats>({ pending: 0, approved: 0, rejected: 0, todayPending: 0 });
const loading = ref(false);
const error = ref("");

const filters = reactive({ status: "pending", address: "", search: "" });

/** 正在处理的单：id → "approve" | "reject"，用来只锁住被点的那一行。 */
const busy = ref<Record<number, string>>({});

/** 驳回批注弹窗：为空表示没在驳回。 */
const rejectTarget = ref<MailUnbindRequest | null>(null);
const rejectNote = ref("");

const columns: Column[] = [
  { key: "address", label: "邮箱地址", mobile: "title", width: "200px" },
  { key: "account", label: "申请人", width: "110px" },
  { key: "createdAt", label: "提交时间", width: "140px" },
  { key: "reason", label: "申请理由" },
  { key: "status", label: "状态", width: "80px" },
  { key: "ops", label: "操作", width: "150px" },
];

const rows = computed<Record<string, unknown>[]>(() =>
  items.value.map((item) => ({
    id: item.id,
    address: item.address,
    account: item.account || `#${item.userId}`,
    createdAt: formatTime(item.createdAt),
    reason: item.reason || "—",
    status: item.status,
    note: item.note || "",
    pending: item.status === "pending",
  })),
);

const statItems = computed<StatItem[]>(() => [
  { label: "待审核", value: stats.value.pending, tone: stats.value.pending > 0 ? "warn" : "" },
  { label: "今日新增", value: stats.value.todayPending, hint: "今天提交的待审单" },
  { label: "已通过", value: stats.value.approved },
  { label: "已驳回", value: stats.value.rejected },
]);

const gate = createRequestGate();

async function load(): Promise<void> {
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const res = await adminApi.listUnbinds({
      status: filters.status || undefined,
      address: filters.address.trim() || undefined,
      search: filters.search.trim() || undefined,
    });
    if (!gate.isCurrent(token)) return;
    items.value = res.items ?? [];
    stats.value = res.stats;
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.unbind.list", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

function reset(): void {
  filters.status = "pending";
  filters.address = "";
  filters.search = "";
  void load();
}

/** approve 真删除地址。二次确认放在这里的原因：不可逆。 */
async function approve(item: MailUnbindRequest): Promise<void> {
  if (!window.confirm(`确认通过「${item.address}」的解绑申请？\n\n通过后该地址将被删除，发往它的邮件会立刻收到退信。`)) {
    return;
  }
  busy.value = { ...busy.value, [item.id]: "approve" };
  try {
    await adminApi.approveUnbind(item.id);
    await load();
  } catch (err) {
    toastApiError(toasts, err);
  } finally {
    const next = { ...busy.value };
    delete next[item.id];
    busy.value = next;
  }
}

function openReject(item: MailUnbindRequest): void {
  rejectTarget.value = item;
  rejectNote.value = "";
}

async function confirmReject(): Promise<void> {
  const target = rejectTarget.value;
  if (!target) return;
  busy.value = { ...busy.value, [target.id]: "reject" };
  try {
    await adminApi.rejectUnbind(target.id, rejectNote.value.trim());
    rejectTarget.value = null;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
  } finally {
    const next = { ...busy.value };
    delete next[target.id];
    busy.value = next;
  }
}

onMounted(load);
</script>

<template>
  <AdminPage :error="error">
    <StatStrip :items="statItems" />

    <FilterBar flat :busy="loading" @search="load" @reset="reset">
      <select v-model="filters.status" class="input tf-status" aria-label="申请状态">
        <option value="pending">待审核</option>
        <option value="approved">已通过</option>
        <option value="rejected">已驳回</option>
        <option value="">全部</option>
      </select>
      <input
        v-model="filters.address"
        class="input tf-address"
        placeholder="邮箱地址：精确匹配"
        aria-label="邮箱地址精确匹配"
      />
      <input
        v-model="filters.search"
        class="input tf-search"
        placeholder="模糊搜索：地址或账号"
        aria-label="模糊搜索"
      />
    </FilterBar>

    <Panel title="解绑申请" :count="`共 ${items.length} 条`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="没有匹配的解绑申请"
        empty-hint="换个状态或清空筛选条件后重试。"
      >
        <template #address="{ row }">
          <span class="mono truncate--block" :title="String(row.address)">{{ row.address }}</span>
        </template>
        <template #account="{ row }">
          <span class="nowrap">{{ row.account }}</span>
        </template>
        <template #createdAt="{ row }">
          <span class="nowrap">{{ row.createdAt }}</span>
        </template>
        <template #reason="{ row }">
          <span class="truncate--block" :title="String(row.reason)">{{ row.reason }}</span>
        </template>
        <template #status="{ row }">
          <span
            class="badge"
            :class="row.status === 'pending' ? 'warn' : row.status === 'approved' ? 'ok' : ''"
          >
            {{ row.status === "pending" ? "待审核" : row.status === "approved" ? "已通过" : "已驳回" }}
          </span>
        </template>
        <template #ops="{ row }">
          <div v-if="row.pending" class="row-ops">
            <AppButton
              size="sm"
              :loading="busy[Number(row.id)] === 'approve'"
              :disabled="Boolean(busy[Number(row.id)])"
              @click="approve(items.find((i) => i.id === row.id)!)"
            >
              通过
            </AppButton>
            <AppButton
              size="sm"
              variant="ghost"
              :disabled="Boolean(busy[Number(row.id)])"
              @click="openReject(items.find((i) => i.id === row.id)!)"
            >
              驳回
            </AppButton>
          </div>
          <span v-else class="row-note">{{ row.note || "—" }}</span>
        </template>
      </AppTable>
    </Panel>

    <AppModal
      :open="rejectTarget !== null"
      title="驳回解绑申请"
      @close="rejectTarget = null"
    >
      <div class="stack">
        <p v-if="rejectTarget" class="mv-addr-tip">
          驳回后地址 <strong class="mono">{{ rejectTarget.address }}</strong> 会保留，用户可重新申请。
        </p>
        <label class="mv-addr-tip">
          批注（会展示给用户）
          <input v-model="rejectNote" type="text" maxlength="200" placeholder="例如：该前缀仍有在用，暂不解绑" />
        </label>
        <AppButton size="sm" :loading="rejectTarget !== null && Boolean(busy[rejectTarget.id])" @click="confirmReject">
          确认驳回
        </AppButton>
      </div>
    </AppModal>
  </AdminPage>
</template>
