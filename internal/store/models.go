package store

import "time"

// ---------------------------------------------------------------- 枚举

// FileStatus 是内容池对象的状态。
type FileStatus int

const (
	// FileUploading 表示对象正在写入 123，尚不可被秒传命中或下载。
	FileUploading FileStatus = 0
	// FileNormal 表示对象可用。
	FileNormal FileStatus = 1
	// FileDisabled 表示对象被拉黑（全局生效，影响所有引用者）。
	FileDisabled FileStatus = 2
	// FileArchive 表示引用计数归零，等待按留存期回收 123 侧对象。
	FileArchive FileStatus = 3
	// FilePurged 表示远端对象已真实删除，仅保留记录供审计与追溯。
	FilePurged FileStatus = 4
	// FileUnknown 是"不适用"哨兵，不落库：文件夹在内容池里没有对应对象，
	// 全站节点列表用它在 LEFT JOIN 的 NULL 之外再表达一层——
	// 否则"文件夹"和"对象状态未知"会在同一个字段上混为一谈。
	FileUnknown FileStatus = -1
)

// String 便于日志与错误信息。
func (s FileStatus) String() string {
	switch s {
	case FileUploading:
		return "uploading"
	case FileNormal:
		return "normal"
	case FileDisabled:
		return "disabled"
	case FileArchive:
		return "archive"
	case FilePurged:
		return "purged"
	default:
		return "unknown"
	}
}

// ParseFileStatus 是 String 的逆运算，返回是否识别。
//
// 与 String 成对提供而不是让调用方各自 switch：状态名会同时出现在接口
// 路径、查询参数和前端下拉里，三处各写一份映射必然漂移。
func ParseFileStatus(s string) (FileStatus, bool) {
	switch s {
	case "uploading":
		return FileUploading, true
	case "normal":
		return FileNormal, true
	case "disabled":
		return FileDisabled, true
	case "archive":
		return FileArchive, true
	case "purged":
		return FilePurged, true
	default:
		return FileUnknown, false
	}
}

// NodeType 区分文件与文件夹，二者互斥。
type NodeType int

// ArchiveState 是归档批次的流转状态。
type ArchiveState int

const (
	// ArchiveStaged 归档暂存：仅标记，引用计数与配额仍被占用，可恢复。
	ArchiveStaged ArchiveState = 1
	// ArchiveDeleted 归档删除：已释放引用与配额，远端对象尚未删除。
	ArchiveDeleted ArchiveState = 2
	// ArchiveStorageDeleted 存储删除：远端对象已真实删除，记录保留。
	ArchiveStorageDeleted ArchiveState = 3
)

// ArchiveBatch 是一次删除操作（一棵子树）在归档里的批次记录。
type ArchiveBatch struct {
	ID          string       `json:"id"`
	UserID      int64        `json:"userId"`
	UserAccount string       `json:"userAccount"`
	RootName    string       `json:"rootName"`
	RootPath    string       `json:"rootPath"`
	NodeType    NodeType     `json:"nodeType"`
	SizeTotal   int64        `json:"sizeTotal"`
	DeletedAt   int64        `json:"deletedAt"`
	State       ArchiveState `json:"state"`
	PurgeAt     int64        `json:"purgeAt"`
	PurgedAt    int64        `json:"purgedAt"`
}

// ArchiveNode 是批次内的单个节点快照，恢复时按它重建节点行。
type ArchiveNode struct {
	OriginalPath string
	Name         string
	NodeType     NodeType
	FileChecksum string // 文件夹为空
	SizePlain    int64
}

const (
	// NodeFile 是文件节点，必须指向内容池。
	NodeFile NodeType = 0
	// NodeFolder 是文件夹节点，不指向内容池。
	NodeFolder NodeType = 1
)

// UserStatus 是账号状态。
type UserStatus int

const (
	// UserDisabled 表示账号被封禁。
	UserDisabled UserStatus = 0
	// UserEnabled 表示账号可用。
	UserEnabled UserStatus = 1
)

