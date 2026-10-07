<script setup lang="ts">
import { computed } from "vue";

// 图标。
//
// 字形来自 Remix Icon（全量 woff2，自托管在 styles/remixicon.woff2）。之前是
// 三十来个手写 SVG 路径：加一个图标就要画一遍，画出来的形状和其余图标也不是
// 同一个设计语言。字体图标用 currentColor 上色、靠 font-size 定尺寸，因此
// 深色主题下与文字颜色天然一致，不需要为每种状态各留一份。
//
// name 接受两种写法：
//   - 语义名（folder、chevronRight）—— 见下面这张表
//   - Remix Icon 字形名（ri-folder-line，ri- 前缀可省）
// 语义名保留，是为了让 AppSideNav 的菜单定义、FileKindIcon 的类型映射
// 这些地方只表达"要什么含义"而不绑定具体字形；将来换图标集只改这张表。

const ALIASES: Record<string, string> = {
  menu: "ri-menu-line",
  close: "ri-close-line",
  plus: "ri-add-line",
  minus: "ri-subtract-line",
  search: "ri-search-line",
  check: "ri-check-line",
  chevronRight: "ri-arrow-right-s-line",
  chevronDown: "ri-arrow-down-s-line",
  chevronLeft: "ri-arrow-left-s-line",
  folder: "ri-folder-line",
  file: "ri-file-line",
  image: "ri-image-line",
  video: "ri-video-line",
  audio: "ri-music-line",
  text: "ri-file-text-line",
  archive: "ri-file-zip-line",
  upload: "ri-upload-2-line",
  download: "ri-download-line",
  share: "ri-share-line",
  trash: "ri-delete-bin-line",
  edit: "ri-edit-line",
  move: "ri-drag-move-2-line",
  refresh: "ri-refresh-line",
  sun: "ri-sun-line",
  moon: "ri-moon-line",
  user: "ri-user-line",
  users: "ri-group-line",
  shield: "ri-shield-check-line",
  bell: "ri-notification-3-line",
  settings: "ri-settings-3-line",
  chart: "ri-bar-chart-2-line",
  list: "ri-list-check",
  link: "ri-link",
  key: "ri-key-2-line",
  copy: "ri-file-copy-line",
  eye: "ri-eye-line",
  logout: "ri-logout-box-r-line",
  warning: "ri-error-warning-line",
  info: "ri-information-line",
  external: "ri-external-link-line",
  play: "ri-play-line",
  pause: "ri-pause-line",
  clock: "ri-time-line",
  lock: "ri-lock-line",
  tag: "ri-price-tag-3-line",
  filter: "ri-filter-3-line",
  cloud: "ri-cloud-line",
  database: "ri-database-2-line",
  hardDrive: "ri-hard-drive-2-line",
  activity: "ri-pulse-line",
  mail: "ri-mail-line",
};

const props = withDefaults(
  defineProps<{
    name: string;
    size?: number;
  }>(),
  { size: 18 },
);

const glyph = computed(() => {
  const alias = ALIASES[props.name];
  if (alias) {
    return alias;
  }
  // 认不出来的名字按 Remix Icon 字形名处理，而不是静默退回某个默认图标：
  // 拼错时看到的是一块空白，比看到一个"看起来没错"的图标更容易发现。
  return props.name.startsWith("ri-") ? props.name : `ri-${props.name}`;
});
</script>

<template>
  <i class="icon" :class="glyph" :style="{ fontSize: `${size}px` }" aria-hidden="true" />
</template>

<style scoped>
.icon {
  display: inline-block;
  /* 不参与 flex 收缩：图标被压扁是最常见的布局事故。 */
  flex: none;
  line-height: 1;
  vertical-align: middle;
}
</style>
