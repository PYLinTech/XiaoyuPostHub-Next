<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { RouterView, useRoute, useRouter } from "vue-router";
import { applyDocumentTitle } from "@/router";
import AppSideNav from "@/components/AppSideNav.vue";
import AppTickerBar from "@/components/AppTickerBar.vue";
import AppToast from "@/components/AppToast.vue";
import AppTopBar from "@/components/AppTopBar.vue";
import AppUploadDock from "@/components/AppUploadDock.vue";
import { useOverlayScroll } from "@/lib/overlayScroll";
import { onSessionChange, useSession } from "@/stores/session";
import { loadAnnouncements } from "@/stores/site";

// 应用外壳。
//
// 三种版面：整屏（初始化/登录/注册，没有导航可去）、带导航的常规版面、
// 以及探活完成前的占位。分成三条明确的路径而不是靠隐藏元素，
// 是为了避免"未登录时侧栏闪一下"这类观感问题。
//
// 两条路由出口都包了进入动画：页面切换只做进入不做离开，
// 离开动画会让导航等前一页退场。管理端 /admin/* 的实际页面在
// AdminLayout 的嵌套出口里，那边也要包一层才能在管理页之间生效。

const route = useRoute();
const router = useRouter();
const session = useSession();
const drawerOpen = ref(false);

// 令牌中途过期时，client 的 setUnauthorizedHandler 只清了登录状态，没负责把人
// 送回登录页：界面会停在一个还挂着旧数据的页面上，侧栏却已经切成访客布局，
// 要等用户下一次导航才被守卫弹走。跳转放在这里而不是 stores/session.ts——
// 那里 import router 会和 router.ts 形成循环依赖。
watch(
  () => session.state.authenticated,
  (authed, was) => {
    // 只在"由已登录变为未登录"时跳：主动登出走的是 logout()，那里自己会跳。
    if (was && !authed && route.name !== "login" && route.name !== "setup") {
      void router.push({ name: "login", query: { next: route.fullPath } });
    }
  },
);

// 滚动条挂在真正的滚动容器上（顶栏下面那块内容区，见 app.css 的 .shell 一节）：
// 滑块的轨道因此只覆盖那块区域，而不是整页。
const contentEl = ref<HTMLElement | null>(null);
const { visible, style, onThumbPointerDown, onThumbPointerMove, onThumbPointerUp } = useOverlayScroll(
  contentEl,
  "y",
);

/** 整屏版面：这些页面出现在用户还没有身份、或流程尚未走通的阶段。 */
const BARE_ROUTES = new Set(["setup", "login", "register", "pickup", "not-found"]);
// 取件码是唯一的例外：访客（登录页入口、外链）看整屏版式，
// 已登录用户从侧栏进入时在壳内打开，整页替换会让侧栏入口"把自己关掉"。
const bare = computed(() => {
  if (route.name === "pickup" && session.state.authenticated) {
    return false;
  }
  return BARE_ROUTES.has(String(route.name ?? ""));
});

watch(() => route.fullPath, () => {
  drawerOpen.value = false;
});

// 站点名可能在管理端被改，标签页标题要跟着变（路由切换之外的另一条更新路径）。
watch(
  () => session.state.siteName,
  () => applyDocumentTitle(route.meta.title),
);

// 登录状态变化时重新拉公告：访客与登录用户可见的范围不同，
// 不重拉会让刚登录的人看不到发给"指定用户"的消息。
onSessionChange(() => {
  void loadAnnouncements();
});
void loadAnnouncements();
</script>

<template>
  <!-- 首次路由解析完成前（matched 为空）也按未就绪处理：ready 在路由守卫里
       置位，会先于路由提交被绘制——不补这个条件的话，每次硬加载整屏页
       （/login、/pickup……）都会先闪过一帧带侧栏的空壳。 -->
  <div v-if="!session.state.ready || route.matched.length === 0" class="boot">
    <span class="spinner" />
    <p class="muted">正在加载</p>
  </div>

  <RouterView v-else-if="bare" v-slot="{ Component }">
    <!-- out-in：新旧两页不能共存于文档流——两个整屏页叠放会把文档撑高
         一倍，动画期间闪出滚动条。旧页没有离场动画，out-in 的"等"只有一帧。 -->
    <Transition name="route-anim" mode="out-in" appear>
      <div :key="route.path" class="route-anim">
        <component :is="Component" />
      </div>
    </Transition>
  </RouterView>

  <div
    v-else
    class="shell"
    :class="{
      'shell--admin': route.path.startsWith('/admin'),
      'shell--mail': route.path === '/mail' || route.path.startsWith('/mail/'),
    }"
  >
    <AppSideNav :open="drawerOpen" @close="drawerOpen = false" />
    <button v-if="drawerOpen" type="button" class="scrim" aria-label="关闭导航" @click="drawerOpen = false" />
    <div class="shell__main">
      <AppTickerBar />
      <AppTopBar @toggle-drawer="drawerOpen = !drawerOpen" />
      <div class="shell__scroll ovscroll-layer ovscroll-layer--y">
        <main ref="contentEl" class="shell__content ovscroll">
          <RouterView v-slot="{ Component }">
            <Transition name="route-anim" mode="out-in" appear>
              <div :key="route.path" class="route-anim">
                <component :is="Component" />
              </div>
            </Transition>
          </RouterView>
        </main>
        <div class="ovscroll__track" :class="{ 'ovscroll__track--on': visible }">
          <div
            class="ovscroll__thumb"
            :style="style"
            @pointerdown="onThumbPointerDown"
            @pointermove="onThumbPointerMove"
            @pointerup="onThumbPointerUp"
          />
        </div>
      </div>
    </div>
  </div>

  <AppUploadDock />
  <AppToast />
</template>

<style scoped>
.boot {
  min-height: 100dvh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--sp-3);
}
</style>
