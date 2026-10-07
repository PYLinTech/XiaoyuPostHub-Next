package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// handleAnnouncements 返回当前访问者可见的公告与滚动公告。
//
// 访客只能看到"全体"范围的公告（定向消息的接收者是账号，访客没有账号）；
// 已读状态按需求由前端本地保存，服务端不记录。
func (s *Server) handleAnnouncements(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r.Context())
	var kinds []store.AnnouncementKind
	for _, raw := range strings.Split(r.URL.Query().Get("kinds"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		kinds = append(kinds, store.AnnouncementKind(raw))
	}
	items, err := s.Svc.ListVisibleAnnouncements(r.Context(), p, kinds)
	if err != nil {
		fail(w, err)
		return
	}
	ticker, err := s.Svc.ActiveTicker(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ticker": ticker, "items": itemsOf(items)})
}

// ---------------------------------------------------------------- 概览

// handleAdminOverview 返回站点概览。
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminAudit)
	if !ok {
		return
	}
	overview, err := s.Svc.AdminOverview(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, overview)
}

// ---------------------------------------------------------------- 账号

// handleAdminListUsers 分页列出账号。
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminUsers)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	users, err := s.Svc.AdminListUsers(r.Context(), p, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(users)})
}

type statusRequest struct {
	Enabled bool `json:"enabled"`
}

// handleAdminUserStatus 启用或禁用账号。
func (s *Server) handleAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminUsers)
	if !ok {
		return
	}
	var req statusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.AdminSetUserStatus(r.Context(), p, id, req.Enabled); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

type groupRequest struct {
	GroupName string `json:"groupName"`
}

// handleAdminUserGroup 变更账号所属组。
func (s *Server) handleAdminUserGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminUsers)
	if !ok {
		return
	}
	var req groupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.AdminSetUserGroup(r.Context(), p, id, req.GroupName); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

type passwordRequest struct {
	NewPassword string `json:"newPassword"`
}

// handleAdminUserPassword 重置某个账号的口令，并吊销其全部会话。
func (s *Server) handleAdminUserPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminUsers)
	if !ok {
		return
	}
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.AdminChangePassword(r.Context(), p, id, req.NewPassword); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 用户组

// handleAdminListGroups 列出用户组及其配额与成员数。
func (s *Server) handleAdminListGroups(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminGroups)
	if !ok {
		return
	}
	groups, err := s.Svc.AdminListGroups(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(groups)})
}

type saveGroupRequest struct {
	Name        string           `json:"name"`
	DisplayName string           `json:"displayName"`
	Permissions int64            `json:"permissions"`
	Priority    int              `json:"priority"`
	Quotas      map[string]int64 `json:"quotas"`
	// 收件域名随组一起提交。指针区分三态：没这个键 = 不动域名，[] = 解绑全部，
	// 非空列表 = 该组应当托管的完整域名集合（不是增量追加）。少了这个区分，
	// 只想调配额的调用就会顺手把域名全解绑掉。
	ReceiveDomains *[]service.GroupMailDomainInput `json:"receiveDomains"`
}

// handleAdminSaveGroup 新建或修改用户组。
func (s *Server) handleAdminSaveGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminGroups)
	if !ok {
		return
	}
	var req saveGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	group, err := s.Svc.AdminSaveGroup(r.Context(), p, service.SaveGroupRequest{
		Name:           req.Name,
		DisplayName:    req.DisplayName,
		Permissions:    req.Permissions,
		Priority:       req.Priority,
		Quotas:         req.Quotas,
		ReceiveDomains: req.ReceiveDomains,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"group": group})
}

// handleAdminDeleteGroup 删除用户组。预设组与仍有成员的组会被拒绝。
func (s *Server) handleAdminDeleteGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminGroups)
	if !ok {
		return
	}
	if err := s.Svc.AdminDeleteGroup(r.Context(), p, pathParam(r, "name")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 流量与审计

// handleAdminTraffic 查询流量明细。
func (s *Server) handleAdminTraffic(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminAudit)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	items, total, err := s.Svc.AdminListTraffic(r.Context(), p, store.TrafficFilter{
		ActorType: store.ActorType(strings.TrimSpace(r.URL.Query().Get("actorType"))),
		UserID:    int64Query(r, "userId", 0),
		ClientIP:  strings.TrimSpace(r.URL.Query().Get("clientIp")),
		GroupName: strings.TrimSpace(r.URL.Query().Get("groupName")),
		Action:    strings.TrimSpace(r.URL.Query().Get("action")),
		From:      timeQuery(r, "from"),
		To:        timeQuery(r, "to"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items), "total": total})
}

// handleAdminAudit 查询审计记录。
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminAudit)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	items, total, err := s.Svc.AdminListAudit(r.Context(), p,
		strings.TrimSpace(r.URL.Query().Get("action")),
		strings.TrimSpace(r.URL.Query().Get("actionPrefix")),
		timeQuery(r, "from"), timeQuery(r, "to"), limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items), "total": total})
}

// ---------------------------------------------------------------- 公告管理

