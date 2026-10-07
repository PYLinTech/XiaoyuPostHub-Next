<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { AdminMailListItem, AdminMailStats, AdminMailboxDetail } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import FilterBar from "@/components/admin/FilterBar.vue";
import Panel from "@/components/admin/Panel.vue";
import StatStrip, { type StatItem } from "@/components/admin/StatStrip.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import AppModal from "@/components/ui/AppModal.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import { createRequestGate, describeError, toastApiError } from "@/lib/async";
import { formatBytes, formatTime } from "@/lib/format";
import { useToasts } from "@/stores/toast";

// 邮件管理：全站邮件检索 + 对单条归属行归档/恢复/彻底删除。
// 详情只读元数据（含部件清单），不提供正文与附件下载通道。
//
// 页面骨架与文件管理页保持一致：统计条 → 筛选条 → 表格（分页收在面板底栏）。
// 两个页面问的是同一类问题，管理员在它们之间来回切时不该重新学一遍布局。

const toasts = useToasts();

const items = ref<AdminMailListItem[]>([]);
const total = ref(0);
const loading = ref(false);
const error = ref("");
const limit = ref(50);
const offset = ref(0);

// 空站也要给出结构完整的统计：map 为 null 会让前端取值时抛错，
// 整页白屏比显示一排 0 糟糕得多。
const EMPTY_STATS: AdminMailStats = {
  total: 0,
  unread: 0,
  starred: 0,
  archived: 0,
  released: 0,
  userCount: 0,
};

const stats = ref<AdminMailStats>(EMPTY_STATS);

/**
 * 统计条恒为全站值，不随筛选变化——它回答的是"整个系统里有多少邮件"，
 * 而这正是这一页存在的理由。一旦跟着筛选走，它就只是表格上方的副本。
 */
const statItems = computed<StatItem[]>(() => [
  {
    label: "全站邮件",
    value: stats.value.total,
    hint: `分布在 ${stats.value.userCount} 个用户的收件箱`,
  },
  { label: "未读", value: stats.value.unread, tone: "badge--warn" },
  { label: "已星标", value: stats.value.starred },
  { label: "已归档", value: stats.value.archived },
  { label: "已销毁", value: stats.value.released, tone: "" },
]);

// 状态筛选取值与后端 adminMailStatuses 一致；"role/归属" 不提供——
// mailboxes 表的 CHECK 写死 role = 'inbox'，一个只有单一取值的下拉是纯噪音。
const MAIL_STATES = [
  { value: "", label: "全部状态" },
  { value: "normal", label: "正常" },
  { value: "archived", label: "归档" },
  { value: "released", label: "已销毁" },
] as const;

const filters = reactive({
  q: "",
  // 默认不过滤状态：这一页回答的是"整个系统里有哪些邮件"，
  // 一上来就只给"正常"等于把归档与已销毁的藏起来。
  status: "",
  owner: "",
});

// ---- 详情弹窗 ----
const detailOpen = ref(false);
const detail = ref<AdminMailboxDetail | null>(null);
const detailLoading = ref(false);

// ---- 处置确认 ----
// 列表行与详情弹窗共用，只需归属 ID 与当前状态。
interface MailboxTarget {
  id: number;
  status: string;
}
const acting = ref(false);
const confirmState = reactive<{
  open: boolean;
  action: "archive" | "purge" | null;
  target: MailboxTarget | null;
}>({ open: false, action: null, target: null });

interface AdminMailRow {
  raw: AdminMailListItem;
  sentAt: string;
  owner: string;
  peer: string;
  subject: string;
  status: string;
  statusCls: string;
  unread: boolean;
  attachCount: number;
}

const columns: Column[] = [
  { key: "sentAt", label: "时间", width: "150px", mobile: "title" },
  { key: "owner", label: "属主", width: "110px" },
  { key: "peer", label: "发件人", width: "220px" },
  { key: "subject", label: "主题" },
  { key: "status", label: "状态", width: "82px" },
  { key: "actions", label: "操作", width: "190px" },
];

