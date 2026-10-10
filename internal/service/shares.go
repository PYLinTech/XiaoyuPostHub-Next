package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// maxShareSecretBytes 是提取码的长度上限。
//
// 提取码是给人抄写的短凭据，只会有几个字节；设上限只是为了让 HMAC 的输入
// 有界，同时避免"把一整个文件塞进提取码框"这种误用。
const maxShareSecretBytes = 128

// CreateShareRequest 是一次新建分享的请求。
//
// AllowDownload / AllowPreview 按字面取值，不做"两假即默认全开"的推测：
// 只开放预览或只开放下载都是合法配置，猜调用方意图会让"我明明关掉了下载"
// 失效。默认值由 HTTP 层在构造请求时决定。
type CreateShareRequest struct {
	Path           string
	Kind           store.ShareKind
	AccessMode     store.AccessMode
	Password       string
	AllowDownload  bool
	AllowPreview   bool
	ShowSharerName bool
	AllowSubpath   bool
	ExpiresAt      int64
	MaxVisits      int
}

// UpdateShareRequest 是分享的部分更新。指针为 nil 表示该字段不变——
// 用零值表示"不变"就无法把 MaxVisits 改成 0（不限次数）或把 Disabled
// 改成 false（重新启用）。
type UpdateShareRequest struct {
	AccessMode     *store.AccessMode
	Password       *string
	AllowDownload  *bool
	AllowPreview   *bool
	ShowSharerName *bool
	AllowSubpath   *bool
	ExpiresAt      *int64
	MaxVisits      *int
	Disabled       *bool
	// ExpectUpdatedAt 是调用方读到的分享版本（store.Share.UpdatedAt）。
	//
	// 非零时按乐观锁提交：版本已被别人推进则返回 ErrConflict，而不是静默覆盖
	// ——提取码这类凭据一旦被陈旧写回覆盖，等于把已经改掉的旧口令又放回线上。
	// 为零表示调用方未声明版本，此时用本次读取到的版本提交；这仍能拦住
	// "读取与写回之间"发生的并发修改，但拦不住拿着旧快照的客户端。
	ExpectUpdatedAt int64
}

