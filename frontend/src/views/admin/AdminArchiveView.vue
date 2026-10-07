<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { ArchiveBatch } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import FilterBar from "@/components/admin/FilterBar.vue";
import Panel from "@/components/admin/Panel.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import { topbarSlot } from "@/stores/shell";
import { describeError, logError, toastApiError, createRequestGate } from "@/lib/async";
import { formatBytes, formatTime } from "@/lib/format";
import { useToasts } from "@/stores/toast";

// 归档管理：全局批次列表 + 真实删除。
//
// 管理员只能向"存储删除"单向推进，没有恢复能力。真实删除远端对象的前提是
// 引用归零：去重共享的内容即使本批次被清理，只要别人还在用，存储对象必须保留。

const toasts = useToasts();

const items = ref<ArchiveBatch[]>([]);
const total = ref(0);
const limit = ref(20);
const offset = ref(0);
const loading = ref(false);
const error = ref("");
const filters = reactive({ state: 0 });

const selected = ref(new Set<string>());

function toggleSelect(id: string, checked: boolean): void {
  const next = new Set(selected.value);
  if (checked) {
    next.add(id);
  } else {
    next.delete(id);
  }
  selected.value = next;
}

function toggleAll(checked: boolean): void {
  selected.value = checked
    ? new Set(items.value.filter((b) => b.state !== 3).map((b) => b.id))
    : new Set<string>();
}

const allSelected = computed(
  () => items.value.length > 0 && items.value.every((b) => b.state === 3 || selected.value.has(b.id)),
);

const columns: Column[] = [
  { key: "select", label: "", width: "36px" },
  { key: "name", label: "名称", mobile: "title" },
  { key: "type", label: "类型" },
  { key: "user", label: "用户" },
  { key: "size", label: "大小", align: "right" },
  { key: "deletedAt", label: "删除时间" },
  { key: "state", label: "状态" },
];

const STATE_META: Record<number, { label: string; tone: string }> = {
  1: { label: "归档暂存", tone: "badge--accent" },
  2: { label: "归档删除", tone: "" },
  3: { label: "存储删除", tone: "badge--danger" },
};

const rows = computed<Record<string, unknown>[]>(() =>
  items.value.map((batch) => ({
    id: batch.id,
    name: batch.rootName,
    type: batch.nodeType,
    user: batch.userAccount,
    size: formatBytes(batch.sizeTotal),
    deletedAt: formatTime(batch.deletedAt),
    state: batch.state,
    stateLabel: (STATE_META[batch.state] ?? { label: "未知" }).label,
    stateTone: (STATE_META[batch.state] ?? { tone: "" }).tone,
  })),
);

const filterItems = [
  { value: 0, label: "全部状态" },
  { value: 1, label: "归档暂存" },
  { value: 2, label: "归档删除" },
  { value: 3, label: "存储删除" },
];

const gate = createRequestGate();

async function load(): Promise<void> {
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await adminApi.archiveList(filters.state, limit.value, offset.value);
    if (!gate.isCurrent(token)) return;
    items.value = result.items ?? [];
    total.value = result.total ?? 0;
    selected.value = new Set(
      [...selected.value].filter((id) => items.value.some((b) => b.id === id && b.state !== 3)),
    );
    if (offset.value >= total.value && offset.value > 0) {
      offset.value = 0;
      await load();
    }
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.archive", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

onMounted(load);

// 翻页与换页长：改完条数回到第一页，否则新页长下旧的 offset 可能直接越界。
function changePage(nextOffset: number, nextLimit: number): void {
  offset.value = nextOffset;
  limit.value = nextLimit;
  void load();
}

// ---------------------------------------------------------------- 真实删除

const confirmPurge = ref(false);
const purging = ref(false);

async function purge(): Promise<void> {
  purging.value = true;
  try {
    const ids = [...selected.value];
    const result = await adminApi.archivePurge(ids);
    toasts.success(`已真实删除 ${result.purged ?? ids.length} 个批次`, "记录保留，存储侧对象已删除");
    confirmPurge.value = false;
    selected.value = new Set();
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.archive.purge", err);
  } finally {
    purging.value = false;
  }
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load()">刷新</AppButton>
      <AppButton
        size="sm"
        variant="danger"
        icon="trash"
        :disabled="selected.size === 0 || purging"
        @click="confirmPurge = true"
      >
        真实删除（{{ selected.size }}）
      </AppButton>
    </Teleport>

    <!-- 显式查询：与 AuditView / TrafficView 同一形态。此前状态选择器上挂了
         @update:model-value="load()"，改一次下拉就发一次请求，紧接着再按「查询」
         又发一次参数完全相同的请求；而这里只有一个筛选项，即时查询并没有省掉
         什么操作。busy 则必须有，否则请求在途时「查询」按钮没有任何反馈。 -->
    <FilterBar flat :busy="loading" @search="load()" @reset="((filters.state = 0), load())">
      <label class="tf-label">
        状态
        <AppSelect
          v-model="filters.state"
          class="tf-state"
          aria-label="状态"
          :options="filterItems"
        />
      </label>
      <template #extra>
        <button
          type="button"
          class="btn btn--default btn--sm"
          :disabled="items.length === 0"
          @click="toggleAll(!allSelected)"
        >
          {{ allSelected ? "取消全选" : "全选本页（可删除项）" }}
        </button>
      </template>
    </FilterBar>

    <Panel :count="`共 ${total} 个批次`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="没有匹配的批次"
        empty-hint="用户删除的文件与文件夹会出现在这里。"
      >
        <template #select="{ row }">
          <input
            type="checkbox"
            :checked="selected.has(String(row.id))"
            :disabled="row.state === 3"
            :aria-label="`选择 ${row.name}`"
            @change="toggleSelect(String(row.id), ($event.target as HTMLInputElement).checked)"
          />
        </template>

        <template #name="{ row }">
          <span class="truncate" :title="String(row.name)">{{ row.name }}</span>
        </template>

        <template #type="{ row }">
          <span class="badge">{{ row.type === 1 ? "文件夹" : "文件" }}</span>
        </template>

        <template #state="{ row }">
          <span class="badge" :class="String(row.stateTone)">{{ row.stateLabel }}</span>
        </template>
      </AppTable>
      <!-- 分页收进 Panel 底栏：批次只增不减，翻页是这页的主要动作，
           挂在卡片外面会像一条没有落点的浮条。 -->
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

    <ConfirmDialog
      :open="confirmPurge"
      danger
      confirm-text="真实删除"
      :loading="purging"
      :message="`真实删除选中的 ${selected.size} 个批次？`"
      detail="存储侧对象将被真实删除且不可恢复（记录保留）。去重共享且他人仍在使用的内容只会解除本批次的引用，存储对象保留。"
      @confirm="purge()"
      @cancel="confirmPurge = false"
    />
  </AdminPage>
</template>

<style scoped>
.tf-label {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-sm);
  color: var(--c-text-muted);
}

.tf-state {
  min-width: 140px;
}
</style>
