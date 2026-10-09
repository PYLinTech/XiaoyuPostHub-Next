import { getToken, request, requestBlob, requestWithUploadProgress } from "./client";
import type {
  AdminMailListResult,
  AdminMailQuery,
  AdminMailboxDetail,
  AdminNodeListResult,
  AdminNodeQuery,
  AdminOverview,
  Announcement,
  AnnouncementKind,
  AuditLog,
  ConfigEntry,
  DatabaseStats,
  DeliveryPlan,
  DeliveryTarget,
  Group,
  GroupDetail,
  InitUploadResult,
  InviteCode,
  InviteUse,
  ListResult,
  ListNode,
  MailAddress,
  MailUnbindRequest,
  MailUnbindListResult,
  MyMailDomain,
  MailDetail,
  MailListResult,
  MaintenanceReport,
  Node,
  PickupCode,
  ArchiveBatch,
  ProfileResult,
  SessionPayload,
  SettingsListResult,
  SetupRequest,
  SetupResult,
  SetupState,
  SetupValidateRequest,
  Share,
  StatsResult,
  StorageProbeSession,
  StorageProbeVerifyResult,
  StorageStatus,
  TrafficLog,
  UploadChunkResult,
  UploadProgress,
  UploadJobStatus,
  User,
} from "./types";

// 按业务域分组，与 internal/httpapi/router.go 的分段保持一致：
// 找接口时不必先猜它属于哪个模块。

// ---------------------------------------------------------------- 初始化

export const setupApi = {
  state: () => request<SetupState>("/api/setup/state", { clearSessionOn401: false }),
  /** 校验单步输入；校验不过时后端返回结构化错误，调用方就地展示。 */
  validate: (body: SetupValidateRequest) =>
    request<{ ok: boolean }>("/api/setup/validate", {
      method: "POST",
      body,
      clearSessionOn401: false,
    }),
  init: (body: SetupRequest) =>
    request<SetupResult>("/api/setup/init", { method: "POST", body, clearSessionOn401: false }),
};

// ---------------------------------------------------------------- 身份

export const authApi = {
  login: (account: string, password: string) =>
    request<SessionPayload>("/api/auth/login", {
      method: "POST",
      body: { account, password },
      clearSessionOn401: false,
    }),
  logout: () => request<{ ok: boolean }>("/api/auth/logout", { method: "POST" }),
  register: (account: string, password: string, inviteCode: string) =>
    request<{ user: User }>("/api/auth/register", {
      method: "POST",
      body: { account, password, inviteCode },
      clearSessionOn401: false,
    }),
  profile: () => request<ProfileResult>("/api/auth/profile"),
  changePassword: (oldPassword: string, newPassword: string) =>
    request<{ ok: boolean }>("/api/auth/password", {
      method: "POST",
      body: { oldPassword, newPassword },
    }),
};

// ---------------------------------------------------------------- 文件树

interface DeliveryRequestInput {
  path: string;
  /** base64 SPKI DER 的临时公钥；提供时内容密钥以 RSA-OAEP 信封下发。 */
  clientPublicKey?: string;
}

export const fsApi = {
  list: (path: string) => request<ListResult>("/api/fs/list", { query: { path } }),
  stat: (path: string) => request<{ node: Node }>("/api/fs/stat", { query: { path } }),
  stats: (path: string) => request<StatsResult>("/api/fs/stats", { query: { path } }),
  mkdir: (parentPath: string, name: string) =>
    request<{ node: Node }>("/api/fs/mkdir", { method: "POST", body: { parentPath, name } }),
  rename: (path: string, newName: string) =>
    request<{ path: string }>("/api/fs/rename", { method: "POST", body: { path, newName } }),
  move: (path: string, destParent: string) =>
    request<{ path: string }>("/api/fs/move", { method: "POST", body: { path, destParent } }),
  remove: (path: string) => request<{ ok: boolean }>("/api/fs/delete", { method: "POST", body: { path } }),
  download: (input: DeliveryRequestInput) =>
    request<DeliveryPlan>("/api/fs/download", { method: "POST", body: input }),
  preview: (input: DeliveryRequestInput) =>
    request<DeliveryPlan>("/api/fs/preview", { method: "POST", body: input }),
  // 直链/中转加密票据按预扣全额结算，客户端上报的字节数不被信任，因此
  // 结算请求只需票据标识；中转解密票据由服务端实测结算，调它会被拒绝。
  settle: (ticketId: string) =>
    request<{ ok: boolean }>("/api/fs/settle", {
      method: "POST",
      body: { ticketId },
    }),
};