// ActorType 区分登录用户与访客。访客按 IP 当"用户"计量。
type ActorType string

const (
	// ActorUser 是登录用户。
	ActorUser ActorType = "user"
	// ActorGuest 是访客。
	ActorGuest ActorType = "guest"
)

// Purpose 区分下载与预览。两者共用同一条准备路径，仅消费端不同。
type Purpose string

const (
	// PurposeDownload 是下载。
	PurposeDownload Purpose = "download"
	// PurposePreview 是预览。
	PurposePreview Purpose = "preview"
)

// AnnouncementKind 是公告的三种形态。
type AnnouncementKind string

const (
	// KindTicker 是顶部滚动公告，全局有且仅有一条。
	KindTicker AnnouncementKind = "ticker"
	// KindAnnouncement 是消息类公告。
	KindAnnouncement AnnouncementKind = "announcement"
	// KindMessage 是普通消息。
	KindMessage AnnouncementKind = "message"
)

// Audience 是公告的接收者范围。
type Audience string

const (
	// AudienceAll 表示全体可见（含访客）。
	AudienceAll Audience = "all"
	// AudienceUsers 表示仅指定用户可见。
	AudienceUsers Audience = "users"
)

// ShareKind 是分享的根节点类型。
type ShareKind string

const (
	// ShareFile 是单文件分享。
	ShareFile ShareKind = "file"
	// ShareFolder 是文件夹分享。
	ShareFolder ShareKind = "folder"
)

// AccessMode 是分享的访问控制方式。
type AccessMode string

const (
	// AccessPublic 表示公开，拿到链接即可访问。
	AccessPublic AccessMode = "public"
	// AccessPassword 表示需要提取码。
	AccessPassword AccessMode = "password"
	// AccessLogin 表示需要登录。
	AccessLogin AccessMode = "login"
	// AccessRestricted 表示仅创建者与其指定对象可见（由上层策略细化）。
	AccessRestricted AccessMode = "restricted"
)

// ---------------------------------------------------------------- 实体

// File 是内容池中的一个密文对象。
type File struct {
	Checksum       string     `json:"checksum"`
	SizePlain      int64      `json:"sizePlain"`
	PanFileID      string     `json:"panFileId"`
	PanObjectName  string     `json:"panObjectName"`
	PanSizeWire    int64      `json:"panSizeWire"`
	EncAlgo        string     `json:"encAlgo"`
	EncChunkLog2   int        `json:"encChunkLog2"`
	EncNoncePrefix int64      `json:"-"`
	EncSalt        []byte     `json:"-"`
	DEKEnvelope    []byte     `json:"-"`
	KEKKeyID       string     `json:"kekKeyId"`
	Status         FileStatus `json:"status"`
	DisableReason  string     `json:"disableReason,omitempty"`
	DisabledBy     int64      `json:"disabledBy,omitempty"`
	DisabledAt     int64      `json:"disabledAt,omitempty"`
	RefCount       int64      `json:"refCount"`
	CreatedBy      int64      `json:"createdBy,omitempty"`
	CreatedAt      int64      `json:"createdAt"`
	UpdatedAt      int64      `json:"updatedAt"`
}

// BlockLog2 返回该对象使用的块大小指数。
func (f File) BlockLog2() byte { return byte(f.EncChunkLog2) }

// Node 是用户逻辑路径树中的一个节点。
type Node struct {
	UserID       int64    `json:"-"`
	LogicalPath  string   `json:"path"`
	NodeType     NodeType `json:"nodeType"`
	FileChecksum string   `json:"checksum,omitempty"`
	Name         string   `json:"name"`
	ParentPath   string   `json:"parentPath"`
	SizePlain    int64    `json:"size"`
	Mtime        int64    `json:"mtime"`
	NodeStatus   int      `json:"nodeStatus"`
	CreatedAt    int64    `json:"createdAt"`
}

// IsFolder 表示该节点是文件夹。
func (n Node) IsFolder() bool { return n.NodeType == NodeFolder }

