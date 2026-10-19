import { fsApi } from "@/api/endpoints";
import type { DeliveryPlan } from "@/api/types";

const SETTLEMENT_OUTBOX_PREFIX = "xph.delivery.settlement-outbox:";
const settlementMemory = new Map<string, PendingSettlement>();
const settlementInFlight = new Map<string, Promise<{ done: boolean; error?: unknown }>>();
let settlementReplayerStarted = false;
let settlementFlush: Promise<void> | null = null;

type PendingSettlement = { ticketId: string; completed: boolean };

function settlementStorage(): Storage | null {
  try {
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null;
  }
}

function storageKey(ticketId: string): string {
  return `${SETTLEMENT_OUTBOX_PREFIX}${encodeURIComponent(ticketId)}`;
}

function pendingSettlements(): PendingSettlement[] {
  const entries = new Map(settlementMemory);
  const storage = settlementStorage();
  if (!storage) return [...entries.values()];
  try {
    // 每张票据独立存储：不同标签页新增/删除不同票据时不会覆盖整份队列快照。
    for (let index = 0; index < storage.length; index += 1) {
      const key = storage.key(index);
      if (!key?.startsWith(SETTLEMENT_OUTBOX_PREFIX)) continue;
      let item: unknown;
      try {
        const raw = storage.getItem(key);
        item = raw ? JSON.parse(raw) : null;
      } catch {
        continue;
      }
      if (typeof item !== "object" || item === null) continue;
      const candidate = item as { ticketId?: unknown; completed?: unknown };
      if (typeof candidate.ticketId !== "string" || !candidate.ticketId ||
          typeof candidate.completed !== "boolean" || key !== storageKey(candidate.ticketId)) continue;
      const old = entries.get(candidate.ticketId);
      entries.set(candidate.ticketId, {
        ticketId: candidate.ticketId,
        completed: !!old?.completed || candidate.completed,
      });
    }
  } catch {
    // 存储不可用或损坏时仍使用本页内存队列，不能阻断交付。
  }
  return [...entries.values()];
}

function persistSettlement(entry: PendingSettlement): void {
  const storage = settlementStorage();
  if (!storage) {
    settlementMemory.set(entry.ticketId, entry);
    return;
  }
  try {
    storage.setItem(storageKey(entry.ticketId), JSON.stringify(entry));
    settlementMemory.delete(entry.ticketId);
  } catch {
    // 私密浏览等环境可能禁用存储；当前页面仍可继续重试。
    settlementMemory.set(entry.ticketId, entry);
  }
}

/** 在提交结算前先持久化客户端结果，页面关闭后可由下次启动重放。 */
function rememberPendingSettlement(plan: DeliveryPlan, completed = false): void {
  if (!plan.ticketId || plan.mode === "proxy_decrypt") return;
  const entries = pendingSettlements();
  const old = entries.find((entry) => entry.ticketId === plan.ticketId);
  const next = { ticketId: plan.ticketId, completed: !!old?.completed || completed };
  persistSettlement(next);
}

function removePendingSettlement(ticketId: string): void {
  settlementMemory.delete(ticketId);
  try {
    settlementStorage()?.removeItem(storageKey(ticketId));
  } catch {
    // 移除失败时保留本地持久记录，后续重放仍是安全的幂等操作。
  }
}

function settleOnce(entry: PendingSettlement): Promise<{ done: boolean; error?: unknown }> {
  const active = settlementInFlight.get(entry.ticketId);
  if (active) return active;
  const request = Promise.resolve().then(async () => {
    try {
      await fsApi.settle(entry.ticketId, entry.completed);
      removePendingSettlement(entry.ticketId);
      return { done: true };
    } catch (error) {
      const apiError = error as { status?: unknown; detail?: unknown } | null;
      const status = apiError?.status;
      const detail = typeof apiError?.detail === "string" ? apiError.detail : "";
      // 仅在服务端明确确认票据已失效/不存在时清理；不能用本机时钟推断服务器过期。
      if (status === 403 && /票据(?:已失效|不存在)/.test(detail)) {
        removePendingSettlement(entry.ticketId);
        return { done: true };
      }
      // 身份不符、认证失效及限流都可能在用户会话/网络恢复后解决。
      if (status === 403 || status === 401 || status === 429 || typeof status !== "number") {
        return { done: false, error };
      }
      if (status >= 400 && status < 500) {
        removePendingSettlement(entry.ticketId);
        console.warn("交付结算被服务端永久拒绝，已停止重放", error);
        return { done: true };
      }
      return { done: false, error };
    } finally {
      settlementInFlight.delete(entry.ticketId);
    }
  });
  settlementInFlight.set(entry.ticketId, request);
  return request;
}

/** 当前页面启动后持续重放持久化结算；在线或重新聚焦时也立即尝试。 */
export function startSettlementReplayer(): void {
  if (settlementReplayerStarted || typeof window === "undefined") return;
  settlementReplayerStarted = true;
  const flush = () => { void flushPendingSettlements(); };
  window.addEventListener("online", flush);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") flush();
  });
  window.setInterval(flush, 30_000);
  flush();
}

/** 供应用启动、网络恢复与 worker 完成通知触发的持久队列重放。 */
export function flushPendingSettlements(): Promise<void> {
  if (settlementFlush) return settlementFlush;
  settlementFlush = (async () => {
    for (const entry of pendingSettlements()) await settleOnce(entry);
  })().finally(() => { settlementFlush = null; });
  return settlementFlush;
}

/** 立即重试数次；失败时保留在 outbox 中，供后续页面会话继续重放。 */
export async function settlePendingDelivery(plan: DeliveryPlan, completed: boolean): Promise<void> {
  if (!plan.ticketId || plan.mode === "proxy_decrypt") return;
  rememberPendingSettlement(plan, completed);
  const current = pendingSettlements().find((entry) => entry.ticketId === plan.ticketId) ?? {
    ticketId: plan.ticketId,
    completed,
  };
  let lastError: unknown;
  const retryDelays = [250, 750];
  for (let attempt = 0; attempt < 3; attempt += 1) {
    const result = await settleOnce(current);
    if (result.done) return;
    lastError = result.error;
    if (attempt < retryDelays.length) {
      await new Promise<void>((resolve) => setTimeout(resolve, retryDelays[attempt]));
    }
  }
  console.warn("交付结算多次失败；已保留待重放记录，后续联网时会重试", lastError);
}