// ---------------------------------------------------------------- 上传

export const uploadApi = {
  init: (input: {
    checksum?: string;
    sizePlain: number;
    parentPath: string;
    name: string;
    conflictAction?: string;
  }) => request<InitUploadResult>("/api/upload/init", { method: "POST", body: input }),

  resolve: (sessionId: string, checksum: string, signal?: AbortSignal) =>
    request<{ dedup: boolean; node?: Node | null }>(
      `/api/upload/${encodeURIComponent(sessionId)}/resolve`,
      { method: "POST", body: { checksum }, signal },
    ),

  /** 分片按二进制原样发送：包一层 JSON 会让传输量膨胀三分之一。 */
  chunk: (
    sessionId: string,
    index: number,
    data: Blob,
    signal?: AbortSignal,
    onProgress?: (loaded: number, total: number) => void,
  ) => requestWithUploadProgress<UploadChunkResult>(
    `/api/upload/${encodeURIComponent(sessionId)}/chunk/${index}`,
    data,
    { method: "PUT", signal },
    onProgress,
  ),

  complete: (sessionId: string) =>
    request<UploadJobStatus>(`/api/upload/${encodeURIComponent(sessionId)}/complete`, { method: "POST" }),

  status: (sessionId: string, signal?: AbortSignal) =>
    request<UploadProgress>(`/api/upload/${encodeURIComponent(sessionId)}`, { signal }),

  cancel: (sessionId: string) =>
    request<{ ok: boolean }>(`/api/upload/${encodeURIComponent(sessionId)}/cancel`, { method: "POST" }),
};

// ---------------------------------------------------------------- 公告

export const announcementApi = {
  list: (kinds?: AnnouncementKind[]) =>
    request<{ ticker: Announcement | null; items: Announcement[] }>("/api/announcements", {
      query: { kinds: kinds?.join(",") },
    }),
};

// ---------------------------------------------------------------- 分享管理

interface CreateShareInput {
  path: string;
  kind: "file" | "folder";
  accessMode: "public" | "password" | "login" | "restricted";
  password?: string;
  allowDownload?: boolean;
  allowPreview?: boolean;
  allowSubpath?: boolean;
  expiresAt?: number;
  maxVisits?: number;
}

export interface UpdateShareInput {
  accessMode?: string;
  password?: string;
  allowDownload?: boolean;
  allowPreview?: boolean;
  allowSubpath?: boolean;
  expiresAt?: number;
  maxVisits?: number;
  disabled?: boolean;
}

export const shareApi = {
  create: (input: CreateShareInput) => request<{ share: Share }>("/api/share", { method: "POST", body: input }),
  list: (limit = 100, offset = 0) =>
    request<{ items: Share[] }>("/api/share", { query: { limit, offset } }),
  update: (id: string, input: UpdateShareInput) =>
    request<{ share: Share }>(`/api/share/${encodeURIComponent(id)}`, { method: "PATCH", body: input }),
  remove: (id: string) =>
    request<{ ok: boolean }>(`/api/share/${encodeURIComponent(id)}`, { method: "DELETE" }),
  accesses: (id: string, limit = 100) =>
    request<{ items: AuditLog[] }>(`/api/share/${encodeURIComponent(id)}/accesses`, {
      query: { limit },
    }),
  createPickup: (id: string, maxUses: number) =>
    request<{ pickup: PickupCode }>(`/api/share/${encodeURIComponent(id)}/pickup`, {
      method: "POST",
      body: { maxUses },
    }),
  listPickup: (id: string) =>
    request<{ items: PickupCode[] }>(`/api/share/${encodeURIComponent(id)}/pickup`),
  deletePickup: (code: string) =>
    request<{ ok: boolean }>(`/api/pickup/${encodeURIComponent(code)}`, { method: "DELETE" }),
};

