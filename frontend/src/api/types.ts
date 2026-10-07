// 与 Go 侧 DTO 一一对应的类型定义。
//
// 这里刻意不引入任何生成器：接口面只有几十个结构，而生成器会带来一个必须
// 同步维护的构建步骤——一旦忘了跑，类型就与实际响应静默不一致，比手写更危险。
// 对应关系以 internal/httpapi 的请求结构与 internal/store/models.go 为准。

// ---------------------------------------------------------------- 信封

export interface ApiError {
  /** 面向最终用户的中性文案，可直接展示。 */
  message: string;
  /** 仅用于排障，可能含技术原因，不要直接展示。 */
  detail?: string;
}

export interface ApiEnvelope<T> {
  data?: T;
  error?: ApiError;
}

// ---------------------------------------------------------------- 枚举

type FileStatus = 0 | 1 | 2 | 3;
export const FILE_DISABLED: FileStatus = 2;
/** 引用计数归零、等待按留存期物理删除远端对象。 */
export const FILE_ARCHIVE: FileStatus = 3;

type NodeType = 0 | 1;

type UserStatus = 0 | 1;
type ActorType = "user" | "guest";
export type Purpose = "download" | "preview";
export type AnnouncementKind = "ticker" | "announcement" | "message";
export type Audience = "all" | "users";
export type ShareKind = "file" | "folder";
export type AccessMode = "public" | "password" | "login" | "restricted";
export type ConflictAction = "rename" | "overwrite" | "reject";
type ContentForm = "plaintext" | "ciphertext";
export type RegisterMode = "open" | "invite" | "closed";

/**
 * 系统模式：决定站点是否提供邮件功能。
 *
 * 与后端 settings.SystemMode* 一一对应。文件侧恒常提供，没有对应的模式值；
 * 判断邮件是否可用一律走 mailEnabled，不要在模板里散着比字符串。
 */
export type SystemMode = "both" | "files_only";

/** 权限位。必须与 internal/perm 的 iota 顺序一致：错一位就是越权。 */
export const Perm = {
  Upload: 1 << 0,
  Download: 1 << 1,
  Preview: 1 << 2,
  Share: 1 << 3,
  Pickup: 1 << 4,
  ManageOwnNodes: 1 << 5,
  BypassQuota: 1 << 6,
  AdminFiles: 1 << 7,
  AdminUsers: 1 << 8,
  AdminGroups: 1 << 9,
  AdminAnnouncements: 1 << 10,
  AdminInvites: 1 << 11,
  AdminStorage: 1 << 12,
  AdminSystem: 1 << 13,
  AdminAudit: 1 << 14,
  MailAccess: 1 << 15,
  AdminMail: 1 << 16,
  AdminUnbind: 1 << 17,
} as const;

/** 权限位的展示名，用于用户组编辑界面。 */
export const PERM_LABELS: ReadonlyArray<{ bit: number; label: string; group: string }> = [
  { bit: Perm.Upload, label: "上传", group: "文件" },
  { bit: Perm.Download, label: "下载", group: "文件" },
  { bit: Perm.Preview, label: "在线预览", group: "文件" },
  { bit: Perm.ManageOwnNodes, label: "管理自己的文件与目录", group: "文件" },
  { bit: Perm.Share, label: "创建分享", group: "分享" },
  { bit: Perm.Pickup, label: "创建取件码", group: "分享" },
  { bit: Perm.BypassQuota, label: "不受配额限制", group: "分享" },
  { bit: Perm.AdminFiles, label: "拉黑 / 回收文件对象", group: "管理" },
  { bit: Perm.AdminUsers, label: "管理账号", group: "管理" },
  { bit: Perm.AdminGroups, label: "管理用户组与配额", group: "管理" },
  { bit: Perm.AdminAnnouncements, label: "管理公告与消息", group: "管理" },
  { bit: Perm.AdminInvites, label: "管理邀请码", group: "管理" },
  { bit: Perm.AdminStorage, label: "存储与数据库运维", group: "管理" },
  { bit: Perm.AdminSystem, label: "修改系统配置", group: "管理" },
  { bit: Perm.AdminAudit, label: "查看流量与审计", group: "管理" },
  { bit: Perm.MailAccess, label: "访问邮件（收件箱 / 阅读）", group: "邮件" },
  { bit: Perm.AdminMail, label: "管理收件域名与全局邮件", group: "邮件" },
  { bit: Perm.AdminUnbind, label: "审核邮箱解绑申请", group: "邮件" },
];

