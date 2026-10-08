package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// GroupMailDomain 是「用户组 × 收件域名」的一条绑定。
//
// 一个组可以有多条：一个域名也可以被多个组同时使用。组没有域名（空列表）
// 与"有域名但暂停收件"是两种不同的状态——前者收不到任何信，后者只是暂时
// 不收——界面上必须能分开表达。
type GroupMailDomain struct {
	Domain         string `json:"domain"`
	ReceiveEnabled bool   `json:"receiveEnabled"`
	// AddressCount 是该域名下**本组用户**的邮箱地址数（含冻结）。>0 时
	// 该绑定不可移除：那些地址是用户数据，移除后它们会失去所属组、永远
	// 收不到信。注意口径是本组而不是该域名全部——域名可被多组共用，
	// 别组有没有地址与本组能不能解绑无关。
	AddressCount int64 `json:"addressCount"`
}

// groupDomainKey 拼「组 × 域名」的统计键。域名文本里可以出现任意合法字符，
// 直接用分隔符拼接理论上可能撞键；这里用长度前缀编码彻底消除歧义。
func groupDomainKey(groupName, domain string) string {
	return strconv.Itoa(len(groupName)) + ":" + groupName + domain
}

// GroupDetail 是管理端的用户组视图：组本身 + 配额 + 成员数 + 收件域名列表。
type GroupDetail struct {
	Group       store.Group        `json:"group"`
	Quotas      []store.GroupQuota `json:"quotas"`
	MemberCount int64              `json:"memberCount"`
	MailDomains []GroupMailDomain  `json:"mailDomains"`
}

// SaveGroupRequest 是保存用户组的请求。
//
// Quotas 为 nil 表示"不改配额"，为 -1 表示"删除该项配额"。删除与设为 0
// 的语义不同：没有行表示不受限，值为 0 表示完全禁止，因此必须能把一行
// 真正删掉而不是写成 0。
type SaveGroupRequest struct {
	Name                       string
	DisplayName                string
	Permissions                int64
	Priority                   int
	ResourceSchedulingPriority int
	Quotas                     map[string]int64
	// ReceiveDomains 是该组应当托管的收件域名**完整列表**。指针是有意的：
	// nil（请求里没有这个字段）表示"这次不管域名"，与显式传空切片（解绑
	// 全部）必须区分开。否则一个只想调配额的调用会顺手把域名全解绑掉——
	// 而域名下面有地址时移除会被挡下，于是配额也一起存不进去。
	//
	// 列表元素同时带上收件开关：开关属于「组 × 域名」这条绑定，管理员就在
	// 这一行上直接改，不另开一个"全局收件开关"入口。
	ReceiveDomains *[]GroupMailDomainInput
}

// GroupMailDomainInput 是管理端提交的一条域名绑定。
type GroupMailDomainInput struct {
	Domain         string `json:"domain"`
	ReceiveEnabled bool   `json:"receiveEnabled"`
}

// knownQuotaKeys 是系统实际会读取的配额键集合。
//
// 限制为已知键而不是任意字符串：未知键会被写进库却永远不会被读取，
// 那是最难排查的一类"配置了但不生效"。
//
// 这份表必须与"真正被读取的集合"保持一致：少一个键，管理员在界面上调
// 不了那个已经生效的限制；多一个键，则是把一个后端从不读取的值摆到
// 界面上骗人。邮件配额长期漏在这里——收件域名并进用户组之后，「每人可
// 建多少个邮箱地址」正是管理员要按组调的第一个开关。
var knownQuotaKeys = map[string]bool{
	store.QuotaStorageTotal:     true,
	store.QuotaFileMax:          true,
	store.QuotaTrafficDailyDown: true,
	store.QuotaPendingUploads:   true,
	store.QuotaMailStorageTotal: true,
	store.QuotaMailAddresses:    true,
}