// ---------------------------------------------------------------- 访客入口

interface GuestAccessInput {
  password?: string;
  relPath?: string;
  clientPublicKey?: string;
}

export const guestApi = {
  resolveShare: (id: string, input: GuestAccessInput) =>
    request<{ share: Share; target: DeliveryTarget }>(`/api/s/${encodeURIComponent(id)}/resolve`, {
      method: "POST",
      body: { password: input.password ?? "", relPath: input.relPath ?? "" },
      clearSessionOn401: false,
    }),
  listShare: (id: string, input: GuestAccessInput) =>
    request<{ items: ListNode[] }>(`/api/s/${encodeURIComponent(id)}/list`, {
      method: "POST",
      body: { password: input.password ?? "", relPath: input.relPath ?? "" },
      clearSessionOn401: false,
    }),
  downloadShare: (id: string, input: GuestAccessInput) =>
    request<DeliveryPlan>(`/api/s/${encodeURIComponent(id)}/download`, {
      method: "POST",
      body: {
        password: input.password ?? "",
        relPath: input.relPath ?? "",
        clientPublicKey: input.clientPublicKey ?? "",
      },
      clearSessionOn401: false,
    }),
  previewShare: (id: string, input: GuestAccessInput) =>
    request<DeliveryPlan>(`/api/s/${encodeURIComponent(id)}/preview`, {
      method: "POST",
      body: {
        password: input.password ?? "",
        relPath: input.relPath ?? "",
        clientPublicKey: input.clientPublicKey ?? "",
      },
      clearSessionOn401: false,
    }),
  resolvePickup: (code: string) =>
    request<{ share: Share; target: DeliveryTarget }>(
      `/api/p/${encodeURIComponent(code)}/resolve`,
      { method: "POST", body: {}, clearSessionOn401: false },
    ),
  downloadPickup: (code: string, clientPublicKey?: string) =>
    request<DeliveryPlan>(`/api/p/${encodeURIComponent(code)}/download`, {
      method: "POST",
      body: { clientPublicKey: clientPublicKey ?? "" },
      clearSessionOn401: false,
    }),
};

// ---------------------------------------------------------------- 管理端

export interface TrafficQuery {
  actorType?: string;
  userId?: number;
  clientIp?: string;
  groupName?: string;
  action?: string;
  from?: number;
  to?: number;
  limit?: number;
  offset?: number;
}

export const archiveApi = {
  list: (limit: number, offset: number) =>
    request<{ items: ArchiveBatch[]; total: number }>("/api/archive", { query: { limit, offset } }),
  // 批次 id 进路径前一律编码：它带进来的是外部可见的标识，而同文件其余二十来处
  // 路径参数都编码了。id 里出现 / ? # 就会改变请求的路径语义。
  restore: (id: string) =>
    request<{ restored: string }>(`/api/archive/${encodeURIComponent(id)}/restore`, { method: "POST" }),
  clear: (id: string) =>
    request<{ cleared: string }>(`/api/archive/${encodeURIComponent(id)}/clear`, { method: "POST" }),
  clearAll: () => request<{ cleared: number }>("/api/archive/clear-all", { method: "POST" }),
};

// ---------------------------------------------------------------- 邮件

export interface MailListQuery {
  view?: string;
  unread?: 0 | 1;
  q?: string;
  limit?: number;
  offset?: number;
}