// CreateShare 新建分享。
//
// 先确认路径存在且属于创建者：分享的根路径是一份快照，指向不存在的路径会让
// 访客只得到一个无法解释的错误，指向别人的路径则等于把越权访问包装成分享。
func (s *Service) CreateShare(ctx context.Context, p auth.Principal, req CreateShareRequest) (store.Share, error) {
	if err := auth.RequirePermission(p, perm.Share); err != nil {
		return store.Share{}, err
	}
	norm, err := vpath.Normalize(req.Path)
	if err != nil {
		return store.Share{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	node, err := s.requireOwnNode(ctx, p.UserID(), norm)
	if err != nil {
		return store.Share{}, err
	}

	// 分享类型必须与节点的真实类型一致。不一致时后续会按 kind 决定"是否
	// 允许子路径"，一个"文件分享"指向目录就等于绕过了这条限制。
	kind := req.Kind
	if kind == "" {
		if node.IsFolder() {
			kind = store.ShareFolder
		} else {
			kind = store.ShareFile
		}
	}
	if kind != store.ShareFile && kind != store.ShareFolder {
		return store.Share{}, fmt.Errorf("%w: 未知的分享类型 %q", ErrBadRequest, kind)
	}
	if kind == store.ShareFolder && !node.IsFolder() {
		return store.Share{}, fmt.Errorf("%w: %s 不是目录", ErrBadRequest, norm)
	}
	if kind == store.ShareFile && node.IsFolder() {
		return store.Share{}, fmt.Errorf("%w: %s 是目录，不能作为文件分享", ErrBadRequest, norm)
	}

	mode := req.AccessMode
	if mode == "" {
		mode = store.AccessPublic
	}
	if !validAccessMode(mode) {
		return store.Share{}, fmt.Errorf("%w: 未知的访问模式 %q", ErrBadRequest, mode)
	}
	if req.ExpiresAt < 0 || req.MaxVisits < 0 {
		return store.Share{}, fmt.Errorf("%w: 有效期与访问上限不得为负", ErrBadRequest)
	}

	var pwdHash string
	if mode == store.AccessPassword {
		code := strings.TrimSpace(req.Password)
		if code == "" {
			return store.Share{}, fmt.Errorf("%w: 密码模式必须提供提取码", ErrBadRequest)
		}
		if len(code) > maxShareSecretBytes {
			return store.Share{}, fmt.Errorf("%w: 提取码过长", ErrBadRequest)
		}
		hashed, err := s.hashShareSecret(ctx, code)
		if err != nil {
			return store.Share{}, err
		}
		pwdHash = hashed
	}

	// id 由这里生成而不是交给 store 层：CreateShare 按值接收分享，无法把
	// 生成的 id 回传，而我们必须把它返回给 HTTP 层。
	id, err := store.GenerateToken(16)
	if err != nil {
		return store.Share{}, err
	}
	share := store.Share{
		ID:            strings.ToLower(id),
		OwnerID:       p.UserID(),
		RootPath:      norm,
		Kind:          kind,
		AccessMode:    mode,
		PwdHash:       pwdHash,
		HasPassword:   pwdHash != "",
		AllowDownload: req.AllowDownload,
		AllowPreview:  req.AllowPreview,
		// 文件分享没有"子路径"这个概念，落库为假以免前端据此渲染出
		// 一个永远不会生效的开关。
		AllowSubpath:   kind == store.ShareFolder && req.AllowSubpath,
		ShowSharerName: req.ShowSharerName,
		ExpiresAt:      req.ExpiresAt,
		MaxVisits:      req.MaxVisits,
		CreatedAt:      s.Now(),
		// 乐观锁版本从非零起版：零是"调用方未声明版本"的哨兵值，创建时就落零
		// 会让它无法与"未声明"区分。
		UpdatedAt: s.Now(),
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		// 路径存在检查与分享创建必须在同一写事务内；否则节点在两次
		// 操作之间被删除，会留下无法解析的分享根。
		current, err := store.GetNode(ctx, tx, p.UserID(), norm)
		if err != nil {
			return err
		}
		if current.IsFolder() != (kind == store.ShareFolder) {
			return fmt.Errorf("%w: 分享类型与节点不一致", ErrBadRequest)
		}
		return store.CreateShare(ctx, tx, share)
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.Share{}, fmt.Errorf("%w: 分享标识冲突，请重试", ErrConflict)
		}
		if errors.Is(err, store.ErrNotFound) {
			return store.Share{}, ErrNotFound
		}
		return store.Share{}, err
	}
	s.audit(ctx, p, "share.create", share.ID, fmt.Sprintf("%s mode=%s", norm, mode))
	return share, nil
}

// ListShares 列出当前用户创建的分享。
//
// 只要求登录、不要求 perm.Share：权限被回收后用户仍应能查看并删除自己
// 已经建出来的分享，否则那些分享会永久留在线上且无人可关。
func (s *Service) ListShares(ctx context.Context, p auth.Principal, limit, offset int) ([]store.Share, error) {
	if err := auth.RequireUser(p); err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	return store.ListSharesByOwner(ctx, s.DB.R(), p.UserID(), limit, offset)
}

// UpdateShare 修改分享的可变字段。
//
// 读取—修改—整体写回：store.UpdateShareIfUnchanged 是全字段 UPDATE，必须先
// 取出原值再逐项覆盖，否则未在请求里出现的字段会被零值清空（例如 root_path
// 变空串）。写回带 updated_at 乐观锁，语义见 ExpectUpdatedAt。
func (s *Service) UpdateShare(ctx context.Context, p auth.Principal, id string, req UpdateShareRequest) (store.Share, error) {
	if err := auth.RequireUser(p); err != nil {
		return store.Share{}, err
	}
	share, err := s.ownShare(ctx, p.UserID(), id)
	if err != nil {
		return store.Share{}, err
	}
	wasDisabled := share.Disabled
	wasPwdHash := share.PwdHash
	wasExpiresAt := share.ExpiresAt
	wasAllowDownload := share.AllowDownload
	wasAllowPreview := share.AllowPreview
	// 调用方声明的版本优先；未声明时退回本次读到的版本，用于拦住读取之后、
	// 写回之前挤进来的并发修改。
	expectUpdatedAt := req.ExpectUpdatedAt
	if expectUpdatedAt == 0 {
		expectUpdatedAt = share.UpdatedAt
	}

	if req.AccessMode != nil {
		if !validAccessMode(*req.AccessMode) {
			return store.Share{}, fmt.Errorf("%w: 未知的访问模式 %q", ErrBadRequest, *req.AccessMode)
		}
		share.AccessMode = *req.AccessMode
	}
	if share.AccessMode != store.AccessPassword && req.Password != nil && strings.TrimSpace(*req.Password) != "" {
		return store.Share{}, fmt.Errorf("%w: 非密码分享不能设置提取码", ErrBadRequest)
	}
	if req.Password != nil {
		code := strings.TrimSpace(*req.Password)
		switch {
		case code == "":
			share.PwdHash = ""
		case len(code) > maxShareSecretBytes:
			return store.Share{}, fmt.Errorf("%w: 提取码过长", ErrBadRequest)
		default:
			hashed, err := s.hashShareSecret(ctx, code)
			if err != nil {
				return store.Share{}, err
			}
			share.PwdHash = hashed
		}
		share.HasPassword = share.PwdHash != ""
	}
	if req.ShowSharerName != nil {
		share.ShowSharerName = *req.ShowSharerName
	}
	if req.AllowDownload != nil {
		share.AllowDownload = *req.AllowDownload
	}
	if req.AllowPreview != nil {
		share.AllowPreview = *req.AllowPreview
	}
	if req.AllowSubpath != nil {
		share.AllowSubpath = *req.AllowSubpath
	}
	if req.ExpiresAt != nil {
		if *req.ExpiresAt < 0 {
			return store.Share{}, fmt.Errorf("%w: 有效期不得为负", ErrBadRequest)
		}
		share.ExpiresAt = *req.ExpiresAt
	}
	if req.MaxVisits != nil {
		if *req.MaxVisits < 0 {
			return store.Share{}, fmt.Errorf("%w: 访问上限不得为负", ErrBadRequest)
		}
		share.MaxVisits = *req.MaxVisits
	}
	if req.Disabled != nil {
		share.Disabled = *req.Disabled
	}

	// 最终态必须自洽：切到密码模式却把提取码清空，会让这条分享永远无法
	// 被任何人打开（包括创建者自己）。
	if share.AccessMode == store.AccessPassword && share.PwdHash == "" {
		return store.Share{}, fmt.Errorf("%w: 密码模式必须设置提取码", ErrBadRequest)
	}
	if share.AccessMode != store.AccessPassword {
		// 非密码模式不需要保留旧的短凭据哈希；清掉它可以避免管理端显示
		// “仍有密码”，也避免无用的敏感数据继续留在数据库里。
		share.PwdHash = ""
		share.PwdSalt = ""
		share.HasPassword = false
	}
	if share.Kind != store.ShareFolder {
		share.AllowSubpath = false
	}

	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := store.UpdateShareIfUnchanged(ctx, tx, share, expectUpdatedAt); err != nil {
			return err
		}
		// 分享的凭据/授权状态发生变化时，必须吊销已经签发的 CDN/中转票据；
		// 否则票据数据面不会再次经过分享解析，变更形同虚设：
		//   - 停用：链接整体失效；
		//   - 提取码更改：旧提取码签出的票据不能活到新提取码生效之后；
		//   - 有效期被改短到已过期：同"过期即失效"。
		credChanged := share.PwdHash != wasPwdHash
		expiredNow := share.ExpiresAt != 0 && share.ExpiresAt <= s.Now() &&
			(wasExpiresAt == 0 || wasExpiresAt > s.Now())
		// 权限位收回同理。checkShareDelivery 只在**签发**票据时执行，签出去的
		// 票据此后完全由票据自身的 TTL 与 max_uses 约束，不再回看分享。因此
		// 关闭"允许下载"后，已经发出去的票据仍能继续下载到过期——用户视角
		// 就是"我明明关掉了却还在下载"。
		permTightened := (wasAllowDownload && !share.AllowDownload) ||
			(wasAllowPreview && !share.AllowPreview)
		if (!wasDisabled && share.Disabled) || credChanged || expiredNow || permTightened {
			_, err := store.RevokeTicketsByShareRoot(ctx, tx, share.OwnerID, share.RootPath)
			return err
		}
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Share{}, ErrNotFound
		}
		if errors.Is(err, store.ErrConflict) {
			return store.Share{}, fmt.Errorf("%w: 分享已被其他人修改，请刷新后重试", ErrConflict)
		}
		return store.Share{}, err
	}
	s.audit(ctx, p, "share.update", share.ID, fmt.Sprintf("disabled=%v", share.Disabled))
	return share, nil
}

