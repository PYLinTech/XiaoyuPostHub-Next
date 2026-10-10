package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// RegisterRequest 是一次注册请求。
type RegisterRequest struct {
	Account    string
	Password   string
	InviteCode string
	ClientIP   netip.Addr
}

// 账号与密码的长度规则是固定约束，不开放配置：
// 这两项一旦可配，就会出现"界面提示一套、服务端生效另一套"的漂移，
// 而漂移的代价是用户按提示填了却提交失败。改规则请改这里。
const (
	// AccountMinChars 账号最少字符数。
	AccountMinChars = 3
	// AccountMaxChars 账号最多字符数。
	AccountMaxChars = 16
)

// Register 注册账号。
//
// 三件事的顺序是刻意的：
//  1. 先做纯本地的参数校验与口令哈希（慢哈希约几十毫秒）——把注定失败的
//     请求挡在消费邀请码之前；
//  2. 再原子消费邀请码（"未超发/未过期/未停用"与用量递增在同一条 UPDATE 里，
//     分开写并发下必然超发）；
//  3. 最后建账号；建账号失败必须回补邀请码用量，否则一次失败会白白吃掉
//     一个不可再生的额度。
//
// 注册模式用白名单判定：配置损坏时直接拒绝注册，不能把未知值静默当成
// 某个现有模式，更不能让配置笔误意外改变开放范围。
func (s *Service) Register(ctx context.Context, req RegisterRequest) (store.User, error) {
	account := strings.TrimSpace(req.Account)
	if err := validateAccount(account); err != nil {
		return store.User{}, err
	}
	// 密码字符集与长度由 auth 包的固定规则把关（8-32 个可打印 ASCII 字符）。
	if err := auth.ValidatePassword(req.Password); err != nil {
		return store.User{}, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	mode := s.Settings.Runtime(ctx).Auth.RegisterMode
	if mode != settings.RegisterOpen && mode != settings.RegisterInvite && mode != settings.RegisterClosed {
		return store.User{}, fmt.Errorf("%w: 注册模式配置无效", ErrUnavailable)
	}
	if mode == settings.RegisterClosed {
		return store.User{}, fmt.Errorf("%w: 本站未开放注册", ErrForbidden)
	}
	needInvite := mode != settings.RegisterOpen

	groupName := perm.GroupNormal
	var inviteHash string
	if needInvite {
		code := strings.TrimSpace(req.InviteCode)
		if code == "" {
			return store.User{}, fmt.Errorf("%w: 需要邀请码", ErrForbidden)
		}
		pepper, err := s.Auth.Pepper(ctx)
		if err != nil {
			return store.User{}, fmt.Errorf("%w: 邀请码校验不可用（缺少 pepper 配置）", ErrUnavailable)
		}
		// 邀请码不区分大小写：短码靠人转述，大小写敏感只会制造无谓的失败。
		inviteHash = auth.HashLookupSecret(string(pepper), code)
		invite, err := store.GetInviteCode(ctx, s.DB.R(), inviteHash)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return store.User{}, fmt.Errorf("%w: 邀请码无效", ErrForbidden)
			}
			return store.User{}, err
		}
		groupName = invite.GroupName
	}

	// 口令哈希放在消费邀请码之前：慢哈希要几十毫秒，先消费会让额度在一次
	// 注定失败的注册里被占住更久。
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		return store.User{}, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	now := s.Now()
	user := store.User{
		Account:      account,
		DisplayName:  account,
		PasswordHash: passwordHash,
		GroupName:    groupName,
		Status:       store.UserEnabled,
		// users.invite_code 留空：明文邀请码是与口令同级的凭据，不落库；
		// "谁用了哪个码"由 invite_uses 记录。
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 邀请码消费、账号创建和使用记录必须是一笔事务。旧实现靠请求返回
	// 后再异步回补用量，任何进程退出或数据库故障都会留下“码已消费但
	// 账号不存在”的不可解释状态。
	err = s.DB.InTx(ctx, func(tx store.Querier) error {
		if needInvite {
			if err := store.ConsumeInviteCode(ctx, tx, inviteHash, now); err != nil {
				return err
			}
		}
		id, err := store.CreateUser(ctx, tx, user)
		if err != nil {
			return err
		}
		user.ID = id
		if needInvite {
			if err := store.InsertInviteUse(ctx, tx, store.InviteUse{
				CodeHash:    inviteHash,
				UserID:      id,
				UserAccount: account,
				UsedAt:      now,
				ClientIP:    req.ClientIP.String(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNoRowsAffected) {
			return store.User{}, fmt.Errorf("%w: 邀请码无效、已用尽或已过期", ErrForbidden)
		}
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, fmt.Errorf("%w: 账号 %s 已存在", ErrConflict, account)
		}
		return store.User{}, err
	}
	s.audit(ctx, auth.Principal{
		Actor:    store.ActorUser,
		User:     user,
		ClientIP: req.ClientIP,
		IPPrefix: s.Auth.IPPrefix(ctx, req.ClientIP),
	}, "user.register", account, "")
	return user, nil
}

// AdminListUsers 分页列出账号。
func (s *Service) AdminListUsers(ctx context.Context, p auth.Principal, limit, offset int) ([]store.User, error) {
	if err := auth.RequirePermission(p, perm.AdminUsers); err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	return store.ListUsers(ctx, s.DB.R(), limit, offset)
}

// AdminSetUserStatus 启用或禁用账号。
//
// 禁用必须连带吊销会话与已签发的下载票据：只改状态位会让"封禁"只挡住新的
// 登录，正在进行的会话与已经发出去的直链照旧可用，等于没封。
// 同时拒绝禁用自己——这是最容易误触且后果最严重的一步（把自己锁在门外）。
func (s *Service) AdminSetUserStatus(ctx context.Context, p auth.Principal, userID int64, enabled bool) error {
	if err := auth.RequirePermission(p, perm.AdminUsers); err != nil {
		return err
	}
	if userID == p.UserID() && !enabled {
		return fmt.Errorf("%w: 不能禁用当前登录的账号", ErrForbidden)
	}
	status := store.UserDisabled
	if enabled {
		status = store.UserEnabled
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		// 与组变更共用同一条不变量：禁用最后一个可用管理员等于把站点锁死。
		// 组变更那条路拦得住"移出管理员组"，这条路拦的是"原地禁用"。
		if !enabled {
			user, err := store.GetUserByID(ctx, tx, userID)
			if err != nil {
				return err
			}
			if user.GroupName == perm.GroupAdmin && user.Status == store.UserEnabled {
				if err := s.adminGroupFloorExceeded(ctx, tx, userID); err != nil {
					return err
				}
			}
		}
		if err := store.SetUserStatus(ctx, tx, userID, status); err != nil {
			return err
		}
		if !enabled {
			if _, err := store.RevokeTicketsByActor(ctx, tx, store.ActorUser, userID, ""); err != nil {
				return err
			}
			// 公开分享签发的票据属于 guest，按用户 actor 吊销不到它们。
			// 账号停用必须同时收回该账号内容产生的所有交付授权。
			if _, err := store.RevokeTicketsByOwner(ctx, tx, userID); err != nil {
				return err
			}
		}
		return store.RevokeUserSessions(ctx, tx, userID)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.Auth.InvalidateUserCache(userID)
	s.audit(ctx, p, "user.status", fmt.Sprintf("%d", userID), fmt.Sprintf("enabled=%v", enabled))
	return nil
}

// adminGroupFloorExceeded 检查把 userID 移出（或禁用）管理员组后，
// 组里是否还剩启用的账号。必须在使用写锁的事务内调用：两个并发的
// 组调整只有一把写锁，不会同时通过校验。
func (s *Service) adminGroupFloorExceeded(ctx context.Context, tx store.Querier, userID int64) error {
	others, err := store.CountEnabledUsersInGroup(ctx, tx, perm.GroupAdmin, userID)
	if err != nil {
		return err
	}
	if others == 0 {
		return fmt.Errorf("%w: 管理员组至少要保留一个启用的账号", ErrBadRequest)
	}
	return nil
}

// AdminSetUserGroup 变更账号所属组。
//
// 变更后必须吊销该账号的会话：鉴权层缓存里有组权限的副本，不吊销意味着
// 降级要等缓存过期才生效——权限回收的延迟就是越权窗口。
// 把最后一个管理员移出组会被拒绝：那条路一旦走通，站点只剩手工改库一条恢复路径。
func (s *Service) AdminSetUserGroup(ctx context.Context, p auth.Principal, userID int64, groupName string) error {
	if err := auth.RequirePermission(p, perm.AdminUsers); err != nil {
		return err
	}
	groupName = strings.TrimSpace(groupName)
	if _, err := store.GetGroup(ctx, s.DB.R(), groupName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 用户组 %s 不存在", ErrNotFound, groupName)
		}
		return err
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		user, err := store.GetUserByID(ctx, tx, userID)
		if err != nil {
			return err
		}
		if user.GroupName == perm.GroupAdmin && groupName != perm.GroupAdmin {
			if err := s.adminGroupFloorExceeded(ctx, tx, userID); err != nil {
				return err
			}
		}
		if err := store.UpdateUserGroup(ctx, tx, userID, groupName); err != nil {
			return err
		}
		// 邮箱地址的可用性跟着组走：这一步必须排在 UpdateUserGroup 之后，
		// SyncMailAddressStatus 要按刚落库的 group_name 重算地址状态。与改组
		// 同事务，提交时不留下「组已变、地址仍可收发」的中间态。
		if err := store.SyncMailAddressStatus(ctx, tx, userID, groupName); err != nil {
			return err
		}
		return store.RevokeUserSessions(ctx, tx, userID)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.Auth.InvalidateUserCache(userID)
	s.audit(ctx, p, "user.group", fmt.Sprintf("%d", userID), groupName)
	return nil
}

// AdminChangePassword 由管理员重置某个账号的口令。
func (s *Service) AdminChangePassword(ctx context.Context, p auth.Principal, userID int64, newPassword string) error {
	if err := auth.RequirePermission(p, perm.AdminUsers); err != nil {
		return err
	}
	if err := auth.ValidatePassword(newPassword); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := store.UpdateUserPassword(ctx, tx, userID, hash); err != nil {
			return err
		}
		// 重置口令后必须踢掉全部会话：否则"密码改了但旧会话还活着"，
		// 重置口令就起不到"收回访问权"的作用。
		return store.RevokeUserSessions(ctx, tx, userID)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.Auth.InvalidateUserCache(userID)
	s.audit(ctx, p, "user.password_reset", fmt.Sprintf("%d", userID), "")
	return nil
}

// ChangeOwnPassword 用户修改自己的口令。
//
// 先校验原口令再改：仅仅"已登录"不足以证明改密意图来自账号本人
// （例如被临时借用的设备上打开了页面）。改密后保留当前会话、吊销其它会话。
func (s *Service) ChangeOwnPassword(ctx context.Context, p auth.Principal, oldPassword, newPassword string) error {
	if err := auth.RequireUser(p); err != nil {
		return err
	}
	ok, err := auth.VerifyPassword(p.User.PasswordHash, oldPassword)
	if err != nil || !ok {
		return fmt.Errorf("%w: 原密码不正确", ErrForbidden)
	}
	if err := auth.ValidatePassword(newPassword); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := store.UpdateUserPassword(ctx, tx, p.UserID(), hash); err != nil {
			return err
		}
		if err := store.RevokeUserSessions(ctx, tx, p.UserID()); err != nil {
			return err
		}
		if p.Session.TokenHash == "" {
			return fmt.Errorf("%w: 当前会话令牌缺失", ErrUnavailable)
		}
		return store.RestoreSession(ctx, tx, p.UserID(), p.Session.TokenHash)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.Auth.InvalidateUserCache(p.UserID())
	s.audit(ctx, p, "user.password_change", fmt.Sprintf("%d", p.UserID()), "")
	return nil
}

// UploadProfile 是前端上传编排参数：系统分片大小、前端分片并发和用户组任务上限。
type UploadProfile struct {
	ChunkSize      int64 `json:"chunkSize"`
	MaxConcurrency int   `json:"maxConcurrency"`
	MaxTasks       int   `json:"maxTasks"`
}

// ProfileResult 是个人资料视图。
type ProfileResult struct {
	User        store.User  `json:"user"`
	Group       store.Group `json:"group"`
	Permissions int64       `json:"permissions"`
	StorageUsed int64       `json:"storageUsed"`
	// StorageLimit 为 0 表示未设置上限（配额语义里"没有行"= 不受限，
	// "值为 0" = 完全禁止；这里的 0 只承载前者）。
	StorageLimit int64         `json:"storageLimit"`
	IsGuest      bool          `json:"isGuest"`
	ActorKey     string        `json:"actorKey"`
	Upload       UploadProfile `json:"upload"`
}

// Profile 返回当前身份的资料与配额快照。
//
// 访客也允许调用：前端需要知道"匿名身份能做什么"才能决定首页渲染哪些入口，
// 而访客没有账号，只要求登录等于让这条接口在首页永远不可用。
//
// 登录用户的信息与用量重新读库而不是用鉴权层缓存里的副本：缓存有 10 秒
// 生命周期，用它展示"我的用量"会出现明显的数字回退。
func (s *Service) Profile(ctx context.Context, p auth.Principal) (ProfileResult, error) {
	if p.IsGuest() {
		return s.guestProfile(ctx, p)
	}
	rt := s.Settings.Runtime(ctx)
	user, err := store.GetUserByID(ctx, s.DB.R(), p.UserID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ProfileResult{}, ErrNotFound
		}
		return ProfileResult{}, err
	}
	group, err := store.GetGroup(ctx, s.DB.R(), user.GroupName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ProfileResult{}, fmt.Errorf("%w: 用户组不存在", ErrUnavailable)
		}
		return ProfileResult{}, err
	}

	plain, _, err := store.UserStorageUsage(ctx, s.DB.R(), user.ID)
	if err != nil {
		return ProfileResult{}, err
	}
	quotas, err := store.GroupQuotaMap(ctx, s.DB.R(), group.Name)
	if err != nil {
		return ProfileResult{}, err
	}

	res := ProfileResult{
		User:         user,
		Group:        group,
		Permissions:  group.Permissions,
		StorageUsed:  plain,
		StorageLimit: quotas[store.QuotaStorageTotal],
		IsGuest:      false,
		ActorKey:     p.ActorKey(),
		Upload:       uploadProfileOf(rt, quotas, perm.Has(group.Permissions, perm.BypassQuota)),
	}
	applyUnlimited(&res, group)
	return res, nil
}

// uploadProfileOf 抽出前端上传编排需要的运行参数。
func uploadProfileOf(rt settings.Runtime, quotas map[string]int64, quotaBypass bool) UploadProfile {
	// 前端本地队列没有组配额时，使用全局前端接收上限作为合理的客户端并发兜底；
	// 后台收尾任务上限只约束服务端 worker 与排队队列。
	maxTasks := rt.Upload.FrontendMaxTasks
	if !quotaBypass {
		if groupLimit, ok := quotas[store.QuotaPendingUploads]; ok {
			maxTasks = int(groupLimit)
		}
	}
	return UploadProfile{
		ChunkSize:      rt.Upload.ChunkSize,
		MaxConcurrency: rt.Upload.FrontendConcurrency,
		MaxTasks:       maxTasks,
	}
}

// guestProfile 构造访客的资料视图。
//
// 访客没有账号行，因此 User 为空值、StorageUsed 为 0：他们按 IP 计量，
// "已用存储"这个概念对访客并不存在，硬凑一个数字只会误导。
func (s *Service) guestProfile(ctx context.Context, p auth.Principal) (ProfileResult, error) {
	group := p.Group
	if group.Name == "" {
		return ProfileResult{}, fmt.Errorf("%w: 访客身份缺少用户组", ErrUnavailable)
	}
	quotas, err := store.GroupQuotaMap(ctx, s.DB.R(), group.Name)
	if err != nil {
		return ProfileResult{}, err
	}
	rt := s.Settings.Runtime(ctx)
	res := ProfileResult{
		Group:        group,
		Permissions:  group.Permissions,
		StorageLimit: quotas[store.QuotaStorageTotal],
		IsGuest:      true,
		ActorKey:     p.ActorKey(),
		Upload:       uploadProfileOf(rt, quotas, perm.Has(group.Permissions, perm.BypassQuota)),
	}
	applyUnlimited(&res, group)
	return res, nil
}

// applyUnlimited 清掉"不受配额限制"的组上残留的上限数值。
//
// 留着组里的数值会让界面展示一个并不生效的额度，用户会以为自己快满了。
func applyUnlimited(res *ProfileResult, group store.Group) {
	if perm.Has(group.Permissions, perm.BypassQuota) {
		res.StorageLimit = 0
	}
}

// validateAccount 校验账号名的形态与长度。
//
// 长度上下限来自配置而不是常量：不同部署对"账号能不能放宽到邮箱长度"的答案
// 不一样，写死在代码里会让这种调整必须改代码重新发版。拒绝空白与控制字符：
// 账号名会出现在日志与审计里，空白会带来"看起来一样但实际是两个账号"的混淆。
// validateAccountText 返回账号不合规的原因；空串表示合规。
//
// 单独拆出"只给原因、不给错误"的一份，是为了让引导页能把这句原因直接
// 展示给用户：那边需要的是一句话，而不是一个还要再解包的错误。
func validateAccountText(account string) string {
	length := utf8.RuneCountInString(account)
	if length < AccountMinChars {
		return fmt.Sprintf("账号至少 %d 个字符", AccountMinChars)
	}
	if length > AccountMaxChars {
		return fmt.Sprintf("账号最多 %d 个字符", AccountMaxChars)
	}
	for _, r := range account {
		if r <= 0x1F || r == 0x7F || unicode.IsSpace(r) {
			return "账号不得包含空白或控制字符"
		}
	}
	return ""
}

// validateAccount 校验账号名：3-16 个字符（按 rune 计，中英文与符号各算一个），
// 且不得包含空白或控制字符。长度规则是固定值，不接受调用方传入。
func validateAccount(account string) error {
	if msg := validateAccountText(account); msg != "" {
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	}
	return nil
}