export const mailApi = {
  list: (query: MailListQuery) =>
    request<MailListResult>("/api/mail/messages", { query: { ...query } }),
  detail: (id: string) => request<MailDetail>(`/api/mail/messages/${encodeURIComponent(id)}`),
  proxyExternalResource: (
    messageId: string,
    batchId: string,
    url: string,
    kind: "image" | "style" | "font",
    signal?: AbortSignal,
  ) => requestBlob(
    `/api/mail/messages/${encodeURIComponent(messageId)}/external-resource`,
    { batchId, url, kind },
    signal,
  ),
  star: (id: string, starred: boolean) =>
    request<{ ok: boolean }>(`/api/mail/messages/${encodeURIComponent(id)}/star`, {
      method: "POST",
      body: { starred },
    }),
  archive: (id: string) =>
    request<{ ok: boolean }>(`/api/mail/messages/${encodeURIComponent(id)}/archive`, {
      method: "POST",
    }),
  restore: (id: string) =>
    request<{ ok: boolean }>(`/api/mail/messages/${encodeURIComponent(id)}/restore`, {
      method: "POST",
    }),
  purge: (id: string) =>
    request<{ ok: boolean }>(`/api/mail/messages/${encodeURIComponent(id)}/purge`, {
      method: "POST",
    }),
  listDomains: () => request<{ items: MyMailDomain[] }>("/api/mail/domains"),
  listAddresses: () => request<{ items: MailAddress[] }>("/api/mail/addresses"),
  createAddress: (localPart: string, domain: string) =>
    request<{ address: MailAddress }>("/api/mail/addresses", {
      method: "POST",
      body: { localPart, domain },
    }),
  deliverPart: (partId: number, clientPublicKey: string) =>
    request<DeliveryPlan>(`/api/mail/parts/${partId}/delivery`, {
      method: "POST",
      body: { clientPublicKey },
    }),
  listUnbinds: () => request<{ items: MailUnbindRequest[] }>("/api/mail/unbind-requests"),
  requestUnbind: (address: string, reason: string) =>
    request<MailUnbindRequest>("/api/mail/unbind-requests", {
      method: "POST",
      body: { address, reason },
    }),
  cancelUnbind: (id: number) =>
    request<{ id: number }>(`/api/mail/unbind-requests/${id}`, { method: "DELETE" }),
};

