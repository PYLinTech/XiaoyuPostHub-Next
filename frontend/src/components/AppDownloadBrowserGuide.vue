<script setup lang="ts">
import { onBeforeUnmount, watch } from "vue";
import { useRoute } from "vue-router";
import AppModal from "./ui/AppModal.vue";
import AppButton from "./ui/AppButton.vue";
import { downloadBrowserOpen, finishDownloadBrowser } from "@/stores/downloadBrowser";

const route = useRoute();
watch(() => route.fullPath, () => finishDownloadBrowser(false));
onBeforeUnmount(() => finishDownloadBrowser(false));
</script>

<template>
  <AppModal :open="downloadBrowserOpen" title="请在浏览器中下载" panel-class="download-browser-panel" hide-header
    @close="finishDownloadBrowser(false)">
    <div class="download-browser__icon" aria-hidden="true">
      <svg width="25" height="25" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6">
        <circle cx="12" cy="12" r="9" /><path d="M12 7v6" stroke-linecap="round" />
        <circle cx="12" cy="17" r="1" fill="currentColor" stroke="none" />
      </svg>
    </div>
    <h2 class="download-browser__title">请在浏览器中下载</h2>
    <p class="download-browser__desc">当前应用内可能无法正常保存文件。<br>请通过右上角菜单，在浏览器中打开。</p>
    <ol class="download-browser__steps">
      <li>点击右上角 <strong class="download-browser__dots" aria-label="三个点">···</strong></li>
      <li>选择 <strong>“在浏览器中打开”</strong></li>
      <li>打开后，再次点击 <strong>下载</strong></li>
    </ol>
    <AppButton block variant="primary" @click="finishDownloadBrowser(false)">我知道了</AppButton>
    <AppButton block variant="ghost" class="download-browser__try" @click="finishDownloadBrowser(true)">仍要尝试直接下载</AppButton>
  </AppModal>
  <Teleport to="body">
    <svg v-if="downloadBrowserOpen" class="download-browser__arrow" viewBox="0 0 100 100" fill="none" aria-hidden="true">
      <path d="M6 88C57 86 84 57 84 6M66 24L84 5L98 25" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
  </Teleport>
</template>

<style>
.download-browser-panel { width: min(360px, 100%); align-self: flex-start; margin-top: clamp(90px, 16dvh, 140px); }
.download-browser-panel .modal__body { padding: 25px 22px 14px; }
.download-browser__icon { width: 42px; height: 42px; display: grid; place-items: center; border-radius: 12px; background: var(--c-accent-weak); color: var(--c-accent); margin-bottom: 18px; }
.download-browser__title { font-size: 21px; line-height: 1.4; margin: 0 0 10px; }
.download-browser__desc { color: var(--c-text-muted); font-size: 14px; line-height: 1.8; margin: 0 0 22px; }
.download-browser__steps { list-style: none; counter-reset: download-step; padding: 0; margin: 0 0 24px; }
.download-browser__steps li { counter-increment: download-step; display: flex; align-items: center; flex-wrap: wrap; gap: 0 5px; margin: 16px 0; font-size: 14px; }
.download-browser__steps li::before { content: counter(download-step); display: grid; place-items: center; width: 23px; height: 23px; margin-right: 5px; border-radius: 50%; background: var(--c-accent-weak); color: var(--c-accent); font-size: 12px; font-weight: 600; }
.download-browser__dots { display: inline-flex; align-items: center; justify-content: center; min-height: 24px; padding: 0 7px; letter-spacing: 2px; border-radius: 5px; background: var(--c-surface-sunken); }
.download-browser__try { margin-top: 8px; color: var(--c-text-muted); font-size: 13px; }
.download-browser__arrow { position: fixed; top: max(16px, env(safe-area-inset-top)); right: max(22px, env(safe-area-inset-right)); width: 88px; height: 88px; color: white; z-index: calc(var(--z-modal) + 1); pointer-events: none; }
@media (max-height: 600px) {
  .download-browser-panel { align-self: center; margin-top: 0; }
  .download-browser__arrow { width: 54px; height: 54px; right: 12px; }
}
</style>
