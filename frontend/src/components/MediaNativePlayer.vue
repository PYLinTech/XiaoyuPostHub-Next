<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from "vue";

// 原生直放内核：<video>/<audio> 直接指向 Service Worker 的虚拟明文地址。
//
// 与兼容内核的差别在于整条链路的长度——浏览器自己发 Range，SW 换算成密文区间
// 取回、解密、按 206 返回。字节从存储直接进解码器，没有解封装/重封装、没有跨
// Worker 传输、没有 SourceBuffer 追加。常见 H.264 + AAC 的 MP4 走这里。
//
// 代价是这条路径没有动态缓冲控制（那属于兼容内核，见 mediaPlayback.ts）：
// 缓冲多少完全由浏览器和 SW 的批次设置决定。所以它只在浏览器明确认可这个
// 容器+编码时才启用，其余情况一律回退兼容内核。

const props = defineProps<{
  url: string;
  fileName: string;
  kind: "audio" | "video";
}>();

const emit = defineEmits<{
  /** 播放就绪，界面可以撤掉等待态。 */
  (event: "ready"): void;
  /** 播放失败：交给上层决定是回退兼容内核还是终止。 */
  (event: "failed", reason: unknown): void;
  (event: "playing"): void;
  (event: "waiting"): void;
}>();

const player = ref<HTMLMediaElement>();
/** URL 变化时重新加载（重新授权换了直链的场景）。 */
let currentUrl = "";

function attach(url: string) {
  const element = player.value;
  if (!element) return;
  currentUrl = url;
  // 先摘掉 src 再设，避免同一元素复用时旧缓冲被误算作新资源已就绪。
  element.removeAttribute("src");
  element.load();
  element.src = url;
  element.load();
}

function onCanPlay() { emit("ready"); }

function onError() {
  const element = player.value;
  // 原生内核失败不是终局：交回上层改走兼容内核，那条路能重封装与转码。
  emit("failed", new Error(element?.error?.message || "浏览器无法直接播放该文件"));
}

onMounted(() => {
  if (!props.url) return;
  attach(props.url);
});

watch(() => props.url, url => {
  if (url && url !== currentUrl) attach(url);
});

onBeforeUnmount(() => {
  const element = player.value;
  if (element) { element.pause(); element.removeAttribute("src"); element.load(); }
});
</script>

<template>
  <component :is="kind" ref="player" controls playsinline preload="auto" :aria-label="fileName"
    @canplay="onCanPlay" @playing="emit('playing')"
    @waiting="emit('waiting')" @error="onError" />
</template>
