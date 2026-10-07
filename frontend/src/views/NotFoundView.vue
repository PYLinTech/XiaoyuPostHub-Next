<script setup lang="ts">
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import { useRouter } from "vue-router";

const router = useRouter();
</script>

<template>
  <div class="shell__content">
    <AppEmpty icon="search" title="页面不存在" hint="链接可能已经失效，或者地址输错了">
      <AppButton variant="primary" icon="chevronLeft" @click="router.push('/files')">返回文件</AppButton>
    </AppEmpty>
  </div>
</template>
