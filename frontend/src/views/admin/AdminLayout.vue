<script setup lang="ts">
import { RouterView, useRoute } from "vue-router";

// 管理端外壳。
//
// 只留一个 <RouterView>：页面名在顶栏左侧（带「管理 ·」前缀），管理菜单在全局
// 侧栏（进入 /admin/* 时整条换成管理分组，顶部有「返回文件」）。这里再放一套
// 标题或返回入口，就会出现两个互不同步的菜单与两个返回路径。
//
// 嵌套出口包进入动画：App.vue 的动画只覆盖到 AdminLayout 本身，
// 管理页之间的切换要在这里才能看到。
const route = useRoute();
</script>

<template>
  <RouterView v-slot="{ Component }">
    <Transition name="route-anim" appear>
      <div :key="route.path" class="route-anim">
        <component :is="Component" />
      </div>
    </Transition>
  </RouterView>
</template>
