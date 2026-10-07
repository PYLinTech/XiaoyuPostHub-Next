import { reactive } from "vue";
import { mailApi } from "@/api/endpoints";
import type { MailAddress, MailboxCounters, MailUnbindRequest, MyMailDomain } from "@/api/types";
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
    /**
     * 我提交过的**待审**解绑申请，按地址索引。弹层用它决定每个地址右侧
     * 显示"申请解绑"还是"待审核 / 撤销"——不必为此给每个地址单开一个查询。
     */
    unbinds: Record<string, MailUnbindRequest>;
  }

const state = reactive<MailShellState>({
  unreadInbox: 0,
  addrOpen: false,
  addresses: [],
    addressesLoaded: false,
    domains: [],
    unbinds: {},
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
      const unb = await mailApi.listUnbinds();
      state.unbinds = indexPendingUnbinds(unb.items);
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

/**
 * 提交解绑申请。
 *
 * 申请不等于删除：地址一旦消失，外部发信人立刻收到退信，而这个代价由不在场
 * 的人承担。所以后端只落一条待审单，真正生效要等管理员点头。
 */
async function requestUnbind(address: string, reason: string): Promise<void> {
  const created = await mailApi.requestUnbind(address, reason);
  state.unbinds[address] = created;
}

/** 撤销自己的待审申请。撤销后可再次申请。 */
async function cancelUnbind(address: string): Promise<void> {
  const pending = state.unbinds[address];
  if (!pending) return;
  await mailApi.cancelUnbind(pending.id);
  delete state.unbinds[address];
}

/** indexPendingUnbinds 只保留待审的：已处理的单子对按钮状态没有意义。 */
function indexPendingUnbinds(items: MailUnbindRequest[]): Record<string, MailUnbindRequest> {
  const out: Record<string, MailUnbindRequest> = {};
  for (const it of items) {
    if (it.status === "pending") out[it.address] = it;
  }
  return out;
}

export function useMailShell() {
  return {
    state,
    setCounters,
    badgeOf,
    openAddresses,
    closeAddresses,
    createAddress,
    requestUnbind,
    cancelUnbind,
  };
}
