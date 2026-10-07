<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { createRequestGate, toastApiError } from "@/lib/async";
import { useRoute, useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppTabs, { type TabItem } from "@/components/ui/AppTabs.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import MailAddressesDialog from "@/views/mail/MailAddressesDialog.vue";
import { mailApi } from "@/api/endpoints";
import type { MailListItem } from "@/api/types";
import { useToasts } from "@/stores/toast";
import { MAIL_VIEWS, useMailShell, type MailViewKey } from "@/stores/mail";
import { pad } from "@/lib/format";

// 邮件页（收件 / 星标 / 归档 三视图共用一个整页列表）。
//
// 邮件是普通页面而不是二级壳：视图切换由页内选项卡完成，侧栏只有一个「邮件」入口。
// 视图完全由路由参数 :view 派生：选项卡高亮、标题、本页数据共用同一事实来源，
// 切换时路由重挂载，搜索/分页随之自然重置。
// 单击邮件跳到整页详情（/mail/:view/:id）。
// 邮件的"未读即读"由后端在详情接口里自动完成，详情返回后重挂载本页即刷新。

const route = useRoute();
const router = useRouter();
const toasts = useToasts();
const mail = useMailShell();

const currentView = computed<MailViewKey>(() => route.params.view as MailViewKey);
const isTrash = computed(() => currentView.value === "trash");

/** 三个视图即三个选项卡；未读数挂在「收件」上，与原先侧栏徽标同一来源。 */
const tabs = computed<TabItem[]>(() =>
  MAIL_VIEWS.map((v) => ({
    key: v.key,
    label: v.label,
    badge: v.key === "inbox" && mail.state.unreadInbox > 0 ? mail.state.unreadInbox : undefined,
  })),
);

function switchView(key: string): void {
  if (key !== currentView.value) {
    void router.push(`/mail/${key}`);
  }
}

const onlyUnread = ref(false);
const searchInput = ref("");
const appliedQuery = ref("");
const limit = ref(20);
const offset = ref(0);

const loading = ref(false);
const items = ref<MailListItem[]>([]);
const total = ref(0);

const gate = createRequestGate();

async function reloadList(): Promise<void> {
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  try {
    const res = await mailApi.list({
      view: currentView.value,
      unread: onlyUnread.value ? 1 : undefined,
      q: appliedQuery.value || undefined,
      limit: limit.value,
      offset: offset.value,
    });
    if (!gate.isCurrent(token)) return;
    items.value = res.items;
    total.value = res.total;
    // 未读计数回写给「收件」选项卡的徽标。
    mail.setCounters(res.counters);
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    toastApiError(toasts, err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

onMounted(() => {
  void reloadList();
});

function applySearch(): void {
  appliedQuery.value = searchInput.value.trim();
  offset.value = 0;
  void reloadList();
}

function clearSearch(): void {
  searchInput.value = "";
  if (appliedQuery.value) {
    appliedQuery.value = "";
    offset.value = 0;
    void reloadList();
  }
}

function toggleUnread(): void {
  offset.value = 0;
  void reloadList();
}

// 列表第一行的"对方"：只收信，一律显示发件人。
function rowParty(item: MailListItem): string {
  return item.fromName || item.fromAddress;
}

// 列表时间：今天只给时分、今年给月日、更早给完整日期，密度向主流邮箱看齐。
function listTime(unix: number): string {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  const now = new Date();
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate();
  if (sameDay) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}/${d.getDate()}`;
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
}

function openMail(item: MailListItem): void {
  void router.push(`/mail/${currentView.value}/${item.messageId}`);
}

// 行内操作的 aria-label 必须自带"对哪一封"：光报"归档"的话，读屏用户无从判断
// 操作落在哪一行。主题为空时沿用行内的占位说法。
function rowTitle(item: MailListItem): string {
  return item.subject || "（无主题）";
}

type RowOp = "star" | "archive" | "restore" | "purge";

/**
 * 行内操作的忙碌锁。
 *
 * 四个操作都是"先读当前状态、再发一次相反语义的请求"：连点两下星标会用同一个
 * !item.isStarred 连发一加一撤，最后一次生效，用户只会觉得"没点上"；归档的第二
 * 次则必然失败，弹一个用户根本看不懂的错误 toast。挡住重复点击比事后解释便宜。
 *
 * 记到 id → 操作这一级，而不是简单地记一个 id：忙的是刚点的那一个按钮，
 * 同一行的其它按钮只做禁用，不必跟着一起转圈。
 */
const pending = ref(new Map<number, RowOp>());

/** 该行是否有操作正在执行（用于禁用同行其它按钮）。 */
function isPending(item: MailListItem): boolean {
  return pending.value.has(item.id);
}

/** 某个具体操作是否正转圈。 */
function rowBusy(item: MailListItem, op: RowOp): boolean {
  return pending.value.get(item.id) === op;
}

async function runRowOp(item: MailListItem, op: RowOp, run: () => Promise<void>): Promise<void> {
  if (pending.value.has(item.id)) {
    return;
  }
  // 整体换新 Map 而不是原地 set：Map 的原地改动不会触发 Vue 重新渲染。
  const started = new Map(pending.value);
  started.set(item.id, op);
  pending.value = started;
  try {
    await run();
  } finally {
    const rest = new Map(pending.value);
    rest.delete(item.id);
    pending.value = rest;
  }
}

/**
 * 行的键盘激活。
 *
 * role="button" 的激活键是 Enter 与 Space 两个，两个都得接：只接 Enter 的话，
 * 空格会变成滚页面而不是打开邮件。行内那四个操作按钮也在焦点序列里，它们按下的
 * 键会冒泡到行上——只认"按键就发生在行自己身上"的那一次，否则点归档会顺带把
 * 邮件打开。
 */
function onRowKeydown(event: KeyboardEvent, item: MailListItem): void {
  if (event.target !== event.currentTarget) {
    return;
  }
  if (event.key === "Enter" || event.key === " ") {
    event.preventDefault();
    openMail(item);
  }
}

async function toggleRowStar(item: MailListItem, event: Event): Promise<void> {
  event.stopPropagation();
  await runRowOp(item, "star", async () => {
    try {
      await mailApi.star(item.messageId, !item.isStarred);
      void reloadList();
    } catch (err) {
      toastApiError(toasts, err);
    }
  });
}

async function archiveRow(item: MailListItem, event: Event): Promise<void> {
  event.stopPropagation();
  await runRowOp(item, "archive", async () => {
    try {
      await mailApi.archive(item.messageId);
      toasts.success("已移入归档");
      void reloadList();
    } catch (err) {
      toastApiError(toasts, err);
    }
  });
}

async function restoreRow(item: MailListItem, event: Event): Promise<void> {
  event.stopPropagation();
  await runRowOp(item, "restore", async () => {
    try {
      await mailApi.restore(item.messageId);
      toasts.success("已恢复");
      void reloadList();
    } catch (err) {
      toastApiError(toasts, err);
    }
  });
}

/** 彻底删除不可恢复，走全站统一的确认窗而不是内联 window.confirm。 */
const purgeTarget = ref<MailListItem | null>(null);
const purgeBusy = ref(false);

function askPurge(item: MailListItem, event: Event): void {
  event.stopPropagation();
  if (isPending(item)) {
    return;
  }
  purgeTarget.value = item;
}

async function confirmPurge(): Promise<void> {
  const item = purgeTarget.value;
  if (!item) {
    return;
  }
  await runRowOp(item, "purge", async () => {
    purgeBusy.value = true;
    try {
      await mailApi.purge(item.messageId);
      toasts.success("已彻底删除");
      // 先关窗再重拉：重拉是异步的，让用户立刻看见自己做的操作已经结束。
      purgeTarget.value = null;
      void reloadList();
    } catch (err) {
      toastApiError(toasts, err);
    } finally {
      purgeBusy.value = false;
    }
  });
}

</script>

<template>
  <section class="mvp">
    <AppTabs :tabs="tabs" :model-value="currentView" @update:model-value="switchView" />

    <div class="mv-toolbar">
      <h1 class="mv-toolbar__title">邮件</h1>
      <div class="mv-toolbar__actions">
        <AppButton size="sm" icon="ri-at-line" @click="mail.openAddresses()">我的邮箱地址</AppButton>
      </div>
      <div class="mv-toolbar__search">
        <i class="ri-search-line mv-toolbar__search-icon" />
        <input
          v-model="searchInput"
          type="search"
          placeholder="搜索主题、发件人、收件人"
          @keydown.enter="applySearch"
        />
        <button
          v-if="searchInput"
          type="button"
          class="mv-toolbar__clear"
          title="清除搜索"
          @click="clearSearch"
        >
          <i class="ri-close-circle-fill" />
        </button>
      </div>
      <label
        v-if="currentView === 'inbox'"
        class="mv-toolbar__unread"
        :class="{ 'mv-toolbar__unread--on': onlyUnread }"
      >
        <input v-model="onlyUnread" type="checkbox" @change="toggleUnread" />
        仅未读
      </label>
    </div>

    <div class="mv-rows">
      <AppEmpty
        v-if="!loading && items.length === 0"
        class="mv-rows__empty"
        :icon="isTrash ? 'ri-delete-bin-line' : 'ri-mail-line'"
        :title="appliedQuery || onlyUnread ? '没有匹配的邮件' : '没有邮件'"
        :hint="appliedQuery || onlyUnread ? '试试放宽筛选条件' : undefined"
      />
      <p v-else-if="loading" class="mv-hint">加载中…</p>
      <!-- 行是 role="button" 而不是 <button>：行内还站着四个真正可聚焦的操作按钮，
           button 套 button 是无效 HTML，浏览器会把结构拆坏，键盘与读屏随之失效。 -->
      <div
        v-for="item in items"
        v-else
        :key="item.id"
        class="mv-row"
        :class="{ 'mv-row--unread': !item.isRead }"
        role="button"
        tabindex="0"
        @click="openMail(item)"
        @keydown="onRowKeydown($event, item)"
      >
        <button
          type="button"
          class="mv-row__star"
          :disabled="isPending(item)"
          :title="item.isStarred ? '取消星标' : '加星标'"
          :aria-label="`${item.isStarred ? '取消星标' : '加星标'} ${rowTitle(item)}`"
          @click="toggleRowStar(item, $event)"
        >
          <span v-if="rowBusy(item, 'star')" class="spinner" />
          <i v-else :class="item.isStarred ? 'ri-star-fill' : 'ri-star-line'" />
        </button>
        <span class="mv-row__main">
          <span class="mv-row__line1">
            <span class="mv-row__from truncate">{{ rowParty(item) }}</span>
            <span class="mv-row__time">{{ listTime(item.sentAt) }}</span>
          </span>
          <span class="mv-row__line2">
            <i v-if="item.attachmentCount > 0" class="ri-attachment-2 mv-row__att" />
            <span class="mv-row__subject truncate">{{ item.subject || "（无主题）" }}</span>
            <span v-if="item.snippet" class="mv-row__snippet truncate">{{ item.snippet }}</span>
          </span>
        </span>
        <span class="mv-row__ops">
          <template v-if="isTrash">
            <button
              type="button"
              class="mv-row__op"
              :disabled="isPending(item)"
              title="恢复"
              :aria-label="`恢复 ${rowTitle(item)}`"
              @click="restoreRow(item, $event)"
            >
              <span v-if="rowBusy(item, 'restore')" class="spinner" />
              <i v-else class="ri-arrow-go-back-line" />
            </button>
            <button
              type="button"
              class="mv-row__op mv-row__op--danger"
              :disabled="isPending(item)"
              title="彻底删除"
              :aria-label="`彻底删除 ${rowTitle(item)}`"
              @click="askPurge(item, $event)"
            >
              <span v-if="rowBusy(item, 'purge')" class="spinner" />
              <i v-else class="ri-delete-bin-6-line" />
            </button>
          </template>
          <button
            v-else
            type="button"
            class="mv-row__op mv-row__op--danger"
            :disabled="isPending(item)"
            title="移入归档"
            :aria-label="`归档 ${rowTitle(item)}`"
            @click="archiveRow(item, $event)"
          >
            <span v-if="rowBusy(item, 'archive')" class="spinner" />
            <i v-else class="ri-delete-bin-line" />
          </button>
        </span>
      </div>
    </div>

    <AppPagination
      v-if="total > limit"
      class="mv-pager"
      :total="total"
      :limit="limit"
      :offset="offset"
      @update:offset="(v) => { offset = v; void reloadList(); }"
      @update:limit="(v) => { limit = v; offset = 0; void reloadList(); }"
    />

    <MailAddressesDialog />

    <ConfirmDialog
      :open="purgeTarget !== null"
      title="彻底删除邮件"
      message="彻底删除该邮件？"
      detail="邮件配额立即释放且不可恢复。"
      danger
      confirm-text="彻底删除"
      :loading="purgeBusy"
      @confirm="confirmPurge"
      @cancel="purgeTarget = null"
    />
  </section>
</template>

<style scoped>
.mvp {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  min-height: calc(100dvh - 10rem);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  background: var(--c-surface);
  padding: var(--sp-4);
}

.mv-toolbar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex: none;
}

.mv-toolbar__title {
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 650;
  flex: none;
}

.mv-toolbar__actions {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  flex: none;
}

.mv-toolbar__search {
  position: relative;
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
}

.mv-toolbar__search input {
  width: 100%;
  height: 32px;
  padding: 0 28px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-pill);
  background: var(--c-surface-sunken);
  color: var(--c-text);
  font-size: var(--fs-xs);
  transition: border-color 0.12s ease, background-color 0.12s ease;
}

.mv-toolbar__search input:focus {
  outline: none;
  border-color: var(--c-accent);
  background: var(--c-surface);
}

.mv-toolbar__search-icon {
  position: absolute;
  left: 10px;
  font-size: 14px;
  color: var(--c-text-faint);
  pointer-events: none;
}

.mv-toolbar__clear {
  position: absolute;
  right: 6px;
  display: inline-flex;
  border: 0;
  padding: 2px;
  background: transparent;
  color: var(--c-text-faint);
  font-size: 15px;
  cursor: pointer;
}

.mv-toolbar__clear:hover {
  color: var(--c-text-muted);
}

.mv-toolbar__unread {
  flex: none;
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 5px 10px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-pill);
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
  cursor: pointer;
  user-select: none;
  transition: color 0.12s ease, border-color 0.12s ease, background-color 0.12s ease;
}

.mv-toolbar__unread input {
  accent-color: var(--c-accent);
  margin: 0;
}

.mv-toolbar__unread--on {
  color: var(--c-accent);
  border-color: var(--c-accent);
  background: var(--c-accent-weak);
}

.mv-rows {
  display: flex;
  flex-direction: column;
  gap: 2px;
  flex: 1;
}

.mv-rows__empty {
  margin: auto;
}

.mv-hint {
  margin: var(--sp-4);
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  text-align: center;
}

.mv-row {
  position: relative;
  display: flex;
  align-items: flex-start;
  gap: var(--sp-2);
  width: 100%;
  padding: 10px 12px 10px 14px;
  border-radius: var(--r-md);
  color: var(--c-text-muted);
  font-size: var(--fs-xs);
  text-align: left;
  cursor: pointer;
  /* 从 button 改成 div 后丢了 button 隐含的禁选：双击主题会选中文字，
     连点两下也就不再是"打开两次"。 */
  user-select: none;
  transition: background-color 0.1s ease;
}

.mv-row::before {
  content: "";
  position: absolute;
  left: 4px;
  top: 12px;
  bottom: 12px;
  width: 3px;
  border-radius: var(--r-pill);
  background: transparent;
}

.mv-row:hover {
  background: var(--c-hover);
}

.mv-row--unread::before {
  background: var(--c-accent);
}

/* 行内图标按钮：换成真正的 button 后必须显式清掉浏览器默认的边框与底色，
   否则每个图标外面会多出一圈灰底。 */
.mv-row__star,
.mv-row__op {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: 0;
  padding: 0;
  background: transparent;
  cursor: pointer;
}

/* 忙碌锁期间的禁用态：与 .btn:disabled 同一套（半透明 + not-allowed）。 */
.mv-row__star:disabled,
.mv-row__op:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.mv-row__star {
  flex: none;
  margin-top: 2px;
  font-size: 15px;
  color: var(--c-text-faint);
}

.mv-row__star:hover:not(:disabled) {
  color: var(--c-accent);
}

.mv-row--unread .mv-row__star {
  color: var(--c-accent);
}

.mv-row__main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.mv-row__line1 {
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
}

.mv-row__from {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-sm);
  color: var(--c-text-muted);
}

.mv-row__time {
  flex: none;
  color: var(--c-text-faint);
  font-size: 11px;
}

.mv-row__line2 {
  display: flex;
  align-items: baseline;
  gap: 6px;
  min-width: 0;
}

.mv-row__subject {
  flex: none;
  max-width: 55%;
  color: var(--c-text-muted);
}

.mv-row__snippet {
  flex: 1;
  min-width: 0;
  color: var(--c-text-faint);
}

.mv-row__att {
  flex: none;
  font-size: 13px;
  align-self: center;
}

.mv-row--unread .mv-row__from,
.mv-row--unread .mv-row__subject {
  color: var(--c-text);
  font-weight: 650;
}

.mv-row--unread .mv-row__time {
  color: var(--c-text-muted);
}

/* 行尾操作：常驻显示。原先靠悬停显形，但键盘用户没有 hover——操作看不见
   就等于不存在，读屏用户也只能靠遍历发现它。 */
.mv-row__ops {
  flex: none;
  display: flex;
  align-items: center;
  gap: 2px;
  margin-top: 1px;
}

.mv-row__op {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  border-radius: var(--r-sm);
  font-size: 15px;
  color: var(--c-text-muted);
}

/* 悬停反馈统一加 :not(:disabled)：与 .btn:hover:not(:disabled) 同一口径，
   按钮正在执行时不该再亮起来邀人再点一次。 */
.mv-row__op:hover:not(:disabled) {
  background: var(--c-surface);
  color: var(--c-text);
}

.mv-row__op--danger:hover:not(:disabled) {
  color: var(--c-danger);
}

.mv-pager {
  flex: none;
  margin-top: var(--sp-2);
  padding-top: var(--sp-3);
  border-top: 1px solid var(--c-border);
}

/* 中屏：搜索框独占第二行避免被挤碎；导航走全局侧栏抽屉。 */
@media (max-width: 1023px) {
  .mv-toolbar {
    flex-wrap: wrap;
    row-gap: var(--sp-2);
  }

  .mv-toolbar__search {
    order: 3;
    flex-basis: 100%;
  }
}
</style>
