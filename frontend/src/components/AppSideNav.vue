<script setup lang="ts">
import { computed, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import AnnouncementsButton from "@/components/AnnouncementsButton.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppIcon from "@/components/ui/AppIcon.vue";
import logoUrl from "@/assets/logo.svg";
import { Perm } from "@/api/types";
import { formatBytes } from "@/lib/format";
import { useOverlayScroll } from "@/lib/overlayScroll";
import { useSession } from "@/stores/session";
import { useSite } from "@/stores/site";
import { useToasts } from "@/stores/toast";
import { useMailShell } from "@/stores/mail";

// 侧边导航。
//
// 只有管理壳会替换整组菜单：用户侧（/files、/mail、/shares、/pickup）与
// 管理员侧共用同一条侧栏，普通用户和访客不会看到一整套运维入口；进入
// /admin/* 后按域分组，返回入口落在品牌行。
//
// 邮件是普通页面，「收件/星标/归档」三个视图由页内选项卡切换，不再占侧栏。
//
// 原生滚动条被隐藏（见 app.css 的 .shell__sidebar），溢出时由自绘指示条表达：
// 绝对定位、不占布局宽度，悬停或滚动时浮现，停止片刻后淡出。几何与监听复用
// useOverlayScroll，只是不开拖拽。

defineProps<{ open: boolean }>();
const emit = defineEmits<{ close: [] }>();

const route = useRoute();
const router = useRouter();
const session = useSession();
const site = useSite();
interface NavItem {
  label: string;
  to: string;
  icon: string;
  auth?: boolean;
  perm?: number;
  /** 匹配前缀，用于高亮；缺省用 to。 */
  match?: string;
  /** 右侧徽标取值。用函数而不是数字：计数来自 store，菜单项本身是常量。 */
  badge?: () => number;
}

const userItems: NavItem[] = [
  { label: "文件", to: "/files", icon: "folder", auth: true, perm: Perm.Download, match: "/files" },
  {
    label: "邮件",
    to: "/mail",
    icon: "ri-mail-line",
    auth: true,
    perm: Perm.MailAccess,
    match: "/mail",
    badge: () => mail.state.unreadInbox,
  },
  { label: "我的分享", to: "/shares", icon: "share", auth: true, perm: Perm.Share },
  { label: "取件码", to: "/pickup", icon: "key" },
  { label: "归档", to: "/archive", icon: "trash", auth: true },
];

/**
 * 「仅文件」模式下要拿掉的项。
 *
 * 用 to 前缀判断而不是给 NavItem 加一个 needs 字段：禁用条件只有"邮件被关"
 * 这一条，加字段等于为单一用途把数据结构撑宽。放在过滤函数里，规则与判定
 * 写在同一处。
 */
function blockedByMode(item: NavItem): boolean {
  if (!item.to.startsWith("/mail") && !item.to.startsWith("/admin/mail")) {
    return false;
  }
  return !session.mailEnabled.value;
}

/** 管理项按域分组：同级菜单超过 7 项之后，分组比"排得下"更重要。 */
const adminGroups: Array<{ label: string; items: NavItem[] }> = [
  {
    label: "工作台",
    items: [{ label: "概览", to: "/admin/overview", icon: "chart", perm: Perm.AdminAudit }],
  },
  {
    label: "内容",
    items: [
      // 文件与邮件并排放「内容」：这两页回答的是同一个问题——站点里有什么，
      // 而流量/审计回答的是"发生过什么"，属于运营侧。收件域名是用户组的
      // 属性，配置入口在「设置 → 用户组」里，不再单独占一个导航项。
      { label: "文件管理", to: "/admin/files", icon: "hardDrive", perm: Perm.AdminFiles },
      { label: "邮件管理", to: "/admin/mail", icon: "ri-mail-line", perm: Perm.AdminMail },
      { label: "归档管理", to: "/admin/archive", icon: "trash", perm: Perm.AdminFiles },
      { label: "公告", to: "/admin/announcements", icon: "bell", perm: Perm.AdminAnnouncements },
    ],
  },
  {
    label: "账号",
    items: [
      { label: "账号", to: "/admin/users", icon: "users", perm: Perm.AdminUsers },
      { label: "用户组", to: "/admin/groups", icon: "shield", perm: Perm.AdminGroups },
      { label: "邀请码", to: "/admin/invites", icon: "tag", perm: Perm.AdminInvites },
    ],
  },
  {
    label: "运营",
    items: [
      { label: "流量", to: "/admin/traffic", icon: "activity", perm: Perm.AdminAudit },
      { label: "审计", to: "/admin/audit", icon: "list", perm: Perm.AdminAudit },
      // 解绑与注销独立于 AdminMail：它是逐条表态的审核动作，代价由外部
      // 发信人承担，不该跟"改收件域名"共用一个权限位。
      { label: "解绑与注销", to: "/admin/unbind", icon: "logout", perm: Perm.AdminUnbind },
    ],
  },
  {
    label: "系统",
    items: [
      { label: "存储", to: "/admin/storage", icon: "cloud", perm: Perm.AdminStorage },
      { label: "数据库", to: "/admin/database", icon: "database", perm: Perm.AdminStorage },
      { label: "系统配置", to: "/admin/settings", icon: "settings", perm: Perm.AdminSystem },
    ],
  },
];

function visible(item: NavItem): boolean {
  if (blockedByMode(item)) {
    return false;
  }
  if (item.auth && !session.state.authenticated) {
    return false;
  }
  if (item.perm !== undefined && !session.can(item.perm)) {
    return false;
  }
  return true;
}

const mainItems = computed(() => userItems.filter(visible));

/** 每项徽标先求值一次收成表：模板里 badge() 会被调两次（判 0 与显示），
 * 每次都重新读 store。这里让读取只发生在一处，渲染时只做键查找。 */
const mainBadges = computed<Record<string, number>>(() =>
  Object.fromEntries(mainItems.value.map((item): [string, number] => [item.to, item.badge?.() ?? 0])),
);

const visibleAdminGroups = computed(() =>
  adminGroups
    .map((group) => ({ label: group.label, items: group.items.filter(visible) }))
    .filter((group) => group.items.length > 0),
);

const mail = useMailShell();

/** 管理控制台的落点：第一个有权限的管理页，而不是写死 overview。 */
const adminEntry = computed(() => visibleAdminGroups.value[0]?.items[0]?.to ?? "/admin/overview");
const hasAdmin = computed(() => visibleAdminGroups.value.length > 0);
const inAdminShell = computed(() => route.path === "/admin" || route.path.startsWith("/admin/"));

const quota = computed(() => {
  const limit = session.state.storageLimit;
  const used = session.state.storageUsed;
  const ratio = limit > 0 ? used / limit : 0;
  return {
    limit,
    used,
    ratio,
    percent: `${Math.round(ratio * 100)}%`,
    level: ratio >= 0.9 ? "danger" : ratio >= 0.75 ? "warn" : "normal",
  };
});

/** 账号行首字：按 code point 取，否则 emoji 会被 slice(0, 1) 劈成半个代理对
 *  （与 lib/credentials.ts 的 charLength 同一处理）。 */
const avatarInitial = computed(
  () => Array.from(session.state.user?.displayName || session.state.user?.account || "?")[0].toUpperCase(),
);

const isMePage = computed(() => route.path === "/me");

function isActive(item: NavItem): boolean {
  const prefix = item.match ?? item.to;
  return route.path === prefix || route.path.startsWith(`${prefix}/`);
}

async function go(to: string): Promise<void> {
  emit("close");
  if (route.path !== to) {
    await router.push(to);
  }
}

/** 退出登录：顶栏不再放用户菜单，这里就是唯一的出口。 */
async function logout(): Promise<void> {
  emit("close");
  await session.logout();
  useToasts().success("已退出登录");
  await router.push("/login");
}

/** 账号行是 role="button"，Enter 与 Space 都要能激活——Space 不拦就是翻页。
 * 只认事件发生在行自身的情况：行内还嵌着公告、主题、退出三个真按钮，它们的
 * 按键会冒泡上来，顺带把整行导航走等于「刚退出登录又跳去我的」。 */
function onAccountKeydown(event: KeyboardEvent): void {
  if (event.target !== event.currentTarget) {
    return;
  }
  if (event.key !== "Enter" && event.key !== " ") {
    return;
  }
  // Space 默认滚动页面，这里是按钮语义，必须拦掉默认行为。
  event.preventDefault();
  void go("/me");
}

// ---------------------------------------------------------------- 自绘滚动指示条

const scrollEl = ref<HTMLElement | null>(null);

// 几何、滚动/resize/内容变化监听、悬停亮起与卸载清理都交给 useOverlayScroll
// （全站同一套算法，侧栏原本是它的一份手写复制品）。两个参数偏离默认值都是
// 有意为之：
//   - draggable:false：滑块只有 4px 宽且 pointer-events:none，拖拽手势只会
//     和菜单项点击抢事件；
//   - fadeDelay:700（而非全站 800）：侧栏是常驻元素，停下来后指示条要尽快
//     收回去给菜单内容让位。
//
// 上面那个可见性过滤函数已经占了 visible 这个名字，这里解构时改用别名
// thumbOn，与 .sidebar-thumb--on 对齐。悬停不再自己维护：组合式函数已在滚动层
// 上挂了 pointerenter/pointerleave，并且会挡住淡出。
const { visible: thumbOn, style } = useOverlayScroll(scrollEl, "y", {
  draggable: false,
  fadeDelay: 700,
});
</script>

<template>
  <aside class="shell__sidebar" :class="{ 'shell__sidebar--open': open }">
    <!-- 滚动层与指示条必须是兄弟：绝对定位的指示条若放进滚动容器，
         会跟着内容一起被滚走，位置永远对不上（见 app.css .shell__sidebar 注释）。 -->
    <div class="brand">
      <img class="brand__logo" :src="logoUrl" alt="" width="28" height="28" />
      <span class="brand__meta">
        <span class="brand__text truncate">{{ session.state.siteName }}</span>
        <span v-if="inAdminShell" class="brand__sub">管理后台</span>
      </span>
      <!-- 二级壳的返回入口跟着品牌行走：比占一条导航项更省高度，
           样式与底部账号行的退出按钮同一套（图标按钮，悬停只变色）。 -->
      <button
        v-if="inAdminShell"
        type="button"
        class="brand__back"
        title="返回文件"
        aria-label="返回文件"
        @click="go('/files')"
      >
        <!-- 回退箭头（↩）比 chevron 更明确地表达"返回上一个壳"。 -->
        <AppIcon name="ri-arrow-go-back-line" :size="16" />
      </button>
    </div>

    <!-- 中段：唯一产生滚动的区域，指示条只覆盖这一段。 -->
    <div class="shell__sidebar-mid">
      <div ref="scrollEl" class="shell__sidebar-scroll">

      <!-- 管理壳：进入后整体换一组菜单，返回入口在右上角品牌行。 -->
      <template v-if="inAdminShell">
        <template v-for="group in visibleAdminGroups" :key="group.label">
          <p class="nav__heading">{{ group.label }}</p>
          <nav class="nav">
            <button
              v-for="item in group.items"
              :key="item.to"
              type="button"
              class="nav__item"
              :class="{ 'nav__item--active': isActive(item) }"
              @click="go(item.to)"
            >
              <AppIcon :name="item.icon" :size="17" />
              <span class="truncate">{{ item.label }}</span>
            </button>
          </nav>
        </template>
      </template>

      <!-- 用户壳：只保留常驻项 + 一个管理入口 -->
      <template v-else>
        <nav class="nav">
          <button
            v-for="item in mainItems"
            :key="item.to"
            type="button"
            class="nav__item"
            :class="{ 'nav__item--active': isActive(item) }"
            @click="go(item.to)"
          >
            <AppIcon :name="item.icon" :size="17" />
            <span class="truncate">{{ item.label }}</span>
            <span v-if="mainBadges[item.to]" class="nav__badge">{{ mainBadges[item.to] }}</span>
          </button>
        </nav>

        <template v-if="hasAdmin">
          <hr class="nav__divider" />
          <nav class="nav">
            <button type="button" class="nav__item nav__item--entry" @click="go(adminEntry)">
              <AppIcon name="shield" :size="17" />
              <span class="truncate">管理后台</span>
            </button>
          </nav>
        </template>
      </template>

      <!-- 存储用量卡片：钉在导航末尾，管理壳不显示；
           未设上限时进度条留空、右侧与用量文字显示 ∞。 -->
      <div v-if="session.state.authenticated && !inAdminShell" class="quota nav__quota">
        <div class="quota__row">
          <span>存储用量</span>
          <span v-if="quota.limit > 0">{{ quota.percent }}</span>
          <span v-else class="quota__infinite" title="未设置上限">∞</span>
        </div>
        <!-- meter--slim：卡片里的进度条用全站 .meter 的一半高度（规则见 app.css），
             免得 8px 的条在窄卡片里压过「存储用量」这行字。 -->
        <div class="meter meter--slim">
          <div
            class="meter__fill"
            :class="{
              'meter__fill--warn': quota.level === 'warn',
              'meter__fill--danger': quota.level === 'danger',
            }"
            :style="{ width: `${Math.min(100, quota.ratio * 100)}%` }"
          />
        </div>
        <p class="quota__text">
          {{ formatBytes(quota.used) }} / <span class="quota__limit">{{ quota.limit > 0 ? formatBytes(quota.limit) : "∞" }}</span>
        </p>
      </div>
      </div>

      <!-- 自绘滚动指示条：绝对定位不占布局，活动范围只在中段。
           几何与亮灭由 useOverlayScroll 给出，滑块是 pointer-events:none 的
           纯装饰件（不可拖），所以没有挂拖拽处理器。 -->
      <div
        class="sidebar-thumb"
        :class="{ 'sidebar-thumb--on': thumbOn }"
        :style="style"
        aria-hidden="true"
      />
    </div>

    <!-- 底部区固定不滚：分隔线与它以下的内容始终可见。 -->
    <div class="nav__foot">
      <template v-if="!session.state.authenticated">
        <div class="tools">
          <AnnouncementsButton />
          <button
            type="button"
            class="toolbtn"
            :title="site.state.theme === 'dark' ? '切换到浅色' : '切换到深色'"
            :aria-label="site.state.theme === 'dark' ? '切换到浅色' : '切换到深色'"
            @click="site.toggleTheme()"
          >
            <!-- 图标表达当前主题（深色显示月亮），点击后切换到另一种。 -->
            <AppIcon :name="site.state.theme === 'dark' ? 'moon' : 'sun'" :size="17" />
          </button>
        </div>

        <AppButton variant="primary" block icon="logout" @click="go('/login')">
          登录
        </AppButton>
      </template>

      <template v-else>
        <!-- role="button" 里嵌真按钮在 ARIA 上并不理想，但拆结构要动这套行内样式，
             收益不抵风险。这里只补齐 button 的必备激活键（Enter/Space 都在
             onAccountKeydown 里），并把当前页状态用 aria-current 讲出来——
             视觉上已有 account--active，只是辅助技术读不到。 -->
        <div
          class="account"
          :class="{ 'account--active': isMePage }"
          :aria-current="isMePage ? 'page' : undefined"
          role="button"
          tabindex="0"
          title="我的"
          @click="go('/me')"
          @keydown="onAccountKeydown"
        >
          <span class="account__avatar">
            {{ avatarInitial }}
          </span>
          <span class="account__text">
            <span class="account__name truncate">{{ session.state.user?.displayName || session.state.user?.account }}</span>
            <span class="account__group truncate">{{ session.state.group?.displayName }}</span>
          </span>
          <div class="account__tools">
            <!-- 主题按钮在这里阻断冒泡；铃铛的阻断在组件自己的按钮上——
                 它是 fragment，监听器传不进去。 -->
            <AnnouncementsButton />
            <button
              type="button"
              class="toolbtn"
              :title="site.state.theme === 'dark' ? '切换到浅色' : '切换到深色'"
              :aria-label="site.state.theme === 'dark' ? '切换到浅色' : '切换到深色'"
              @click.stop="site.toggleTheme()"
            >
              <!-- 图标表达当前主题，与未登录工具行保持同一语义。 -->
              <AppIcon :name="site.state.theme === 'dark' ? 'moon' : 'sun'" :size="17" />
            </button>
          </div>
          <button
            type="button"
            class="account__exit"
            title="退出登录"
            aria-label="退出登录"
            @click.stop="logout"
          >
            <AppIcon name="logout" :size="16" />
          </button>
        </div>
      </template>
    </div>
  </aside>
