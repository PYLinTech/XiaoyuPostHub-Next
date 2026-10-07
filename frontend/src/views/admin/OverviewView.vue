<script setup lang="ts">
import { computed } from "vue";
import { adminApi } from "@/api/endpoints";
import type { AdminOverview } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import Panel from "@/components/admin/Panel.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import ChartArea, { type AreaSeries } from "@/components/ui/ChartArea.vue";
import ChartBars, { type BarItem } from "@/components/ui/ChartBars.vue";
import ChartDonut, { type DonutItem } from "@/components/ui/ChartDonut.vue";
import { topbarSlot } from "@/stores/shell";
import { useAsync } from "@/lib/async";
import { formatBytes, formatRelative } from "@/lib/format";

// 概览页：站点运行态势的一屏总览。
//
// 这一页是登录后台后第一个打开的页面，所以它只回答一个问题——"站点现在
// 是什么状态"。所有卡片都是聚合口径，没有任何可点击的下钻入口：需要看
// 明细的页面（账号、分享、审计、流量）各自已经存在，再在这里铺一层入口
// 只会让人多点一次却看不到新东西。

const EMPTY: AdminOverview = {
  users: 0,
  filesByStatus: {},
  storageWire: 0,
  tickerActive: false,
  activeUsers: 0,
  usersByGroup: [],
  trafficDaily: [],
  trafficByAction: [],
  shares: { total: 0, alive: 0, visits: 0, pickups: 0, views: 0, downloads: 0 },
  mail: { total: 0, unread: 0, starred: 0, archived: 0, bytes: 0, addresses: 0 },
  recentAudit: [],
};

const { data, loading, error, run } = useAsync<AdminOverview>(() => adminApi.overview(), EMPTY);

void run();

/**
 * 数组字段兜底。
 *
 * 后端是 Go，nil 切片会序列化成 JSON `null` 而不是 `[]`。这里集中收口而不是
 * 在每个 .map() 上写 ?? []：漏一处就是整页白屏，而漏掉的恰恰是平时没数据、
 * 只有新站点才走得到的那条分支。
 */
function list<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

const totalFiles = computed(() =>
  Object.values(data.value.filesByStatus ?? {}).reduce((sum, count) => sum + count, 0),
);

const trafficTotal = computed(() => ({
  up: list(data.value.trafficDaily).reduce((s, p) => s + p.upPlain, 0),
  down: list(data.value.trafficDaily).reduce((s, p) => s + p.downPlain, 0),
}));

// ---- 文件状态 ----

/** 键是 store.FileStatus 的字符串形式。颜色直接用语义色，环形图不需要额外调色板。 */
const STATUS_META: Record<string, { label: string; color: string }> = {
  uploading: { label: "上传中", color: "var(--c-accent)" },
  normal: { label: "正常", color: "var(--c-success)" },
  disabled: { label: "已停用", color: "var(--c-danger)" },
  archive: { label: "待回收", color: "var(--c-warn)" },
  purged: { label: "存储删除", color: "var(--c-text-faint)" },
  unknown: { label: "未知", color: "var(--c-border-strong)" },
};
const STATUS_ORDER = ["normal", "uploading", "disabled", "archive", "purged", "unknown"];

const fileDonut = computed<DonutItem[]>(() => {
  // filesByStatus 后端已预置四种状态为 0，正常不会是 null；这里仍兜一层，
  // 是因为这个 computed 与上面几个数组字段同属一次响应，契约不该只保一半。
  const byStatus = data.value.filesByStatus ?? {};
  return STATUS_ORDER.filter((key) => (byStatus[key] ?? 0) > 0).map((key) => ({
    key,
    label: STATUS_META[key]?.label ?? key,
    value: byStatus[key] ?? 0,
    color: STATUS_META[key]?.color ?? "var(--c-border-strong)",
  }));
});

// ---- 流量趋势 ----

/** "2026-01-10" → "1/10"。坐标轴放不下完整日期，也没人需要看年份。 */
function shortDay(day: string): string {
  const parts = day.split("-");
  return parts.length === 3 ? `${Number(parts[1])}/${Number(parts[2])}` : day;
}

const dayLabels = computed(() => list(data.value.trafficDaily).map((p) => shortDay(p.day)));

const trafficSeries = computed<AreaSeries[]>(() => [
  {
    key: "down",
    label: "下载",
    color: "var(--c-accent)",
    values: list(data.value.trafficDaily).map((p) => p.downPlain),
  },
  {
    key: "up",
    label: "上传",
    color: "var(--c-success)",
    values: list(data.value.trafficDaily).map((p) => p.upPlain),
  },
]);

// ---- 近 7 天动作分类 ----

