import { computed, reactive } from "vue";
import { ApiError, getToken, setToken, setUnauthorizedHandler } from "@/api/client";
import { authApi, setupApi } from "@/api/endpoints";
import {
  Perm,
  hasPerm,
  type Group,
  type ProfileResult,
  type SetupState,
  type User,
  type UploadProfile,
} from "@/api/types";

// 会话与站点初始化状态。
//
// 身份、权限、配额都集中在这里：界面上"显示什么、能点什么"全部由它推导，
// 而不是每个视图各自记一份权限判断——那样一定会出现某处漏判，
// 表现为"按钮在，点了报 403"。

interface SessionState {
  /** 首次探活是否完成。未完成前不要渲染依赖身份的界面。 */
  ready: boolean;
  authenticated: boolean;
  user: User | null;
  group: Group | null;
  permissions: number;
  storageUsed: number;
  storageLimit: number;
  actorKey: string;
  /** 站点初始化状态；为空表示尚未取到。 */
  setup: SetupState | null;
  siteName: string;
  // 系统模式、启用收件、加密就绪、存储就绪一律只保留在 setup 这一个来源里。
  // 原来这里各存了一份镜像：镜像只在两处赋值，全站没有任何第二个读取方
  // （派生量读的是 state.setup.*），于是同一份事实有两个真相源，且其中
  // 一个永远不会更新——下次谁照着"有独立字段"的错觉去读它，就会拿到旧值。
  /** 前端上传编排参数（分片大小与两级并发），来自后端设置。 */
  upload: UploadProfile;
}

const state = reactive<SessionState>({
  ready: false,
  authenticated: false,
  user: null,
  group: null,
  permissions: 0,
  storageUsed: 0,
  storageLimit: 0,
  actorKey: "",
  setup: null,
  siteName: "XiaoyuPostHub-Next",
  upload: { chunkSize: 8 << 20, maxConcurrency: 3, maxTasks: 2 },
});

/** 探活与登录状态变化后，需要重跑的副作用（例如加载公告）。 */
const listeners = new Set<() => void>();

function notify(): void {
  for (const listener of listeners) {
    listener();
  }
}