</template>

<style scoped>
/* 自绘滚动指示条：绝对定位浮在右缘，不占布局宽度；
   原生滚动条已在 app.css 里对 .shell__sidebar 整体隐藏。 */
.sidebar-thumb {
  position: absolute;
  top: 0;
  right: 2px;
  width: 4px;
  border-radius: var(--r-pill);
  background: color-mix(in srgb, var(--c-text-faint) 45%, transparent);
  pointer-events: none;
  opacity: 0;
  transition: opacity 0.2s ease;
  /* 浮在菜单内容之上，取最小的"高于常规流"层级即可：这里不需要压住任何弹层。 */
  z-index: var(--z-sticky);
}

.sidebar-thumb--on {
  opacity: 1;
}

.brand {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  /* 上下内边距对称，图标才能在与顶栏同高的带子里垂直居中；
     左侧比滚动区（12px）多缩进一档，向导航图标的视觉位置看齐。 */
  padding: var(--sp-3);
  padding-left: var(--sp-5);
  font-weight: 650;
  font-size: var(--fs-md);
  min-width: 0;
}

.brand__meta {
  min-width: 0;
  display: flex;
  flex-direction: column;
  line-height: 1.3;
}

.brand__sub {
  color: var(--c-text-faint);
  font-size: var(--fs-xs);
  font-weight: 500;
  margin-top: 2px;
}

