<script setup lang="ts">
import { ref } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppProgress from "@/components/ui/AppProgress.vue";
import { formatBytes } from "@/lib/format";
import { useUploads } from "@/stores/uploads";

// 上传进度面板。
//
// 收起来时只留一个悬浮按钮，展开后列出每个文件的进度。它必须在页面切换时
// 存活——这正是上传队列放在全局的原因。

const { items, activeCount, failedCount, cancelUploadItem, retryUpload, removeUpload, clearFinishedUploads } =
  useUploads();
const collapsed = ref(false);

const STATUS_LABEL: Record<string, string> = {
  queued: "排队中",
  running: "上传中",
  done: "已完成",
  error: "失败",
  canceled: "已取消",
};
</script>

<template>
  <div v-if="items.length > 0" class="dock" :class="{ 'dock--collapsed': collapsed }">
    <header class="dock__head">
      <button type="button" class="dock__toggle" @click="collapsed = !collapsed">
        <AppIcon :name="collapsed ? 'upload' : 'chevronDown'" :size="16" />
        <span>
          上传
          <template v-if="activeCount > 0">（{{ activeCount }} 进行中）</template>
          <template v-else-if="failedCount > 0">（{{ failedCount }} 失败）</template>
        </span>
      </button>
      <div class="row" style="gap: 4px">
        <AppButton size="sm" variant="ghost" title="清除已结束" @click="clearFinishedUploads()">
          <AppIcon name="trash" :size="14" />
        </AppButton>
      </div>
    </header>

    <div v-show="!collapsed" class="dock__body">
      <article v-for="item in items" :key="item.id" class="dock__item">
        <div class="row row--between" style="gap: var(--sp-2)">
          <span class="truncate" style="font-weight: 550; max-width: 60%" :title="item.file.name">
            {{ item.file.name }}
          </span>
          <span
            class="badge"
            :class="{
              'badge--success': item.status === 'done',
              'badge--danger': item.status === 'error',
              'badge--accent': item.status === 'running',
            }"
          >
            {{ STATUS_LABEL[item.status] }}
          </span>
        </div>

        <AppProgress
          v-if="item.status === 'running' || item.status === 'queued'"
          :ratio="item.status === 'queued' ? 0 : item.progress.ratio"
          :bytes-done="item.progress.sent"
          :bytes-total="item.progress.total"
          :label="item.progress.message"
        />
        <p v-else-if="item.status === 'error'" class="dock__error">{{ item.errorMessage }}</p>
        <p v-else class="faint" style="font-size: var(--fs-xs)">
          <template v-if="item.dedup">秒传命中，未传输数据</template>
          <template v-else>{{ formatBytes(item.file.size) }}</template>
        </p>

        <div class="row" style="gap: 4px">
          <AppButton
            v-if="item.status === 'running' || item.status === 'queued'"
            size="sm"
            variant="ghost"
            @click="cancelUploadItem(item.id)"
          >
            取消
          </AppButton>
          <AppButton v-if="item.status === 'error'" size="sm" variant="ghost" @click="retryUpload(item.id)">
            重试
          </AppButton>
          <AppButton
            v-if="item.status === 'done' || item.status === 'error' || item.status === 'canceled'"
            size="sm"
            variant="ghost"
            @click="removeUpload(item.id)"
          >
            移除
          </AppButton>
        </div>
      </article>
    </div>
  </div>
</template>

<style scoped>
.dock {
  position: fixed;
  right: var(--sp-4);
  bottom: var(--sp-4);
  width: min(400px, calc(100vw - 32px));
  max-height: min(60dvh, 520px);
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  box-shadow: var(--shadow-lg);
  z-index: 70;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.dock--collapsed {
  width: auto;
}

.dock__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  border-bottom: 1px solid var(--c-border);
}

.dock__toggle {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  border: 0;
  background: transparent;
  cursor: pointer;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--c-text);
  padding: 0;
}

.dock__body {
  overflow-y: auto;
  padding: var(--sp-2) var(--sp-3) var(--sp-3);
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

.dock__item {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding-bottom: var(--sp-3);
  border-bottom: 1px solid var(--c-border);
}

.dock__item:last-child {
  border-bottom: 0;
  padding-bottom: 0;
}

.dock__error {
  margin: 0;
  color: var(--c-danger);
  font-size: var(--fs-xs);
  word-break: break-word;
}

@media (max-width: 640px) {
  .dock {
    left: var(--sp-2);
    right: var(--sp-2);
    bottom: var(--sp-2);
    width: auto;
  }
}
</style>
