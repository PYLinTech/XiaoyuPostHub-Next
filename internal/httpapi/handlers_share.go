package httpapi

import (
	"net/http"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// ---------------------------------------------------------------- 分享管理

type createShareRequest struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	AccessMode     string `json:"accessMode"`
	Password       string `json:"password"`
	AllowDownload  *bool  `json:"allowDownload"`
	AllowPreview   *bool  `json:"allowPreview"`
	AllowSubpath   *bool  `json:"allowSubpath"`
	ShowSharerName *bool  `json:"showSharerName"`
	ExpiresAt      int64  `json:"expiresAt"`
	MaxVisits      int    `json:"maxVisits"`
}

// handleCreateShare 创建分享。
func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req createShareRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	kind := store.ShareKind(strings.TrimSpace(req.Kind))
	mode := store.AccessMode(strings.TrimSpace(req.AccessMode))
	share, err := s.Svc.CreateShare(r.Context(), p, service.CreateShareRequest{
		Path:           req.Path,
		Kind:           kind,
		AccessMode:     mode,
		Password:       req.Password,
		AllowDownload:  boolOr(req.AllowDownload, true),
		AllowPreview:   boolOr(req.AllowPreview, true),
		AllowSubpath:   boolOr(req.AllowSubpath, true),
		ShowSharerName: boolOr(req.ShowSharerName, true),
		ExpiresAt:      req.ExpiresAt,
		MaxVisits:      req.MaxVisits,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"share": share})
}

// handleListShares 列出本人创建的分享。
func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	shares, err := s.Svc.ListShares(r.Context(), p, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(shares)})
}

type updateShareRequest struct {
	AccessMode      *string `json:"accessMode"`
	Password        *string `json:"password"`
	AllowDownload   *bool   `json:"allowDownload"`
	AllowPreview    *bool   `json:"allowPreview"`
	AllowSubpath    *bool   `json:"allowSubpath"`
	ShowSharerName  *bool   `json:"showSharerName"`
	ExpiresAt       *int64  `json:"expiresAt"`
	MaxVisits       *int    `json:"maxVisits"`
	Disabled        *bool   `json:"disabled"`
	ExpectUpdatedAt int64   `json:"expectUpdatedAt"`
}

// handleUpdateShare 修改分享。字段用指针表达"是否要改"，避免零值把已有
// 配置覆盖掉——否则想把 MaxVisits 改成 0 与"不想改"无法区分。
func (s *Server) handleUpdateShare(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req updateShareRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	update := service.UpdateShareRequest{
		Password:        req.Password,
		AllowDownload:   req.AllowDownload,
		AllowPreview:    req.AllowPreview,
		AllowSubpath:    req.AllowSubpath,
		ShowSharerName:  req.ShowSharerName,
		ExpiresAt:       req.ExpiresAt,
		MaxVisits:       req.MaxVisits,
		Disabled:        req.Disabled,
		ExpectUpdatedAt: req.ExpectUpdatedAt,
	}
	if req.AccessMode != nil {
		mode := store.AccessMode(*req.AccessMode)
		update.AccessMode = &mode
	}
	share, err := s.Svc.UpdateShare(r.Context(), p, pathParam(r, "id"), update)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"share": share})
}

// handleDeleteShare 删除分享（取件码与访问记录随之级联删除）。
func (s *Server) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.DeleteShare(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// handleShareAccesses 查看某分享的访问记录。
func (s *Server) handleShareAccesses(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	limit, _ := pagingOf(r)
	items, err := s.Svc.ListShareAccesses(r.Context(), p, pathParam(r, "id"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items)})
}

// ---------------------------------------------------------------- 取件码

type createPickupRequest struct {
	MaxUses int `json:"maxUses"`
}

// handleCreatePickup 为某分享生成取件码。取件码只在创建时返回一次明文。
//
// 有效期不在请求里：它是管理员统一配置（管理后台「分享与取件」分区），
// 单个取件码不允许用户自定义。
func (s *Server) handleCreatePickup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req createPickupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	code, err := s.Svc.CreatePickupCode(r.Context(), p, pathParam(r, "id"), req.MaxUses)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"pickup": code})
}

// handleListPickup 列出某分享下的取件码。
func (s *Server) handleListPickup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	items, err := s.Svc.ListPickupCodes(r.Context(), p, pathParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items)})
}