export function hasPerm(mask: number, bit: number): boolean {
  return (mask & bit) !== 0;
}

// ---------------------------------------------------------------- 实体

export interface Node {
  path: string;
  nodeType: NodeType;
  checksum?: string;
  name: string;
  parentPath: string;
  size: number;
  mtime: number;
  nodeStatus: number;
  createdAt: number;
}

export interface User {
  id: number;
  account: string;
  displayName: string;
  groupName: string;
  status: UserStatus;
  lastActionAt: number;
  createdAt: number;
  updatedAt: number;
}

export interface Group {
  name: string;
  displayName: string;
  isBuiltin: boolean;
  permissions: number;
  priority: number;
  createdAt: number;
}

interface GroupQuota {
  groupName: string;
  quotaKey: string;
  limitValue: number;
}

/** 列表项。与 store.Node 不同，它额外带上内容池状态，用于标出"已停用"。 */
export interface ListNode {
  path: string;
  name: string;
  nodeType: NodeType;
  isFolder: boolean;
  size: number;
  mtime: number;
  checksum?: string;
  status?: FileStatus;
  disableReason?: string;
}

export interface Announcement {
  id: string;
  kind: AnnouncementKind;
  audience: Audience;
  title: string;
  body: string;
  enabled: boolean;
  pinned: boolean;
  expireAt?: number;
  createdBy?: number;
  createdAt: number;
  updatedAt: number;
  targets?: number[];
}

export interface Share {
  id: string;
  ownerId: number;
  rootPath: string;
  kind: ShareKind;
  accessMode: AccessMode;
  hasPassword: boolean;
  allowDownload: boolean;
  allowPreview: boolean;
  allowSubpath: boolean;
  expiresAt?: number;
  maxVisits: number;
  visits: number;
  disabled: boolean;
  createdAt: number;
  /** 访客视角（/api/s 与 /api/p 的 resolve）才有的脱敏字段：只暴露内容名称 */
  rootName?: string;
  /** 访客视角：当前登录用户是否就是分享创建者（用于受限分享的自访判定） */
  isOwner?: boolean;
}

export interface PickupCode {
  code: string;
  shareId: string;
  maxUses: number;
  usedCount: number;
  expiresAt?: number;
  disabled: boolean;
  createdBy?: number;
  createdAt: number;
}

export interface InviteCode {
  id: number;
  codeHint: string;
  groupName: string;
  maxUses: number;
  usedCount: number;
  expiresAt?: number;
  disabled: boolean;
  createdBy?: number;
  createdAt: number;
  note?: string;
}

export interface InviteUse {
  id: number;
  userId: number;
  userAccount: string;
  usedAt: number;
  clientIp: string;
}

export interface TrafficLog {
  id: number;
  actorType: ActorType;
  userId?: number;
  clientIp?: string;
  groupName: string;
  action: string;
  bytesPlain: number;
  bytesWire: number;
  resourcePath?: string;
  shareId?: string;
  pickupCode?: string;
  occurredAt: number;
}

export interface AuditLog {
  id: number;
  actorType: string;
  actorId: number;
  clientIp: string;
  action: string;
  target: string;
  detail: string;
  occurredAt: number;
}

export interface ConfigEntry {
  key: string;
  value: string;
  valueType: string;
  updatedAt: number;
  updatedBy?: number;
}

// ---------------------------------------------------------------- 身份

export interface SessionPayload {
  token: string;
  user: User;
  group: Group;
  permissions: number;
  expiresAt: number;
}

export interface UploadProfile {
  chunkSize: number;
  maxConcurrency: number;
  maxTasks: number;
}

export interface ProfileResult {
  user: User;
  group: Group;
  permissions: number;
  storageUsed: number;
  storageLimit: number;
  isGuest: boolean;
  actorKey: string;
  upload: UploadProfile;
}

// ---------------------------------------------------------------- 归档

export interface ArchiveBatch {
  id: string;
  userId: number;
  userAccount: string;
  rootName: string;
  rootPath: string;
  /** 0 文件，1 文件夹。 */
  nodeType: number;
  sizeTotal: number;
  deletedAt: number;
  state: number;
  purgeAt: number;
  purgedAt: number;
}