.brand__logo {
  width: 28px;
  height: 28px;
  flex: none;
  display: block;
}

/* 管理壳的返回按钮：与账号行退出按钮同一套图标按钮规格（28px、无底、悬停变色）。 */
.brand__back {
  width: 28px;
  height: 28px;
  flex: none;
  margin-left: auto;
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--c-text-faint);
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.brand__back:hover {
  color: var(--c-accent);
}

.nav {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.nav__divider {
  border: 0;
  border-top: 1px solid var(--c-border);
  margin: var(--sp-3) var(--sp-2);
}

.nav__heading {
  margin: var(--sp-5) 0 var(--sp-2);
  padding: 0 var(--sp-2);
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--c-text-faint);
  letter-spacing: 0.04em;
}

/* 第一个分组标题不额外加顶部间距：滚动层自身已有 8px 内边距，两组间距会显得头重。 */
.nav__heading:first-child {
  margin-top: 0;
}

/* 导航项右侧徽标（目前只有邮件的未读数）。 */

.nav__badge {
  margin-left: auto;
  flex: none;
  min-width: 18px;
  padding: 0 5px;
  border-radius: var(--r-pill);
  background: var(--c-accent);
  color: var(--c-text-inverse);
  font-size: 10px;
  font-weight: 600;
  line-height: 16px;
  text-align: center;
}

