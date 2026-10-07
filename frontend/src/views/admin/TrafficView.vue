<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi, type TrafficQuery } from "@/api/endpoints";
import type { TrafficLog } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import FilterBar from "@/components/admin/FilterBar.vue";
import Panel from "@/components/admin/Panel.vue";
import AppDateTimePicker from "@/components/ui/AppDateTimePicker.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import { describeError, logError, createRequestGate } from "@/lib/async";
import { dateTimeLocalToUnix, formatBytes, formatTime } from "@/lib/format";

// 流量明细：查询显式提交，高频条件常显，低频条件折叠。

const items = ref<TrafficLog[]>([]);
const total = ref(0);
const loading = ref(false);
const error = ref("");
const limit = ref(100);
const offset = ref(0);

const filters = reactive({
  actorType: "",
  userId: "",
  clientIp: "",
  groupName: "",
  action: "",
  from: "",
  to: "",
});

const ACTOR_TYPES = [
  { value: "", label: "全部类型" },
  { value: "user", label: "用户" },
  { value: "guest", label: "访客" },
] as const;

const columns: Column[] = [
  { key: "occurredAt", label: "时间", mobile: "title", width: "160px" },
  { key: "actor", label: "访问者", width: "150px" },
  { key: "groupName", label: "用户组", width: "110px" },
  { key: "action", label: "动作", width: "120px" },
  { key: "bytesPlain", label: "明文大小", align: "right", width: "100px" },
  { key: "bytesWire", label: "密文大小", align: "right", width: "100px" },
  { key: "resourcePath", label: "资源路径" },
  { key: "clientIp", label: "来源 IP", width: "130px" },
  { key: "share", label: "分享 / 取件码", width: "130px" },
];

const rows = computed<Record<string, unknown>[]>(() =>
  items.value.map((item) => ({
    occurredAt: formatTime(item.occurredAt),
    // 访客没有账号，身份只能由 IP 表达：配额与限流本身也是按 IP 计量的。
    actor: item.actorType === "guest" ? `访客（${item.clientIp || "未知 IP"}）` : `用户 #${item.userId ?? "—"}`,
    groupName: item.groupName || "—",
    action: item.action,
    bytesPlain: formatBytes(item.bytesPlain),
    bytesWire: formatBytes(item.bytesWire),
    resourcePath: item.resourcePath || "—",
    clientIp: item.clientIp || "—",
    share: item.pickupCode || item.shareId || "—",
  })),
);

function buildQuery(): TrafficQuery {
  const query: TrafficQuery = { limit: limit.value, offset: offset.value };
  if (filters.actorType) {
    query.actorType = filters.actorType;
  }
  if (filters.userId.trim()) {
    query.userId = Number(filters.userId.trim());
  }
  if (filters.clientIp.trim()) {
    query.clientIp = filters.clientIp.trim();
  }
  if (filters.groupName.trim()) {
    query.groupName = filters.groupName.trim();
  }
  if (filters.action.trim()) {
    query.action = filters.action.trim();
  }
  const from = dateTimeLocalToUnix(filters.from);
  const to = dateTimeLocalToUnix(filters.to);
  if (from) {
    query.from = from;
  }
  if (to) {
    query.to = to;
  }
  return query;
}

const userIdInvalid = computed(() => !!filters.userId.trim() && !/^\d+$/.test(filters.userId.trim()));

const gate = createRequestGate();

async function search(): Promise<void> {
  // 校验失败要先于取号返回：否则一次非法的提交会把正在飞行的合法请求
  // 判为过期，而它自己的 finally 又不会清 loading——转圈会一直卡住。
  if (userIdInvalid.value) {
    error.value = "用户编号只能是数字";
    return;
  }
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await adminApi.traffic(buildQuery());
    if (!gate.isCurrent(token)) return;
    items.value = result.items ?? [];
    total.value = result.total ?? 0;
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.traffic", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

function reset(): void {
  filters.actorType = "";
  filters.userId = "";
  filters.clientIp = "";
  filters.groupName = "";
  filters.action = "";
  filters.from = "";
  filters.to = "";
  limit.value = 100;
  offset.value = 0;
  void search();
}

function changePage(nextOffset: number, nextLimit: number): void {
  offset.value = nextOffset;
  limit.value = nextLimit;
  void search();
}

onMounted(search);
</script>

<template>
  <AdminPage :error="error">
    <FilterBar flat :busy="loading" @search="search" @reset="reset">
      <AppSelect v-model="filters.actorType" class="tf-select" aria-label="访问者类型" :options="ACTOR_TYPES" />
      <input
        v-model="filters.userId"
        class="input tf-id"
        inputmode="numeric"
        placeholder="用户编号"
        aria-label="用户编号"
      />
      <input
        v-model="filters.action"
        class="input tf-action"
        placeholder="动作：download / preview"
        aria-label="动作"
      />
      <template #extra>
        <input
          v-model="filters.clientIp"
          class="input tf-ip"
          placeholder="来源 IP（精确匹配）"
          aria-label="来源 IP"
        />
        <input
          v-model="filters.groupName"
          class="input tf-group"
          placeholder="用户组（精确匹配）"
          aria-label="用户组"
        />
        <span class="tf-range">
          <AppDateTimePicker v-model="filters.from" class="tf-time" aria-label="起始时间" />
          <span class="tf-sep">至</span>
          <AppDateTimePicker v-model="filters.to" class="tf-time" aria-label="结束时间" />
        </span>
      </template>
    </FilterBar>

    <Panel title="流量明细" :count="`共 ${total} 条`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="没有匹配的流量记录"
        empty-hint="放宽时间范围或清空条件后重试。"
      >
        <template #occurredAt="{ row }">
          <span class="nowrap">{{ row.occurredAt }}</span>
        </template>
        <template #actor="{ row }">
          <span class="nowrap">{{ row.actor }}</span>
        </template>
        <template #action="{ row }">
          <span class="mono">{{ row.action }}</span>
        </template>
        <template #bytesPlain="{ row }">
          <span class="mono">{{ row.bytesPlain }}</span>
        </template>
        <template #bytesWire="{ row }">
          <span class="mono">{{ row.bytesWire }}</span>
        </template>
        <template #resourcePath="{ row }">
          <span class="mono truncate--block" :title="String(row.resourcePath)">{{ row.resourcePath }}</span>
        </template>
        <template #clientIp="{ row }">
          <span class="mono nowrap">{{ row.clientIp }}</span>
        </template>
      </AppTable>
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
  </AdminPage>
</template>

<style scoped>

.tf-select {
  width: auto;
}

.tf-id {
  width: 110px;
}

.tf-action {
  width: 190px;
}

.tf-ip,
.tf-group {
  width: 180px;
}

.tf-time {
  width: 210px;
}

/* 起止时间作为一组参与换行，避免「至」孤悬行尾。 */
.tf-range {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.tf-sep {
  color: var(--c-text-faint);
  font-size: var(--fs-sm);
}
</style>