// ---------------------------------------------------------------- 文件树

export interface ListResult {
  path: string;
  items: ListNode[];
}

export interface StatsResult {
  path: string;
  files: number;
  folders: number;
  bytes: number;
}

// ---------------------------------------------------------------- 交付

interface DeliveryMeta {
  chunkLog2: number;
  plainSize: number;
  cipherSize: number;
  noncePrefix: number;
  checksum: string;
  /** RSA-OAEP 信封包裹的内容密钥（base64），本地解开后用于解密。 */
  keyEnvelope: string;
}

/**
 * 交付模式（前后端共同遵守的三态枚举）：
 * - direct：客户端直连 123 直链拉密文，本地解密（直链加密流量）；
 * - proxy：服务器纯反向代理密文，客户端本地解密（中转加密流量）；
 * - proxy_decrypt：服务器中转同时解密，客户端直接收到明文（中转解密流量）。
 */
export type DeliveryMode = "direct" | "proxy" | "proxy_decrypt";

export interface DeliveryPlan {
  purpose: Purpose;
  fileName: string;
  mimeType: string;
  checksum: string;
  /** 本次交付的通道与字节形态，必须以此字段为准。 */
  mode: DeliveryMode;
  /** 直链地址（mode=direct 时有意义）。 */
  url?: string;
  /** 本机中转地址（mode=proxy/proxy_decrypt 时有意义）。 */
  streamUrl?: string;
  /** 本次下发字节的形态：ciphertext 需本地解密，plaintext 可直接消费。 */
  contentForm: ContentForm;
  /** 密钥与文件头材料：仅 ciphertext 形态（direct/proxy）下发。 */
  encryption?: DeliveryMeta;
  ticketId?: string;
  expiresAt: number;
  /** 明文总长度，三种模式都有（明文模式下进度与流式落盘只能靠它）。 */
  plainSize: number;
  reservedBytes: number;
}

/** 分享解析结果。target 描述"要交付哪个用户的哪条路径"。 */
export interface DeliveryTarget {
  ownerUserId: number;
  path: string;
  shareId?: string;
}

// ---------------------------------------------------------------- 上传

export interface InitUploadResult {
  dedup: boolean;
  node?: Node;
  sessionId?: string;
  chunkSize?: number;
  chunkTotal?: number;
  received?: number[];
  expiresAt?: number;
}

export interface UploadProgress {
  sessionId: string;
  chunkSize: number;
  chunkTotal: number;
  received: number[];
  receivedBytes: number;
  expiresAt: number;
}

export interface UploadChunkResult {
  sessionId: string;
  received: number[];
  receivedBytes: number;
}

// ---------------------------------------------------------------- 配置

interface SettingsSection {
  id: string;
  title: string;
}

export interface SettingsEntry {
  key: string;
  section: string;
  title: string;
  help: string;
  kind: "string" | "secret" | "int" | "bool" | "duration" | "size" | "enum" | "csv";
  scope: "hot" | "restart";
  enum?: string[];
  min?: number;
  max?: number;
  placeholder?: string;
  warn?: string;
  secret: boolean;
  /** 展示值；敏感项为掩码，未设置时为空串。 */
  value: string;
  /** 区分"已设置"与"未设置"——掩码后的空串表达不了这个区别。 */
  hasValue: boolean;
  /** true 表示被数据库覆盖，false 表示仍是内置默认值。 */
  overridden: boolean;
  defaultValue?: string;
}

export interface SettingsListResult {
  sections: SettingsSection[];
  items: SettingsEntry[];
}

// ---------------------------------------------------------------- 管理端

/** 概览看板：某一天的流量合计（明文口径）。 */
export interface TrafficPoint {
  day: string;
  upPlain: number;
  downPlain: number;
}

/** 概览看板：某类流量的次数与字节合计。 */
export interface TrafficActionStat {
  action: string;
  count: number;
  bytes: number;
}

/** 概览看板：某个用户组的成员数。 */
export interface GroupStat {
  groupName: string;
  count: number;
}

export interface SharesOverview {
  total: number;
  alive: number;
  visits: number;
  pickups: number;
  views: number;
  downloads: number;
}

