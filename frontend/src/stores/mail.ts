import { reactive } from "vue";
import { mailApi } from "@/api/endpoints";
import type { MailAddress, MailboxCounters, MyMailDomain } from "@/api/types";
import { describeError } from "@/lib/async";
import { useToasts } from "@/stores/toast";

// 邮件的跨组件状态。
//
// 邮件是普通页面：侧栏只有一个「邮件」入口，收件/星标/归档由页内选项卡切换。
// 未读计数要同时出现在侧栏入口徽标与「收件」选项卡上，邮箱地址弹层也要跨
// 路由保活，因此这些状态统一收在这个模块级单例里（与 site/session 等 store
// 同一风格）。
//
// 本系统只收信：没有发件、草稿与写信预填。

export type MailViewKey = "inbox" | "starred" | "trash";

export const MAIL_VIEWS: Array<{ key: MailViewKey; label: string; icon: string }> = [
  { key: "inbox", label: "收件", icon: "ri-inbox-line" },
  { key: "starred", label: "星标", icon: "ri-star-line" },
  { key: "trash", label: "归档", icon: "ri-delete-bin-line" },
];

interface MailShellState {
  /** 列表接口 counters 回写的计数，供侧栏徽标使用。 */
  unreadInbox: number;

  /** 我的邮箱地址弹层。 */
  addrOpen: boolean;
  addresses: MailAddress[];
  addressesLoaded: boolean;
  /**
   * 本组可用于自助创建地址的域名。只读：域名由管理员按组配置，
   * 用户侧只负责在允许的范围里选，不提供任何编辑入口。
   * 空数组 = 管理员还没配域名，或该域名的收件被关掉了。
   */
  domains: MyMailDomain[];
}

const state = reactive<MailShellState>({
  unreadInbox: 0,
  addrOpen: false,
  addresses: [],
  addressesLoaded: false,
  domains: [],
});

function setCounters(counters: MailboxCounters): void {
  state.unreadInbox = counters.unreadInbox;
}

/** 侧栏徽标：只有收件显未读数。 */
function badgeOf(view: MailViewKey): number {
  if (view === "inbox") return state.unreadInbox;
  return 0;
}

async function openAddresses(): Promise<void> {
  state.addrOpen = true;
  // 域名与地址分两次取：地址是"我已有的"，域名是"我还能建在哪儿"。
  // 后者决定新建表单能不能用，前者决定列表里显示什么。
  //
  // 失败时**关掉弹层**而不是让它空着：弹层的空态文案是"管理员没为当前用户组
  // 分配邮箱域名"，拿接口失败去套这句话，等于把一次 5xx 报成"你没权限"。
  try {
    const domains = await mailApi.listDomains();
    state.domains = domains.items;
    if (!state.addressesLoaded) {
      const res = await mailApi.listAddresses();
      state.addresses = res.items;
      state.addressesLoaded = true;
    }
  } catch (err) {
    state.addrOpen = false;
    useToasts().error("无法加载邮箱地址", describeError(err));
  }
}

function closeAddresses(): void {
  state.addrOpen = false;
}

async function createAddress(localPart: string, domain: string): Promise<void> {
  await mailApi.createAddress(localPart, domain);
  const res = await mailApi.listAddresses();
  state.addresses = res.items;
  state.addressesLoaded = true;
}

export function useMailShell() {
  return {
    state,
    setCounters,
    badgeOf,
    openAddresses,
    closeAddresses,
    createAddress,
  };
}