/* 导航项本体与选中态在 app.css 全局定义（用户壳/管理壳共用一份）。 */

.nav__foot {
  /* 横向留出与滚动层相同的内缩，让分隔线保持原来的悬空效果。 */
  margin: 0 var(--sp-3);
  padding: var(--sp-3) var(--sp-2) var(--sp-1);
  border-top: 1px solid var(--c-border);
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

/* 工具行：公告与主题。放在侧栏而不是顶栏——它们与"当前页面"无关。
   仅未登录（账号行不存在）时独立成行；登录后并入账号行。 */
.tools {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  /* 让图标与上面的导航图标对齐。 */
  margin-left: -3px;
}

/* 账号行内的工具组：贴着退出按钮左侧排布。 */
.account__tools {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  margin-left: auto;
}

/* 账号行可点击进入「我的」。悬停底色需要内边距撑开，用等量负外边距抵消，
   否则头像会与上方导航的图标错位。内边距统一 8px。 */
.account {
  display: flex;
  align-items: center;
  /* 行内统一 4px；头像与名称之间需要更宽的一档，由头像自身的边距补足。 */
  gap: var(--sp-1);
  min-width: 0;
  padding: var(--sp-2);
  margin: calc(-1 * var(--sp-2));
  border-radius: var(--r-sm);
  cursor: pointer;
  transition: background-color 0.15s ease, box-shadow 0.15s ease, color 0.15s ease;
}

/* 悬停：底色浮现之外，头像从淡染点亮为实色——给「这一行有去处」一个明确信号。 */
.account:hover {
  background: var(--c-hover);
}

.account:hover .account__avatar {
  background: var(--c-accent);
  color: var(--c-text-inverse);
}

/* 当前页：与导航项同语言的弱强调底色，另加一圈内描边作选中标记。
   不用导航那种左侧竖条——账号行有负边距，竖条会和上面对不齐。
   选中态必须压过 hover（因此提高特异性），否则悬停时选中色会被灰底盖掉。 */
.account.account--active,
.account.account--active:hover {
  background: var(--c-accent-weak);
  color: var(--c-accent);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--c-accent) 45%, transparent);
}

