import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";
import { Perm } from "@/api/types";
import { bootstrap, useSession } from "@/stores/session";
import { useToasts } from "@/stores/toast";

// 路由表。
//
// 权限要求写在路由的 meta 上而不是散在页面里：这样"哪些页面需要什么权限"
// 在一条清单上就能看全，页面组件只管渲染自己的内容。

declare module "vue-router" {
  interface RouteMeta {
    /** 需要已登录。 */
    auth?: boolean;
    /** 需要的权限位。 */
    perm?: number;
    /** 站点未初始化时也允许访问。 */
    allowUninitialized?: boolean;
    title?: string;
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: "/setup",
    name: "setup",
    component: () => import("@/views/SetupView.vue"),
    meta: { title: "初始化", allowUninitialized: true },
  },
  {
    path: "/login",
    name: "login",
    component: () => import("@/views/LoginView.vue"),
    meta: { title: "登录" },
  },
  {
    path: "/register",
    name: "register",
    component: () => import("@/views/RegisterView.vue"),
    meta: { title: "注册" },
  },
  { path: "/", redirect: "/files" },
  {
    path: "/files/:path(.*)*",
    name: "files",
    component: () => import("@/views/FilesView.vue"),
    meta: { auth: true, perm: Perm.Download, title: "文件" },
  },
  {
    path: "/shares",
    name: "shares",
    component: () => import("@/views/SharesView.vue"),
    meta: { auth: true, title: "我的分享" },
  },
  {
    path: "/pickup",
    name: "pickup",
    component: () => import("@/views/PickupView.vue"),
    meta: { title: "取件码" },
  },
  {
    path: "/archive",
    name: "archive",
    component: () => import("@/views/ArchiveView.vue"),
    meta: { auth: true, title: "归档" },
  },
  // 邮件是普通页面，不再是二级壳：与「文件」同级，视图切换靠页内选项卡。
  // :view 仍然进地址，刷新与前进/后退都能回到原处。
  { path: "/mail", redirect: "/mail/inbox" },
  {
    path: "/mail/:view(inbox|starred|trash)",
    name: "mail-list",
    component: () => import("@/views/mail/MailListPage.vue"),
    meta: { auth: true, perm: Perm.MailAccess, title: "邮件" },
  },
  {
    path: "/mail/:view(inbox|starred|trash)/:id",
    name: "mail-message",
    component: () => import("@/views/mail/MailMessagePage.vue"),
    meta: { auth: true, perm: Perm.MailAccess, title: "邮件" },
  },
  {
    path: "/s/:id",
    name: "share",
    component: () => import("@/views/ShareView.vue"),
    meta: { title: "分享" },
  },
  {
    path: "/me",
    name: "me",
    component: () => import("@/views/MeView.vue"),
    meta: { auth: true, title: "我的" },
  },
  {
    path: "/admin",
    component: () => import("@/views/admin/AdminLayout.vue"),
    meta: { auth: true, title: "管理" },
    children: [
      { path: "", redirect: "/admin/overview" },
      {
        path: "overview",
        name: "admin-overview",
        component: () => import("@/views/admin/OverviewView.vue"),
        meta: { auth: true, perm: Perm.AdminAudit, title: "概览" },
      },
      {
        path: "users",
        name: "admin-users",
        component: () => import("@/views/admin/UsersView.vue"),
        meta: { auth: true, perm: Perm.AdminUsers, title: "账号" },
      },
      {
        path: "groups",
        name: "admin-groups",
        component: () => import("@/views/admin/GroupsView.vue"),
        meta: { auth: true, perm: Perm.AdminGroups, title: "用户组" },
      },
      {
        path: "announcements",
        name: "admin-announcements",
        component: () => import("@/views/admin/AnnouncementsView.vue"),
        meta: { auth: true, perm: Perm.AdminAnnouncements, title: "公告" },
      },
      {
        path: "invites",
        name: "admin-invites",
        component: () => import("@/views/admin/InvitesView.vue"),
        meta: { auth: true, perm: Perm.AdminInvites, title: "邀请码" },
      },
      {
        path: "files",
        name: "admin-files",
        component: () => import("@/views/admin/FilesAdminView.vue"),
        meta: { auth: true, perm: Perm.AdminFiles, title: "文件管理" },
      },
      {
        path: "archive",
        name: "admin-archive",
        component: () => import("@/views/admin/AdminArchiveView.vue"),
        meta: { auth: true, perm: Perm.AdminFiles, title: "归档管理" },
      },
      {
        path: "traffic",
        name: "admin-traffic",
        component: () => import("@/views/admin/TrafficView.vue"),
        meta: { auth: true, perm: Perm.AdminAudit, title: "流量" },
      },
      {
        path: "audit",
        name: "admin-audit",
        component: () => import("@/views/admin/AuditView.vue"),
        meta: { auth: true, perm: Perm.AdminAudit, title: "审计" },
      },
      {
        path: "settings",
        name: "admin-settings",
        component: () => import("@/views/admin/SettingsView.vue"),
        meta: { auth: true, perm: Perm.AdminSystem, title: "系统配置" },
      },
      {
        path: "storage",
        name: "admin-storage",
        component: () => import("@/views/admin/StorageView.vue"),
        meta: { auth: true, perm: Perm.AdminStorage, title: "存储" },
      },
      {
        path: "database",
        name: "admin-database",
        component: () => import("@/views/admin/DatabaseView.vue"),
        meta: { auth: true, perm: Perm.AdminStorage, title: "数据库" },
      },
      // 旧路径重定向：收件域名曾经独占一页，现在并进用户组；这一页从「运营」
      // 挪到了「内容」并改名为「邮件管理」。旧书签都不应该因此 404。
      { path: "mail-domains", redirect: { name: "admin-groups" } },
      { path: "mail-global", redirect: { name: "admin-mail" } },
      {
        path: "mail",
        name: "admin-mail",
        component: () => import("@/views/admin/AdminMailMessagesView.vue"),
        meta: { auth: true, perm: Perm.AdminMail, title: "邮件管理" },
      },
    ],
  },
  {
    path: "/:pathMatch(.*)*",
    name: "not-found",
    component: () => import("@/views/NotFoundView.vue"),
    meta: { allowUninitialized: true, title: "未找到" },
  },
];

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior(_to, _from, saved) {
    return saved ?? { top: 0 };
  },
});

