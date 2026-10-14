package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// 直链是「分享」的直链版本：同样的 token、有效期、次数上限、停用与乐观锁，
// 差别在绑定物与出口。
//
// 绑定物是内容哈希而不是逻辑路径，因此重命名与跨目录移动都不影响它；反向的
// 失效由内容池状态（内容被删或拉黑）与删除联动（用户删除自己的文件）承担。
//
// 出口是服务端中转解密：直链的请求方是 wget、播放器、第三方脚本这类不跑本站
// 前端、也没有 WebCrypto 的裸 HTTP 客户端，它们要的就是明文字节。这条通道
// 复用既有的 proxy_decrypt 交付管线，密码学上没有新增能力——但它确实意味着
// 文件明文会流经服务器，这是它与分享最本质的代价差异。

// CreateDirectLinkRequest 是新建直链的入参。
type CreateDirectLinkRequest struct {
	// Path 是创建时的逻辑路径，只用于确认归属与展示快照。
	Path string
	// ExpiresAt 为 0 表示永久；MaxHits 为 0 表示不限。
	ExpiresAt int64
	MaxHits   int
}

// DirectLinkRequest 是一次直链交付请求。
type DirectLinkRequest struct {
	Token string
	// RangeHeader 原样透传给区间解析，含后缀式区间。
	RangeHeader string
	// HeadOnly 表示只取响应元数据：不打开对象、不计命中、不预扣流量。
	HeadOnly bool
}

// DirectLinkDelivery 是一次直链交付的完整结果。
type DirectLinkDelivery struct {
	Stream ServerStream
	// Actor 是开流及结算共用的访客身份，计费仍按原请求身份确定。
	Actor    auth.Principal
	Filename string
}

