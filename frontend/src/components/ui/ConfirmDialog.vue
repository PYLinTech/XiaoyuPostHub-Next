<script setup lang="ts">
import AppButton from "./AppButton.vue";
import AppModal from "./AppModal.vue";

// 危险操作确认。
//
// 单独成组件是为了让"确认文案里必须写清后果"成为强制项：内联写 window.confirm
// 会退化成浏览器原生弹窗（不可样式化、在移动端还可能被拦截），而自己拼一个
// 弹窗又容易漏掉"正在执行时要禁用按钮"这一步。

withDefaults(
  defineProps<{
    open: boolean;
    title?: string;
    message: string;
    detail?: string;
    confirmText?: string;
    danger?: boolean;
    loading?: boolean;
  }>(),
  { title: "请确认", confirmText: "确认", danger: false, loading: false },
);

const emit = defineEmits<{ confirm: []; cancel: [] }>();
</script>

<template>
  <AppModal :open="open" :title="title" :dismissible="!loading" @close="emit('cancel')">
    <div class="stack">
      <p style="margin: 0">{{ message }}</p>
      <p v-if="detail" class="notice notice--warn" style="margin: 0">{{ detail }}</p>
    </div>
    <template #footer>
      <AppButton :disabled="loading" @click="emit('cancel')">取消</AppButton>
      <AppButton
        :variant="danger ? 'danger' : 'primary'"
        :loading="loading"
        @click="emit('confirm')"
      >
        {{ confirmText }}
      </AppButton>
    </template>
  </AppModal>
</template>