// handleDeletePickup 删除取件码。
func (s *Server) handleDeletePickup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.DeletePickupCode(r.Context(), p, pathParam(r, "code")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 访客入口

type shareAccessRequest struct {
	Password string `json:"password"`
	RelPath  string `json:"relPath"`
}

// guestShare / guestTarget 是面向访客的分享视图。resolve 端点对匿名访客
// 开放，直接回序列 store.Share / service.DeliveryTarget 会外泄属主的绝对
// 路径、账号标识与访问明细——这里只保留访客界面渲染所需的最小集合。
type guestShare struct {
	ID            string           `json:"id"`
	Kind          store.ShareKind  `json:"kind"`
	AccessMode    store.AccessMode `json:"accessMode"`
	HasPassword   bool             `json:"hasPassword"`
	AllowDownload bool             `json:"allowDownload"`
	AllowPreview  bool             `json:"allowPreview"`
	AllowSubpath  bool             `json:"allowSubpath"`
	ExpiresAt     int64            `json:"expiresAt,omitempty"`
	MaxVisits     int              `json:"maxVisits"`
	Visits        int              `json:"visits"`
	// RootName 是分享内容的展示名（不含属主路径层级）。
	RootName string `json:"rootName"`
	// IsOwner 供受限分享判断"访问者是否就是创建者"，替代暴露 ownerId。
	IsOwner    bool   `json:"isOwner"`
	SharerName string `json:"sharerName,omitempty"`
	Size       int64  `json:"size"`
}

// guestTarget 只保留展示名。访客的目录导航走 share 内相对路径，不消费绝对路径。
type guestTarget struct {
	Path string `json:"path"`
}

func guestShareView(share store.Share, p auth.Principal) (guestShare, guestTarget) {
	sharerName := ""
	if share.ShowSharerName {
		sharerName = share.SharerName
	}
	rootName := vpath.Base(share.RootPath)
	if share.RootPath == vpath.Root {
		rootName = "根目录"
	}
	return guestShare{
		ID:            share.ID,
		Kind:          share.Kind,
		AccessMode:    share.AccessMode,
		HasPassword:   share.HasPassword,
		AllowDownload: share.AllowDownload,
		AllowPreview:  share.AllowPreview,
		AllowSubpath:  share.AllowSubpath,
		ExpiresAt:     share.ExpiresAt,
		MaxVisits:     share.MaxVisits,
		Visits:        share.Visits,
		RootName:      rootName,
		SharerName:    sharerName,
		Size:          share.Size,
		IsOwner:       !p.IsGuest() && p.UserID() == share.OwnerID,
	}, guestTarget{Path: rootName}
}

// handleShareResolve 让访客确认分享可用并取得根节点信息。
func (s *Server) handleShareResolve(w http.ResponseWriter, r *http.Request) {
	var req shareAccessRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	_, share, err := s.Svc.ResolveShare(r.Context(), pathParam(r, "id"), req.Password, principalOf(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	gs, gt := guestShareView(share, principalOf(r.Context()))
	writeData(w, map[string]any{"share": gs, "target": gt})
}

// handleShareList 列出分享内的目录内容（文件夹分享浏览）。
func (s *Server) handleShareList(w http.ResponseWriter, r *http.Request) {
	var req shareAccessRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	items, err := s.Svc.ListShareDir(r.Context(), pathParam(r, "id"), req.Password, req.RelPath, principalOf(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items)})
}

type shareDeliveryRequest struct {
	Password        string `json:"password"`
	RelPath         string `json:"relPath"`
	ClientPublicKey string `json:"clientPublicKey"`
}

// handleShareDownload 访客下载分享内容。
//
// 与登录用户走同一条准备路径：交付方式由存储策略决定，不因身份改写，
// 否则会出现"登录能下、访客拿不到"的不一致。
func (s *Server) handleShareDownload(w http.ResponseWriter, r *http.Request) {
	s.shareDelivery(w, r, store.PurposeDownload)
}

// handleSharePreview 访客预览分享内容。
func (s *Server) handleSharePreview(w http.ResponseWriter, r *http.Request) {
	s.shareDelivery(w, r, store.PurposePreview)
}

func (s *Server) shareDelivery(w http.ResponseWriter, r *http.Request, purpose store.Purpose) {
	var req shareDeliveryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	actor := principalOf(r.Context())
	target, _, _, err := s.Svc.ResolveSharePath(r.Context(), pathParam(r, "id"), req.Password, req.RelPath, actor)
	if err != nil {
		fail(w, err)
		return
	}
	plan, err := s.Svc.PrepareDelivery(r.Context(), actor, target, service.DeliveryRequest{
		Purpose:         purpose,
		ClientPublicKey: req.ClientPublicKey,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, plan)
}

// handlePickupResolve 用取件码换出交付目标，**不消耗**使用额度。
//
// 它只服务于"先看到内容是什么、再决定要不要下载"的界面流程；真正的取数走
// /api/p/{code}/download。如果这里也扣次数，maxUses=1（默认值）的取件码会在
// 用户点下下载之前就用尽。
func (s *Server) handlePickupResolve(w http.ResponseWriter, r *http.Request) {
	_, share, err := s.Svc.PeekPickupCode(r.Context(), pathParam(r, "code"), principalOf(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	gs, gt := guestShareView(share, principalOf(r.Context()))
	writeData(w, map[string]any{"share": gs, "target": gt})
}

type pickupDeliveryRequest struct {
	ClientPublicKey string `json:"clientPublicKey"`
}

// handlePickupDownload 用取件码直接下载分享根内容。
//
// 三步的顺序是刻意的：校验（不扣额度）→ 准备交付 → 核销额度。
// 若把核销提前，一次注定失败的交付（对象不可交付、存储未就绪、文件被拉黑）
// 会白吃掉一次额度，而 maxUses 的默认值正是 1——用户会拿到一个已经作废的
// 取件码，却什么都没有得到。
func (s *Server) handlePickupDownload(w http.ResponseWriter, r *http.Request) {
	var req pickupDeliveryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	actor := principalOf(r.Context())
	code := pathParam(r, "code")

	target, _, err := s.Svc.PeekPickupCode(r.Context(), code, actor)
	if err != nil {
		fail(w, err)
		return
	}
	plan, err := s.Svc.PrepareDelivery(r.Context(), actor, target, service.DeliveryRequest{
		Purpose:         store.PurposeDownload,
		ClientPublicKey: req.ClientPublicKey,
	})
	if err != nil {
		fail(w, err)
		return
	}
	// 并发下只有一次核销能命中。落空（额度已被别人用掉）时必须把已经准备好的
	// 交付撤掉：释放预扣的额度，票据本身随有效期自然失效。
	if err := s.Svc.ClaimPickupUseForActor(r.Context(), code, actor); err != nil {
		_ = s.Svc.CancelDelivery(r.Context(), actor, plan.TicketID)
		fail(w, err)
		return
	}
	writeData(w, plan)
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}