// User 是账号。
type User struct {
	ID           int64      `json:"id"`
	Account      string     `json:"account"`
	DisplayName  string     `json:"displayName"`
	PasswordHash string     `json:"-"`
	GroupName    string     `json:"groupName"`
	Status       UserStatus `json:"status"`
	LastActionAt int64      `json:"lastActionAt"`
	CreatedAt    int64      `json:"createdAt"`
	UpdatedAt    int64      `json:"updatedAt"`
}

// Session 是一次登录会话。
type Session struct {
	TokenHash  string    `json:"-"`
	UserID     int64     `json:"userId"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	ClientIP   string    `json:"clientIp"`
	UserAgent  string    `json:"userAgent"`
	RevokedAt  int64     `json:"-"`
}

// Expired 判断会话在给定时刻是否已过期或被吊销。
func (s Session) Expired(now time.Time) bool {
	if s.RevokedAt != 0 {
		return true
	}
	return !now.Before(s.ExpiresAt)
}

// Group 是用户组。
type Group struct {
	Name                       string `json:"name"`
	DisplayName                string `json:"displayName"`
	IsBuiltin                  bool   `json:"isBuiltin"`
	Permissions                int64  `json:"permissions"`
	Priority                   int    `json:"priority"`
	ResourceSchedulingPriority int    `json:"resourceSchedulingPriority"`
	CreatedAt                  int64  `json:"createdAt"`
}

// GroupQuota 是组的一条配额维度。
type GroupQuota struct {
	GroupName  string `json:"groupName"`
	QuotaKey   string `json:"quotaKey"`
	LimitValue int64  `json:"limitValue"`
}

// UploadTask 是一次分片上传会话。
type UploadTask struct {
	ID               string    `json:"id"`
	UserID           int64     `json:"userId"`
	Checksum         string    `json:"checksum"`
	ExpectedChecksum string    `json:"expectedChecksum"`
	SizePlain        int64     `json:"sizePlain"`
	Streaming        bool      `json:"streaming"`
	VolumeSize       int64     `json:"volumeSize"`
	ChunkSize        int64     `json:"chunkSize"`
	ChunkTotal       int       `json:"chunkTotal"`
	ReceivedMask     []byte    `json:"-"`
	TargetParentPath string    `json:"targetParentPath"`
	TargetName       string    `json:"targetName"`
	ConflictAction   string    `json:"conflictAction"`
	ExpiresAt        time.Time `json:"expiresAt"`
	CreatedAt        int64     `json:"createdAt"`
	UpdatedAt        int64     `json:"updatedAt"`
}

// UploadJob 是分片上传完成后的后台收尾任务。
type UploadJob struct {
	SessionID     string `json:"sessionId"`
	UserID        int64  `json:"userId"`
	ClientIP      string `json:"clientIp"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
	ResultJSON    string `json:"-"`
	TotalBytes    int64  `json:"totalBytes"`
	ProgressBytes int64  `json:"progressBytes"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// UploadPart 是一个逻辑上传任务已写入的密文物理卷。
type UploadPart struct {
	SessionID   string
	PartNo      int
	ObjectRef   string
	ObjectName  string
	PlainOffset int64
	PlainSize   int64
	WireOffset  int64
	WireSize    int64
	CipherMD5   string
}

// ReceivedBytes 返回已接收字节数（供进度展示）。
func (t UploadTask) ReceivedBytes() int64 {
	var n int64
	for i := 0; i < t.ChunkTotal; i++ {
		if HasBit(t.ReceivedMask, i) {
			n += t.ChunkSize
		}
	}
	if n > t.SizePlain {
		n = t.SizePlain
	}
	return n
}

// Complete 表示所有分片都已接收。
func (t UploadTask) Complete() bool {
	for i := 0; i < t.ChunkTotal; i++ {
		if !HasBit(t.ReceivedMask, i) {
			return false
		}
	}
	return true
}

// TicketDeliveryMode 是票据的交付模式枚举。
type TicketDeliveryMode string

const (
	// TicketDirect 表示直链加密下发：客户端从 123 直链拉密文，本地解密。
	TicketDirect TicketDeliveryMode = "direct"
	// TicketProxy 表示中转加密下发：服务器从 123 拉密文原样中转，客户端本地解密。
	TicketProxy TicketDeliveryMode = "proxy"
	// TicketProxyDecrypt 表示中转解密下发：服务器中转同时解密，客户端拿明文。
	TicketProxyDecrypt TicketDeliveryMode = "proxy_decrypt"
)

// Ticket 是一次下载/预览授权。
type Ticket struct {
	ID           string    `json:"id"`
	FileChecksum string    `json:"checksum"`
	ActorType    ActorType `json:"actorType"`
	UserID       int64     `json:"userId,omitempty"`
	ClientIP     string    `json:"-"`
	IPPrefix     string    `json:"-"`
	GroupName    string    `json:"groupName"`
	Purpose      Purpose   `json:"purpose"`
	// DeliveryMode 是本次交付的通道与字节形态。直链与中转加密下发密文，中转解密下发明文。
	DeliveryMode  TicketDeliveryMode `json:"-"`
	ReservedBytes int64              `json:"-"`
	SettledBytes  int64              `json:"-"`
	UseCount      int                `json:"-"`
	MaxUses       int                `json:"-"`
	ExpiresAt     time.Time          `json:"expiresAt"`
	Revoked       bool               `json:"-"`
	CreatedAt     int64              `json:"createdAt"`
}

// TrafficLog 是一条流量明细。
type TrafficLog struct {
	ID           int64     `json:"id"`
	ActorType    ActorType `json:"actorType"`
	UserID       int64     `json:"userId,omitempty"`
	ClientIP     string    `json:"clientIp,omitempty"`
	GroupName    string    `json:"groupName"`
	Action       string    `json:"action"`
	BytesPlain   int64     `json:"bytesPlain"`
	BytesWire    int64     `json:"bytesWire"`
	ResourcePath string    `json:"resourcePath,omitempty"`
	ShareID      string    `json:"shareId,omitempty"`
	PickupCode   string    `json:"pickupCode,omitempty"`
	OccurredAt   int64     `json:"occurredAt"`
}

// TrafficDaily 是一个主体某一天的流量聚合。
type TrafficDaily struct {
	ActorKey  string `json:"actorKey"`
	Day       string `json:"day"`
	GroupName string `json:"groupName"`
	UpPlain   int64  `json:"upPlain"`
	UpWire    int64  `json:"upWire"`
	DownPlain int64  `json:"downPlain"`
	DownWire  int64  `json:"downWire"`
}

// CounterScope 是配额计数器的命名空间。
type CounterScope string

const (
	// ScopeStorage 是存储占用计数器，key 形如 "user:12"。
	ScopeStorage     CounterScope = "storage"
	ScopeTrafficDown CounterScope = "traffic_down"
	// ScopeMailStorage 是邮件存储占用计数器，与文件配额完全独立。
	ScopeMailStorage CounterScope = "mail_storage"
)

// Announcement 是一条公告或消息。
type Announcement struct {
	ID        string           `json:"id"`
	Kind      AnnouncementKind `json:"kind"`
	Audience  Audience         `json:"audience"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	Enabled   bool             `json:"enabled"`
	Pinned    bool             `json:"pinned"`
	ExpireAt  int64            `json:"expireAt,omitempty"`
	CreatedBy int64            `json:"createdBy,omitempty"`
	CreatedAt int64            `json:"createdAt"`
	UpdatedAt int64            `json:"updatedAt"`
	Targets   []int64          `json:"targets,omitempty"`
}

