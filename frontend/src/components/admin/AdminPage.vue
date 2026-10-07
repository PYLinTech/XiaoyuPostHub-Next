<script setup lang="ts">
// 管理页唯一骨架：间距节奏 + 错误横幅。操作按钮由各页投递到顶栏（topbarSlot）。
defineProps<{ error?: string }>();
</script>

<template>
  <div class="admin-page stack">
    <div v-if="error" class="notice notice--danger">{{ error }}</div>
    <slot />
  </div>
</template>
