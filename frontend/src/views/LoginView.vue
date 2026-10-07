<script setup lang="ts">
import { computed, reactive, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import FormField from "@/components/ui/FormField.vue";
import logoUrl from "@/assets/logo.svg";
import { ApiError } from "@/api/client";
import { describeError, logError } from "@/lib/async";
import { ACCOUNT_PLACEHOLDER, PASSWORD_PLACEHOLDER, validateAccount } from "@/lib/credentials";
import { useSession } from "@/stores/session";
import { useToasts } from "@/stores/toast";

// 登录页。
//
// 两处刻意的处理：
//   ① 账号不存在与口令错误展示同一句文案。后端已经这样返回了（避免枚举账号），
//      前端不该用"该账号不存在"把这条防线补回来。
//   ② 被退避（429）时把剩余等待时间说出来。不说的话用户会立刻重试，
//      而每次重试都会把退避期推得更长。

const route = useRoute();
const router = useRouter();
const session = useSession();
const toasts = useToasts();

const form = reactive({ account: "", password: "" });
const submitting = ref(false);
const error = ref("");
const waitSeconds = ref(0);

// setup 还没取到时 registerMode 是未知的：这时既不能显示「注册新账号」，
// 也不能反过来断言"本站已关闭注册"——后者会把入口藏掉，用户在注册已开放的
// 站点上也点不进去，等状态到位前只当没有这个入口。
const registerMode = computed(() => session.state.setup?.registerMode);
const registerClosed = computed(() => registerMode.value === "closed");
const canRegister = computed(() => registerMode.value !== undefined && !registerClosed.value);

/** 登录成功后回到原本要去的页面；没有就进文件页。 */
const nextPath = computed(() => {
  const raw = route.query.next;
  if (typeof raw !== "string" || !raw.startsWith("/") || raw.startsWith("//")) {
    return "/files";
  }
  return raw;
});

async function submit(): Promise<void> {
  // 重入保护：submitting 只让按钮变灰，输入框里按回车仍能再触发一次 submit。
  // 登录会白打一次请求，还可能紧跟着撞上 429 退避。
  if (submitting.value) return;
  error.value = "";
  waitSeconds.value = 0;
  const accountError = validateAccount(form.account);
  if (accountError) {
    error.value = accountError;
    return;
  }
  // 登录只校验密码非空：长度规则属于注册侧，旧凭据不该被前端拦下，
  // 是否匹配由服务端判定。
  if (!form.password) {
    error.value = "请输入密码";
    return;
  }
  submitting.value = true;
  try {
    await session.login(form.account.trim(), form.password);
    toasts.success("已登录");
    await router.replace(nextPath.value);
  } catch (err) {
    logError("login", err);
    if (err instanceof ApiError && err.status === 429) {
      waitSeconds.value = err.retryAfter ?? 0;
      error.value = waitSeconds.value
        ? `尝试过于频繁，请在 ${waitSeconds.value} 秒后重试`
        : "尝试过于频繁，请稍后再试";
    } else {
      error.value = describeError(err);
    }
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
        <div>
          <h1 class="page-head__title">{{ session.state.siteName }}</h1>
        </div>
      </header>

      <div class="card__body stack">
        <form class="stack" @submit.prevent="submit">
          <FormField label="账号">
            <input
              v-model="form.account"
              class="input"
              autocomplete="username"
              autofocus
              :placeholder="ACCOUNT_PLACEHOLDER"
            />
          </FormField>

          <FormField label="密码">
            <input
              v-model="form.password"
              class="input"
              type="password"
              autocomplete="current-password"
              :placeholder="PASSWORD_PLACEHOLDER"
            />
          </FormField>

          <p v-if="error" class="notice notice--danger">{{ error }}</p>

          <AppButton type="submit" variant="primary" block :loading="submitting">登录</AppButton>
        </form>

        <div class="row row--between">
          <AppButton v-if="canRegister" variant="ghost" size="sm" @click="router.push('/register')">
            注册新账号
          </AppButton>
          <span v-else-if="registerClosed" class="faint">本站已关闭注册</span>
          <!-- 状态未知时留一个空占位，只为把右边的「用取件码提取」顶在原处。 -->
          <span v-else aria-hidden="true" />
          <AppButton variant="ghost" size="sm" @click="router.push('/pickup')">用取件码提取</AppButton>
        </div>
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
  width: min(420px, 100%);
  /* 入场：卡片浮入 → 标识放大跟上 → 字段与底部按钮依次落位 */
  animation: enter-page 0.44s cubic-bezier(0.2, 0.9, 0.25, 1) both;
}

.auth__head {
  display: flex;
  align-items: flex-start;
  gap: var(--sp-3);
  padding: var(--sp-5) var(--sp-5) 0;
}

.auth__mark {
  width: 36px;
  height: 36px;
  flex: none;
  display: block;
  animation: enter-mark 0.5s cubic-bezier(0.34, 1.32, 0.64, 1) both;
  animation-delay: 0.1s;
}

.auth__panel form > *,
.auth__panel .card__body > .row {
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

.auth__panel .card__body > .row {
  animation-delay: 0.42s;
}

@media (prefers-reduced-motion: reduce) {
  .auth__panel,
  .auth__mark,
  .auth__panel form > *,
  .auth__panel .card__body > .row {
    animation: none;
  }
}
</style>