// DeleteShare 删除分享，其取件码与访问明细随之级联删除。
func (s *Service) DeleteShare(ctx context.Context, p auth.Principal, id string) error {
	if err := auth.RequireUser(p); err != nil {
		return err
	}
	share, err := s.ownShare(ctx, p.UserID(), id)
	if err != nil {
		return err
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if _, err := store.RevokeTicketsByShareRoot(ctx, tx, share.OwnerID, share.RootPath); err != nil {
			return err
		}
		return store.DeleteShare(ctx, tx, share.ID)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, "share.delete", share.ID, share.RootPath)
	return nil
}

// ListShareAccesses 列出自己的分享的访问明细。
func (s *Service) ListShareAccesses(ctx context.Context, p auth.Principal, id string, limit int) ([]store.AuditLog, error) {
	if err := auth.RequireUser(p); err != nil {
		return nil, err
	}
	share, err := s.ownShare(ctx, p.UserID(), id)
	if err != nil {
		return nil, err
	}
	return store.ListShareAccesses(ctx, s.DB.R(), share.ID, limit)
}

// ResolveShare 解析分享并返回交付目标。
//
// 校验顺序是刻意固定的：
//  1. 分享存在——后面每一步都需要它的字段；
//  2. 创建者账号仍启用——账号一旦停用，他创建的公开分享必须同时失效，
//     否则封禁只挡住了登录，挡不住分享链接；
//  3. 未停用、未过期、未达访问上限——这三者是"分享本身"的时效；
//  4. 访问模式——只有前三步都过了，才有必要去付一次提取码 HMAC 的代价；
//  5. 根节点仍存在——根路径是快照，删除/移动后不会自动跟随。
//
// 访问校验失败统一返回 ErrForbidden，具体原因用于访客页提示。
func (s *Service) ResolveShare(ctx context.Context, id, password string, p auth.Principal) (DeliveryTarget, store.Share, error) {
	return s.resolveShare(ctx, id, password, p, false, true, "open", true)
}