/** 流量动作的中文名。发信链路已移除，mail_send 不会再产生，保留映射只为旧数据可读。 */
const ACTION_META: Record<string, { label: string; color: string }> = {
  upload: { label: "上传", color: "var(--c-success)" },
  download: { label: "下载", color: "var(--c-accent)" },
  preview: { label: "预览", color: "var(--c-info, var(--c-accent))" },
  mail_recv: { label: "收信", color: "var(--c-warn)" },
  mail_view: { label: "读信", color: "var(--c-warn)" },
  mail_send: { label: "发信", color: "var(--c-text-faint)" },
};

const actionBars = computed<BarItem[]>(() =>
  list(data.value.trafficByAction).map((a) => ({
    key: a.action,
    label: ACTION_META[a.action]?.label ?? a.action,
    value: a.count,
    color: ACTION_META[a.action]?.color ?? "var(--c-accent)",
    hint: formatBytes(a.bytes),
  })),
);

// ---- 用户分组 ----

const groupBars = computed<BarItem[]>(() =>
  list(data.value.usersByGroup).map((g) => ({
    key: g.groupName,
    label: g.groupName,
    value: g.count,
  })),
);

// ---- 最近操作 ----

/**
 * 审计动作是点分小写（share.create / db.backup / user.status）。
 * 这里按"对象 + 动作"两段翻译，翻译不了的原样显示——活动流宁可显示
 * 一串英文，也不能因为词典缺词就空一行，那会让它看起来像坏了。
 */
const AUDIT_OBJECT: Record<string, string> = {
  share: "分享",
  pickup: "取件码",
  user: "账号",
  group: "用户组",
  invite: "邀请码",
  archive: "归档",
  db: "数据库",
  storage: "存储",
  mail: "邮件",
  settings: "系统配置",
  announcement: "公告",
  maintenance: "维护",
  setup: "初始化",
};
const AUDIT_VERB: Record<string, string> = {
  create: "新建",
  delete: "删除",
  update: "修改",
  save: "保存",
  clear: "清空",
  clear_all: "全部清空",
  purge: "清除",
  restore: "还原",
  backup: "备份",
  checkpoint: "检查点",
  integrity: "完整性检查",
  optimize: "优化",
  disable: "禁用",
  enable: "启用",
  group: "调整分组",
  status: "变更状态",
  password_change: "修改密码",
  password_reset: "重置密码",
  start: "开始",
  finish: "完成",
  complete: "完成",
  run: "执行",
};

function auditLabel(action: string): string {
  const [head, ...rest] = action.split(".");
  const tail = rest.join(".");
  const obj = AUDIT_OBJECT[head];
  const verb = AUDIT_VERB[tail] ?? (tail ? tail.replace(/_/g, " ") : "");
  if (obj && verb) return `${obj}${verb}`;
  if (obj) return obj;
  return action;
}

const activity = computed(() =>
  list(data.value.recentAudit).map((log) => ({
    id: log.id,
    label: auditLabel(log.action),
    target: log.target,
    detail: log.detail,
    when: formatRelative(log.occurredAt),
  })),
);

// ---- 磁贴 ----

