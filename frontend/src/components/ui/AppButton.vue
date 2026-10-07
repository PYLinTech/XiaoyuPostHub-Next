<script setup lang="ts">
import AppIcon from "./AppIcon.vue";

// 按钮。
//
// 存在的意义是把"加载中必须禁用"这条规则固化下来：手动写 :disabled="loading"
// 在几十处调用里总会漏掉一两处，而漏掉的表现是重复提交。
withDefaults(
  defineProps<{
    variant?: "default" | "primary" | "danger" | "ghost";
    size?: "md" | "sm";
    type?: "button" | "submit";
    loading?: boolean;
    disabled?: boolean;
    block?: boolean;
    icon?: string;
    title?: string;
  }>(),
  { variant: "default", size: "md", type: "button", loading: false, disabled: false, block: false },
);
</script>

<template>
  <button
    :type="type"
    :title="title"
    :disabled="disabled || loading"
    class="btn"
    :class="[`btn--${variant}`, size === 'sm' ? 'btn--sm' : '', block ? 'btn--block' : '']"
  >
    <span v-if="loading" class="spinner" />
    <AppIcon v-else-if="icon" :name="icon" :size="size === 'sm' ? 14 : 16" />
    <slot />
  </button>
</template>