// AdminListGroups 列出全部用户组及其配额、成员数与收件域名。
//
// 这里**不**再预写预设组。补写曾经放在读路径上，理由是"管理员清理过组记录"。
// 但它让每次 GET 都取一次写锁：账号页与邀请码页为了填下拉框也会调这个接口，
// 于是并发列表会和真实写入互相等待。补写的两个真实时机已经都在：
// 进程启动（cmd 主程序）与保存用户组（AdminSaveGroup，写路径）。
func (s *Service) AdminListGroups(ctx context.Context, p auth.Principal) ([]GroupDetail, error) {
	if err := auth.RequirePermission(p, perm.AdminGroups); err != nil {
		return nil, err
	}
	groups, err := store.ListGroups(ctx, s.DB.R())
	if err != nil {
		return nil, err
	}
	// 组列表的四项数据（组行、配额、人数、域名）各查一次就够：组数不多，
	// 但逐组再查会把列表退化成 2N 条查询，而每行都要显示这四样。
	quotasByGroup, err := store.ListAllGroupQuotas(ctx, s.DB.R())
	if err != nil {
		return nil, err
	}
	membersByGroup, err := store.CountUsersInAllGroups(ctx, s.DB.R())
	if err != nil {
		return nil, err
	}
	bindings, err := store.ListAllGroupMailDomains(ctx, s.DB.R())
	if err != nil {
		return nil, err
	}
	addrCounts, err := store.CountGroupAddressesByDomain(ctx, s.DB.R())
	if err != nil {
		return nil, err
	}
	// 统计口径是「组 × 域名」而不是「域名」：同一个域名被多个组共用时，
	// 每个组看到的必须是自己用户的地址数，否则别组的地址会让本组无法解绑。
	addrByBinding := make(map[string]int64, len(addrCounts))
	for _, c := range addrCounts {
		addrByBinding[groupDomainKey(c.GroupName, c.Domain)] = c.AddressCount
	}
	byGroup := make(map[string][]GroupMailDomain, len(bindings))
	for _, d := range bindings {
		byGroup[d.GroupName] = append(byGroup[d.GroupName], GroupMailDomain{
			Domain:         d.Domain,
			ReceiveEnabled: d.ReceiveEnabled,
			AddressCount:   addrByBinding[groupDomainKey(d.GroupName, d.Domain)],
		})
	}
	out := make([]GroupDetail, 0, len(groups))
	for _, g := range groups {
		// 缺省必须是空切片而非 nil：它作为嵌套字段进 JSON，null 会让前端
		// 对 null 取 .length 而抛错。
		quotas := quotasByGroup[g.Name]
		if quotas == nil {
			quotas = []store.GroupQuota{}
		}
		mailDomains := byGroup[g.Name]
		if mailDomains == nil {
			mailDomains = []GroupMailDomain{}
		}
		out = append(out, GroupDetail{
			Group:       g,
			Quotas:      quotas,
			MemberCount: membersByGroup[g.Name],
			MailDomains: mailDomains,
		})
	}
	return out, nil
}