// CreateDirectLink 为自己名下的一份文件创建直链。
//
// 只支持文件：直链绑的是内容哈希，目录直链要为每个子项单独签发令牌，是另一个
// 数量级的设计。与分享一样，创建前必须确认路径属于创建者——越权路径在这里
// 等于把别人的文件包装成自己的直链。
func (s *Service) CreateDirectLink(ctx context.Context, p auth.Principal, req CreateDirectLinkRequest) (store.DirectLink, error) {
	if err := s.requireDirectLinks(ctx); err != nil {
		return store.DirectLink{}, err
	}
	if err := auth.RequirePermission(p, perm.DirectLink); err != nil {
		return store.DirectLink{}, err
	}
	if req.ExpiresAt < 0 || req.MaxHits < 0 {
		return store.DirectLink{}, fmt.Errorf("%w: 有效期与次数上限不得为负", ErrBadRequest)
	}
	norm, err := vpath.Normalize(req.Path)
	if err != nil {
		return store.DirectLink{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	node, err := s.requireOwnNode(ctx, p.UserID(), norm)
	if err != nil {
		return store.DirectLink{}, err
	}
	if node.IsFolder() {
		return store.DirectLink{}, fmt.Errorf("%w: 直链只支持单个文件，目录请用分享", ErrBadRequest)
	}
	file, err := store.GetFile(ctx, s.DB.R(), node.FileChecksum)
	if err != nil {
		return store.DirectLink{}, err
	}
	if file.Status != store.FileNormal {
		return store.DirectLink{}, fmt.Errorf("%w: 该文件当前不可用", ErrForbidden)
	}

	// id 在这里生成而不是交给 store 层：返回值必须带上它交给 HTTP 层。
	id, err := store.GenerateToken(16)
	if err != nil {
		return store.DirectLink{}, err
	}
	now := s.Now()
	link := store.DirectLink{
		ID:           strings.ToLower(id),
		OwnerID:      p.UserID(),
		FileChecksum: file.Checksum,
		SourcePath:   norm,
		SourceName:   node.Name,
		// 文件名与类型冻结下来：每次交付都去查节点名，会在匿名数据面上
		// 多一次数据库读取，而文件重命名并不需要反映到已有直链上。
		MimeType:  mimeOf(node.Name),
		SizePlain: file.SizePlain,
		ExpiresAt: req.ExpiresAt,
		MaxHits:   req.MaxHits,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateDirectLink(ctx, s.DB.W(), link); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.DirectLink{}, fmt.Errorf("%w: 直链标识冲突，请重试", ErrConflict)
		}
		return store.DirectLink{}, err
	}
	link.TargetAvailable = true
	s.audit(ctx, p, "directlink.create", link.ID, norm)
	return link, nil
}

// ListDirectLinks 列出当前用户创建的直链。
//
// 只要求登录、不要求 perm.DirectLink：权限被回收后用户仍应能查看并删除自己
// 已经建出来的直链，否则那些链接会永久留在线上且无人可关。
func (s *Service) ListDirectLinks(ctx context.Context, p auth.Principal, limit, offset int) ([]store.DirectLink, error) {
	if err := s.requireDirectLinks(ctx); err != nil {
		return nil, err
	}
	if err := auth.RequireUser(p); err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	return store.ListDirectLinksByOwner(ctx, s.DB.R(), p.UserID(), limit, offset)
}

// ListNodeDirectLinks 列出某用户在一批内容上的直链，服务「文件 → 直链」的
// 双向绑定：文件详情里要能看到这份内容上挂了哪些直链。
func (s *Service) ListNodeDirectLinks(ctx context.Context, p auth.Principal, checksums []string) ([]store.DirectLink, error) {
	if err := s.requireDirectLinks(ctx); err != nil {
		return nil, err
	}
	if err := auth.RequireUser(p); err != nil {
		return nil, err
	}
	return store.ListDirectLinksByOwnerAndChecksums(ctx, s.DB.R(), p.UserID(), checksums)
}

// UpdateDirectLinkRequest 只更新非 nil 字段；显式的 0 和 false 仍会写入。
type UpdateDirectLinkRequest struct {
	ExpiresAt       *int64 `json:"expiresAt"`
	MaxHits         *int   `json:"maxHits"`
	Disabled        *bool  `json:"disabled"`
	ExpectUpdatedAt int64  `json:"expectUpdatedAt"`
}

// UpdateDirectLink 修改有效期、次数上限与停用状态，内容绑定保持不变。
func (s *Service) UpdateDirectLink(ctx context.Context, p auth.Principal, id string, req UpdateDirectLinkRequest) (store.DirectLink, error) {
	if err := s.requireDirectLinks(ctx); err != nil {
		return store.DirectLink{}, err
	}
	if err := auth.RequireUser(p); err != nil {
		return store.DirectLink{}, err
	}
	link, err := s.requireOwnDirectLink(ctx, p.UserID(), id)
	if err != nil {
		return store.DirectLink{}, err
	}
	next := link
	if req.ExpiresAt != nil {
		next.ExpiresAt = *req.ExpiresAt
	}
	if req.MaxHits != nil {
		next.MaxHits = *req.MaxHits
	}
	if req.Disabled != nil {
		next.Disabled = *req.Disabled
	}
	if next.ExpiresAt < 0 || next.MaxHits < 0 {
		return store.DirectLink{}, fmt.Errorf("%w: 有效期与次数上限不得为负", ErrBadRequest)
	}
	expectUpdatedAt := req.ExpectUpdatedAt
	if expectUpdatedAt == 0 {
		expectUpdatedAt = link.UpdatedAt
	}
	if err := store.UpdateDirectLinkIfUnchanged(ctx, s.DB.W(), next, expectUpdatedAt); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.DirectLink{}, fmt.Errorf("%w: 直链已被修改，请刷新后重试", ErrConflict)
		}
		return store.DirectLink{}, err
	}
	updated, err := store.GetDirectLink(ctx, s.DB.R(), link.ID)
	if err != nil {
		return store.DirectLink{}, err
	}
	if updated.Disabled {
		s.cancelDirectLinkStreams(updated.ID)
	}
	s.audit(ctx, p, "directlink.update", updated.ID, fmt.Sprintf("expires=%d max=%d disabled=%t", next.ExpiresAt, next.MaxHits, next.Disabled))
	return updated, nil
}