export interface MailOverview {
  total: number;
  unread: number;
  starred: number;
  archived: number;
  bytes: number;
  addresses: number;
}

export interface AdminOverview {
  users: number;
  filesByStatus: Record<string, number>;
  storageWire: number;
  tickerActive: boolean;
  /** 近 7 天有过动作的启用账号数。 */
  activeUsers: number;
  usersByGroup: GroupStat[];
  /** 近 30 天逐日流量，已由后端补齐为等长连续序列。 */
  trafficDaily: TrafficPoint[];
  /** 近 7 天按动作分类的流量。 */
  trafficByAction: TrafficActionStat[];
  shares: SharesOverview;
  mail: MailOverview;
  recentAudit: AuditLog[];
}

/**
 * 「某个组在某个收件域名上」的绑定。本系统只收信，没有域名的组收不到信。
 *
 * 收件开关挂在绑定上而不是域名上：域名与组是多对多，同一个域名可以被多个组
 * 同时使用，把开关放在域名上会让管理员在 A 组里暂停收件时静默停掉正在共享
 * 该域名的 B 组。域名行本身只表示"这个域名存在"。
 */
export interface GroupMailDomain {
  domain: string;
  receiveEnabled: boolean;
  /** 该域名下【本组用户】的邮箱地址数（含冻结）；>0 时这一行绑定不可解绑。 */
  addressCount: number;
}

export interface GroupDetail {
  group: Group;
  quotas: GroupQuota[];
  memberCount: number;
  /** 本组托管的全部收件域名。恒为数组，无域名时是空数组。 */
  mailDomains: GroupMailDomain[];
}

/** 全站文件检索的查询条件。零值/空串表示该维度不过滤。 */
export interface AdminNodeQuery {
  /** 匹配文件名或完整逻辑路径。 */
  q?: string;
  /** 属主账号子串。 */
  owner?: string;
  /** "" 不限 / "file" / "folder"。 */
  nodeType?: string;
  /** "" 不限 / uploading / normal / disabled / archive / purged。 */
  state?: string;
  userId?: number;
  limit?: number;
  offset?: number;
}

/** 全站文件检索的一行。 */
export interface AdminNodeItem {
  userId: number;
  account: string;
  groupName: string;
  /** 逻辑路径，斜杠分隔。 */
  path: string;
  name: string;
  nodeType: "file" | "folder";
  sizePlain: number;
  mtime: number;
  /** 仅文件节点有值。 */
  checksum: string;
  /** -1 = 不适用（文件夹在内容池里没有对象）。 */
  fileStatus: number;
  disableReason: string;
  /** 该对象被全站引用的次数；为 0 表示没有任何节点指向它。 */
  refCount: number;
}

/**
 * 文件管理页统计条。节点与对象是**两批不同的东西**：
 * 同一份内容被 3 个人各存一次，是 1 个对象、3 个文件节点。
 * 摆放时必须让人一眼看出这层差别，否则「对象总数」永远对不上表格行数。
 */
export interface AdminNodeStats {
  nodeTotal: number;
  fileNodes: number;
  folderNodes: number;
  /** 内容池对象按入库来源拆分，不提供合计。 */
  fileObjectTotal: number;
  mailObjectTotal: number;
  /** 键与 store.FileStatus.String() 一致：uploading/normal/disabled/archive/purged。 */
  objectsByStatus: Record<string, number>;
}

export interface AdminNodeListResult {
  items: AdminNodeItem[];
  /** 当前筛选下的命中节点数。 */
  total: number;
  limit: number;
  offset: number;
  /** 全站统计，**不受筛选影响**。 */
  stats: AdminNodeStats;
}

export interface StorageStatus {
  kind: string;
  ready: boolean;
  detail: string;
  rootDirId?: string;
  updatedAt?: number;
  credentialsConfigured: boolean;
  presignReady: boolean;
  authCallback: boolean;
  /** 配置中是否已启用 123 直链空间（「勾选即指令」的意图记录）。 */
  directLinkConfigured: boolean;
  /** 配置中是否开启「中转时解密」。 */
  proxyDecrypt: boolean;
}

/** 初始化完成页的服务端探针结果（上传/下载/校验/删除四阶段）。 */
interface StorageProbeResult {
  fileName: string;
  fileId?: string;
  bytes: number;
  uploaded: boolean;
  downloaded: boolean;
  verified: boolean;
  deleted: boolean;
}