.account--active .account__avatar {
  background: var(--c-accent);
  color: var(--c-text-inverse);
}

.account--active .account__group {
  color: color-mix(in srgb, var(--c-accent) 70%, transparent);
}

.account:focus-visible {
  outline: 2px solid var(--c-accent);
  outline-offset: -2px;
}

.account__avatar {
  width: 30px;
  height: 30px;
  border-radius: 50%;
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: var(--c-accent-weak);
  color: var(--c-accent);
  font-size: var(--fs-sm);
  font-weight: 650;
  margin-right: var(--sp-1);
  transition: background-color 0.15s ease, color 0.15s ease;
}

/* flex:1 把文本行宽度锁定为「头像/工具/退出之外的剩余空间」：
   名字再长也只在这块区域内被 truncate 省略，不会把右侧按钮挤变形。 */
.account__text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  font-size: var(--fs-xs);
  line-height: 1.45;
}

/* 名称与组名的层级：名称加重、组名再小一号，配合颜色差一眼可分。 */
.account__name {
  font-size: var(--fs-xs);
  font-weight: 620;
  line-height: 1.35;
}

.account__group {
  color: var(--c-text-faint);
  /* 10px 刻意低于令牌下限（--fs-xs=12px）：组名要和名称差出完整一档。 */
  font-size: 10px;
}

.account__exit {
  width: 28px;
  height: 28px;
  flex: none;
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--c-text-faint);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
}

/* 悬停只变图标颜色，不再铺灰底：侧栏里这类"安静的图标按钮"太多，
   每个都浮起一块底色会显得躁。 */
.account__exit:hover {
  color: var(--c-danger);
}

/* 存储用量卡片：钉在滚动层末尾（内容不满时贴底），仅用户壳显示。 */
.nav__quota {
  margin-top: auto;
  background: var(--c-surface-2);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  padding: var(--sp-2) var(--sp-3) var(--sp-3);
}

.quota__infinite {
  font-weight: 650;
}

.quota {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.quota__row {
  display: flex;
  justify-content: space-between;
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.quota__text {
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
}
</style>