// InviteCode 是注册邀请码。
type InviteCode struct {
	// ID 是管理端引用的代理主键；码哈希本身不对外暴露，短码的哈希暴露出去
	// 等于给了离线爆破的素材。
	ID        int64  `json:"id"`
	CodeHash  string `json:"-"`
	CodeHint  string `json:"codeHint"`
	GroupName string `json:"groupName"`
	MaxUses   int    `json:"maxUses"`
	UsedCount int    `json:"usedCount"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	Disabled  bool   `json:"disabled"`
	CreatedBy int64  `json:"createdBy,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	Note      string `json:"note,omitempty"`
}

// InviteUse 记录一次邀请码使用。
type InviteUse struct {
	ID          int64  `json:"id"`
	CodeHash    string `json:"-"`
	UserID      int64  `json:"userId"`
	UserAccount string `json:"userAccount"`
	UsedAt      int64  `json:"usedAt"`
	ClientIP    string `json:"clientIp"`
}

// Share 是一次分享。
type Share struct {
	ID            string     `json:"id"`
	OwnerID       int64      `json:"ownerId"`
	RootPath      string     `json:"rootPath"`
	Kind          ShareKind  `json:"kind"`
	AccessMode    AccessMode `json:"accessMode"`
	PwdHash       string     `json:"-"`
	PwdSalt       string     `json:"-"`
	HasPassword   bool       `json:"hasPassword"`
	AllowDownload bool       `json:"allowDownload"`
	AllowPreview  bool       `json:"allowPreview"`
	AllowSubpath  bool       `json:"allowSubpath"`
	ExpiresAt     int64      `json:"expiresAt,omitempty"`
	MaxVisits     int        `json:"maxVisits"`
	Visits        int        `json:"visits"`
	Disabled      bool       `json:"disabled"`
	CreatedAt     int64      `json:"createdAt"`
	// UpdatedAt 是乐观锁版本，每次更新自增。
	UpdatedAt int64 `json:"updatedAt,omitempty"`
}