let bootstrapped = false;

router.beforeEach(async (to) => {
  // 首次进入任何页面前先完成探活：未初始化的实例必须被送去引导页，
  // 而"是否已初始化"只有一次请求之后才知道。
  if (!bootstrapped) {
    bootstrapped = true;
    await bootstrap();
  }

  const session = useSession();

  if (session.state.setup && !session.state.setup.initialized && !to.meta.allowUninitialized) {
    return { name: "setup" };
  }

  if (to.meta.auth && !session.state.authenticated) {
    return { name: "login", query: { next: to.fullPath } };
  }

  // 系统模式闸门：邮件侧关闭时挡住全部邮件页面，落到一个能去的地方，
  // 而不是让守卫把用户丢到 NotFound——那是"地址写错了"的语义，
  // 与"这个站点没开邮件功能"不是一回事。
  //
  // **两个前缀都要挡**。侧栏的 blockedByMode 已经同时过滤 /mail 与
  // /admin/mail，但守卫原先只写了 /mail：管理员手敲 /admin/mail 照样进得去，
  // 落在一个 403 错误页上——入口藏了、地址却还能走，正好是"拒绝手动访问"
  // 最该堵的那种缝。服务端 mailOnly 会兜底返回 403，但那让用户看到的是
  // 接口错误，而不是一句"这个站点没开邮件功能"。
  //
  // 文件侧恒常提供，不设对称的守卫：那是刻意的，不是遗漏。
  if (!session.mailEnabled.value) {
    if (to.path.startsWith("/mail")) {
      useToasts().error("当前站点未启用邮件功能");
      return { name: "files" };
    }
    if (to.path.startsWith("/admin/mail")) {
      // 落点得是当前用户**进得去**的管理页。写死 admin-overview 的话，
      // 只有 AdminGroups 权限的管理员会在这里吃一条错误提示、被弹到概览、
      // 概览的权限守卫再弹一次，最后落到文件页——两次跳转两条提示。
      useToasts().error("当前站点未启用邮件功能");
      return { path: firstReachableAdmin() };
    }
  }

  if (to.meta.perm && !session.can(to.meta.perm)) {
    useToasts().error("没有权限访问该页面");
    return { name: session.state.authenticated ? "files" : "login" };
  }

  return true;
});

/**
 * 第一个当前用户进得去的管理页。
 *
 * 直接从路由表推，而不是另抄一份"管理页 + 权限位"的清单：那份清单一旦和
 * 路由表不同步，守卫就会把人弹到一个他没权限的页面上。
 * 侧栏的落点（AppSideNav 的 adminEntry）用的是同一套口径。
 */
function firstReachableAdmin(): string {
  const session = useSession();
  const admin = routes.find((route) => route.path === "/admin");
  for (const child of admin?.children ?? []) {
    if (typeof child.path !== "string" || !child.path) {
      continue;
    }
    const perm = child.meta?.perm;
    if (!perm || session.can(perm)) {
      return `/admin/${child.path}`;
    }
  }
  return "/files";
}

/**
 * 应用文档标题。
 *
 * 单独抽出来是因为它不只发生在路由切换时：站点名可以在管理端被改，
 * 改完之后标签页标题应当立刻跟上，而不是等用户下一次导航。
 */
export function applyDocumentTitle(metaTitle?: string): void {
  const siteName = useSession().state.siteName || "XiaoyuPostHub-Next";
  document.title = metaTitle ? `${metaTitle} · ${siteName}` : siteName;
}

router.afterEach((to) => {
  applyDocumentTitle(to.meta.title);
});