export const adminApi = {
  overview: () => request<AdminOverview>("/api/admin/overview"),

  listUsers: (limit = 100, offset = 0) =>
    request<{ items: User[] }>("/api/admin/users", { query: { limit, offset } }),
  setUserStatus: (id: number, enabled: boolean) =>
    request<{ ok: boolean }>(`/api/admin/users/${id}/status`, { method: "POST", body: { enabled } }),
  setUserGroup: (id: number, groupName: string) =>
    request<{ ok: boolean }>(`/api/admin/users/${id}/group`, { method: "POST", body: { groupName } }),
  resetUserPassword: (id: number, newPassword: string) =>
    request<{ ok: boolean }>(`/api/admin/users/${id}/password`, {
      method: "POST",
      body: { newPassword },
    }),

  listGroups: () => request<{ items: GroupDetail[] }>("/api/admin/groups"),
  saveGroup: (input: {
    name: string;
    displayName: string;
    permissions: number;
    priority: number;
    resourceSchedulingPriority: number;
    quotas: Record<string, number>;
    /**
     * 本次要保存的「组 × 域名」绑定集合，是**整体替换**而不是增量追加。
     * 三态后端严格区分，不能用"空值"合并表达：
     * 不传这个键 = 这次不动域名；[] = 解绑全部；若干项 = 替换成这份列表。
     */
    receiveDomains?: { domain: string; receiveEnabled: boolean }[];
  }) => request<{ group: Group }>("/api/admin/groups", { method: "POST", body: input }),
  deleteGroup: (name: string) =>
    request<{ ok: boolean }>(`/api/admin/groups/${encodeURIComponent(name)}`, { method: "DELETE" }),

  traffic: (query: TrafficQuery) =>
    request<{ items: TrafficLog[]; total: number }>("/api/admin/traffic", { query: { ...query } }),
  audit: (query: {
    action?: string;
    actionPrefix?: string;
    from?: number;
    to?: number;
    limit?: number;
    offset?: number;
  }) => request<{ items: AuditLog[]; total: number }>("/api/admin/audit", { query: query }),

  listAnnouncements: (kind: string, limit = 100, offset = 0) =>
    request<{ items: Announcement[] }>("/api/admin/announcements", {
      query: { kind, limit, offset },
    }),
  saveAnnouncement: (input: {
    id?: string;
    kind: string;
    audience: string;
    title: string;
    body: string;
    enabled: boolean;
    pinned: boolean;
    expireAt?: number;
    targets?: number[];
  }) =>
    request<{ announcement: Announcement }>("/api/admin/announcements", {
      method: "POST",
      body: input,
    }),
  deleteAnnouncement: (id: string) =>
    request<{ ok: boolean }>(`/api/admin/announcements/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),

  listInvites: (limit = 100, offset = 0) =>
    request<{ items: InviteCode[] }>("/api/admin/invites", { query: { limit, offset } }),
  createInvite: (input: { groupName: string; maxUses: number; expiresAt: number; note: string }) =>
    request<{ invite: InviteCode; code: string }>("/api/admin/invites", {
      method: "POST",
      body: input,
    }),
  setInviteStatus: (id: number, enabled: boolean) =>
    request<{ ok: boolean }>(`/api/admin/invites/${id}/status`, { method: "POST", body: { enabled } }),
  listInviteUses: (id: number, limit = 100) =>
    request<{ items: InviteUse[] }>(`/api/admin/invites/${id}/uses`, { query: { limit } }),
  deleteInvite: (id: number) =>
    request<{ ok: boolean }>(`/api/admin/invites/${id}`, { method: "DELETE" }),

  archiveList: (state: number, limit: number, offset: number) =>
    request<{ items: ArchiveBatch[]; total: number }>("/api/admin/archive", {
      query: { state: state || undefined, limit, offset },
    }),
  archivePurge: (ids: string[]) =>
    request<{ purged: number }>("/api/admin/archive/purge", { method: "POST", body: { ids } }),
  // signal：这一页的检索是 `LIKE '%x%'` 全站扫描，是所有列表里服务端开销最大
  // 的一个。管理员连按查询时，把已经过时的请求真正取消掉，省下的不是几十毫秒
  // 而是一整次扫描。其它列表是带索引的等值查询，不值得为此改签名。
  listNodes: (query: AdminNodeQuery, signal?: AbortSignal) =>
    request<AdminNodeListResult>("/api/admin/files", { query: { ...query }, signal }),
  setFileStatus: (checksum: string, disabled: boolean, reason: string) =>
    request<{ ok: boolean }>(`/api/admin/files/${encodeURIComponent(checksum)}/status`, {
      method: "POST",
      body: { disabled, reason },
    }),

  listSettings: () => request<SettingsListResult>("/api/admin/settings"),
  updateSettings: (values: Record<string, string>) =>
    request<{ updated: string[] }>("/api/admin/settings", { method: "PUT", body: { values } }),
  resetSetting: (key: string) =>
    request<{ reset: string }>("/api/admin/settings", { method: "DELETE", query: { key } }),
  exportSettings: () =>
    request<{ values: Record<string, string> }>("/api/admin/settings/export"),
  importSettings: (values: Record<string, string>, overwrite: boolean) =>
    request<{ applied: number }>("/api/admin/settings/import", {
      method: "POST",
      body: { values, overwrite },
    }),

  storageStatus: () => request<StorageStatus>("/api/admin/storage"),
  // 连通性自检由前端主导：start 上传探针并返回通道材料，前端自行下载验证，
  // 最后 finish 上报结果并触发清理。
  storageProbeStart: () =>
    request<StorageProbeSession>("/api/admin/storage/probe/start", { method: "POST" }),
  storageProbeFinish: (fileId: string, result: StorageProbeVerifyResult) =>
    request<{ ok: boolean }>("/api/admin/storage/probe/finish", {
      method: "POST",
      body: { fileId, result },
    }),

  databaseStats: () => request<DatabaseStats>("/api/admin/database"),
  databaseOptimize: () => request<{ steps: Record<string, string> }>("/api/admin/database/optimize", {
    method: "POST",
  }),
  databaseCheckpoint: (truncate: boolean) =>
    request<{ ok: boolean }>("/api/admin/database/checkpoint", { method: "POST", body: { truncate } }),
  databaseIntegrity: (quick: boolean) =>
    request<{ healthy: boolean; results: string[] | null }>("/api/admin/database/integrity", {
      method: "POST",
      body: { quick },
    }),
  databaseBackup: () => request<Record<string, unknown>>("/api/admin/database/backup", { method: "POST" }),
  runMaintenance: () => request<MaintenanceReport>("/api/admin/maintenance", { method: "POST" }),

  // ---- 全局邮件管理（收件域名本身在用户组里维护） ----
  listAllMail: (query: AdminMailQuery) =>
    request<AdminMailListResult>("/api/admin/mail/messages", { query: { ...query } }),
  adminMailboxDetail: (id: number) =>
    request<AdminMailboxDetail>(`/api/admin/mail/mailboxes/${id}`),
  adminArchiveMail: (id: number) =>
    request<{ ok: boolean }>(`/api/admin/mail/mailboxes/${id}/archive`, { method: "POST" }),
  adminRestoreMail: (id: number) =>
    request<{ ok: boolean }>(`/api/admin/mail/mailboxes/${id}/restore`, { method: "POST" }),
  adminPurgeMail: (id: number) =>
    request<{ ok: boolean }>(`/api/admin/mail/mailboxes/${id}/purge`, { method: "POST" }),

  // ---- 邮箱解绑审核（权限位 AdminUnbind）----
  listUnbinds: (query: { status?: string; address?: string; search?: string } = {}) =>
    request<MailUnbindListResult>("/api/admin/unbind-requests", { query: { ...query } }),
  approveUnbind: (id: number, note = "") =>
    request<MailUnbindRequest>(`/api/admin/unbind-requests/${id}/approve`, {
      method: "POST",
      body: { note },
    }),
  rejectUnbind: (id: number, note = "") =>
    request<MailUnbindRequest>(`/api/admin/unbind-requests/${id}/reject`, {
      method: "POST",
      body: { note },
    }),
};

// ---------------------------------------------------------------- 直链取密文

/**
 * 从密文直链读取一段字节。
 *
 * 直链是跨域的，因此只能带 Range 一个头：任何自定义头都会触发预检并被拒。
 * 也因此不能复用 request()——它会带上 Authorization。
 */
export async function fetchCipherRange(
  url: string,
  start: number,
  endInclusive: number,
  signal?: AbortSignal,
): Promise<Uint8Array> {
  const response = await fetch(url, {
    headers: { Range: `bytes=${start}-${endInclusive}` },
    signal,
  });
  if (!response.ok && response.status !== 206) {
    throw new Error(`取密文失败：HTTP ${response.status}`);
  }
  const buffer = await response.arrayBuffer();
  const expected = endInclusive - start + 1;
  // 响应头里的 Content-Length 在跨域下未必可读，因此用"请求区间 + 实际字节数"自校验。
  if (buffer.byteLength !== expected) {
    throw new Error(`密文长度不符：期望 ${expected}，实际 ${buffer.byteLength}`);
  }
  return new Uint8Array(buffer);
}

/** 构造带令牌的中转流地址。令牌必须走查询串：媒体元素无法自定义请求头。 */
export function streamUrlWithToken(streamUrl: string): string {
  const token = getToken();
  if (!token) {
    return streamUrl;
  }
  const sep = streamUrl.includes("?") ? "&" : "?";
  return `${streamUrl}${sep}token=${encodeURIComponent(token)}`;
}

/** 供配置导出使用：把后端返回的裸值转成下载用的 Blob。 */
export function valuesToJsonBlob(values: Record<string, string>): Blob {
  return new Blob([JSON.stringify({ values }, null, 2)], { type: "application/json" });
}

export type { ConfigEntry };
