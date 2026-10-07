// Package perm 定义权限位与预设用户组。
//
// 权限用整数位掩码而不是逗号串：判定是位运算，可以直接塞进会话令牌，也少一层
// 字符串解析。位掩码约束在 63 位内，动态权限留在 user_groups.permission_ext。
package perm

// Bit 是一个权限位。
type Bit int64

// 权限位定义。新增权限只能追加，不得改动既有位，否则存量组的语义会漂移。
const (
	// Upload 允许上传（占用存储配额）。
	Upload Bit = 1 << iota
	// Download 允许下载。
	Download
	// Preview 允许在线预览。
	Preview
	// Share 允许创建分享。
	Share
	// Pickup 允许创建取件码。
	Pickup
	// ManageOwnNodes 允许增删改自己的文件与目录。
	ManageOwnNodes
	// BypassQuota 不受配额限制。
	BypassQuota
	// AdminFiles 允许拉黑/回收文件。
	AdminFiles
	// AdminUsers 允许管理账号。
	AdminUsers
	// AdminGroups 允许管理用户组与配额。
	AdminGroups
	// AdminAnnouncements 允许管理公告与消息。
	AdminAnnouncements
	// AdminInvites 允许管理邀请码。
	AdminInvites
	// AdminStorage 允许管理存储后端与迁移。
	AdminStorage
	// AdminSystem 允许修改系统配置。
	AdminSystem
	// AdminAudit 允许查看审计与流量记录。
	AdminAudit
	// MailAccess 允许访问邮件功能（持有邮箱地址、收件箱、阅读邮件）。
	MailAccess
	// AdminMail 允许后台管理收件域名与全局收件视图。
	AdminMail
	// AdminUnbind 允许审核邮箱地址解绑申请。
	//
	// 独立于 AdminMail：域名配置是日常设置，解绑审核是逐条表态的动作。一个
	// 能改域名的管理员未必该有权删掉别人已注册的地址——那会让外部发信人
	// 立刻收到退信，而误删的代价不由管理员承担。
	AdminUnbind
)

// All 是全部已定义权限的并集。
const All Bit = Upload | Download | Preview | Share | Pickup | ManageOwnNodes |
	BypassQuota | AdminFiles | AdminUsers | AdminGroups | AdminAnnouncements |
	AdminInvites | AdminStorage | AdminSystem | AdminAudit |
	MailAccess | AdminMail | AdminUnbind

// Has 判断掩码中是否包含指定权限。
func Has(mask int64, bit Bit) bool {
	return mask&int64(bit) != 0
}

// HasAll 判断掩码是否同时包含全部指定权限。
func HasAll(mask int64, bits ...Bit) bool {
	for _, b := range bits {
		if !Has(mask, b) {
			return false
		}
	}
	return true
}

// Of 把若干权限位合成掩码。
func Of(bits ...Bit) int64 {
	var mask int64
	for _, b := range bits {
		mask |= int64(b)
	}
	return mask
}

// 预设组的名称（也是 user_groups 主键）。这三个组不可删除、不可改名。
const (
	// GroupGuest 是访客组。访客没有账号，"属于访客组"由"无有效会话"隐式推出。
	GroupGuest = "guest"
	// GroupNormal 是默认注册用户组。
	GroupNormal = "normal"
	// GroupAdmin 是管理员组。
	GroupAdmin = "admin"
)

// BuiltinGroup 描述一个预设组的初始配置。
type BuiltinGroup struct {
	Name        string
	DisplayName string
	Permissions int64
	Priority    int
}

// BuiltinGroups 返回预设组及其默认权限。
//
// Permissions 只是初始值：预设组的约束是"不可删除、不可改名"，权限仍可被管理员调整。
func BuiltinGroups() []BuiltinGroup {
	return []BuiltinGroup{
		{
			Name:        GroupGuest,
			DisplayName: "访客",
			Permissions: Of(Download, Preview),
			Priority:    0,
		},
		{
			Name:        GroupNormal,
			DisplayName: "普通用户",
			Permissions: Of(Upload, Download, Preview, Share, Pickup, ManageOwnNodes, MailAccess),
			Priority:    100,
		},
		{
			Name:        GroupAdmin,
			DisplayName: "管理员",
			Permissions: int64(All),
			Priority:    1000,
		},
	}
}