/**
 * 管理端自检 start 接口返回的材料。后端只负责上传探针文件与准备通道，
 * 前端自己向直链 URL 或中转 URL 发起请求并验证，从而实测当前下载通道。
 */
export interface StorageProbeSession {
  fileId: string;
  fileName: string;
  /** 按当前配置解析出的交付模式，与真实下载决策一致。 */
  mode: DeliveryMode;
  sizePlain: number;
  sizeWire: number;
  /** 以下交叉校验/解密材料仅 ciphertext 模式（direct/proxy）下发。 */
  blockLog2?: number;
  noncePrefix?: number;
  key?: string;
  /** 明文 SHA-256 摘要（hex）：密文模式解密后比对，明文模式直接比对。 */
  plainSha256: string;
  directUrl?: string;
  streamUrl?: string;
}

/** 前端完成自检后回报给后端的验证结果（仅用于审计与清理）。 */
export interface StorageProbeVerifyResult {
  mode: DeliveryMode;
  ok: boolean;
  detail?: string;
  bytesGot: number;
  latencyMs: number;
}

interface TableStat {
  name: string;
  rows: number;
  payloadBytes: number;
}

interface TrafficStats {
  pendingDetails: number;
  pendingActors: number;
  recorded: number;
  flushes: number;
  dropped: number;
  failedFlushes: number;
}

export interface DatabaseStats {
  path: string;
  pageSize: number;
  pageCount: number;
  freelistCount: number;
  inUseBytes: number;
  freeBytes: number;
  fileBytes: number;
  walBytes: number;
  tables: TableStat[];
  journalMode: string;
  autoVacuum: string;
  foreignKeys: boolean;
  busyTimeoutMs: number;
  traffic: TrafficStats;
  backend: StorageStatus;
}

export interface MaintenanceReport {
  expiredUploads: number;
  expiredTickets: number;
  expiredSessions: number;
  archiveFiles: number;
  purgedTrafficLogs: number;
  purgedAuditLogs: number;
  failures: number;
}

// ---------------------------------------------------------------- 初始化

export interface SetupState {
  initialized: boolean;
  siteName: string;
  encryptionReady: boolean;
  storageConfigured: boolean;
  masterSecretSource: string;
  registerMode: RegisterMode;
  systemMode: SystemMode;
  /**
   * 「启用收件」开关。用户侧邮件门禁要求 systemMode 为 both **且** 它为真
   * （后端见 settings.Runtime.MailAvailable），前端必须拿同一份事实，
   * 否则侧栏会显示一个点进去就是 404 的邮件入口。
   */
  mailReceiveEnabled: boolean;
  minAccountLen: number;
  maxAccountLen: number;
  minPasswordLen: number;
  maxPasswordLen: number;
  dataDir?: string;
  dbPath?: string;
}

interface SetupStorageInput {
  clientId: string;
  clientSecret: string;
  rootDirId: string;
  privateKey: string;
  /** 上游是否已配置把下载请求回源到本程序鉴权（pan123.auth_callback）。 */
  authCallback?: boolean;
  /** 是否在提交初始化时对存放根目录启用直链空间（pan123.direct_link）。 */
  directLink?: boolean;
}

export interface SetupRequest {
  account: string;
  password: string;
  siteName?: string;
  encryptionKey?: string;
  registerMode?: string;
  systemMode?: SystemMode;
  storage?: SetupStorageInput;
  token?: string;
}

export interface SetupResult {
  adminAccount: string;
  siteName: string;
  keyId?: string;
  /** 只在服务端自动生成主密钥时返回，且只返回这一次。 */
  encryptionKey?: string;
  encryptionGenerated: boolean;
  storageConfigured: boolean;
  /** 提交时对存储链路执行的探针结果（与存储页连通性自检同一套）。 */
  storageProbe?: StorageProbeResult;
  storageWarning?: string;
}

/** 需要发到后端校验的引导步骤。 */
export type SetupValidateStep = "token" | "admin" | "encryption" | "storage";

/**
 * 分步校验请求。
 *
 * 引导页点"下一步"时把当前步骤的字段发过去，校验通过才前进：令牌填错、
 * 账号不合规、密钥格式不对都在当场暴露，而不是填完整套流程到最后才知道。
 * 未涉及的字段一律省略。
 */