// DeleteDirectLink 删除直链。
//
// 不碰内容池：直链是旁路凭证而不是所有权，删掉它不该改变任何对象的存亡。
func (s *Service) DeleteDirectLink(ctx context.Context, p auth.Principal, id string) error {
	if err := s.requireDirectLinks(ctx); err != nil {
		return err
	}
	if err := auth.RequireUser(p); err != nil {
		return err
	}
	link, err := s.requireOwnDirectLink(ctx, p.UserID(), id)
	if err != nil {
		return err
	}
	if err := store.DeleteDirectLink(ctx, s.DB.W(), link.ID); err != nil {
		return err
	}
	s.cancelDirectLinkStreams(link.ID)
	s.audit(ctx, p, "directlink.delete", link.ID, link.SourcePath)
	return nil
}

func (s *Service) requireOwnDirectLink(ctx context.Context, ownerID int64, id string) (store.DirectLink, error) {
	link, err := store.GetDirectLink(ctx, s.DB.R(), strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.DirectLink{}, ErrNotFound
		}
		return store.DirectLink{}, err
	}
	if link.OwnerID != ownerID {
		// 他人的直链一律按不存在处理：区分"存在但不是你的"等于提供一个
		// 枚举直链是否存在的探针。
		return store.DirectLink{}, ErrNotFound
	}
	return link, nil
}

// ServeDirectLink 处理一次直链交付。
//
// 判定链固定为：总开关 → 直链存在 → 创建者可用 → 未停用 → 未过期 →
// 未超次数 → 绑定内容可用 → 原子计命中 → 预扣流量并签发票据 → 复用中转解密。
//
// 每一步都是拒绝式的，不存在降级路径：拿不到明文就返回错误，绝不退回"返回
// 密文让客户端自己解"——那会让一条本该交付明文的直链变成内容泄露的旁路。
func (s *Service) ServeDirectLink(ctx context.Context, actor auth.Principal, req DirectLinkRequest) (DirectLinkDelivery, error) {
	if err := s.requireDirectLinks(ctx); err != nil {
		return DirectLinkDelivery{}, err
	}
	link, err := store.GetDirectLink(ctx, s.DB.R(), strings.TrimSpace(req.Token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DirectLinkDelivery{}, ErrNotFound
		}
		return DirectLinkDelivery{}, err
	}
	file, err := store.GetFile(ctx, s.DB.R(), link.FileChecksum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DirectLinkDelivery{}, ErrNotFound
		}
		return DirectLinkDelivery{}, err
	}
	stream, err := directLinkStreamMetadata(link, file, req.RangeHeader)
	if err != nil {
		return DirectLinkDelivery{}, err
	}
	out := DirectLinkDelivery{Stream: stream, Filename: link.SourceName}

	// HEAD 只确认响应元数据：不计命中、不预扣流量、不签发票据。
	// 媒体元素与 curl -I 都会先探一次长度，把它当 GET 会凭空烧掉一次
	// 次数上限和一份流量额度。
	if req.HeadOnly {
		// HEAD 与 GET 使用相同的有效性条件，但不消费命中次数。
		owner, err := store.GetUserByID(ctx, s.DB.R(), link.OwnerID)
		if err != nil {
			return DirectLinkDelivery{}, err
		}
		if link.Disabled || (link.ExpiresAt > 0 && link.ExpiresAt <= s.Now()) ||
			(link.MaxHits > 0 && link.Hits >= link.MaxHits) ||
			owner.Status != store.UserEnabled || file.Status != store.FileNormal {
			return DirectLinkDelivery{}, fmt.Errorf("%w: 直链已失效、已过期、已达次数上限或绑定内容不可用", ErrForbidden)
		}
		return out, nil
	}

	guest, err := s.Auth.GuestPrincipal(ctx, actor.ClientIP, actor.IPPrefix)
	if err != nil {
		return DirectLinkDelivery{}, err
	}
	ticketID, err := store.NewID("tk")
	if err != nil {
		return DirectLinkDelivery{}, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	s.activeDirectLinks.Store(ticketID, activeDirectLink{linkID: link.ID, cancel: cancel})
	opened := false
	defer func() {
		if !opened {
			cancel()
			s.activeDirectLinks.Delete(ticketID)
		}
	}()
	plainBytes := file.SizePlain - stream.Offset
	if stream.Limit > 0 && stream.Limit < plainBytes {
		plainBytes = stream.Limit
	}
	ticket, err := s.issueDirectLinkTicket(ctx, actor, ticketID, link, file, plainBytes)
	if err != nil {
		return DirectLinkDelivery{}, err
	}
	stream, err = s.serverStreamForActor(streamCtx, ticket.ID, guest, req.RangeHeader, true)
	if err != nil {
		rollbackCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stop()
		if rollbackErr := s.rollbackDirectLink(rollbackCtx, ticket); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		return DirectLinkDelivery{}, err
	}
	stream.Reader = &directLinkReader{ReadCloser: stream.Reader, svc: s, ctx: streamCtx, ticketID: ticket.ID, cancel: cancel}
	opened = true
	stream.MimeType = out.Stream.MimeType
	out.Stream = stream
	out.Actor = guest
	return out, nil
}