// AdminSaveGroup 新建或更新用户组。名称已存在则更新，否则新建。
//
// 预设组（guest/normal/admin）的名称不可改、不可删，但权限与配额可以改：
// 约束是"结构稳定"而不是"配置冻结"。改动后必须让配置与访客组缓存失效，
// 否则权限改了要等缓存过期才生效——对权限回收来说那就是越权窗口。
func (s *Service) AdminSaveGroup(ctx context.Context, p auth.Principal, req SaveGroupRequest) (store.Group, error) {
	if err := auth.RequirePermission(p, perm.AdminGroups); err != nil {
		return store.Group{}, err
	}
	name := strings.TrimSpace(req.Name)
	if err := validateGroupName(name); err != nil {
		return store.Group{}, err
	}
	display := strings.TrimSpace(req.DisplayName)
	if display == "" {
		return store.Group{}, fmt.Errorf("%w: 用户组显示名不得为空", ErrBadRequest)
	}
	// 未知权限位必须拒绝：多出来的位不会生效，却会让界面显示一个
	// "已经授予"的权限，排查时极其误导。
	if req.Permissions&^int64(perm.All) != 0 {
		return store.Group{}, fmt.Errorf("%w: 权限掩码包含未定义的位", ErrBadRequest)
	}
	// 预设 admin 组必须保留系统管理位：撤销它会让整站失去唯一的管理入口，
	// 与"管理员组至少留一个启用账号"一样属于不可逆自锁——一次误保存就只能
	// 手工改库恢复。
	if name == perm.GroupAdmin && req.Permissions&int64(perm.AdminSystem) == 0 {
		return store.Group{}, fmt.Errorf("%w: 管理员组必须保留系统管理位", ErrBadRequest)
	}
	for key, value := range req.Quotas {
		if !knownQuotaKeys[key] {
			return store.Group{}, fmt.Errorf("%w: 未知的配额项 %q", ErrBadRequest, key)
		}
		if value < -1 {
			return store.Group{}, fmt.Errorf("%w: 配额 %s 只支持 -1（删除）或不小于 0 的值", ErrBadRequest, key)
		}
	}

	// 补齐预设组：否则在一个尚未初始化的库上保存名为 normal 的组会撞上
	// "预设组不能以同名新建"而无法保存。读路径不做这件事——补写要写锁，
	// 而 GET /api/admin/groups 会被账号页、邀请码页高频调用。
	if err := store.EnsureBuiltinGroups(ctx, s.DB.W()); err != nil {
		return store.Group{}, err
	}

	group := store.Group{
		Name:                       name,
		DisplayName:                display,
		Permissions:                req.Permissions,
		Priority:                   req.Priority,
		ResourceSchedulingPriority: req.ResourceSchedulingPriority,
	}
	groupUpdated := false
	permissionsChanged := false
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		existing, lookupErr := store.GetGroup(ctx, tx, name)
		switch {
		case lookupErr == nil:
			groupUpdated = true
			permissionsChanged = existing.Permissions != req.Permissions
			if err := store.UpdateGroup(ctx, tx, name, display, req.Permissions, req.Priority, req.ResourceSchedulingPriority); err != nil {
				return err
			}
		case errors.Is(lookupErr, store.ErrNotFound):
			if err := store.CreateGroup(ctx, tx, group); err != nil {
				if errors.Is(err, store.ErrConflict) {
					return fmt.Errorf("%w: 用户组 %s 已存在", ErrConflict, name)
				}
				return err
			}
		default:
			return lookupErr
		}
		if permissionsChanged {
			// 修改组权限与吊销成员会话必须处于同一写事务。否则事务提交
			// 前已有请求仍可能拿着旧权限完成一次敏感操作。
			if err := store.RevokeGroupSessions(ctx, tx, name); err != nil {
				return err
			}
		}

		for key, value := range req.Quotas {
			if value == -1 {
				if err := store.DeleteGroupQuota(ctx, tx, name, key); err != nil {
					return err
				}
				continue
			}
			if err := store.SetGroupQuota(ctx, tx, name, key, value); err != nil {
				return err
			}
		}

		// 收件域名跟组行同一事务：绑定的外键指向 user_groups(name)，分开写
		// 会出现"组建好了域名没写进去"这种半截状态，而组没有域名等于收不到信。
		if req.ReceiveDomains != nil {
			if err := syncGroupMailDomains(ctx, tx, name, *req.ReceiveDomains); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return store.Group{}, err
	}

	// 访客组与系统配置都带短缓存；登录用户的 Principal 也含有组权限快照，
	// 组发生任何更新都应丢弃。权限位变化还在事务内吊销了数据库会话。
	if groupUpdated {
		s.Auth.InvalidateGroupCache(name)
	}
	s.Auth.InvalidateGuestGroup()
	s.Settings.Invalidate()
	// 域名没被这次请求碰到时不要在审计里写一个空串，那会被读成"解绑了"。
	domainNote := "unchanged"
	if req.ReceiveDomains != nil {
		domainNote = strings.Join(normalizedDomains(*req.ReceiveDomains), ",")
	}
	s.audit(ctx, p, "group.save", name, fmt.Sprintf("perm=%d priority=%d resource_scheduling_priority=%d domain=%q",
		req.Permissions, req.Priority, req.ResourceSchedulingPriority, domainNote))

	saved, err := store.GetGroup(ctx, s.DB.R(), name)
	if err != nil {
		return store.Group{}, err
	}
	return saved, nil
}

// AdminDeleteGroup 删除用户组。
//
// 预设组、仍有成员的组、仍被邀请码引用的组都由 store 层拒绝：把成员或
// 邀请码悬置到一个不存在的组上，会在后续注册或登录时才暴露出问题。
// 仍绑着收件域名的组同样拒绝，但那要看管理员说得清是哪个域名挡住的，
// 因此在这里先查一次、报出名字，而不是丢一个外键错误过去。
func (s *Service) AdminDeleteGroup(ctx context.Context, p auth.Principal, name string) error {
	if err := auth.RequirePermission(p, perm.AdminGroups); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	// 仍托管着收件域名的组不允许删：绑定行会跟着组一起级联消失，管理员却
	// 没收到任何提示。列出域名名，管理员才知道该去哪里解绑。
	bindings, err := store.ListGroupMailDomains(ctx, s.DB.R(), name)
	if err != nil {
		return err
	}
	if len(bindings) > 0 {
		names := make([]string, len(bindings))
		for i, b := range bindings {
			names[i] = b.Domain
		}
		return fmt.Errorf("%w: 用户组 %s 还托管着收件域名 %s，请先在组设置里移除",
			ErrConflict, name, strings.Join(names, "、"))
	}
	if err := store.DeleteGroup(ctx, s.DB.W(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return err
	}
	s.Auth.InvalidateGuestGroup()
	s.Settings.Invalidate()
	s.audit(ctx, p, "group.delete", name, "")
	return nil
}

// validateGroupName 校验组名形态。
//
// 组名是主键，也会出现在权限判定与审计里，因此限定为无空白的短标识符：
// 中文或含空格的组名应该放在 display_name 里。
func validateGroupName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: 用户组名不得为空", ErrBadRequest)
	}
	if len(name) > 64 {
		return fmt.Errorf("%w: 用户组名过长", ErrBadRequest)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return fmt.Errorf("%w: 用户组名只能包含字母、数字以及下划线、减号、句点", ErrBadRequest)
		}
	}
	return nil
}
