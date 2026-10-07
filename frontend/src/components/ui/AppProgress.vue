<script setup lang="ts">
// 进度条。ratio 为 null 时显示不确定态——用于"还不知道总量"的场景，
// 例如已经在传输但服务端尚未给出明文长度。

import { computed } from "vue";
import { formatBytes } from "@/lib/format";

const props = withDefaults(
  defineProps<{
    ratio: number | null;
    bytesDone?: number;
    bytesTotal?: number;
    label?: string;
    /**
     * 进度条的无障碍名称。调用方最好给一个能区分场景的（"上传进度"/"下载进度"），
     * 因为四个调用点传进来的 label 是会变的进度描述（"正在准备""下载中"），
     * 拿它当名字等于让读屏每跳一次百分比就换个名字。
     */
    name?: string;
  }>(),
  { bytesDone: 0, bytesTotal: 0, name: "进度" },
);

const percent = computed(() => {
  if (props.ratio === null) {
    return 0;
  }
  return Math.min(100, Math.max(0, Math.round(props.ratio * 1000) / 10));
});

/**
 * 读屏要读的是一句话而不是一个孤零零的百分数：百分比、状态文案、已传字节
 * 拼在一起，才能替代视觉上"条 + 右侧数字"所传达的全部信息。
 */
const valueText = computed(() => {
  const parts: string[] = [];
  if (props.ratio !== null) parts.push(`${percent.value}%`);
  if (props.label) parts.push(props.label);
  if (props.bytesTotal > 0) {
    parts.push(`${formatBytes(props.bytesDone)} / ${formatBytes(props.bytesTotal)}`);
  } else if (props.bytesDone > 0) {
    parts.push(formatBytes(props.bytesDone));
  }
  return parts.join("，");
});
</script>

<template>
  <div class="progress-wrap">
    <!-- 不确定态（ratio 为 null）时不给 aria-valuenow：ARIA 规定缺了 valuenow
         就是"总量未知"，此时报一个 0% 比不报更糟。进度靠 aria-valuetext 说明。 -->
    <div
      class="progress"
      role="progressbar"
      :aria-label="name"
      aria-valuemin="0"
      aria-valuemax="100"
      :aria-valuenow="ratio === null ? undefined : percent"
      :aria-valuetext="valueText"
    >
      <div
        class="progress__bar"
        :class="{ 'progress__bar--indeterminate': ratio === null }"
        :style="ratio === null ? undefined : { width: `${percent}%` }"
      />
    </div>
    <p class="progress-wrap__meta faint">
      <span v-if="label">{{ label }}</span>
      <span v-if="bytesTotal > 0">{{ formatBytes(bytesDone) }} / {{ formatBytes(bytesTotal) }}</span>
      <span v-else-if="bytesDone > 0">{{ formatBytes(bytesDone) }}</span>
    </p>
  </div>
</template>

<style scoped>
.progress-wrap {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
}

.progress-wrap__meta {
  display: flex;
  justify-content: space-between;
  gap: var(--sp-2);
  font-size: var(--fs-xs);
  margin: 0;
}
</style>
