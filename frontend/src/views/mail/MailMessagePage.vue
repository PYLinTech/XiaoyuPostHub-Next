<script setup lang="ts">
import { onMounted, ref } from "vue";
import { toastApiError } from "@/lib/async";
import { useRoute, useRouter } from "vue-router";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import MailDetailPane from "@/views/mail/MailDetailPane.vue";
import { mailApi } from "@/api/endpoints";
import type { MailDetail } from "@/api/types";
import { useToasts } from "@/stores/toast";
import type { MailViewKey } from "@/stores/mail";

// 邮件整页详情：从任意列表点入，左上角返回对应视图列表。
//
// 详情主体复用 MailDetailPane（安全沙箱渲染原样保留）。
// 星标/恢复后就地重拉；删除（移入归档）后邮件仍可读，重拉后状态栏切换；
// 彻底删除后详情接口 404，此时自动回到列表。

const route = useRoute();
const router = useRouter();
const toasts = useToasts();

const view = route.params.view as MailViewKey;
const messageId = String(route.params.id);

const detail = ref<MailDetail | null>(null);
const loading = ref(true);
const missing = ref(false);

async function load(): Promise<void> {
  loading.value = true;
  missing.value = false;
  try {
    detail.value = await mailApi.detail(messageId);
  } catch (err) {
    missing.value = true;
    toastApiError(toasts, err);
  } finally {
    loading.value = false;
  }
}

onMounted(() => {
  void load();
});

function backToList(): void {
  void router.push(`/mail/${view}`);
}

// 详情内操作后的统一处理：就地重拉；邮件已不存在（彻底删除）时回列表。
async function onChanged(): Promise<void> {
  try {
    detail.value = await mailApi.detail(messageId);
  } catch {
    backToList();
  }
}

</script>

<template>
  <section class="mmp">
    <button type="button" class="mmp__back" @click="backToList">
      <i class="ri-arrow-left-line" />
      返回
    </button>

    <AppEmpty
      v-if="missing"
      class="mmp__empty"
      icon="ri-mail-close-line"
      title="邮件不存在或已被彻底删除"
    >
      <button type="button" class="mmp__backlink" @click="backToList">返回邮件列表</button>
    </AppEmpty>
    <p v-else-if="loading" class="mmp__hint">加载中…</p>
    <MailDetailPane
      v-else-if="detail"
      :detail="detail"
      @changed="onChanged"
    />
  </section>
</template>

<style scoped>
.mmp {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  min-height: calc(100dvh - 10rem);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  background: var(--c-surface);
  padding: var(--sp-4);
}

.mmp__back {
  align-self: flex-start;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  flex: none;
  border: 0;
  padding: 4px 8px 4px 0;
  background: transparent;
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
  cursor: pointer;
}

.mmp__back:hover {
  color: var(--c-accent);
}

.mmp__hint {
  margin: var(--sp-4);
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  text-align: center;
}

.mmp__empty {
  margin: auto;
}

.mmp__backlink {
  margin-top: var(--sp-2);
  border: 0;
  background: transparent;
  color: var(--c-accent);
  font-size: var(--fs-sm);
  cursor: pointer;
  padding: 4px 8px;
}

.mmp__backlink:hover {
  text-decoration: underline;
}
</style>