// directLinkStreamMetadata 统一 GET/HEAD 的区间与响应元数据，不改变状态。
func directLinkStreamMetadata(link store.DirectLink, file store.File, rangeHeader string) (ServerStream, error) {
	offset, limit, ranged, err := parseByteRange(rangeHeader, file.SizePlain)
	if err != nil {
		return ServerStream{}, fmt.Errorf("%w: %v", ErrRangeNotSatisfiable, err)
	}
	if ranged && file.SizePlain == 0 {
		return ServerStream{}, fmt.Errorf("%w: 空对象不支持区间请求", ErrRangeNotSatisfiable)
	}
	mimeType := link.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return ServerStream{
		MimeType:       mimeType,
		File:           file,
		ContentForm:    ContentFormPlaintext,
		PlainSize:      file.SizePlain,
		CipherSize:     file.PanSizeWire,
		RangeRequested: ranged,
		Offset:         offset,
		Limit:          limit,
	}, nil
}

// issueDirectLinkTicket 预扣流量并签发一张中转解密票据。
//
// 票据是每请求现签的一次性凭据：直链的 URL 自身才是长期凭据。复用票据表而不是
// 把直链塞进票据字段，是为了让计费、结算、退款、过期回收整条链路原样复用。
func (s *Service) issueDirectLinkTicket(ctx context.Context, actor auth.Principal, ticketID string, link store.DirectLink, file store.File, plainBytes int64) (store.Ticket, error) {
	ttl := s.ticketTTL(ctx)
	if ttl <= 0 {
		return store.Ticket{}, fmt.Errorf("%w: 交付票据参数配置无效", ErrUnavailable)
	}
	reserved := xph.CipherSizeOf(plainBytes, byte(file.EncChunkLog2))
	createdAt := s.Now()
	ticket := store.Ticket{
		ID:            ticketID,
		FileChecksum:  file.Checksum,
		ActorType:     store.ActorGuest,
		ClientIP:      actor.ClientIP.String(),
		IPPrefix:      actor.IPPrefix,
		GroupName:     perm.GroupGuest,
		Purpose:       store.PurposeDownload,
		DeliveryMode:  store.TicketProxyDecrypt,
		DirectLinkID:  link.ID,
		ReservedBytes: reserved,
		// 每个直链请求单独预扣并结算，票据只允许本次开流消费一次。
		MaxUses:   1,
		ExpiresAt: time.Now().UTC().Add(ttl),
		CreatedAt: createdAt,
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := s.requireDirectLinks(ctx); err != nil {
			return err
		}
		if err := store.ConsumeDirectLinkHit(ctx, tx, link.ID, createdAt); err != nil {
			if errors.Is(err, store.ErrNoRowsAffected) {
				return fmt.Errorf("%w: 直链已失效或内容不可用", ErrForbidden)
			}
			return err
		}
		billing, err := s.reserveDeliveryBillingQ(ctx, tx, actor, DeliveryTarget{
			OwnerUserID:  link.OwnerID,
			Path:         link.SourcePath,
			DirectLinkID: link.ID,
			Checksum:     link.FileChecksum,
		}, createdAt, reserved)
		if err != nil {
			return err
		}
		ticket.QuotaScope, ticket.QuotaKey = billing.scope, billing.key
		ticket.BilledUserID, ticket.BilledGroupName = billing.userID, billing.groupName
		ticket.ResourcePath = link.SourcePath
		return store.CreateTicket(ctx, tx, ticket)
	}); err != nil {
		return store.Ticket{}, err
	}
	return ticket, nil
}

