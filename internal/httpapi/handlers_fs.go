package httpapi

import "net/http"

// handleList 列出目录内容。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	items, err := s.Svc.ListDir(r.Context(), p, path)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"path": path, "items": itemsOf(items)})
}

// handleStat 取单个节点的元信息；同时带上内容池状态，
// 让前端能在列表与详情里一致地展示"已停用"。
func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	node, err := s.Svc.StatNode(r.Context(), p, path)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"node": node})
}

// handleStats 返回目录子树的统计。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	files, folders, bytes, err := s.Svc.NodeStats(r.Context(), p, path)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{
		"path":    path,
		"files":   files,
		"folders": folders,
		"bytes":   bytes,
	})
}

type mkdirRequest struct {
	ParentPath string `json:"parentPath"`
	Name       string `json:"name"`
}

// handleMkdir 新建目录。
func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req mkdirRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	node, err := s.Svc.MakeDir(r.Context(), p, req.ParentPath, req.Name)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"node": node})
}

type renameRequest struct {
	Path    string `json:"path"`
	NewName string `json:"newName"`
}

// handleRename 重命名节点。
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req renameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	newPath, err := s.Svc.RenameNode(r.Context(), p, req.Path, req.NewName)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"path": newPath})
}

type moveRequest struct {
	Path       string `json:"path"`
	DestParent string `json:"destParent"`
}

// handleMove 移动节点到另一个目录。
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req moveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	newPath, err := s.Svc.MoveNode(r.Context(), p, req.Path, req.DestParent)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"path": newPath})
}

type deleteRequest struct {
	Path string `json:"path"`
}

// handleDelete 删除节点及其子树。删除的是"引用"，内容池由引用计数决定回收。
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req deleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Svc.DeleteNode(r.Context(), p, req.Path); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}
