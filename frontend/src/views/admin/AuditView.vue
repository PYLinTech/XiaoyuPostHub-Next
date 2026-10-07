<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { AuditLog } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import FilterBar from "@/components/admin/FilterBar.vue";
import Panel from "@/components/admin/Panel.vue";
import AppDateTimePicker from "@/components/ui/AppDateTimePicker.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import { describeError, logError, createRequestGate } from "@/lib/async";
import { dateTimeLocalToUnix, formatTime } from "@/lib/format";

// 审计记录：动作常显，时间范围折叠。

const items = ref<AuditLog[]>([]);
const total = ref(0);
const loading = ref(false);
const error = ref("");
const limit = ref(100);
const offset = ref(0);

const filters = reactive({ action: "", actionPrefix: "", from: "", to: "" });

const columns: Column[] = [
  { key: "occurredAt", label: "时间", mobile: "title", width: "140px" },
  { key: "actor", label: "操作者", width: "95px" },
  { key: "clientIp", label: "来源 IP", width: "100px" },
  { key: "action", label: "动作", width: "140px" },
  { key: "target", label: "目标", width: "80px" },
  { key: "detail", label: "详情" },
];

const rows = computed<Record<string, unknown>[]>(() =>
  items.value.map((item) => ({
    occurredAt: formatTime(item.occurredAt),
    actor: `${item.actorType === "guest" ? "访客" : item.actorType === "admin" ? "管理员" : "用户"} #${item.actorId}`,
    clientIp: item.clientIp || "—",
    action: item.action,
    target: item.target || "—",
    detail: item.detail || "—",
  })),
);

const gate = createRequestGate();

async function search(): Promise<void> {
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const from = dateTimeLocalToUnix(filters.from);
    const to = dateTimeLocalToUnix(filters.to);
    const result = await adminApi.audit({
      action: filters.action.trim() || undefined,
      actionPrefix: filters.action.trim() ? undefined : filters.actionPrefix.trim() || undefined,
      from: from || undefined,
      to: to || undefined,
      limit: limit.value,
      offset: offset.value,
    });
    if (!gate.isCurrent(token)) return;
    items.value = result.items ?? [];
    total.value = result.total ?? 0;
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.audit", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

function reset(): void {
  filters.action = "";
  filters.actionPrefix = "";
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
      <input
        v-model="filters.action"
        class="input tf-action"
        placeholder="动作：精确匹配，例如 settings.update"
        aria-label="动作精确匹配"
      />
      <input
        v-model="filters.actionPrefix"
        class="input tf-prefix"
        placeholder="动作前缀，例如 admin.mail（精确匹配留空时生效）"
        aria-label="动作前缀匹配"
      />
      <template #extra>
        <span class="tf-range">
          <AppDateTimePicker v-model="filters.from" class="tf-time" aria-label="起始时间" />
          <span class="tf-sep">至</span>
          <AppDateTimePicker v-model="filters.to" class="tf-time" aria-label="结束时间" />
        </span>
      </template>
    </FilterBar>

    <Panel title="审计记录" :count="`共 ${total} 条`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="没有匹配的审计记录"
        empty-hint="放宽时间范围或清空动作名后重试。"
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
        <template #target="{ row }">
          <span class="mono truncate--block" :title="String(row.target)">{{ row.target }}</span>
        </template>
        <template #detail="{ row }">
          <span class="truncate--block" :title="String(row.detail)">{{ row.detail }}</span>
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
/* 详情这类长文本在自动布局下 min-content 不可收缩，会把表撑出横向滚动；
   日志表用固定布局 + 明确列宽是可靠解法。窄屏卡片模式改 display 后不受影响。 */
:deep(.table) {
  table-layout: fixed;
}

:deep(tbody td) {
  overflow: hidden;
}

.tf-action {
  width: 290px;
}

.tf-prefix {
  width: 300px;
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
