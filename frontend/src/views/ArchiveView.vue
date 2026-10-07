<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { archiveApi } from "@/api/endpoints";
import type { ArchiveBatch } from "@/api/types";
import AppButton from "@/components/ui/AppButton.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import Panel from "@/components/admin/Panel.vue";
import { topbarSlot } from "@/stores/shell";
import { describeError, logError, toastApiError, createRequestGate } from "@/lib/async";
import { formatBytes, formatTime } from "@/lib/format";
import { useToasts } from "@/stores/toast";

// 归档：删除的文件与文件夹在这里暂存，期间可以恢复。
//
// 用户侧只有"暂存中"这一态。清除（无论单条还是清空）之后条目就不再属于用户：
// 接口查不到、也无法恢复，后续怎么处置由留存期和管理员决定。这条边界不写进
// 界面——用户不需要知道存储侧发生了什么，只需要知道清除后找不回来了。

const toasts = useToasts();

const items = ref<ArchiveBatch[]>([]);
const total = ref(0);
const limit = ref(20);
const offset = ref(0);
const loading = ref(false);
const error = ref("");

const columns: Column[] = [
  { key: "name", label: "名称", mobile: "title" },
  { key: "type", label: "类型" },
  { key: "size", label: "大小", align: "right" },
  { key: "deletedAt", label: "删除时间" },
  { key: "actions", label: "操作", align: "right" },
];

// 行是展示映射，实体随行一起带上：行内按钮要的正是这一行对应的批次，先放进
// 行里，模板就不必再按 id 回查 items、也不必处理"查不到"的情况（AppTable 的
// 行是泛型插槽，row 带上类型后模板直接读）。写法与 SharesView 的 toRow 一致。
interface ArchiveRow {
  id: string;
  batch: ArchiveBatch;
  name: string;
  type: number;
  size: string;
  deletedAt: string;
}

const rows = computed<ArchiveRow[]>(() =>
  items.value.map((batch) => ({
    id: batch.id,
    batch,
    name: batch.rootName,
    type: batch.nodeType,
    size: formatBytes(batch.sizeTotal),
    deletedAt: formatTime(batch.deletedAt),
  })),
);

const gate = createRequestGate();

async function load(): Promise<void> {
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await archiveApi.list(limit.value, offset.value);
    if (!gate.isCurrent(token)) return;
    items.value = result.items ?? [];
    total.value = result.total ?? 0;
    if (offset.value >= total.value && offset.value > 0) {
      offset.value = 0;
      await load();
    }
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("archive.list", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

onMounted(load);

// ---------------------------------------------------------------- 恢复 / 清除

const confirmRestore = ref<ArchiveBatch | null>(null);
const confirmClear = ref<ArchiveBatch | null>(null);
const confirmClearAll = ref(false);
const busy = ref(false);

async function restore(batch: ArchiveBatch): Promise<void> {
  busy.value = true;
  try {
    await archiveApi.restore(batch.id);
    toasts.success(`已恢复 ${batch.rootName}`);
    confirmRestore.value = null;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("archive.restore", err);
  } finally {
    busy.value = false;
  }
}

async function clear(batch: ArchiveBatch): Promise<void> {
  busy.value = true;
  try {
    await archiveApi.clear(batch.id);
    toasts.success(`已清除 ${batch.rootName}`);
    confirmClear.value = null;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("archive.clear", err);
  } finally {
    busy.value = false;
  }
}

async function clearAll(): Promise<void> {
  busy.value = true;
  try {
    const result = await archiveApi.clearAll();
    toasts.success(`已清空 ${result.cleared} 个条目`);
    confirmClearAll.value = false;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("archive.clear-all", err);
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="stack">
    <!-- 操作项投递到顶栏（页面名在顶栏左侧）。 -->
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <!-- 不按"当前页有几条"来禁用：那个计数只覆盖本页 limit 条，归档超过一页时
           第 2 页起按钮会被无理由地禁掉。有没有东西可清空由服务端判定。 -->
      <AppButton
        size="sm"
        variant="danger"
        icon="trash"
        :disabled="busy"
        @click="confirmClearAll = true"
      >
        清空归档
      </AppButton>
    </Teleport>

    <!-- 失败必须自己说出来：没有这条提示时接口一挂，表格就落进"归档是空的"那张
         空状态卡，用户会以为归档被清空了。写法与文件页的错误区一致。 -->
    <p v-if="error" class="notice notice--danger">{{ error }}</p>

    <Panel title="归档" :count="`${total} 个条目`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="归档是空的"
        empty-hint="在文件页删除的文件与文件夹会出现在这里，清除后不再保留。"
      >
        <template #name="{ row }">
          <span class="archive__name truncate" :title="String(row.name)">{{ row.name }}</span>
        </template>

        <template #type="{ row }">
          <span class="badge">{{ row.type === 1 ? "文件夹" : "文件" }}</span>
        </template>

        <template #actions="{ row }">
          <div class="actions">
            <AppButton
              size="sm"
              icon="refresh"
              @click="confirmRestore = row.batch"
            >
              恢复
            </AppButton>
            <AppButton
              size="sm"
              variant="danger"
              icon="trash"
              @click="confirmClear = row.batch"
            >
              清除
            </AppButton>
          </div>
        </template>
      </AppTable>
    </Panel>

    <AppPagination
      :total="total"
      :limit="limit"
      :offset="offset"
      @update:limit="(v: number) => ((limit = v), (offset = 0), load())"
      @update:offset="(v: number) => ((offset = v), load())"
    />

    <ConfirmDialog
      :open="!!confirmRestore"
      title="恢复条目"
      confirm-text="恢复"
      :loading="busy"
      :message="`把「${confirmRestore?.rootName ?? ''}」恢复到原路径？`"
      detail="原路径被占用时会自动改名；缺失的父文件夹会一并重建。"
      @confirm="confirmRestore && restore(confirmRestore)"
      @cancel="confirmRestore = null"
    />

    <ConfirmDialog
      :open="!!confirmClear"
      danger
      confirm-text="清除"
      :loading="busy"
      :message="`清除「${confirmClear?.rootName ?? ''}」？`"
      detail="清除后条目会从归档中移除，无法再恢复。"
      @confirm="confirmClear && clear(confirmClear)"
      @cancel="confirmClear = null"
    />

    <!-- 确认文案不写具体条数：接口只回当前页的 items，写上去的数字在第 2 页起
         就是错的，而真清空多少条由服务端在成功后告知（见 clearAll 的成功提示）。 -->
    <ConfirmDialog
      :open="confirmClearAll"
      danger
      confirm-text="全部清空"
      :loading="busy"
      message="清空归档中的全部条目？"
      detail="清空后全部条目会从归档中移除，无法再恢复。"
      @confirm="clearAll()"
      @cancel="confirmClearAll = false"
    />
  </div>
</template>

<style scoped>
.archive__name {
  max-width: 320px;
}
</style>
