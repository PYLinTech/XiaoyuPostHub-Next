package httpapi

import (
	"net/http"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/sdkasset"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
)

// apiKeysOnly 在鉴权前统一关闭 API 管理面：站点未启用时表现为功能不存在。
func (s *Server) apiKeysOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Settings.Runtime(r.Context()).Site.APIEnabled {
			fail(w, service.ErrNotFound)
			return
		}
		next(w, r)
	}
}

// handleListAPIKeys 列出当前用户的 API Key。
func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	items, err := s.Svc.ListAPIKeys(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items)})
}

// handleCreateAPIKey 创建 API Key；明文令牌只在此响应中出现一次。
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req service.CreateAPIKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	created, err := s.Svc.CreateAPIKey(r.Context(), p, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, created)
}

// handleDeleteAPIKey 删除 API Key。
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.DeleteAPIKey(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// handleAPIKeySDK 下发示例 SDK 压缩包。
func (s *Server) handleAPIKeySDK(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePerm(w, r, perm.API); !ok {
		return
	}
	data, err := sdkasset.Zip()
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="XiaoyuPostHub-Next_SDK_v1.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
