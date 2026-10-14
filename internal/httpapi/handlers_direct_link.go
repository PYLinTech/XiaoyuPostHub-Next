package httpapi

import (
	"mime"
	"net/http"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
)

// ---------------------------------------------------------------- 直链管理

type createDirectLinkRequest struct {
	Path      string `json:"path"`
	ExpiresAt int64  `json:"expiresAt"`
	MaxHits   int    `json:"maxHits"`
}

// handleCreateDirectLink 创建直链。
func (s *Server) handleCreateDirectLink(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.DirectLink)
	if !ok {
		return
	}
	var req createDirectLinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	link, err := s.Svc.CreateDirectLink(r.Context(), p, service.CreateDirectLinkRequest{
		Path:      req.Path,
		ExpiresAt: req.ExpiresAt,
		MaxHits:   req.MaxHits,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"link": link})
}

// handleListDirectLinks 列出当前用户的直链。
func (s *Server) handleListDirectLinks(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	links, err := s.Svc.ListDirectLinks(r.Context(), p, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(links)})
}

// handleUpdateDirectLink 修改直链的有效期、次数上限与启停。
func (s *Server) handleUpdateDirectLink(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req service.UpdateDirectLinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	link, err := s.Svc.UpdateDirectLink(r.Context(), p, pathParam(r, "id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"link": link})
}

// handleDeleteDirectLink 删除直链。
func (s *Server) handleDeleteDirectLink(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.DeleteDirectLink(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// handleNodeDirectLinks 列出某个节点上已创建的直链，服务「文件 → 直链」的
// 双向绑定：文件详情弹窗要能看到这份内容挂了哪些直链。
func (s *Server) handleNodeDirectLinks(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var checksums []string
	for _, raw := range strings.Split(r.URL.Query().Get("checksums"), ",") {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			checksums = append(checksums, trimmed)
		}
	}
	links, err := s.Svc.ListNodeDirectLinks(r.Context(), p, checksums)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(links)})
}

// ---------------------------------------------------------------- 直链交付

// handleDirectLink 是直链的数据面入口：GET/HEAD /d/{token}。
//
// 与 /s/{id} 的本质区别是它**直接返回文件字节而不是页面**——直链的请求方是
// wget、播放器、第三方脚本这类不跑本站前端、也没有 WebCrypto 的裸 HTTP 客户端，
// 它们要的就是明文字节。因此它必须注册成真实路由；一旦落进 SPA 的
// index.html 兜底，贴进 wget 就变成下载一个网页。
//
// 不需要会话：凭据全在 URL 上。
func (s *Server) handleDirectLink(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(pathParam(r, "token"))
	if token == "" {
		writeErr(w, http.StatusNotFound, "直链不存在", "")
		return
	}
	p := principalOf(r.Context())
	headOnly := r.Method == http.MethodHead
	delivery, err := s.Svc.ServeDirectLink(r.Context(), p, service.DirectLinkRequest{
		Token:       token,
		RangeHeader: strings.TrimSpace(r.Header.Get("Range")),
		HeadOnly:    headOnly,
	})
	if err != nil {
		fail(w, err)
		return
	}

	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": delivery.Filename}))
	s.writeServerStream(w, r, delivery.Actor, delivery.Stream)
}

// directLinksOnly 在鉴权与请求体解析前统一关闭管理面和数据面。
func (s *Server) directLinksOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Settings.Runtime(r.Context()).Site.DirectLinkEnabled {
			fail(w, service.ErrNotFound)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleDirectLinkTarget(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	path, err := s.Svc.DirectLinkTarget(r.Context(), actor, pathParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"path": path})
}