// resolveShare 是 ResolveShare 与取件码共用的实现。viaPickup 为真表示
// 调用方已经用取件码完成了凭据校验，因此不再要求提取码。countVisit 控制
// 是否把这次解析计入分享访问次数；取件码的“查看”不能消耗分享访问额度，
// 真正取数时再由核销事务统一计数。recordAccess 控制是否写访问明细；取件码
// 真正取数时由核销事务写入，避免失败的核销留下误导性的审计记录。
func (s *Service) resolveShare(ctx context.Context, id, password string, p auth.Principal, viaPickup, countVisit bool, action string, recordAccess bool) (DeliveryTarget, store.Share, error) {
	forbid := func(why string) error { return fmt.Errorf("%w: %s", ErrForbidden, why) }

	id = strings.TrimSpace(id)
	if id == "" {
		return DeliveryTarget{}, store.Share{}, forbid("分享不存在或已失效")
	}
	share, err := store.GetShare(ctx, s.DB.R(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeliveryTarget{}, store.Share{}, forbid("分享不存在或已失效")
		}
		return DeliveryTarget{}, store.Share{}, err
	}

	owner, err := store.GetUserByID(ctx, s.DB.R(), share.OwnerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeliveryTarget{}, store.Share{}, forbid("分享不存在或已失效")
		}
		return DeliveryTarget{}, store.Share{}, err
	}
	if owner.Status != store.UserEnabled {
		return DeliveryTarget{}, store.Share{}, forbid("分享已被停用")
	}
	if share.Disabled {
		return DeliveryTarget{}, store.Share{}, forbid("分享已被停用")
	}

	now := s.Now()
	if share.ExpiresAt != 0 && share.ExpiresAt <= now {
		return DeliveryTarget{}, store.Share{}, forbid("分享已过期")
	}
	if share.MaxVisits > 0 && share.Visits >= share.MaxVisits {
		return DeliveryTarget{}, store.Share{}, forbid("分享已达访问上限")
	}

	switch share.AccessMode {
	case store.AccessPublic:
		// 公开分享：拿到链接即可访问。
	case store.AccessLogin:
		if p.IsGuest() {
			return DeliveryTarget{}, store.Share{}, forbid("该分享需要登录后访问")
		}
	case store.AccessRestricted:
		// 受限分享当前只对创建者本人开放：这一层之上没有"指定接收者"的
		// 数据模型，先按最紧的语义执行，宁可拒绝对外访问。
		if p.IsGuest() || p.UserID() != share.OwnerID {
			return DeliveryTarget{}, store.Share{}, forbid("该分享仅创建者可访问")
		}
	case store.AccessPassword:
		if !viaPickup {
			if err := s.checkSharePassword(ctx, share, password, p); err != nil {
				return DeliveryTarget{}, store.Share{}, err
			}
		}
	default:
		// 未知模式按拒绝处理：将来新增模式时，老代码不会因为"不认识"而
		// 误把它当成公开分享放行。
		return DeliveryTarget{}, store.Share{}, forbid("未知的访问模式")
	}

	node, err := store.GetNode(ctx, s.DB.R(), share.OwnerID, share.RootPath)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeliveryTarget{}, store.Share{}, forbid("分享内容已不存在")
		}
		return DeliveryTarget{}, store.Share{}, err
	}
	if node.IsFolder() != (share.Kind == store.ShareFolder) {
		return DeliveryTarget{}, store.Share{}, forbid("分享内容与分享类型不符")
	}

	if countVisit {
		// 有效性判定与计数递增在同一条 UPDATE 里（见
		// store.ConsumeShareVisit）：分开判会在并发下超发，而“访问上限”
		// 正是靠这条来兜住的。
		if err := store.ConsumeShareVisit(ctx, s.DB.W(), share.ID, now); err != nil {
			if errors.Is(err, store.ErrNoRowsAffected) {
				return DeliveryTarget{}, store.Share{}, forbid("分享已失效、已过期或已达访问上限")
			}
			return DeliveryTarget{}, store.Share{}, err
		}
		share.Visits++
	}
	// 访问明细是审计附属数据，写失败不回滚已完成的访问计数；这类失败由运维
	// 报表发现，不应把可用的分享链接变成不可用。
	if recordAccess {
		_ = store.InsertShareAccess(ctx, s.DB.W(), share.ID, p.Actor, p.UserID(), p.ClientIP.String(), action)
	}

	// 展示字段仅在访问授权通过后填充；不以账号或邮箱作为名称回退。
	if share.ShowSharerName {
		share.SharerName = strings.TrimSpace(owner.DisplayName)
	}
	share.Size = node.SizePlain

	// 返回的快照与库内保持一致，调用方不必再查一次。
	return DeliveryTarget{
		OwnerUserID: share.OwnerID,
		Path:        share.RootPath,
		ShareID:     share.ID,
	}, share, nil
}

