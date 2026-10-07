<script setup lang="ts">
// 表单字段外壳：标签 + 说明 + 错误三件套。
//
// 抽出来是因为"错误信息挂在哪里"必须有统一答案：散在每个表单里会变成
// 有的字段把错误显示在下方、有的只把输入框描红，用户很难判断到底哪一项不对。
defineProps<{
  label?: string;
  hint?: string;
  error?: string;
  /** 标签右侧的操作区（例如"重置为默认"）。 */
  required?: boolean;
}>();
</script>

<template>
  <div class="field">
    <label v-if="label" class="field__label">
      <span>{{ label }}</span>
      <span v-if="required" class="field__required">*</span>
      <span class="spacer" />
      <slot name="action" />
    </label>
    <slot />
    <p v-if="error" class="field__error">{{ error }}</p>
    <p v-else-if="hint" class="field__hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.field__required {
  color: var(--c-danger);
}
</style>
