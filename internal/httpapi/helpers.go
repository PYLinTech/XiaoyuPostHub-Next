package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
)

// requireUser 要求已登录，失败时已写出响应。
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p := principalOf(r.Context())
	if err := auth.RequireUser(p); err != nil {
		fail(w, err)
		return p, false
	}
	return p, true
}

// requirePerm 要求已登录且具备全部指定权限。
func (s *Server) requirePerm(w http.ResponseWriter, r *http.Request, bits ...perm.Bit) (auth.Principal, bool) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return p, false
	}
	if !perm.HasAll(p.Permissions(), bits...) {
		writeErr(w, http.StatusForbidden, "没有权限执行该操作", "缺少所需权限位")
		return p, false
	}
	return p, true
}

// pagingOf 解析分页参数。
func pagingOf(r *http.Request) (limit, offset int) {
	limit = intQuery(r, "limit", 100)
	offset = intQuery(r, "offset", 0)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func intQuery(r *http.Request, key string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func int64Query(r *http.Request, key string, def int64) int64 {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// timeQuery 解析 RFC3339 或 Unix 秒形式的时间参数。
func timeQuery(r *http.Request, key string) int64 {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return 0
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix()
	}
	return 0
}

func pathParam(r *http.Request, key string) string {
	return strings.TrimSpace(r.PathValue(key))
}