func (s *Service) requireDirectLinks(ctx context.Context) error {
	if !s.Settings.Runtime(ctx).Site.DirectLinkEnabled {
		return ErrNotFound
	}
	return nil
}

// rollbackDirectLink 只回滚尚未交付的请求：预扣、票据和次数必须一并撤回。
func (s *Service) rollbackDirectLink(ctx context.Context, ticket store.Ticket) error {
	return s.DB.InTx(ctx, func(tx store.Querier) error {
		canceled, err := store.CancelTicket(ctx, tx, ticket.ID)
		if err != nil || !canceled {
			return err
		}
		if err := s.releaseDeliveryQuotaByTicketQ(ctx, tx, ticket, ticket.ReservedBytes); err != nil {
			return err
		}
		return store.ReleaseDirectLinkHit(ctx, tx, ticket.DirectLinkID)
	})
}

// directLinkReader 将活动票据的生命周期绑定到响应流，并让总开关中断在途交付。
type directLinkReader struct {
	io.ReadCloser
	svc      *Service
	ctx      context.Context
	ticketID string
	cancel   context.CancelFunc
	once     sync.Once
}

func (r *directLinkReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.svc.requireDirectLinks(r.ctx); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(p)
}
func (r *directLinkReader) Close() error {
	var err error
	r.once.Do(func() { err = r.ReadCloser.Close(); r.cancel(); r.svc.activeDirectLinks.Delete(r.ticketID) })
	return err
}

type activeDirectLink struct {
	linkID string
	cancel context.CancelFunc
}

func (s *Service) cancelDirectLinkStreams(linkID string) {
	s.activeDirectLinks.Range(func(_, value any) bool {
		active := value.(activeDirectLink)
		if linkID == "" || active.linkID == linkID {
			active.cancel()
		}
		return true
	})
}

// DirectLinkTarget 按内容定位当前用户的文件，避免创建时的路径快照在移动后失效。
func (s *Service) DirectLinkTarget(ctx context.Context, actor auth.Principal, id string) (string, error) {
	if err := s.requireDirectLinks(ctx); err != nil {
		return "", err
	}
	if err := auth.RequireUser(actor); err != nil {
		return "", err
	}
	link, err := s.requireOwnDirectLink(ctx, actor.UserID(), id)
	if err != nil {
		return "", err
	}
	node, err := store.FindOwnedNodeByChecksum(ctx, s.DB.R(), actor.UserID(), link.FileChecksum)
	if err != nil {
		return "", err
	}
	return node.LogicalPath, nil
}