// PickupCode 是分享的短码别名。
type PickupCode struct {
	Code      string `json:"code"`
	ShareID   string `json:"shareId"`
	MaxUses   int    `json:"maxUses"`
	UsedCount int    `json:"usedCount"`
	// ExpiresAt 不持久化：取件码有效期是管理员统一配置的全局时长，
	// 由业务层按 created_at + 配置值计算后填入，仅供列表与创建响应展示；
	// 0 表示永久。
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	Disabled  bool  `json:"disabled"`
	CreatedBy int64 `json:"createdBy,omitempty"`
	CreatedAt int64 `json:"createdAt"`
}

// AuditLog 是一条管理动作审计。
type AuditLog struct {
	ID         int64  `json:"id"`
	ActorType  string `json:"actorType"`
	ActorID    int64  `json:"actorId"`
	ClientIP   string `json:"clientIp"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	Detail     string `json:"detail"`
	OccurredAt int64  `json:"occurredAt"`
}

// Throttle 是某个键维度的失败退避状态。
type Throttle struct {
	Key        string `json:"key"`
	FailCount  int    `json:"failCount"`
	DelayUntil int64  `json:"delayUntil"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// ConfigEntry 是系统配置表中的一行。
type ConfigEntry struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	ValueType string `json:"valueType"`
	UpdatedAt int64  `json:"updatedAt"`
	UpdatedBy int64  `json:"updatedBy,omitempty"`
}

// ---------------------------------------------------------------- 位图工具

// HasBit 判断位图中第 index 位是否为 1。
func HasBit(mask []byte, index int) bool {
	if index < 0 {
		return false
	}
	byteIdx := index / 8
	if byteIdx >= len(mask) {
		return false
	}
	return mask[byteIdx]&(1<<uint(index%8)) != 0
}

// SetBit 置位，必要时原地扩容后返回新位图。
func SetBit(mask []byte, index int) []byte {
	if index < 0 {
		return mask
	}
	byteIdx := index / 8
	if byteIdx >= len(mask) {
		grown := make([]byte, byteIdx+1)
		copy(grown, mask)
		mask = grown
	}
	mask[byteIdx] |= 1 << uint(index%8)
	return mask
}

// BitmapBytes 返回能容纳 count 个位的字节数。
func BitmapBytes(count int) int {
	if count <= 0 {
		return 1
	}
	return (count + 7) / 8
}
