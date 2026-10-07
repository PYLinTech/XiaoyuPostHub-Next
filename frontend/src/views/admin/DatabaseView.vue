<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { DatabaseStats, MaintenanceReport } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import StatStrip from "@/components/admin/StatStrip.vue";
import Panel from "@/components/admin/Panel.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppModal from "@/components/ui/AppModal.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import { topbarSlot } from "@/stores/shell";
import { describeError, logError, toastApiError } from "@/lib/async";
import { formatBytes, formatPercent } from "@/lib/format";
import { useToasts } from "@/stores/toast";

// 数据库运维。
// 页面上每个数字都能被维护操作改掉，因此每次操作完成后都重新拉一次统计。

const toasts = useToasts();

const stats = ref<DatabaseStats | null>(null);
const loading = ref(false);
const error = ref("");

const checkpointTruncate = ref(false);
const integrityQuick = ref(true);

const opBusy = ref(false);
const maintOpen = ref(false);
const confirmKind = ref<"" | "optimize" | "checkpoint" | "integrity" | "backup" | "maintenance">(
  "",
);

const optimizeResult = ref<Record<string, string> | null>(null);
const checkpointDone = ref("");
const integrityResult = ref<{ healthy: boolean; results: string[] } | null>(null);
const backupResult = ref<Record<string, unknown> | null>(null);
const maintenanceResult = ref<MaintenanceReport | null>(null);

const hasResult = computed(
  () =>
    optimizeResult.value !== null ||
    checkpointDone.value !== "" ||
    integrityResult.value !== null ||
    backupResult.value !== null ||
    maintenanceResult.value !== null,
);

