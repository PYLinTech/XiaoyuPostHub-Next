<script setup lang="ts">
import AppIcon from "@/components/ui/AppIcon.vue";
import { useToasts } from "@/stores/toast";

// 全局通知条：顶部居中，一次只显示一条。
//
// 单条由 store 保证（新的顶掉旧的），out-in 过渡让旧条先隐去、新条再落下；
// 锚层铺满整宽但关闭指针事件，提示悬浮在顶栏上方时也不挡底下的点击。

const toasts = useToasts();

const ICON_BY_KIND = { info: "info", success: "check", error: "warning" } as const;
</script>

<template>
  <div class="toast-anchor" role="status" aria-live="polite">
    <Transition name="toast" mode="out-in">
      <div
        v-if="toasts.current"
        :key="toasts.current.id"
        class="toast"
        :class="`toast--${toasts.current.kind}`"
        @mouseenter="toasts.pause()"
        @mouseleave="toasts.resume()"
      >
        <span class="toast__icon">
          <AppIcon :name="ICON_BY_KIND[toasts.current.kind]" :size="16" />
        </span>
        <div class="toast__body">
          <div>{{ toasts.current.message }}</div>
          <div v-if="toasts.current.detail" class="toast__detail">{{ toasts.current.detail }}</div>
        </div>
        <button type="button" class="toast__close" aria-label="关闭" @click="toasts.dismiss()">
          <AppIcon name="close" :size="14" />
        </button>
      </div>
    </Transition>
  </div>
</template>