// ResolveSharePath 在分享根之下解析相对路径，返回交付目标与规范化后的绝对路径。
//
// 路径穿越防护的落点：相对路径先与根拼接、再整体走一次 vpath.Normalize，
// 最后用 vpath.IsWithin 复核。绝对不能用裸的 strings.HasPrefix——根为
// "/a/b" 的分享会因此接受 "/a/bc"，而那是分享之外的另一棵目录树。
func (s *Service) ResolveSharePath(ctx context.Context, id, password, relPath string, p auth.Principal) (DeliveryTarget, store.Share, string, error) {
	target, share, err := s.ResolveShare(ctx, id, password, p)
	if err != nil {
		return DeliveryTarget{}, store.Share{}, "", err
	}
	abs, err := resolveSubPath(share, relPath)
	if err != nil {
		return DeliveryTarget{}, store.Share{}, "", err
	}
	node, err := store.GetNode(ctx, s.DB.R(), share.OwnerID, abs)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeliveryTarget{}, store.Share{}, "", ErrNotFound
		}
		return DeliveryTarget{}, store.Share{}, "", err
	}
	if !share.AllowSubpath && abs != share.RootPath && node.IsFolder() {
		return DeliveryTarget{}, store.Share{}, "", fmt.Errorf("%w: 因分享者设置，无法浏览子目录", ErrForbidden)
	}
	target.Path = abs
	return target, share, abs, nil
}

