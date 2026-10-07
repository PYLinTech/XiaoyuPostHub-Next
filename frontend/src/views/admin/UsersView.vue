<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { User } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { topbarSlot } from "@/stores/shell";
import { describeError, logError, toastApiError, createRequestGate } from "@/lib/async";
import { PASSWORD_PLACEHOLDER, validatePassword } from "@/lib/credentials";
import { formatDate, formatRelative } from "@/lib/format";
import { refreshAfterAdminChange, useSession } from "@/stores/session";
import { useToasts } from "@/stores/toast";
import { useGroupOptions } from "@/composables/useGroupOptions";

// 账号管理。
//
// 列表接口没有总数也没有搜索：/api/admin/users 只回当前页。因此这里的搜索是
// 纯客户端过滤，界面上必须把这个范围说清楚——否则管理员会以为"搜不到就是没这个人"，
// 而实际上只是没翻到那一页。

const session = useSession();
const toasts = useToasts();

const users = ref<User[]>([]);
// 组下拉与显示名交给公共实现：UsersView / InvitesView 原本各抄了一份逐字相同的
// 副本（连注释都相同），却各自带了一套错误处理。这里用 groupsError 是因为改组
// 需要「管理用户组与配额」权限，缺权限时这一项功能真的不可用，必须说出来。
const { groupsError, groupOptions, groupLabel, loadGroups } = useGroupOptions("admin.users");

const loading = ref(false);
const error = ref("");
const keyword = ref("");
const limit = ref(50);
const offset = ref(0);

const filtered = computed(() => {
  const needle = keyword.value.trim().toLowerCase();
  if (!needle) {
    return users.value;
  }
  return users.value.filter(
    (user) =>
      user.account.toLowerCase().includes(needle) ||
      user.displayName.toLowerCase().includes(needle) ||
      String(user.id) === needle,
  );
});

const gate = createRequestGate();

