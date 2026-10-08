package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
)

// /api/mail 分段：Webmail 阅读链路。所有端点只操作调用者自己的归属行，
// 越权访问统一由 service 层回 403（不泄露邮件是否存在）。

// handleMailList 邮件列表：?view=&unread=1&q=&limit=&offset=
func (s *Server) handleMailList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, offset := pagingOf(r)
	req := service.ListMailRequest{
		View:       q.Get("view"),
		OnlyUnread: q.Get("unread") == "1",
		Query:      q.Get("q"),
		Limit:      limit,
		Offset:     offset,
	}
	res, err := s.Svc.ListMail(r.Context(), p, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, res)
}

// handleMailDetail 邮件详情（打开即自动已读）。
func (s *Server) handleMailDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	d, err := s.Svc.GetMail(r.Context(), p, pathParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, d)
}

type mailReadRequest struct {
	Read bool `json:"read"`
}

func (s *Server) handleMailRead(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req mailReadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Svc.MarkMailRead(r.Context(), p, pathParam(r, "id"), req.Read); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

type mailStarRequest struct {
	Starred bool `json:"starred"`
}

func (s *Server) handleMailStar(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req mailStarRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Svc.StarMail(r.Context(), p, pathParam(r, "id"), req.Starred); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

func (s *Server) handleMailArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.ArchiveMyMail(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

func (s *Server) handleMailRestore(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.RestoreMyMail(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

func (s *Server) handleMailPurge(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.PurgeMyMail(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// ---- 我的邮箱地址 ----

// handleMailListDomains 返回本组可用于自助创建地址的域名。
//
// 与 handleMailListAddresses 分开而不是合并成一个返回：地址是"我已有的"，
// 域名是"我还能建在哪儿"，两者的生命周期不同——管理员关掉收件域名时，
// 用户已有的地址必须留着，域名列表却要立刻变空。
func (s *Server) handleMailListDomains(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	domains, err := s.Svc.ListMyMailDomains(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(domains)})
}

func (s *Server) handleMailListAddresses(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	addrs, err := s.Svc.ListMyMailAddresses(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(addrs)})
}

type mailAddressRequest struct {
	LocalPart string `json:"localPart"`
	Domain    string `json:"domain"`
}

func (s *Server) handleMailCreateAddress(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req mailAddressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	addr, err := s.Svc.CreateMyMailAddress(r.Context(), p, req.LocalPart, req.Domain)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"address": addr})
}

// ---- 部件借壳交付 ----

type mailPartDeliveryRequest struct {
	ClientPublicKey string `json:"clientPublicKey"`
}

func (s *Server) handleMailPartDelivery(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	partID, err := strconv.ParseInt(pathParam(r, "id"), 10, 64)
	if err != nil || partID <= 0 {
		writeErr(w, http.StatusBadRequest, "部件 ID 非法", "")
		return
	}
	var req mailPartDeliveryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	plan, err := s.Svc.PrepareMailPart(r.Context(), p, partID, req.ClientPublicKey)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, plan)
}

type mailExternalResourceRequest struct {
	BatchID string `json:"batchId"`
	URL     string `json:"url"`
	Kind    string `json:"kind"`
}

// handleMailExternalResource proxies one failed resource after the reader has
// explicitly confirmed the fallback. The message ownership check keeps this
// endpoint inside the caller's mailbox; the gateway then applies egress limits.
func (s *Server) handleMailExternalResource(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req mailExternalResourceRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "外部资源请求无效", "")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if len(req.BatchID) < 16 || len(req.BatchID) > 64 {
		writeErr(w, http.StatusBadRequest, "代理批次无效", "")
		return
	}
	if req.URL == "" || len(req.URL) > 4096 {
		writeErr(w, http.StatusBadRequest, "外部资源地址无效", "")
		return
	}
	if req.Kind != "image" && req.Kind != "style" && req.Kind != "font" {
		writeErr(w, http.StatusBadRequest, "外部资源类型不支持", "")
		return
	}
	if err := s.Svc.CheckMailAccess(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	mailSettings := s.Settings.Runtime(r.Context()).Mail
	resource, err := s.mailResources.Fetch(
		r.Context(), p.UserID(), req.BatchID, req.URL, req.Kind,
		mailSettings.ExternalResourceMaxItem, mailSettings.ExternalResourceMaxTotal,
	)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "外部资源代理加载失败", "请检查资源地址和资源类型后重试")
		return
	}
	w.Header().Set("Content-Type", resource.ContentType)
	w.Header().Set("Content-Location", resource.SourceURL)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(resource.Body)
}