const tiles = computed(() => [
  {
    key: "users",
    icon: "user",
    label: "账号",
    value: String(data.value.users),
    hint: `近 7 天活跃 ${data.value.activeUsers}`,
  },
  {
    key: "files",
    icon: "file",
    label: "文件对象",
    value: String(totalFiles.value),
    hint: formatBytes(data.value.storageWire),
  },
  {
    key: "shares",
    icon: "share",
    label: "分享链接",
    value: String(data.value.shares.total),
    hint: `生效 ${data.value.shares.alive} · 累计访问 ${data.value.shares.visits}`,
  },
  {
    key: "pickups",
    icon: "archive",
    label: "取件码",
    value: String(data.value.shares.pickups),
    hint: `近 7 天取件 ${data.value.shares.downloads}`,
  },
  {
    key: "mail",
    icon: "mail",
    label: "收件箱",
    value: String(data.value.mail.total),
    hint: `未读 ${data.value.mail.unread} · 星标 ${data.value.mail.starred}`,
  },
  {
    key: "views",
    icon: "eye",
    label: "近 7 天访问",
    value: String(data.value.shares.views + data.value.shares.downloads),
    hint: `浏览 ${data.value.shares.views} · 下载 ${data.value.shares.downloads}`,
  },
]);
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="run()">刷新</AppButton>
    </Teleport>

    <div class="kpis">
      <div v-for="tile in tiles" :key="tile.key" class="kpi">
        <span class="kpi__icon"><AppIcon :name="tile.icon" :size="18" /></span>
        <div class="kpi__body">
          <span class="kpi__label">{{ tile.label }}</span>
          <strong class="kpi__value">{{ tile.value }}</strong>
          <span class="kpi__hint">{{ tile.hint }}</span>
        </div>
      </div>
    </div>

    <div class="board">
      <Panel class="board__wide" title="近 30 天流量趋势">
        <template #actions>
          <span class="board__sum">上传 {{ formatBytes(trafficTotal.up) }} · 下载 {{ formatBytes(trafficTotal.down) }}</span>
        </template>
        <ChartArea
          :series="trafficSeries"
          :labels="dayLabels"
          :height="260"
          :format-value="formatBytes"
          empty-text="近 30 天还没有流量记录"
        />
      </Panel>

      <Panel title="文件状态分布">
        <template #actions>
          <span class="board__sum">{{ totalFiles }} 个对象</span>
        </template>
        <ChartDonut
          :items="fileDonut"
          center-label="对象总数"
          :format-value="(n: number) => String(n)"
          empty-text="还没有文件对象"
        />
      </Panel>

      <Panel title="用户分组分布">
        <template #actions>
          <span class="board__sum">{{ list(data.usersByGroup).length }} 个分组</span>
        </template>
        <ChartBars
          :items="groupBars"
          :format-value="(n: number) => `${n} 人`"
          empty-text="还没有账号"
        />
      </Panel>

      <Panel title="近 7 天操作类型">
        <template #actions>
          <span class="board__sum">按流量明细计数</span>
        </template>
        <ChartBars
          :items="actionBars"
          :format-value="(n: number) => String(n)"
          empty-text="近 7 天没有流量记录"
        />
      </Panel>

      <Panel class="board__wide" title="最近操作">
        <template #actions>
          <span class="board__sum">{{ data.tickerActive ? "顶部公告运行中" : "顶部公告未启用" }}</span>
        </template>
        <ul v-if="activity.length" class="feed">
          <li v-for="item in activity" :key="item.id" class="feed__row">
            <span class="feed__dot" />
            <span class="feed__label">{{ item.label }}</span>
            <span v-if="item.target" class="feed__target mono" :title="item.target">{{ item.target }}</span>
            <span class="feed__when">{{ item.when }}</span>
          </li>
        </ul>
        <AppEmpty v-else title="还没有管理操作记录" />
      </Panel>
    </div>
  </AdminPage>
</template>

<style scoped>
/* ---- 磁贴行 ---- */

.kpis {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(210px, 100%), 1fr));
  gap: var(--sp-3);
}

.kpi {
  display: flex;
  align-items: flex-start;
  gap: var(--sp-3);
  min-width: 0;
  padding: var(--sp-3) var(--sp-4);
  border: 1px solid var(--c-border-card);
  border-radius: var(--r-lg);
  background: var(--c-surface);
  box-shadow: var(--shadow-sm);
}

.kpi__icon {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  flex: none;
  border-radius: var(--r-md);
  background: var(--c-accent-weak);
  color: var(--c-accent);
}

.kpi__body {
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.kpi__label {
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
}

.kpi__value {
  margin-top: 2px;
  font-size: clamp(20px, 1.6vw, 25px);
  line-height: 1.2;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
}

.kpi__hint {
  margin-top: 2px;
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* ---- 看板网格 ---- */

/* 自动流 + 显式跨列：窄屏不用写断点，格子自己往下掉。
   只有"趋势图要两列宽"这一处需要跨度，所以用 grid-column 而不是
   为每种宽度各写一套模板。 */
.board {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(330px, 100%), 1fr));
  gap: var(--sp-4);
  /* 不写 align-items: start —— 同一行里的卡片必须等高。内容少的卡片
     （环形图固定 168px，条形图随条目数变化）若各自收缩，行内就会参差不齐，
     这正是"看板"和"一堆卡片"在视觉上的分界线。 */
  align-items: stretch;
}

/* 卡片撑满格子，body 再撑满卡片：内容在剩余高度里垂直居中，
   短内容不会顶在上沿、长内容也不会把卡片撑破。 */
.board :deep(.panelbox) {
  height: 100%;
  display: flex;
  flex-direction: column;
}

.board :deep(.panelbox__body) {
  flex: 1;
  min-height: 200px;
  display: flex;
  flex-direction: column;
  justify-content: center;
}

.board__wide {
  grid-column: 1 / -1;
}

.board__sum {
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  font-variant-numeric: tabular-nums;
}

/* ---- 活动流 ---- */

.feed {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
}

.feed__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) 0;
  border-top: 1px solid var(--c-border-line);
  font-size: var(--fs-sm);
}

.feed__row:first-child {
  border-top: 0;
}

.feed__dot {
  width: 6px;
  height: 6px;
  flex: none;
  border-radius: var(--r-pill);
  background: var(--c-border-strong);
}

.feed__label {
  flex: none;
  color: var(--c-text);
}

.feed__target {
  min-width: 0;
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
}

.feed__when {
  flex: none;
  margin-left: auto;
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  font-variant-numeric: tabular-nums;
}

</style>
