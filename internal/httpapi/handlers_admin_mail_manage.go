package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
)

// /api/admin/mail 全局邮件视图：跨用户列表、只读详情、归属行回收/恢复/彻底删除。
// 全部端点要求 perm.AdminMail。

func (s *Server) handleAdminMailListMessages(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminMail)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, offset := pagingOf(r)
	res, err := s.Svc.AdminListMail(r.Context(), p, service.AdminListMailRequest{
		UserID: int64Query(r, "userId", 0),
		Owner:  q.Get("owner"),
		Role:   q.Get("role"),
		Status: q.Get("status"),
		Query:  q.Get("q"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, res)
}

func (s *Server) handleAdminMailMailboxDetail(w http.ResponseWriter, r *http.Request) {
	p, boxID, ok := s.adminMailboxOp(w, r)
	if !ok {
		return
	}
	d, err := s.Svc.AdminGetMail(r.Context(), p, boxID)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, d)
}

func (s *Server) handleAdminMailArchive(w http.ResponseWriter, r *http.Request) {
	s.adminMailboxAction(w, r, s.Svc.AdminArchiveMail)
}

func (s *Server) handleAdminMailRestore(w http.ResponseWriter, r *http.Request) {
	s.adminMailboxAction(w, r, s.Svc.AdminRestoreMail)
}

func (s *Server) handleAdminMailPurge(w http.ResponseWriter, r *http.Request) {
	s.adminMailboxAction(w, r, s.Svc.AdminPurgeMail)
}

// adminMailboxAction 是三个归属处置端点的共用骨架。
func (s *Server) adminMailboxAction(w http.ResponseWriter, r *http.Request,
	fn func(context.Context, auth.Principal, int64) error) {
	p, boxID, ok := s.adminMailboxOp(w, r)
	if !ok {
		return
	}
	if err := fn(r.Context(), p, boxID); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// adminMailboxOp 统一鉴权与路径 ID 解析；ok=false 时响应已写好。
func (s *Server) adminMailboxOp(w http.ResponseWriter, r *http.Request) (auth.Principal, int64, bool) {
	p, ok := s.requirePerm(w, r, perm.AdminMail)
	if !ok {
		return auth.Principal{}, 0, false
	}
	boxID, err := strconv.ParseInt(pathParam(r, "id"), 10, 64)
	if err != nil || boxID <= 0 {
		writeErr(w, http.StatusBadRequest, "请求参数不正确", "归属 ID 非法")
		return auth.Principal{}, 0, false
	}
	return p, boxID, true
}