const statusLabel: Record<string, string> = {
  normal: "正常",
  archived: "归档",
  released: "已销毁",
};
// 用全局那套语义类（app.css）：这页原先 scoped 重定义了一套同名 .badge，
// 于是同一个"已归档"在本页是手写橙色、在别处是令牌橙，且暗色模式不跟随主题。
const statusClass: Record<string, string> = {
  normal: "badge--success",
  archived: "badge--warn",
  released: "",
};

const rows = computed<AdminMailRow[]>(() =>
  items.value.map((it) => ({
    raw: it,
    sentAt: formatTime(it.sentAt),
    owner: it.ownerDisplayName ? `${it.ownerDisplayName}（${it.ownerAccount}）` : it.ownerAccount,
    peer: it.fromName ? `${it.fromName} <${it.fromAddress}>` : it.fromAddress,
    subject: it.subject || "（无主题）",
    status: statusLabel[it.status] ?? it.status,
    statusCls: statusClass[it.status] ?? "",
    unread: !it.isRead,
    attachCount: it.attachmentCount ?? 0,
  })),
);

const gate = createRequestGate();
const detailGate = createRequestGate();

async function search(): Promise<void> {
  // 序号守卫：连按查询/翻页时，只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    // 属主输入框：纯数字（1..安全整数）按用户 ID 精确过滤；
    // 其余（含 "0"、超界数字、非数字）一律按账号子串过滤，
    // 不能把 0 发给后端——后端把 userId=0 解释成"不过滤"，会查出全部。
    const ownerInput = filters.owner.trim();
    let numericOwner: number | undefined;
    if (/^\d+$/.test(ownerInput)) {
      const n = Number(ownerInput);
      if (n >= 1 && n <= Number.MAX_SAFE_INTEGER) {
        numericOwner = n;
      }
    }
    const res = await adminApi.listAllMail({
      q: filters.q.trim() || undefined,
      status: filters.status || undefined,
      userId: numericOwner,
      owner: numericOwner === undefined && ownerInput !== "" ? ownerInput : undefined,
      limit: limit.value,
      offset: offset.value,
    });
    if (!gate.isCurrent(token)) return;
    items.value = res.items ?? [];
    total.value = res.total ?? 0;
    // 统计条跟着列表一起回来，省掉一次额外请求；它不受筛选影响，
    // 所以翻页与筛选都不需要重新统计。
    stats.value = res.stats ?? EMPTY_STATS;
    // 处置后若当前页已空（如最后一页的唯一一行被销毁），自动回退一页。
    // 重入会取新的令牌，本层随即过期，loading 由内层负责清——这正是
    // finally 加守卫要保住的行为。
    if (items.value.length === 0 && total.value > 0 && offset.value > 0) {
      offset.value = Math.max(0, offset.value - limit.value);
      await search();
    }
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

function doSearch(): void {
  offset.value = 0;
  void search();
}

function reset(): void {
  filters.q = "";
  // 与 filters 的初始值一致：重置是"回到刚进页面时的样子"，
  // 设成别的默认值会让刚打开就点重置的列表凭空变窄。
  filters.status = "";
  filters.owner = "";
  offset.value = 0;
  void search();
}

function changePage(nextOffset: number, nextLimit: number): void {
  offset.value = nextOffset;
  limit.value = nextLimit;
  void search();
}

// loadDetail 打开并加载指定归属；处置后复用它刷新弹窗内状态。
async function loadDetail(id: number): Promise<void> {
  // 详情单独一个序号守卫：弹窗开着时关掉再点另一行，先发的慢请求若后返回，
  // 会把上一封邮件写进新弹窗；更糟的是它失败时会把正在正常加载的弹窗一起关掉。
  const token = detailGate.next();
  detail.value = null;
  detailOpen.value = true;
  detailLoading.value = true;
  try {
    const result = await adminApi.adminMailboxDetail(id);
    if (!detailGate.isCurrent(token)) return;
    detail.value = result;
  } catch (err) {
    if (!detailGate.isCurrent(token)) return;
    detailOpen.value = false;
    toastApiError(toasts, err);
  } finally {
    if (detailGate.isCurrent(token)) detailLoading.value = false;
  }
}

// 归档行的主按钮语义是"恢复"，正常行是"归档"；列表与详情弹窗共用确认窗。
function ask(action: "archive" | "purge", target: MailboxTarget): void {
  confirmState.open = true;
  confirmState.action = action;
  confirmState.target = target;
}

async function runAction(): Promise<void> {
  const { action, target } = confirmState;
  if (!action || !target) {
    return;
  }
  acting.value = true;
  try {
    if (action === "archive") {
      if (target.status === "archived") {
        await adminApi.adminRestoreMail(target.id);
        toasts.success("已恢复到用户邮箱");
      } else {
        await adminApi.adminArchiveMail(target.id);
        toasts.success("已移入对应用户的归档");
      }
    } else {
      await adminApi.adminPurgeMail(target.id);
      toasts.success("已彻底删除");
    }
    confirmState.open = false;
    const wasDetailOpen = detailOpen.value && detail.value?.box.id === target.id;
    await search();
    // 弹窗里发起的处置：销毁/归档后重新拉取，弹窗直接反映新状态（销毁后为留痕视图）。
    if (wasDetailOpen) {
      await loadDetail(target.id);
    }
  } catch (err) {
    toastApiError(toasts, err);
  } finally {
    acting.value = false;
  }
}

function partKindLabel(kind: string): string {
  if (kind === "body") {
    return "正文";
  }
  if (kind === "inline") {
    return "内嵌图";
  }
  return "附件";
}

onMounted(search);
</script>

<template>
  <AdminPage :error="error">
    <StatStrip :items="statItems" />

    <!-- 状态筛选常显：归档/销毁都是这一页的主要操作入口，
         折进「更多筛选」等于让常用条件多点一次。
         归属（role）不提供——表上的 CHECK 写死 inbox，没有第二个取值可选。 -->
    <FilterBar flat :busy="loading" @search="doSearch" @reset="reset">
      <input
        v-model="filters.q"
        class="input tf-q"
        placeholder="搜索全站邮件：主题 / 发件人 / 收件人 / 邮件 ID"
        aria-label="关键词"
      />
      <AppSelect v-model="filters.status" :options="MAIL_STATES" aria-label="状态" size="sm" />
      <input
        v-model="filters.owner"
        class="input tf-uid"
        placeholder="属主账号或用户 ID"
        aria-label="属主账号或用户 ID"
      />
      <template #extra>
        <p class="filterbar__hint">
          检索范围是全站所有用户的邮件归属行，不限于当前管理员能登录的邮箱。
          「属主」按账号或用户 ID 定位到具体某个人；关键词支持中间片段匹配。
        </p>
      </template>
    </FilterBar>

    <Panel :title="filters.q || filters.owner ? '检索结果' : '全站邮件'" :count="`共 ${total} 封`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="没有匹配的邮件"
        empty-hint="放宽筛选条件后重试。"
      >
        <template #peer="{ row }">
          <span class="mono" :title="row.peer">{{ row.peer }}</span>
        </template>
        <template #subject="{ row }">
          <span class="subj">
            <span v-if="row.unread" class="unread-dot" title="用户未读" aria-label="未读" />
            <i v-if="row.attachCount > 0" class="ri-attachment-2 att" :title="`${row.attachCount} 个附件`" />
            <span :class="{ 'subj--unread': row.unread }" :title="row.subject">{{ row.subject }}</span>
          </span>
        </template>
        <template #status="{ row }">
          <span class="badge" :class="row.statusCls">{{ row.status }}</span>
        </template>
        <template #actions="{ row }">
          <div class="acts">
            <AppButton size="sm" variant="ghost" @click="loadDetail(row.raw.id)">详情</AppButton>
            <AppButton
              v-if="row.raw.status === 'normal'"
              size="sm"
              variant="ghost"
              @click="ask('archive', { id: row.raw.id, status: row.raw.status })"
            >
              归档
            </AppButton>
            <AppButton
              v-else-if="row.raw.status === 'archived'"
              size="sm"
              variant="ghost"
              @click="ask('archive', { id: row.raw.id, status: row.raw.status })"
            >
              恢复
            </AppButton>
            <AppButton
              v-if="row.raw.status === 'archived'"
              size="sm"
              variant="danger"
              @click="ask('purge', { id: row.raw.id, status: row.raw.status })"
            >
              销毁
            </AppButton>
          </div>
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

    <AppModal :open="detailOpen" title="邮件详情（管理视图）" wide @close="detailOpen = false">
      <div v-if="detailLoading" class="hint">加载中…</div>
      <div v-else-if="detail" class="stack">
        <dl class="meta">
          <dt>属主</dt>
          <dd>
            {{ detail.ownerDisplayName || detail.ownerAccount }}
            （{{ detail.ownerAccount }} #{{ detail.box.userId }}）
          </dd>
          <dt>状态</dt>
          <dd>
            <span class="badge" :class="statusClass[detail.box.status]">
              {{ statusLabel[detail.box.status] ?? detail.box.status }}
            </span>
            <span class="read-state">{{ detail.box.isRead ? "用户已读" : "用户未读" }}</span>
          </dd>
          <template v-if="detail.box.status === 'archived'">
            <dt>归档时间</dt>
            <dd>{{ formatTime(detail.box.archivedAt ?? 0) }}</dd>
            <dt>计划销毁</dt>
            <dd>
              {{ detail.box.purgeAt ? formatTime(detail.box.purgeAt) : "到期清理未启用" }}
              <span class="hint">（在此之前可恢复）</span>
            </dd>
          </template>
          <dt>发件人</dt>
          <dd class="mono">
            {{ detail.message.fromName || "（无名称）" }}
            &lt;{{ detail.message.fromAddress }}&gt;
          </dd>
          <dt>收件人</dt>
          <dd class="rcpts">
            <span v-for="r in detail.recipients" :key="`${r.kind}-${r.address}-${r.seq}`" class="rcpt">
              <em class="rcpt-kind">{{ r.kind }}</em>
              <span class="mono">{{ r.name ? `${r.name} <${r.address}>` : r.address }}</span>
            </span>
            <span v-if="detail.recipients.length === 0" class="hint">（无收件人记录）</span>
          </dd>
          <dt>主题</dt>
          <dd>{{ detail.message.subject || "（无主题）" }}</dd>
          <dt>时间</dt>
          <dd>{{ formatTime(detail.message.sentAt) }}</dd>
          <dt>大小</dt>
          <dd>{{ formatBytes(detail.message.sizePlain) }}（{{ detail.parts.length }} 个部件）</dd>
          <dt>SPF</dt>
          <dd class="mono">{{ detail.message.spfResult || "—" }}</dd>
          <dt>部件清单</dt>
          <dd>
            <table class="parts">
              <thead>
                <tr>
                  <th>类型</th>
                  <th>文件名</th>
                  <th>Content-Type</th>
                  <th class="num">大小</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="p in detail.parts" :key="p.id">
                  <td>{{ partKindLabel(p.kind) }}</td>
                  <td class="mono">{{ p.fileName || "（正文容器）" }}</td>
                  <td class="mono">{{ p.contentType || "—" }}</td>
                  <td class="num">{{ formatBytes(p.sizePlain) }}</td>
                </tr>
              </tbody>
            </table>
            <p class="hint">管理视图不提供正文渲染与附件下载。</p>
          </dd>
        </dl>
      </div>
      <template #footer>
        <div v-if="detail" class="modal-foot">
          <AppButton
            v-if="detail.box.status === 'normal'"
            @click="ask('archive', { id: detail.box.id, status: detail.box.status })"
          >
            移入归档
          </AppButton>
          <AppButton
            v-if="detail.box.status === 'archived'"
            @click="ask('archive', { id: detail.box.id, status: detail.box.status })"
          >
            恢复
          </AppButton>
          <AppButton
            v-if="detail.box.status === 'archived'"
            variant="danger"
            @click="ask('purge', { id: detail.box.id, status: detail.box.status })"
          >
            彻底销毁
          </AppButton>
          <span v-if="detail.box.status === 'released'" class="hint">
            已销毁，仅保留元数据留痕，不可处置
          </span>
          <span class="modal-foot__sp" />
          <AppButton @click="detailOpen = false">关闭</AppButton>
        </div>
      </template>
    </AppModal>

    <ConfirmDialog
      :open="confirmState.open"
      :title="confirmState.action === 'purge' ? '彻底删除邮件' : '邮件处置'"
      :message="
        confirmState.action === 'purge'
          ? '彻底删除该用户持有的这份邮件？'
          : confirmState.target?.status === 'archived'
            ? '将该邮件从用户归档恢复？'
            : '将该邮件移入对应用户的归档？'
      "
      :detail="
        confirmState.action === 'purge'
          ? '立即归还用户配额；若这是最后一个存活属主，加密部件引用将被释放且不可恢复。'
          : '用户侧会看到该邮件进入或离开归档，留存期与用户自助归档一致。'
      "
      :danger="confirmState.action === 'purge'"
      :confirm-text="confirmState.action === 'purge' ? '确认销毁' : '确认'"
      :loading="acting"
      @confirm="runAction"
      @cancel="confirmState.open = false"
    />
  </AdminPage>
</template>

<style scoped>
.input {
  padding: 6px 10px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface);
  color: var(--c-text);
  font-size: var(--fs-sm);
}

