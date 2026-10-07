// 邮箱地址解绑：用户提交与撤销、管理员审核。
//
// 路由分两组：/api/mail/unbind-requests 归用户（只需 MailAccess），
// /api/admin/unbind-requests 归管理员（需 AdminUnbind）。两组分开而不是
// 靠同一个接口里的身份判断，是因为权限位不同——一个能用邮件的普通用户
// 不该触得到审核路径。
package httpapi

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
)

type unbindCreateRequest struct {
	Address string `json:"address"`
	Reason  string `json:"reason"`
}

type unbindReviewRequest struct {
	Note string `json:"note"`
}

// handleUnbindRequest 用户提交解绑申请。
func (s *Server) handleUnbindRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req unbindCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	created, err := s.Svc.RequestMyUnbind(r.Context(), p, req.Address, req.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, created)
}

// handleUnbindList 列出我提交过的申请。
func (s *Server) handleUnbindList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	items, err := s.Svc.ListMyUnbinds(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items)})
}

// handleUnbindCancel 用户撤销自己的待审申请。
func (s *Server) handleUnbindCancel(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := s.Svc.CancelMyUnbind(r.Context(), p, id); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"id": id})
}

// handleAdminUnbindList 管理端列表：单子加统计条。
func (s *Server) handleAdminUnbindList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := s.Svc.ListUnbindsForAdmin(r.Context(), p,
		q.Get("status"), q.Get("address"), q.Get("search"))
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, out)
}

// handleAdminUnbindApprove 批准：立刻删除地址。
func (s *Server) handleAdminUnbindApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var req unbindReviewRequest
	// 批注可选：没有请求体也要能批。
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	approved, err := s.Svc.ApproveUnbind(r.Context(), p, id, req.Note)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, approved)
}

// handleAdminUnbindReject 驳回：地址保留。
func (s *Server) handleAdminUnbindReject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var req unbindReviewRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	rejected, err := s.Svc.RejectUnbind(r.Context(), p, id, req.Note)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, rejected)
}

// pathInt64 读取路径里的 {id}。解析失败按 BadRequest 回应并返回 false。
func pathInt64(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(pathParam(r, key), 10, 64)
	if err != nil || id <= 0 {
		fail(w, fmt.Errorf("%w: 路径参数非法", service.ErrBadRequest))
		return 0, false
	}
	return id, true
}