async function load(): Promise<void> {
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await adminApi.listUsers(limit.value, offset.value);
    if (!gate.isCurrent(token)) return;
    users.value = result.items ?? [];
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.users", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

onMounted(() => {
  void load();
  void loadGroups();
});

// ---------------------------------------------------------------- 启用 / 停用

const statusTarget = ref<User | null>(null);
const statusBusy = ref(false);

// 操作入口直接收实体而不是 id：卡片 v-for 里的 user 就是实体，再按 id 反查一次
// 只是把同一个对象绕一圈，反查 miss 还要写一段静默 return。反查函数原本是给
// 「表格行是扁平对象」的写法准备的，而这一页用的是卡片网格，行本来就是实体。

function askDisable(target: User): void {
  statusTarget.value = target;
}

async function confirmDisable(): Promise<void> {
  const target = statusTarget.value;
  if (!target) {
    return;
  }
  statusBusy.value = true;
  try {
    await adminApi.setUserStatus(target.id, false);
    toasts.success(`已停用 ${target.account}`);
    statusTarget.value = null;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.users.disable", err);
  } finally {
    statusBusy.value = false;
  }
}

async function enable(target: User): Promise<void> {
  try {
    await adminApi.setUserStatus(target.id, true);
    toasts.success(`已启用 ${target.account}`);
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.users.enable", err);
  }
}

// ---------------------------------------------------------------- 改组

const groupOpen = ref(false);
const groupTarget = ref<User | null>(null);
const groupChoice = ref("");
const groupBusy = ref(false);
const groupError = ref("");

function openGroup(target: User): void {
  groupTarget.value = target;
  groupChoice.value = target.groupName;
  groupError.value = "";
  groupOpen.value = true;
}

async function saveGroup(): Promise<void> {
  const target = groupTarget.value;
  if (!target) {
    return;
  }
  if (!groupChoice.value) {
    groupError.value = "请选择用户组";
    return;
  }
  groupBusy.value = true;
  groupError.value = "";
  try {
    await adminApi.setUserGroup(target.id, groupChoice.value);
    toasts.success(`已把 ${target.account} 移到 ${groupChoice.value}`);
    groupOpen.value = false;
    await load();
    // 改组没有"不能改自己"的限制，管理员能把自己挪进别的组。自己所在组的
    // 权限位缓存在会话里，不刷新的话侧栏与菜单会一直按旧权限渲染到手动刷新。
    await refreshAfterAdminChange();
  } catch (err) {
    groupError.value = describeError(err);
    logError("admin.users.group", err);
  } finally {
    groupBusy.value = false;
  }
}

// ---------------------------------------------------------------- 重置口令

const passwordOpen = ref(false);
const passwordTarget = ref<User | null>(null);
const passwordValue = ref("");
const passwordBusy = ref(false);
const passwordError = ref("");

function openPassword(target: User): void {
  passwordTarget.value = target;
  passwordValue.value = "";
  passwordError.value = "";
  passwordOpen.value = true;
}

async function savePassword(): Promise<void> {
  const target = passwordTarget.value;
  if (!target) {
    return;
  }
  const passwordValidationError = validatePassword(passwordValue.value);
  if (passwordValidationError) {
    passwordError.value = passwordValidationError.replace(/^密码/, "新口令");
    return;
  }
  passwordBusy.value = true;
  passwordError.value = "";
  try {
    await adminApi.resetUserPassword(target.id, passwordValue.value);
    toasts.success(`已重置 ${target.account} 的口令`, "该账号的其它设备已被强制退出");
    passwordOpen.value = false;
    await load();
  } catch (err) {
    passwordError.value = describeError(err);
    logError("admin.users.password", err);
  } finally {
    passwordBusy.value = false;
  }
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <input
        v-model="keyword"
        class="input topbar-search"
        type="search"
        placeholder="过滤：账号 / 显示名 / 编号"
      />
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load()">刷新</AppButton>
    </Teleport>

    <div v-if="loading" class="card user-slot">
      <div class="user-loading">
        <span class="spinner" /> 正在加载
      </div>
    </div>
    <template v-else>
      <div v-if="filtered.length === 0" class="card user-slot">
        <AppEmpty
          title="没有账号"
          :hint="keyword ? '当前页没有匹配的账号，可以翻页或清空过滤词。' : ''"
        />
      </div>
      <div v-else class="user-grid">
        <article v-for="user in filtered" :key="user.id" class="card user-card">
          <header class="user-card__head">
            <div class="user-card__id">
              <span class="user-card__name">
                <span class="truncate">{{ user.displayName || user.account }}</span>
                <span
                  class="badge"
                  :class="user.status === 1 ? 'badge--success' : 'badge--danger'"
                >
                  {{ user.status === 1 ? "已启用" : "已停用" }}
                </span>
              </span>
              <span class="user-card__meta mono truncate">
                {{ user.account }}#{{ user.id }} · 注册于 {{ formatDate(user.createdAt) }}
              </span>
            </div>
            <!-- 后端会拒绝禁用当前登录账号，前端也不该让管理员点了才知道。 -->
            <div class="actions">
              <AppButton
                v-if="user.status !== 1"
                size="sm"
                icon="play"
                data-tip="启用该账号"
                aria-label="启用该账号"
                @click="enable(user)"
              />
              <AppButton
                v-else-if="user.id === session.state.user?.id"
                size="sm"
                icon="lock"
                disabled
                data-tip="不能停用自己"
                aria-label="不能停用自己"
              />
              <AppButton
                v-else
                size="sm"
                variant="danger"
                icon="lock"
                data-tip="停用该账号"
                aria-label="停用该账号"
                @click="askDisable(user)"
              />
              <AppButton
                size="sm"
                icon="users"
                data-tip="变更用户组"
                aria-label="变更用户组"
                @click="openGroup(user)"
              />
              <AppButton
                size="sm"
                icon="key"
                data-tip="重置口令"
                aria-label="重置口令"
                @click="openPassword(user)"
              />
            </div>
          </header>

          <dl class="user-card__facts">
            <div class="user-card__fact">
              <dt>用户组</dt>
              <dd><span class="badge">{{ groupLabel(user.groupName) }}</span></dd>
            </div>
            <div class="user-card__fact">
              <dt>最近活动</dt>
              <dd class="nowrap">{{ formatRelative(user.lastActionAt) }}</dd>
            </div>
          </dl>
        </article>
      </div>

      <!-- 分页条与"过滤后为空"这一支无关：过滤词把当前页筛空时，唯一的出路
           就是翻到别的页或清空过滤词，把分页条一起藏掉等于把出路也藏了。 -->
      <div v-if="users.length > 0" class="user-foot">
        <AppPagination
          :total="offset + users.length"
          :limit="limit"
          :offset="offset"
          :exact="false"
          :has-more="users.length >= limit"
          @update:limit="(value: number) => { limit = value; offset = 0; load(); }"
          @update:offset="(value: number) => { offset = value; load(); }"
        />
      </div>
    </template>

    <ConfirmDialog
      :open="!!statusTarget"
      danger
      confirm-text="停用账号"
      :loading="statusBusy"
      :message="`停用账号 ${statusTarget?.account ?? ''}？`"
      detail="该账号的会话与下载票据将全部吊销。"
      @confirm="confirmDisable"
      @cancel="statusTarget = null"
    />

    <AppModal :open="groupOpen" title="变更用户组" @close="groupOpen = false">
      <div class="stack">
        <p class="muted">
          账号 {{ groupTarget?.account }} 当前属于「{{ groupLabel(groupTarget?.groupName ?? "") }}」。
        </p>
        <FormField
          label="目标用户组"
          required
          :error="groupError"
          hint="变更后该账号的会话会被吊销。"
        >
          <AppSelect
            v-model="groupChoice"
            aria-label="目标用户组"
            :options="groupOptions"
            :disabled="!!groupsError"
          />
        </FormField>
        <p v-if="groupsError" class="notice notice--warn">
          无法读取用户组列表：{{ groupsError }}。改组需要「管理用户组与配额」权限。
        </p>
      </div>
      <template #footer>
        <AppButton :disabled="groupBusy" @click="groupOpen = false">取消</AppButton>
        <AppButton variant="primary" :loading="groupBusy" @click="saveGroup">保存</AppButton>
      </template>
    </AppModal>

    <AppModal :open="passwordOpen" title="重置口令" @close="passwordOpen = false">
      <div class="stack">
        <FormField
          label="新口令"
          required
          :error="passwordError"
        >
          <input
            v-model="passwordValue"
            class="input"
            type="password"
            autocomplete="new-password"
            :placeholder="PASSWORD_PLACEHOLDER"
          />
        </FormField>
      </div>
      <template #footer>
        <AppButton :disabled="passwordBusy" @click="passwordOpen = false">取消</AppButton>
        <AppButton variant="primary" :loading="passwordBusy" @click="savePassword">重置</AppButton>
      </template>
    </AppModal>
  </AdminPage>
</template>

<style scoped>
.topbar-search {
  width: 180px;
  /* 与同一行的 sm 按钮等高、同字号：输入框默认按触摸目标给到 36/40px、字号走浏览器
     默认的 16px，排在一行里会明显"肿"出来。padding 归零，文字自己居中。 */
  height: var(--control-h-sm);
  min-height: var(--control-h-sm);
  padding-block: 0;
  font-size: var(--fs-xs);
}

/* 加载与空态占的是"账号卡片本来会在的那个格子"：套上和 .user-card 同一个
   .card 容器，页面才不会一半是卡片、一半悬着一段没有落点的文字。 */
.user-slot {
  width: 100%;
}

.user-loading {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--sp-2);
  padding: var(--sp-6);
  color: var(--c-text-muted);
}

/* 全局的 .actions 样式挂在 .table 下，卡片布局里要自己排：间距给到 8px，
   三个纯图标按钮贴在一起会看成一块。 */
.user-card .actions {
  display: flex;
  gap: var(--sp-2);
  flex: none;
}

.user-grid {
  display: grid;
  gap: var(--sp-4);
  grid-template-columns: repeat(auto-fill, minmax(min(320px, 100%), 1fr));
}

.user-card__head {
  padding: var(--sp-4);
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sp-3);
}

.user-card__id {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.user-card__name {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-md);
  font-weight: 680;
  color: var(--c-text);
  min-width: 0;
}

.user-card__meta {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.user-card__facts {
  display: flex;
  margin: 0;
  border-top: 1px solid var(--c-border);
}

.user-card__fact {
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-4);
}

.user-card__fact + .user-card__fact {
  border-left: 1px solid var(--c-border);
}

.user-card__fact dt {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.user-card__fact dd {
  margin: 0;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--c-text);
}

.user-foot {
  margin-top: var(--sp-4);
}
</style>