.tf-q {
  min-width: 220px;
}

.tf-uid {
  width: 170px;
}

.modal-foot {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.modal-foot__sp {
  flex: 1;
}

.read-state {
  margin-left: var(--sp-2);
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
}

.acts {
  display: flex;
  gap: var(--sp-1);
  white-space: nowrap;
}

/* 全局 .mono 只管字体与字号；这里额外让长标识在单元格里断行，
   免得一个长邮件 ID 把整张表撑出横向滚动。 */
.mono {
  word-break: break-all;
}


.subj {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}

.subj--unread {
  font-weight: 600;
}

.unread-dot {
  flex: none;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--c-accent, #2563eb);
}

.att {
  flex: none;
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
}

.hint {
  margin: 0;
  color: var(--c-text-muted);
  font-size: var(--fs-xs);
}

.meta {
  display: grid;
  grid-template-columns: 110px 1fr;
  gap: var(--sp-2) var(--sp-3);
  margin: 0;
  font-size: var(--fs-sm);
}

.meta dt {
  color: var(--c-text-faint);
}

.meta dd {
  margin: 0;
  word-break: break-word;
}

.rcpts {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.rcpt {
  display: inline-flex;
  gap: 6px;
  align-items: baseline;
}

.rcpt-kind {
  font-style: normal;
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
  width: 28px;
}

.parts {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-xs);
  margin-bottom: var(--sp-2);
}

.parts th,
.parts td {
  padding: 6px 8px;
  text-align: left;
  border: 1px solid var(--c-border-line);
  vertical-align: top;
}

.parts th {
  color: var(--c-text-faint);
  white-space: nowrap;
}

.parts .num {
  text-align: right;
  white-space: nowrap;
}
</style>