// ListShareDir 列出分享内某相对路径下的目录内容。
//
// 只允许文件夹分享：单文件分享没有目录语义，让它"列出一层"会把文件自身
// 当成目录内容返回。
func (s *Service) ListShareDir(ctx context.Context, id, password, relPath string, p auth.Principal) ([]ListNode, error) {
	target, share, abs, err := s.ResolveSharePath(ctx, id, password, relPath, p)
	if err != nil {
		return nil, err
	}
	if share.Kind != store.ShareFolder {
		return nil, fmt.Errorf("%w: 该分享不是目录分享", ErrBadRequest)
	}
	node, err := store.GetNode(ctx, s.DB.R(), target.OwnerUserID, abs)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !node.IsFolder() {
		return nil, fmt.Errorf("%w: %s 不是目录", ErrBadRequest, abs)
	}

	nodes, err := store.ListChildren(ctx, s.DB.R(), target.OwnerUserID, abs)
	if err != nil {
		return nil, err
	}
	if !share.AllowSubpath {
		files := make([]store.Node, 0, len(nodes))
		for _, child := range nodes {
			if !child.IsFolder() {
				files = append(files, child)
			}
		}
		nodes = files
	}
	items, err := s.listNodesWithStatus(ctx, nodes)
	if err != nil {
		return nil, err
	}
	// 访客视角脱敏：路径改为分享内相对路径（属主的绝对目录结构不外泄），
	// 内容校验码是属主的内容指纹，也不属于访客需要的信息。访客界面的
	// 目录导航只依赖 name 与相对层级，不消费绝对路径。
	root := share.RootPath
	accessible := make([]ListNode, 0, len(items))
	for i := range items {
		if items[i].Status == store.FileDisabled {
			continue
		}
		rel := strings.TrimPrefix(items[i].Path, root)
		if rel == "" {
			rel = vpath.Root
		}
		items[i].Path = rel
		items[i].Checksum = ""
		accessible = append(accessible, items[i])
	}
	_ = store.InsertShareAccess(ctx, s.DB.W(), share.ID, p.Actor, p.UserID(), p.ClientIP.String(), "list")
	return accessible, nil
}

// ---------------------------------------------------------------- 内部辅助

// ownShare 取自己的分享。他人的分享返回 ErrNotFound 而不是 ErrForbidden：
// 分享 id 是随机 token，但把"存在但无权"与"不存在"区分开，仍然给出了一个
// 可以逐个验证 id 是否有效的探测面。
func (s *Service) ownShare(ctx context.Context, userID int64, id string) (store.Share, error) {
	share, err := store.GetShare(ctx, s.DB.R(), strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Share{}, ErrNotFound
		}
		return store.Share{}, err
	}
	if share.OwnerID != userID {
		return store.Share{}, ErrNotFound
	}
	return share, nil
}

// hashShareSecret 把提取码转成可反查的散列。
//
// 提取码是短凭据，必须能"由明文按值反查"，因此不能用随机盐的慢哈希；
// 改用带服务端 pepper 的 HMAC，使得只有数据库也无法离线枚举出短码。
func (s *Service) hashShareSecret(ctx context.Context, secret string) (string, error) {
	pepper, err := s.Auth.Pepper(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: 短凭据哈希不可用（缺少 pepper 配置）", ErrUnavailable)
	}
	return auth.HashLookupSecret(string(pepper), secret), nil
}