async function load(): Promise<void> {
  loading.value = true;
  error.value = "";
  try {
    stats.value = await adminApi.databaseStats();
  } catch (err) {
    error.value = describeError(err);
    logError("admin.database", err);
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// ---------------------------------------------------------------- 空间占比

const spaceTotal = computed(() => {
  const value = stats.value;
  if (!value) {
    return 0;
  }
  return value.inUseBytes + value.freeBytes;
});

const inUseRatio = computed(() => (spaceTotal.value ? (stats.value?.inUseBytes ?? 0) / spaceTotal.value : 0));

const inUsePercent = computed(() => formatPercent(stats.value?.inUseBytes ?? 0, spaceTotal.value));
const freePercent = computed(() => formatPercent(stats.value?.freeBytes ?? 0, spaceTotal.value));

// ---------------------------------------------------------------- 流量缓冲

const trafficRows = computed<Array<{ label: string; value: number; hint: string }>>(() => {
  const traffic = stats.value?.traffic;
  return [
    { label: "待写入明细", value: traffic?.pendingDetails ?? 0, hint: "已计入额度、尚未落盘" },
    { label: "待写入主体", value: traffic?.pendingActors ?? 0, hint: "等待合并写入" },
    { label: "已记录", value: traffic?.recorded ?? 0, hint: "累计接收条数" },
    { label: "已刷盘", value: traffic?.flushes ?? 0, hint: "批量落盘次数" },
    { label: "丢弃", value: traffic?.dropped ?? 0, hint: "缓冲溢出等原因丢弃" },
    { label: "刷盘失败", value: traffic?.failedFlushes ?? 0, hint: "持续增长要查磁盘" },
  ];
});

// ---------------------------------------------------------------- 操作

const statItems = computed(() => [
  { label: "数据库大小", value: formatBytes(stats.value?.fileBytes) },
  { label: "数据页", value: stats.value?.pageCount ?? 0, hint: `空闲 ${stats.value?.freelistCount ?? 0} 页` },
  { label: "空闲空间", value: freePercent.value, hint: formatBytes(stats.value?.freeBytes) },
  { label: "写入日志", value: formatBytes(stats.value?.walBytes), hint: "等待检查点" },
]);

/** 维护报告的计数字段：缺字段时补 0，空白比 0 更容易被误读成"没跑"。 */
const maintenanceRows = computed<Array<{ label: string; value: number }>>(() => {
  const report = maintenanceResult.value;
  return [
    { label: "过期上传会话", value: report?.expiredUploads ?? 0 },
    { label: "过期下载票据", value: report?.expiredTickets ?? 0 },
    { label: "过期登录会话", value: report?.expiredSessions ?? 0 },
    { label: "回收的文件对象", value: report?.archiveFiles ?? 0 },
    { label: "清理的流量明细", value: report?.purgedTrafficLogs ?? 0 },
    { label: "清理的审计记录", value: report?.purgedAuditLogs ?? 0 },
    { label: "失败项", value: report?.failures ?? 0 },
  ];
});

const confirmMeta = computed(() => {
  switch (confirmKind.value) {
    case "optimize":
      // 常规优化不是"顺手点一下"的无害操作：VACUUM 会重建整库表文件并在
      // 全程持有写锁，期间所有写入都要排队。此前它直连执行，而弹窗里那句
      // "操作前会再次确认"恰恰对它不成立——五个按钮里唯独这个会直接动手。
      return {
        danger: false,
        title: "执行常规优化",
        confirmText: "开始优化",
        message: "立即执行一次常规优化（VACUUM）？",
        detail: "优化会重建表文件并长时间持写锁，优化期间写入会阻塞等待。",
      };
    case "checkpoint":
      return {
        danger: false,
        title: "执行写入日志检查点",
        confirmText: "执行检查点",
        message: checkpointTruncate.value
          ? "把写入日志合并回主库并清理日志文件？"
          : "把写入日志合并回主库？",
        detail: checkpointTruncate.value
          ? "清理会让正在读取的请求重新建立快照，可能造成短暂停顿。"
          : "不清理时日志文件保持现有大小。",
      };
    case "integrity":
      return {
        danger: false,
        title: "执行完整性检查",
        confirmText: "开始检查",
        message: integrityQuick.value ? "执行快速完整性检查？" : "执行完整完整性检查？",
        detail: integrityQuick.value
          ? "快速检查只扫结构索引，通常很快。"
          : "完整检查会逐页扫描全部数据，库越大耗时越长，期间会占用明显的磁盘 IO。",
      };
    case "backup":
      return {
        danger: false,
        title: "生成数据库快照",
        confirmText: "生成快照",
        message: "生成一份一致性数据库快照？",
        detail: "快照写入数据目录的 backup 文件夹，不会覆盖已有文件。",
      };
    case "maintenance":
      return {
        danger: true,
        title: "立即维护",
        confirmText: "开始维护",
        message: "立即执行一次维护任务？",
        detail: "会清理过期的上传会话、票据、登录会话，回收引用归零的对象，并按保留策略删除过期的流量与审计记录。删除不可恢复。",
      };
    default:
      return { danger: false, title: "", confirmText: "", message: "", detail: "" };
  }
});

async function runConfirmed(): Promise<void> {
  const kind = confirmKind.value;
  opBusy.value = true;
  try {
    if (kind === "optimize") {
      const result = await adminApi.databaseOptimize();
      optimizeResult.value = result.steps ?? {};
      toasts.success("常规优化已完成");
    } else if (kind === "checkpoint") {
      await adminApi.databaseCheckpoint(checkpointTruncate.value);
      checkpointDone.value = `已完成检查点（截断=${checkpointTruncate.value ? "是" : "否"}）`;
      toasts.success("写入日志检查点已完成");
    } else if (kind === "integrity") {
      const result = await adminApi.databaseIntegrity(integrityQuick.value);
      integrityResult.value = { healthy: result.healthy, results: result.results ?? [] };
    } else if (kind === "backup") {
      backupResult.value = await adminApi.databaseBackup();
      toasts.success("快照已生成");
    } else if (kind === "maintenance") {
      maintenanceResult.value = await adminApi.runMaintenance();
      toasts.success("维护已完成");
    }
    confirmKind.value = "";
    maintOpen.value = false;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.database.op", err);
  } finally {
    opBusy.value = false;
  }
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load">刷新</AppButton>
      <AppButton size="sm" variant="primary" icon="database" @click="maintOpen = true">
        维护操作
      </AppButton>
    </Teleport>

    <StatStrip :items="statItems" />

    <div class="admin-two-col">
      <Panel title="空间占用" :count="`${inUsePercent} 在用`">
        <div class="stack">
          <div class="meter meter--spaced">
            <div class="meter__fill" :style="{ width: `${Math.round(inUseRatio * 100)}%` }" />
          </div>
          <dl class="kv">
            <dt>页大小</dt>
            <dd>{{ formatBytes(stats?.pageSize) }}</dd>
            <dt>写入模式</dt>
            <dd class="mono">{{ stats?.journalMode || "—" }}</dd>
            <dt>自动回收</dt>
            <dd>{{ stats?.autoVacuum || "—" }}</dd>
            <dt>外键约束</dt>
            <dd>{{ stats?.foreignKeys ? "已开启" : "已关闭" }}</dd>
            <dt>锁等待上限</dt>
            <dd>{{ stats?.busyTimeoutMs ?? 0 }} 毫秒</dd>
          </dl>
          <p class="faint mono db-path">{{ stats?.path || "—" }}</p>
        </div>
      </Panel>

      <Panel title="写入缓冲">
        <ul class="buf-list">
          <li v-for="row in trafficRows" :key="row.label">
            <span class="buf-list__label">
              {{ row.label }}
              <em>{{ row.hint }}</em>
            </span>
            <strong class="mono">{{ row.value }}</strong>
          </li>
        </ul>
      </Panel>
    </div>

    <Panel v-if="hasResult" title="最近一次操作结果">
      <div class="stack">
        <div v-if="optimizeResult" class="result">
          <p class="result__title">常规优化</p>
          <dl class="kv">
            <template v-for="(value, key) in optimizeResult" :key="key">
              <dt class="mono">{{ key }}</dt>
              <dd>{{ value }}</dd>
            </template>
          </dl>
        </div>
        <p v-if="checkpointDone" class="notice notice--success">{{ checkpointDone }}</p>
        <div v-if="integrityResult" class="result">
          <p v-if="integrityResult.healthy" class="notice notice--success">数据库完整，未发现问题。</p>
          <div v-else class="stack--tight">
            <p class="notice notice--danger">发现 {{ integrityResult.results.length }} 项问题：</p>
            <ul class="issues">
              <li v-for="(item, index) in integrityResult.results" :key="index" class="mono">{{ item }}</li>
            </ul>
          </div>
        </div>
        <div v-if="backupResult" class="result">
          <p class="result__title">数据库快照</p>
          <dl class="kv">
            <template v-for="(value, key) in backupResult" :key="key">
              <dt class="mono">{{ key }}</dt>
              <dd>{{ key === "bytes" && typeof value === "number" ? formatBytes(value) : String(value) }}</dd>
            </template>
          </dl>
        </div>
        <div v-if="maintenanceResult" class="result">
          <p class="result__title">维护报告</p>
          <dl class="kv">
            <template v-for="row in maintenanceRows" :key="row.label">
              <dt>{{ row.label }}</dt>
              <dd>{{ row.value }}</dd>
            </template>
          </dl>
        </div>
      </div>
    </Panel>

    <AppModal :open="maintOpen" title="维护操作" @close="maintOpen = false">
      <div class="stack">
        <div class="maint-actions">
          <!-- 五个按钮同一形态：都只开确认窗，不直接执行。 -->
          <AppButton icon="refresh" :disabled="opBusy" @click="confirmKind = 'optimize'">
            常规优化
          </AppButton>
          <AppButton icon="database" :disabled="opBusy" @click="confirmKind = 'checkpoint'">
            写入日志检查点
          </AppButton>
          <AppButton icon="shield" :disabled="opBusy" @click="confirmKind = 'integrity'">
            完整性检查
          </AppButton>
          <AppButton icon="hardDrive" :disabled="opBusy" @click="confirmKind = 'backup'">
            生成快照
          </AppButton>
          <AppButton variant="danger" icon="activity" :disabled="opBusy" @click="confirmKind = 'maintenance'">
            立即维护
          </AppButton>
        </div>
        <div class="op-options">
          <label class="check">
            <input v-model="checkpointTruncate" type="checkbox" />
            <span class="check__text">
              <span>检查点后清理日志</span>
              <span class="faint">读取中的请求需重建快照</span>
            </span>
          </label>
          <label class="check">
            <input v-model="integrityQuick" type="checkbox" />
            <span class="check__text">
              <span>快速检查</span>
              <span class="faint">关闭则逐页扫描，更慢更彻底</span>
            </span>
          </label>
        </div>
        <p class="faint">操作前会再次确认；完成后结果会显示在页面底部的「最近一次操作结果」。</p>
      </div>
      <template #footer>
        <AppButton @click="maintOpen = false">关闭</AppButton>
      </template>
    </AppModal>

    <ConfirmDialog
      :open="!!confirmKind"
      :danger="confirmMeta.danger"
      :title="confirmMeta.title"
      :confirm-text="confirmMeta.confirmText"
      :message="confirmMeta.message"
      :detail="confirmMeta.detail"
      :loading="opBusy"
      @confirm="runConfirmed"
      @cancel="confirmKind = ''"
    />
  </AdminPage>
</template>

<style scoped>
.meter--spaced {
  margin-top: var(--sp-1);
}

.db-path {
  overflow-wrap: anywhere;
  font-size: var(--fs-xs);
  margin: 0;
}

.buf-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.buf-list li {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--sp-3);
  min-width: 0;
}

.buf-list__label {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
}

.buf-list__label em {
  font-style: normal;
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
}

.buf-list strong {
  font-size: var(--fs-sm);
  font-variant-numeric: tabular-nums;
}

.maint-actions {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  align-items: stretch;
}

.op-options {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.result__title {
  margin: 0 0 var(--sp-2);
  font-weight: 650;
  font-size: var(--fs-sm);
}

.issues {
  margin: 0;
  padding-left: var(--sp-5);
  word-break: break-all;
}
</style>
