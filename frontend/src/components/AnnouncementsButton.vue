<script setup lang="ts">
import { ref } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import AppModal from "@/components/ui/AppModal.vue";
import { formatRelative } from "@/lib/format";
import { useSite } from "@/stores/site";

// 公告入口：铃铛 + 未读角标，点开是公告与站内消息。
// 按钮与弹窗放在同一个组件里，避免"入口在这、内容在那"。
//
// 点击必须在按钮上就地阻断冒泡：这个组件有两个根节点（按钮 + 弹窗），
// 属于 fragment，调用方写在 <AnnouncementsButton @click.stop> 上的监听
// 挂不进来（Vue 只会警告一句就丢掉），于是点击会冒到外层容器上——
// 侧栏底部那行账号条本身就是"点进我的"，铃铛跟着一起跳走。

const site = useSite();
const unreadCount = site.unreadCount;
const open = ref(false);

/** 打开时先拉一次：弹窗里看到的应该是当下的公告，而不是进站时的那一份。 */
function openModal(): void {
  open.value = true;
  void site.loadAnnouncements();
}
function read(id: string): void {
  site.markRead(id);
}
</script>

<template>
  <button
    type="button"
    class="toolbtn"
    :title="unreadCount > 0 ? `${unreadCount} 条未读` : '公告'"
    aria-label="公告与消息"
    @click.stop="openModal"
  >
    <AppIcon name="bell" :size="17" />
    <span v-if="unreadCount > 0" class="toolbtn__dot">{{ unreadCount }}</span>
  </button>

  <AppModal :open="open" title="公告与消息" compact @close="open = false">
    <!-- 刷新与全部已读常驻在标题行右侧：底部那条操作栏会把"随手看一眼"
         的小窗撑高一大截，而这两个动作本来就该在视线起点上。
         刷新只留图标，宽度才和旁边的关闭按钮一样。 -->
    <template #actions>
      <AppButton size="sm" :disabled="site.state.items.length === 0" @click="site.markAllRead()">
        全部已读
      </AppButton>
      <AppButton
        icon="refresh"
        size="sm"
        title="刷新"
        aria-label="刷新"
        :loading="site.state.loading"
        @click="site.loadAnnouncements()"
      />
    </template>

    <div class="stack">
      <p v-if="site.state.error" class="notice notice--danger">{{ site.state.error }}</p>

      <div v-if="!site.state.loaded || (site.state.loading && site.state.items.length === 0)" class="table-state">
        <span class="spinner" /> 正在加载
      </div>
      <AppEmpty v-else-if="site.state.items.length === 0" icon="bell" title="暂无公告" />
      <template v-else>
        <!-- 整张卡片可点 = 标记已读。原先是带 @click 的 <article>：鼠标独占，
             键盘用户既 Tab 不到也按不下，只能看着未读角标一直挂着。
             换成真正的 <button>，可聚焦、可回车/空格触发；内部结构一字未动。 -->
        <button
          v-for="item in site.state.items"
          :key="item.id"
          type="button"
          class="announcement"
          :class="{ 'announcement--unread': !site.isRead(item.id) }"
          @click="read(item.id)"
        >
          <div class="row row--between">
            <strong class="row" style="gap: 6px">
              <span v-if="!site.isRead(item.id)" class="announcement__dot" />
              {{ item.title || "(无标题)" }}
            </strong>
            <span class="faint" style="font-size: var(--fs-xs)">
              {{ formatRelative(item.createdAt) }}
            </span>
          </div>
          <p class="announcement__body">{{ item.body }}</p>
          <span class="badge">{{ item.kind === "message" ? "消息" : "公告" }}</span>
        </button>
      </template>
    </div>
  </AppModal>
</template>

<style scoped>
/* 正文区是定高的（.modal__panel--compact .modal__body）：让状态块撑满这块
   高度再把内容放到正中。不撑满的话，"暂无公告""正在加载"会贴在定高区的
   顶部，下面那一大片空白反而像是没对齐。 */
.stack {
  min-height: 100%;
}

/* .table-state 已提成全局工具类（styles/app.css），与 AppTable 共用同一份。 */

/* 空态是全站通用样式（.empty 自带横向居中），这里补上"吃掉剩余高度"
   并要求纵向居中；长列表本来就不参与均分，卡片仍是自身高度。 */
.empty {
  flex: 1;
  justify-content: center;
}

.announcement {
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  padding: var(--sp-3);
  cursor: pointer;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  /* 按钮的 UA 默认样式（居中对齐、buttontext 颜色、系统字体）会让整张卡片变形，
     这四条把它按回 article 的样子，视觉零变化。text-align/font 是可继承属性，
     内部的 div 与 p 一并被带正。 */
  width: 100%;
  text-align: left;
  font: inherit;
  color: inherit;
}

.announcement--unread {
  border-color: var(--c-accent);
  background: var(--c-accent-weak);
}

.announcement__body {
  margin: 0;
  white-space: pre-wrap;
  font-size: var(--fs-sm);
  line-height: 1.65;
}

.announcement__dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--c-danger);
  display: inline-block;
}
</style>
