<script setup lang="ts">
import { computed, ref } from "vue";
import { useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppModal from "@/components/ui/AppModal.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { PERM_LABELS } from "@/api/types";
import { describeError, logError, toastApiError } from "@/lib/async";
import { PASSWORD_PLACEHOLDER, validatePassword } from "@/lib/credentials";
import { formatBytes, formatPercent } from "@/lib/format";
import { useSession } from "@/stores/session";
import { topbarSlot } from "@/stores/shell";
import { useToasts } from "@/stores/toast";

// 个人中心。
//
// 版面按"先看身份、再看用量、最后才是可改的东西"排：左边是与自己有关的事实
// （账号、用量），右边是能做的两件事（权限清单、改密码）。页面里不写解释性
// 文案——权限为空的后果、改密码的影响，都由操作本身与提示条承担。

const router = useRouter();
const session = useSession();
const toasts = useToasts();

const user = computed(() => session.state.user);
const group = computed(() => session.state.group);

const initial = computed(() =>
  (user.value?.displayName || user.value?.account || "?").slice(0, 1).toUpperCase(),
);

const storageLimit = computed(() => session.state.storageLimit);
const storageUsed = computed(() => session.state.storageUsed);

const storageRatio = computed(() => {
  if (storageLimit.value <= 0) {
    return 0;
  }
  return Math.min(1, Math.max(0, storageUsed.value / storageLimit.value));
});

const storageTone = computed(() => {
  if (storageRatio.value >= 0.9) {
    return "meter__fill--danger";
  }
  if (storageRatio.value >= 0.75) {
    return "meter__fill--warn";
  }
  return "";
});

const permGroups = computed(() => {
  const grouped = new Map<string, string[]>();
  for (const item of PERM_LABELS) {
    if (!session.can(item.bit)) {
      continue;
    }
    const labels = grouped.get(item.group) ?? [];
    labels.push(item.label);
    grouped.set(item.group, labels);
  }
  return Array.from(grouped, ([name, labels]) => ({ name, labels }));
});

const refreshing = ref(false);

async function refresh(): Promise<void> {
  refreshing.value = true;
  try {
    await session.refreshProfile();
    toasts.success("资料已刷新");
  } catch (err) {
    logError("profile-refresh", err);
    toastApiError(toasts, err);
  } finally {
    refreshing.value = false;
  }
}

// ---------------------------------------------------------------- 修改密码

const oldPassword = ref("");
const newPassword = ref("");
const confirmPassword = ref("");
const passwordOpen = ref(false);
const changing = ref(false);
const passwordError = ref("");

function closePassword(): void {
  passwordOpen.value = false;
  passwordError.value = "";
  oldPassword.value = "";
  newPassword.value = "";
  confirmPassword.value = "";
}

async function submitPassword(): Promise<void> {
  passwordError.value = "";
  if (oldPassword.value === "") {
    passwordError.value = "请输入当前密码";
    return;
  }
  const newPasswordError = validatePassword(newPassword.value);
  if (newPasswordError) {
    passwordError.value = newPasswordError;
    return;
  }
  if (newPassword.value !== confirmPassword.value) {
    passwordError.value = "两次输入的新密码不一致";
    return;
  }
  changing.value = true;
  try {
    await session.changePassword(oldPassword.value, newPassword.value);
    closePassword();
    toasts.success("密码已修改", "其它设备上的登录已失效");
  } catch (err) {
    logError("password-change", err);
    passwordError.value = describeError(err);
  } finally {
    changing.value = false;
  }
}

// ---------------------------------------------------------------- 退出登录

const logoutOpen = ref(false);
const loggingOut = ref(false);

async function doLogout(): Promise<void> {
  loggingOut.value = true;
  try {
    await session.logout();
    toasts.success("已退出登录");
    await router.push("/login");
  } catch (err) {
    // 必须兜住：异常逃成未处理的 rejection 时确认框停在原地、按钮又变回可点，
    // 用户以为已经退出，实际令牌还在。和本页其它动作（刷新 / 改密码）一致，
    // 错误交给提示条说。
    logError("logout", err);
    toastApiError(toasts, err);
  } finally {
    loggingOut.value = false;
  }
}
</script>

<template>
  <div class="me">
    <!-- 操作项投递到顶栏（页面名在顶栏左侧）。 -->
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="refreshing" @click="refresh()">刷新</AppButton>
    </Teleport>

    <template v-if="user">
      <div class="me__col">
        <!-- 账户：身份 + 与自己有关的事实 + 退出 -->
        <div class="card">
          <div class="card__body me__identity">
            <span class="me__avatar">{{ initial }}</span>
            <div class="me__id">
              <p class="me__name truncate">{{ user.displayName || user.account }}</p>
              <!-- 显示名默认就是账号（注册与初始化时都是这样写的），此时再打一行
                   账号只会看起来像重复，所以只在两者不同的时候才补一行。 -->
              <p v-if="user.displayName && user.displayName !== user.account" class="me__account mono truncate">
                {{ user.account }}
              </p>
            </div>
            <span class="badge badge--accent">{{ group?.displayName ?? user.groupName }}</span>
          </div>

          <div class="card__body me__block me__block--actions">
            <AppButton icon="lock" @click="passwordOpen = true">修改密码</AppButton>
            <AppButton variant="danger" icon="logout" @click="logoutOpen = true">退出登录</AppButton>
          </div>
        </div>

        <!-- 存储用量 -->
        <div class="card">
          <div class="card__head">
            <span class="me__title">存储用量</span>
          </div>
          <div class="card__body">
            <div v-if="storageLimit > 0" class="me__stack">
              <div class="row row--between">
                <span class="muted">
                  {{ formatBytes(storageUsed) }} / {{ formatBytes(storageLimit) }}
                </span>
                <span class="me__percent">{{ formatPercent(storageUsed, storageLimit) }}</span>
              </div>
              <div class="meter">
                <div
                  class="meter__fill"
                  :class="storageTone"
                  :style="{ width: `${storageRatio * 100}%` }"
                />
              </div>
              <p v-if="storageRatio >= 0.9" class="notice notice--warn">
                空间即将用满，超限后新的上传会被拒绝。
              </p>
            </div>
            <div v-else class="row row--between">
              <span class="muted">已用 {{ formatBytes(storageUsed) }}</span>
              <span class="badge badge--success">不限</span>
            </div>
          </div>
        </div>
      </div>

      <div class="me__col">
        <!-- 权限清单 -->
        <div class="card">
          <div class="card__head">
            <span class="me__title">权限</span>
          </div>
          <div class="card__body">
            <AppEmpty v-if="permGroups.length === 0" icon="shield" title="没有任何权限" />
            <div v-else class="perm-list">
              <div v-for="item in permGroups" :key="item.name" class="perm-group">
                <span class="perm-group__name">{{ item.name }}</span>
                <div class="perm-group__badges">
                  <span v-for="label in item.labels" :key="label" class="badge">
                    {{ label }}
                  </span>
                </div>
              </div>
            </div>
          </div>
        </div>

      </div>
    </template>

    <!-- 改密码放进弹窗：它是一次性的操作，不该常驻占掉半张页面。 -->
    <AppModal :open="passwordOpen" title="修改密码" @close="closePassword">
      <form id="password-form" class="stack" @submit.prevent="submitPassword()">
        <p v-if="passwordError" class="notice notice--danger">{{ passwordError }}</p>

        <FormField label="旧密码" required>
          <input
            v-model="oldPassword"
            class="input"
            type="password"
            autocomplete="current-password"
            placeholder="请输入当前密码"
          />
        </FormField>
        <FormField label="新密码" required>
          <input
            v-model="newPassword"
            class="input"
            type="password"
            autocomplete="new-password"
            :placeholder="PASSWORD_PLACEHOLDER"
          />
        </FormField>
        <FormField label="确认新密码" required>
          <input
            v-model="confirmPassword"
            class="input"
            type="password"
            autocomplete="new-password"
            :placeholder="PASSWORD_PLACEHOLDER"
          />
        </FormField>
      </form>

      <template #footer>
        <AppButton @click="closePassword">取消</AppButton>
        <AppButton type="submit" form="password-form" variant="primary" :loading="changing">
          修改密码
        </AppButton>
      </template>
    </AppModal>

    <ConfirmDialog
      :open="logoutOpen"
      danger
      title="退出登录"
      confirm-text="退出"
      :loading="loggingOut"
      message="确定要退出当前账号吗？"
      @cancel="logoutOpen = false"
      @confirm="doLogout()"
    />
  </div>
</template>

<style scoped>
.me {
  display: grid;
  gap: var(--sp-4);
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  align-items: start;
}

.me__col {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
  min-width: 0;
}

/* 卡片小标题：不带图标、不抢眼，只负责分段。 */
.me__title {
  font-size: var(--fs-md);
  font-weight: 620;
}

.me__identity {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  min-width: 0;
}

.me__avatar {
  width: 44px;
  height: 44px;
  flex: none;
  border-radius: 50%;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: var(--c-accent-weak);
  color: var(--c-accent);
  font-size: var(--fs-lg);
  font-weight: 650;
}

.me__id {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.me__name {
  margin: 0;
  font-size: var(--fs-lg);
  font-weight: 650;
  letter-spacing: -0.01em;
  line-height: 1.3;
}

.me__account {
  margin: 0;
  font-size: var(--fs-sm);
  color: var(--c-text-faint);
}

/* 与身份块之间用细线分段，而不是再套一层卡片。 */
.me__block {
  border-top: 1px solid var(--c-border);
  padding-top: var(--sp-3);
  padding-bottom: var(--sp-3);
}

/* 两个操作等宽平分一行：比挤在右边更稳，也让危险操作离常规操作有明确分界。 */
.me__block--actions {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sp-2);
}

.me__percent {
  font-size: var(--fs-sm);
  font-weight: 600;
}

.me__stack {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

/* 权限清单：组间细线分段，名称列固定宽保证三组徽章左缘对齐。
   列表自身用负边距抵消首尾组的内边距，卡片的上下留白不会被撑大。 */
.perm-list {
  display: flex;
  flex-direction: column;
  margin: calc(-1 * var(--sp-3)) 0;
}

.perm-group {
  display: grid;
  grid-template-columns: 72px minmax(0, 1fr);
  gap: var(--sp-3);
  align-items: start;
  padding: var(--sp-3) 0;
}

.perm-group + .perm-group {
  border-top: 1px solid var(--c-border);
}

.perm-group__name {
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  font-weight: 600;
  padding-top: 2px;
}

.perm-group__badges {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-2);
}

@media (max-width: 900px) {
  .me {
    grid-template-columns: minmax(0, 1fr);
  }
}

@media (max-width: 640px) {
  .perm-group {
    grid-template-columns: minmax(0, 1fr);
    gap: var(--sp-2);
  }
}
</style>
