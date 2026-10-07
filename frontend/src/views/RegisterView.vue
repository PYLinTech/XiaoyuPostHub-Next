<script setup lang="ts">
import { computed, reactive, ref } from "vue";
import { useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import FormField from "@/components/ui/FormField.vue";
import logoUrl from "@/assets/logo.svg";
import { describeError, logError } from "@/lib/async";
import {
  ACCOUNT_PLACEHOLDER,
  PASSWORD_PLACEHOLDER,
  validateAccount,
  validatePassword,
} from "@/lib/credentials";
import { useSession } from "@/stores/session";
import { useToasts } from "@/stores/toast";

// 注册页。
//
// 邀请码是否必填由站点的注册模式决定（来自公开的初始化状态接口），因此这里不
// 硬编码"必填"——否则站点改成开放注册后，界面会继续拦着用户要邀请码。
//
// 注册成功后**不自动登录**：后端只创建账号并返回公开字段，不签发会话。
// 与其在前端拼一个假的"已登录"状态，不如明确引导用户去登录页。

const router = useRouter();
const session = useSession();
const toasts = useToasts();

const form = reactive({ account: "", password: "", confirm: "", inviteCode: "" });
const submitting = ref(false);
const error = ref("");
const showPassword = ref(false);

// setup 还没取到时 mode 是未知的：既不按"邀请码必填"处理（免得逼用户填一个
// 开放注册站点用不上的码），也不把提交按钮放开——等状态到位再按真实模式显示，
// 与登录页对"入口是否可用"的判定保持一致。
const mode = computed(() => session.state.setup?.registerMode);
const modeKnown = computed(() => mode.value !== undefined);
const needInvite = computed(() => mode.value === "invite");
const closed = computed(() => mode.value === "closed");
/** 只有确认开放注册（mode 已知且非邀请码模式）才允许把邀请码输入框置灰。 */
const inviteDisabled = computed(() => modeKnown.value && !needInvite.value);

async function submit(): Promise<void> {
  // 重入保护：submitting 只让按钮变灰，输入框里按回车仍能再触发一次 submit，
  // 会重复创建账号请求。
  if (submitting.value) return;
  error.value = "";
  const accountError = validateAccount(form.account);
  if (accountError) {
    error.value = accountError;
    return;
  }
  const passwordError = validatePassword(form.password);
  if (passwordError) {
    error.value = passwordError;
    return;
  }
  if (form.password !== form.confirm) {
    error.value = "两次输入的密码不一致";
    return;
  }
  if (needInvite.value && !form.inviteCode.trim()) {
    error.value = "本站需要邀请码才能注册";
    return;
  }

  submitting.value = true;
  try {
    await session.register(form.account.trim(), form.password, form.inviteCode.trim().toUpperCase());
    toasts.success("账号已创建，请登录");
    await router.replace("/login");
  } catch (err) {
    logError("register", err);
    error.value = describeError(err);
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <div class="auth">
    <div class="auth__panel card">
      <header class="auth__head">
        <img class="auth__mark" :src="logoUrl" alt="" width="36" height="36" />
        <h1 class="page-head__title">注册账号</h1>
        <AppButton variant="ghost" size="sm" @click="router.push('/login')">返回登录</AppButton>
      </header>

      <div class="card__body stack">
        <p v-if="closed" class="notice notice--warn">
          本站已关闭注册。如需账号，请联系管理员通过邀请码开通。
        </p>
        <p v-else-if="!modeKnown" class="notice notice--info">
          正在获取站点的注册设置，请稍候再提交。
        </p>

        <form class="stack" @submit.prevent="submit">
          <FormField label="账号" required>
            <input
              v-model="form.account"
              class="input"
              autocomplete="username"
              autofocus
              :placeholder="ACCOUNT_PLACEHOLDER"
            />
          </FormField>

          <FormField label="密码" required>
            <span class="auth__reveal-wrap">
              <input
                v-model="form.password"
                class="input"
                :type="showPassword ? 'text' : 'password'"
                autocomplete="new-password"
                :placeholder="PASSWORD_PLACEHOLDER"
              />
              <button
                type="button"
                class="auth__reveal"
                :aria-label="showPassword ? '隐藏密码' : '显示密码'"
                :aria-pressed="showPassword"
                :title="showPassword ? '隐藏密码' : '显示密码'"
                @click="showPassword = !showPassword"
              >
                <AppIcon :name="showPassword ? 'ri-eye-off-line' : 'eye'" :size="17" />
              </button>
            </span>
          </FormField>

          <FormField label="确认密码" required>
            <input
              v-model="form.confirm"
              class="input"
              type="password"
              autocomplete="new-password"
              :placeholder="PASSWORD_PLACEHOLDER"
            />
          </FormField>

          <FormField label="邀请码" :required="needInvite">
            <input
              v-model="form.inviteCode"
              class="input mono"
              :disabled="inviteDisabled"
              :placeholder="modeKnown ? (needInvite ? '' : '无需填写') : ''"
            />
          </FormField>

          <p v-if="error" class="notice notice--danger">{{ error }}</p>

          <AppButton
            type="submit"
            variant="primary"
            block
            :loading="submitting"
            :disabled="closed || !modeKnown"
          >
            注册
          </AppButton>
        </form>
      </div>
    </div>
  </div>
</template>

<style scoped>
.auth {
  min-height: 100dvh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--sp-6) var(--sp-4);
}

.auth__panel {
  width: min(440px, 100%);
  /* 入场：卡片浮入 → 标识与返回入口跟上 → 字段依次落位 */
  animation: enter-page 0.44s cubic-bezier(0.2, 0.9, 0.25, 1) both;
}

.auth__panel form > * {
  animation: enter-item 0.42s cubic-bezier(0.2, 0.9, 0.25, 1) both;
}

.auth__panel form > *:nth-child(1) {
  animation-delay: 0.18s;
}

.auth__panel form > *:nth-child(2) {
  animation-delay: 0.24s;
}

.auth__panel form > *:nth-child(3) {
  animation-delay: 0.3s;
}

.auth__panel form > *:nth-child(4) {
  animation-delay: 0.36s;
}

.auth__panel form > *:nth-child(5) {
  animation-delay: 0.42s;
}

@media (prefers-reduced-motion: reduce) {
  .auth__panel,
  .auth__mark,
  .auth__panel form > * {
    animation: none;
  }
}

.auth__head {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-5) var(--sp-5) 0;
}

/* 标题吃掉中间空间，把「返回登录」推到右上角。 */
.auth__head .page-head__title {
  flex: 1;
  min-width: 0;
}

.auth__mark {
  width: 36px;
  height: 36px;
  flex: none;
  display: block;
  animation: enter-mark 0.5s cubic-bezier(0.34, 1.32, 0.64, 1) both;
  animation-delay: 0.1s;
}

/* 密码框内的显示/隐藏按钮：输入框块级化，否则行盒的基线间隙会把按钮推到底框上。 */
.auth__reveal-wrap {
  position: relative;
  display: block;
}

.auth__reveal-wrap .input {
  display: block;
  padding-right: 44px;
}

.auth__reveal {
  position: absolute;
  top: 50%;
  right: 6px;
  transform: translateY(-50%);
  width: 30px;
  height: 30px;
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--c-text-muted);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
}

.auth__reveal:hover {
  background: var(--c-hover);
  color: var(--c-text);
}
</style>