export interface SetupValidateRequest {
  step: SetupValidateStep;
  token?: string;
  account?: string;
  password?: string;
  encryptionKey?: string;
  storage?: SetupStorageInput;
}

// ---------------------------------------------------------------- 邮件

export type MailboxRole = "inbox";
export type MailboxStatus = "normal" | "archived" | "released";
export type MailPartKind = "body" | "attachment" | "inline";

export interface MailRecipient {
  kind: "to" | "cc";
  name?: string;
  address: string;
  seq: number;
}

export interface MailPart {
  id: number;
  messageId: string;
  seq: number;
  kind: MailPartKind;
  fileName?: string;
  contentType?: string;
  contentId?: string;
  sizePlain: number;
  fileChecksum: string;
}

export interface MailMessage {
  id: string;
  messageId?: string;
  fromName?: string;
  fromAddress: string;
  subject: string;
  sentAt: number;
  createdAt: number;
  threadRoot?: string;
  inReplyTo?: string;
  sizePlain: number;
  attachmentCount: number;
  spfResult?: string;
}

export interface MailboxRow {
  id: number;
  messageId: string;
  role: MailboxRole;
  isRead: boolean;
  isStarred: boolean;
  status: MailboxStatus;
  chargedBytes: number;
  archivedAt?: number;
  purgeAt?: number;
  createdAt: number;
}

export interface MailListItem extends MailboxRow {
  fromAddress: string;
  fromName: string;
  toAddress?: string;
  subject: string;
  snippet?: string;
  sentAt: number;
  attachmentCount: number;
}

export interface MailboxCounters {
  unreadInbox: number;
}

export interface MailListResult {
  items: MailListItem[];
  total: number;
  limit: number;
  offset: number;
  counters: MailboxCounters;
}

export interface MailDetail {
  box: MailboxRow;
  message: MailMessage;
  recipients: MailRecipient[];
  parts: MailPart[];
}

/** 本组可用于自助创建地址的域名（用户端只读，不提供编辑）。 */
export interface MyMailDomain {
  domain: string;
}

export interface MailAddress {
  address: string;
  localPart: string;
  domain: string;
  status: "active" | "frozen";
  createdAt: number;
  updatedAt: number;
}

/** 邮箱地址解绑申请。地址是快照：批准后该地址即被删除，单子仍留存。 */
export interface MailUnbindRequest {
  id: number;
  address: string;
  userId: number;
  account: string;
  status: "pending" | "approved" | "rejected";
  reason: string;
  note: string;
  reviewedBy: number;
  createdAt: number;
  reviewedAt: number;
}

export interface MailUnbindStats {
  pending: number;
  approved: number;
  rejected: number;
  todayPending: number;
}

export interface MailUnbindListResult {
  items: MailUnbindRequest[];
  stats: MailUnbindStats;
}

// ---------------------------------------------------------------- M5 全局邮件管理

export interface AdminMailListItem extends MailListItem {
  ownerAccount: string;
  ownerDisplayName: string;
}

/** 邮件管理页统计条。恒为全站值，不随筛选变化。 */
export interface AdminMailStats {
  total: number;
  unread: number;
  starred: number;
  archived: number;
  released: number;
  /** 至少有一封邮件的用户数。 */
  userCount: number;
}

export interface AdminMailListResult {
  items: AdminMailListItem[];
  /** 当前筛选下的命中数。 */
  total: number;
  limit: number;
  offset: number;
  stats: AdminMailStats;
}

/** 管理视图的归属行比普通 MailboxRow 多属主 ID。 */
export interface AdminMailboxBox extends MailboxRow {
  userId: number;
}

export interface AdminMailboxDetail {
  box: AdminMailboxBox;
  ownerAccount: string;
  ownerDisplayName: string;
  message: MailMessage;
  recipients: MailRecipient[];
  parts: MailPart[];
}

export interface AdminMailQuery {
  userId?: number;
  owner?: string;
  /**
   * 归属类型。本系统只收信，表上的 CHECK 约束写死 role = 'inbox'，
   * 因此界面上不提供这个筛选——一个只有单一取值的下拉框是纯噪音。
   * 字段保留是因为后端仍在接受它。
   */
  role?: string;
  status?: string;
  q?: string;
  limit?: number;
  offset?: number;
}