// handleAdminListAnnouncements 列出全部公告（含停用）。
func (s *Server) handleAdminListAnnouncements(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminAnnouncements)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	items, err := s.Svc.AdminListAnnouncements(r.Context(), p,
		store.AnnouncementKind(strings.TrimSpace(r.URL.Query().Get("kind"))), limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items)})
}

type announcementRequest struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Audience string  `json:"audience"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	Enabled  bool    `json:"enabled"`
	Pinned   bool    `json:"pinned"`
	ExpireAt int64   `json:"expireAt"`
	Targets  []int64 `json:"targets"`
}

// handleAdminSaveAnnouncement 新建或更新公告。
func (s *Server) handleAdminSaveAnnouncement(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminAnnouncements)
	if !ok {
		return
	}
	var req announcementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a, err := s.Svc.SaveAnnouncement(r.Context(), p, store.Announcement{
		ID:       strings.TrimSpace(req.ID),
		Kind:     store.AnnouncementKind(req.Kind),
		Audience: store.Audience(req.Audience),
		Title:    req.Title,
		Body:     req.Body,
		Enabled:  req.Enabled,
		Pinned:   req.Pinned,
		ExpireAt: req.ExpireAt,
		Targets:  req.Targets,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"announcement": a})
}

// handleAdminDeleteAnnouncement 删除公告。
func (s *Server) handleAdminDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminAnnouncements)
	if !ok {
		return
	}
	if err := s.Svc.DeleteAnnouncement(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 邀请码

// handleAdminListInvites 列出邀请码。
func (s *Server) handleAdminListInvites(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminInvites)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	items, err := s.Svc.AdminListInviteCodes(r.Context(), p, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items)})
}

type createInviteRequest struct {
	GroupName string `json:"groupName"`
	MaxUses   int    `json:"maxUses"`
	ExpiresAt int64  `json:"expiresAt"`
	Note      string `json:"note"`
}

// handleAdminCreateInvite 新建邀请码。
//
// 明文只在创建时返回一次，之后库里只有哈希——短码的哈希如果可读，
// 等于给了离线爆破的素材。
func (s *Server) handleAdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminInvites)
	if !ok {
		return
	}
	var req createInviteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	code, plaintext, err := s.Svc.AdminCreateInviteCode(r.Context(), p, service.CreateInviteRequest{
		GroupName: req.GroupName,
		MaxUses:   req.MaxUses,
		ExpiresAt: req.ExpiresAt,
		Note:      req.Note,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"invite": code, "code": plaintext})
}

// handleAdminInviteStatus 启用或停用邀请码。
func (s *Server) handleAdminInviteStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminInvites)
	if !ok {
		return
	}
	var req statusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.AdminSetInviteDisabled(r.Context(), p, id, !req.Enabled); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// handleAdminInviteUses 列出某个邀请码的使用记录。
func (s *Server) handleAdminInviteUses(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminInvites)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit, _ := pagingOf(r)
	uses, err := s.Svc.AdminListInviteUses(r.Context(), p, id, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(uses)})
}

// handleAdminDeleteInvite 删除邀请码。
func (s *Server) handleAdminDeleteInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminInvites)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.AdminDeleteInviteCode(r.Context(), p, id); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 文件拉黑

type fileStatusRequest struct {
	Disabled bool   `json:"disabled"`
	Reason   string `json:"reason"`
}

// handleAdminListNodes 全站文件检索。
//
// 与 handleAdminFileStatus 配对：那一支按内容校验码处置对象，这一支按
// 名称/路径/属主把对象找出来。管理员先在这里搜到东西，才知道该对哪个
// 校验码动手——原先只有后者，等于盲查。
func (s *Server) handleAdminListNodes(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminFiles)
	if !ok {
		return
	}
	// limit 的上限由 service 层统一裁决（MaxPageLimit）。这里不再重复压一次：
	// 两处各写一个数，改了一处忘了另一处就会让"回显的分页值"与实际页长不符。
	limit, offset := pagingOf(r)
	q := r.URL.Query()
	res, err := s.Svc.AdminListNodes(r.Context(), p, service.AdminListNodesRequest{
		Query:     q.Get("q"),
		Owner:     q.Get("owner"),
		NodeType:  q.Get("nodeType"),
		FileState: q.Get("state"),
		UserID:    int64Query(r, "userId", 0),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, res)
}

// handleAdminFileStatus 拉黑或解除拉黑一个内容对象。
//
// 拉黑是全局动作：同一对象被多人引用时只有一份状态，处置它会影响所有引用者；
// 同时必须吊销已签发但尚未过期的票据，否则"拉黑"在票据有效期内形同虚设。
func (s *Server) handleAdminFileStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminFiles)
	if !ok {
		return
	}
	var req fileStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	checksum := pathParam(r, "checksum")
	if err := s.Svc.AdminSetFileStatus(r.Context(), p, checksum, req.Disabled, req.Reason); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// pathID 解析路径参数里的整数 id。
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(pathParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "标识不正确", "")
		return 0, false
	}
	return id, true
}