// checkSharePassword 校验提取码，并在分享与访问者 IP 两个维度上退避。
//
// 两个维度缺一不可：只按分享退避挡不住"换分享继续试"，只按 IP 退避挡不住
// 分布式爆破。比较用恒定时间函数，避免通过响应时间逐步还原散列。
func (s *Service) checkSharePassword(ctx context.Context, share store.Share, attempt string, p auth.Principal) error {
	shareKey := [2]string{string(auth.ThrottleShare), share.ID}
	ipKey := [2]string{string(auth.ThrottleIP), throttleIP(p)}
	// 退避检查放在最前：爆破流量应当在最便宜的环节被挡掉，也避免每次
	// 尝试都白付一次 HMAC 的代价。
	if err := s.Auth.Throttler().CheckAll(ctx, shareKey, ipKey); err != nil {
		return err
	}
	pepper, err := s.Auth.Pepper(ctx)
	if err != nil {
		return fmt.Errorf("%w: 短凭据校验不可用（缺少 pepper 配置）", ErrUnavailable)
	}
	got := auth.HashLookupSecret(string(pepper), strings.TrimSpace(attempt))
	if strings.TrimSpace(attempt) == "" ||
		subtle.ConstantTimeCompare([]byte(got), []byte(share.PwdHash)) != 1 {
		if throttleErr := s.Auth.Throttler().FailAll(ctx, shareKey, ipKey); throttleErr != nil {
			return fmt.Errorf("%w: 记录提取码失败状态", ErrUnavailable)
		}
		return fmt.Errorf("%w: 提取码不正确", ErrForbidden)
	}
	if err := s.Auth.Throttler().Clear(ctx, auth.ThrottleShare, share.ID); err != nil {
		return fmt.Errorf("%w: 清除分享限流状态", ErrUnavailable)
	}
	// 不清 IP 维度：它的作用是"防同一来源轮换分享/提取码"。解锁了一个分享就
	// 把整个来源的失败计数清零，等于让攻击者用自备分享反复复位爆破预算。
	return nil
}

// resolveSubPath 把分享内的相对路径解析为绝对逻辑路径。
//
// 拼接后整体规范化是关键：`..`、"//"、空段都会被 vpath.Normalize 直接拒绝，
// 最后再用 vpath.IsWithin 做段级边界校验。
func resolveSubPath(share store.Share, relPath string) (string, error) {
	rel := strings.TrimPrefix(strings.TrimSpace(relPath), vpath.Root)
	abs := share.RootPath
	if rel != "" {
		base := share.RootPath
		if base == vpath.Root {
			base = ""
		}
		parsed, err := vpath.Normalize(base + vpath.Root + rel)
		if err != nil {
			return "", fmt.Errorf("%w: 分享内路径非法", ErrBadRequest)
		}
		abs = parsed
	}
	// 必须是"段级"判定：裸用 strings.HasPrefix(share.RootPath, abs)
	// 会让根为 /a/b 的分享接受 /a/bc。
	if abs != share.RootPath && !vpath.IsWithin(share.RootPath, abs) {
		return "", fmt.Errorf("%w: 路径不在分享范围内", ErrForbidden)
	}
	if share.Kind != store.ShareFolder && abs != share.RootPath {
		return "", fmt.Errorf("%w: 文件分享不允许访问子路径", ErrForbidden)
	}
	if share.Kind == store.ShareFolder && abs != share.RootPath && !share.AllowSubpath && vpath.Parent(abs) != share.RootPath {
		return "", fmt.Errorf("%w: 因分享者设置，无法浏览子目录", ErrForbidden)
	}
	return abs, nil
}

// throttleIP 返回退避用的 IP 维度键。
//
// 极少数情况下请求没有可解析的来源地址，此时退化为按 IP 前缀聚合：
// 让退避维度为空等于把这条防线整个关掉。
func throttleIP(p auth.Principal) string {
	if p.ClientIP.IsValid() {
		return p.ClientIP.String()
	}
	return p.IPPrefix
}

// validAccessMode 判断访问模式是否为已定义的枚举值。
func validAccessMode(mode store.AccessMode) bool {
	switch mode {
	case store.AccessPublic, store.AccessPassword, store.AccessLogin, store.AccessRestricted:
		return true
	default:
		return false
	}
}
