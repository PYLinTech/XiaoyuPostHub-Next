<script setup lang="ts">
import { computed } from "vue";
import AppIcon from "./AppIcon.vue";
import { fileKind, type FileKind } from "@/lib/filekind";

// 文件类型图标。颜色按类别区分，让长列表能被快速扫读。

const props = withDefaults(
  defineProps<{
    name: string;
    isFolder?: boolean;
    size?: number;
  }>(),
  { isFolder: false, size: 18 },
);

const ICON_BY_KIND: Record<FileKind, string> = {
  image: "image",
  video: "video",
  audio: "audio",
  pdf: "file",
  text: "text",
  archive: "archive",
  other: "file",
};

const kind = computed(() => fileKind(props.name));
const iconName = computed(() => (props.isFolder ? "folder" : ICON_BY_KIND[kind.value]));
</script>

<template>
  <AppIcon :name="iconName" :size="size" :class="['file-icon', `file-icon--${isFolder ? 'folder' : kind}`]" />
</template>

<style scoped>
.file-icon {
  color: var(--c-text-faint);
}

.file-icon--folder {
  color: var(--c-accent);
}

.file-icon--image {
  color: var(--c-success);
}

/* video/audio 原先写的是裸 hex，同文件里另外五个颜色都走令牌，
   于是切深色模式时只有这两类不跟随（紫与橙在深底上偏暗、还和 --c-warn 打架）。
   令牌 --c-video / --c-audio 的浅/深两套取值见 styles/tokens.css。 */
.file-icon--video {
  color: var(--c-video);
}

.file-icon--audio {
  color: var(--c-audio);
}

.file-icon--archive {
  color: var(--c-warn);
}
</style>
