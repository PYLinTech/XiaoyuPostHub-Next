package httpapi

import (
	"net/http"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 归档端点。用户侧只操作自己的**暂存中**批次（列表 / 恢复 / 清除 / 清空），
// 清除之后条目即从用户侧消失；管理端看得到全部状态，只有单向的"向存储删除
// 推进"，没有恢复能力。

// handleListArchive 列出当前用户的归档批次。
func (s *Server) handleListArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	items, total, err := s.Svc.ListUserArchive(r.Context(), p, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items), "total": total})
}

// handleRestoreArchive 恢复一个暂存中的批次。
func (s *Server) handleRestoreArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.RestoreArchiveBatch(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"restored": pathParam(r, "id")})
}

// handleClearArchive 清除单个批次（标记为归档删除，不真删）。
func (s *Server) handleClearArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.ClearArchiveBatch(r.Context(), p, pathParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"cleared": pathParam(r, "id")})
}

// handleClearAllArchive 清空当前用户的全部暂存批次。
func (s *Server) handleClearAllArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	cleared, err := s.Svc.ClearAllArchive(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"cleared": cleared})
}

// handleAdminListArchive 列出全局归档批次；state 查询参数可选（1/2/3）。
func (s *Server) handleAdminListArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminFiles)
	if !ok {
		return
	}
	limit, offset := pagingOf(r)
	state := store.ArchiveState(0)
	switch r.URL.Query().Get("state") {
	case "1":
		state = store.ArchiveStaged
	case "2":
		state = store.ArchiveDeleted
	case "3":
		state = store.ArchiveStorageDeleted
	}
	items, total, err := s.Svc.AdminListArchive(r.Context(), p, state, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": itemsOf(items), "total": total})
}

type archivePurgeRequest struct {
	IDs []string `json:"ids"`
}

// handleAdminPurgeArchive 真实删除选中的批次（存储侧对象 + 状态置存储删除）。
func (s *Server) handleAdminPurgeArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requirePerm(w, r, perm.AdminFiles)
	if !ok {
		return
	}
	var req archivePurgeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	count, err := s.Svc.AdminPurgeArchiveBatches(r.Context(), p, req.IDs)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"purged": count})
}