export function onSessionChange(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function applyProfile(profile: ProfileResult): void {
  state.authenticated = !profile.isGuest && !!profile.user;
  state.user = profile.isGuest ? null : profile.user;
  state.group = profile.group;
  state.permissions = profile.permissions;
  state.storageUsed = profile.storageUsed;
  state.storageLimit = profile.storageLimit;
  state.actorKey = profile.actorKey;
  if (profile.upload) {
    state.upload = profile.upload;
  }
}

export function clearSession(): void {
  setToken("");
  state.authenticated = false;
  state.user = null;
  state.group = null;
  state.permissions = 0;
  state.storageUsed = 0;
  state.storageLimit = 0;
  notify();
}

/**
 * 首次探活。
 *
 * 顺序是"先问初始化状态，再问身份"：未初始化的实例上没有任何账号，
 * 此时必须把用户送去引导页，而不是让他先看到一个必然失败的登录框。
 */
export async function bootstrap(): Promise<void> {
  try {
    const setup = await setupApi.state();
    state.setup = setup;
    state.siteName = setup.siteName;
  } catch (err) {
    // 探活失败不让界面停在白屏：把错误留给后续请求去暴露。
    state.setup = null;
    void err;
  }

  // 无论是否已登录都要拉一次 profile：未登录时它返回**访客组**的权限位，
  // 而界面要据此决定显示哪些入口。只在有令牌时才拉的话，访客的 permissions
  // 恒为 0，session.can() 会对所有人返回 false——表现是"访客看不到任何按钮"，
  // 而不是"没有权限"。
  try {
    await refreshProfile();
  } catch {
    if (getToken()) {
      clearSession();
    }
  }
  state.ready = true;
  notify();
}

export async function refreshProfile(): Promise<void> {
  const profile = await authApi.profile();
  applyProfile(profile);
  // 站点名以初始化状态为准：它同时服务于未登录的页头。
  if (state.setup) {
    state.siteName = state.setup.siteName;
  }
  notify();
}

export async function login(account: string, password: string): Promise<void> {
  const session = await authApi.login(account, password);
  setToken(session.token);
  applyProfile({
    user: session.user,
    group: session.group,
    permissions: session.permissions,
    storageUsed: 0,
    storageLimit: 0,
    isGuest: false,
    actorKey: `user:${session.user.id}`,
    // 上传参数以随后的 profile 刷新为准，这里先放安全的占位值。
    upload: state.upload,
  });
  await refreshProfile();
}

export async function logout(): Promise<void> {
  try {
    await authApi.logout();
  } catch {
    // 服务端可能已经吊销了会话；本地状态照清。
  }
  clearSession();
}

export async function register(
  account: string,
  password: string,
  inviteCode: string,
): Promise<void> {
  await authApi.register(account, password, inviteCode);
}

export async function changePassword(oldPassword: string, newPassword: string): Promise<void> {
  await authApi.changePassword(oldPassword, newPassword);
}

/** 重新拉取站点公开信息（改过站点名或配置后调用）。 */
export async function reloadSiteInfo(): Promise<void> {
  try {
    state.setup = await setupApi.state();
    state.siteName = state.setup.siteName;
    // 邮件门禁的两个条件都在 setup 里，整个对象换掉即��重新取值：
    // 管理员在系统配置里改完设置回来，侧栏的邮件入口会立刻按新状态出现或消失，
    // 不必等重新登录或刷新页面。
    notify();
  } catch (err) {
    if (err instanceof ApiError) {
      // 无权限或后端不可用时保持旧值。
    }
  }
}

/**
 * 管理端改动之后刷新会话侧的快照。
 *
 * 站点名、注册模式、访客开关、以及"自己所在组的权限"都可能被刚刚的改动影响，
 * 而它们都缓存在会话里。不刷新的话，改完站点名后侧栏仍显示旧名字，直到用户
 * 手动刷新页面——这类"改了没生效"的观感问题最容易让人怀疑功能没实现。
 *
 * 两个刷新都容忍失败：它们只是让界面更准，失败不该让"保存成功"这个结果变样。
 */
export async function refreshAfterAdminChange(): Promise<void> {
  await Promise.all([
    reloadSiteInfo().catch(() => undefined),
    refreshProfile().catch(() => undefined),
  ]);
}

/** 安装 401 的全局处理：会话失效时清本地状态，界面据此跳回登录页。 */
export function installUnauthorizedHandler(): void {
  setUnauthorizedHandler(() => {
    if (state.authenticated) {
      state.authenticated = false;
      state.user = null;
      state.group = null;
      state.permissions = 0;
      setToken("");
      notify();
    }
  });
}

export function useSession() {
  return {
    state,
    isAuthenticated: computed(() => state.authenticated),
    /** 访客也会拿到访客组的权限位，因此这个判断对未登录用户同样有意义。 */
    can: (bit: number) => hasPerm(state.permissions, bit),
    isAdmin: computed(() => hasPerm(state.permissions, Perm.AdminAudit) || hasPerm(state.permissions, Perm.AdminUsers)),
    /**
     * 用户侧邮件是否可用——必须与后端 Runtime.MailAvailable 同口径：
     * 模式为「文件与邮件」**且**开启了收件。
     *
     * 只判模式是不够的：模式开着但收件关闭时没有任何来信，用户侧页面只是
     * 空壳，后端一律 404。前端若仍显示入口，用户点进去只会撞见一个错误页。
     *
     * 探活没回来时（setup 为空）一律当不可用：宁可先不显示入口——后端也会
     * 兜住，顶多是少一个入口；反过来则可能显示一个点进去就是 404 的入口。
     */
    mailEnabled: computed(
      () => state.setup?.systemMode === "both" && state.setup.mailReceiveEnabled,
    ),
    refreshProfile,
    login,
    logout,
    register,
    changePassword,
    reloadSiteInfo,
  };
}
