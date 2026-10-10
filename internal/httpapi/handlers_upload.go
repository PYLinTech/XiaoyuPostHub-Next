package httpapi

import (
	"net/http"
	"strconv"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

type uploadInitRequest struct {
	Checksum       string `json:"checksum"`
	SizePlain      int64  `json:"sizePlain"`
	ParentPath     string `json:"parentPath"`
	Name           string `json:"name"`
	ConflictAction string `json:"conflictAction"`
}

// handleUploadTasks 返回当前用户尚未结束的上传任务。
func (s *Server) handleUploadTasks(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	tasks, err := s.Svc.ActiveUploadJobsForUser(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"items": tasks})
}

// handleUploadInit 初始化上传：先做秒传探测，未命中则返回分片计划。
func (s *Server) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req uploadInitRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := s.Svc.InitUpload(r.Context(), p, service.InitUploadRequest{
		Checksum:       req.Checksum,
		SizePlain:      req.SizePlain,
		ParentPath:     req.ParentPath,
		Name:           req.Name,
		ConflictAction: req.ConflictAction,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, result)
}

func (s *Server) handleUploadResolve(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Checksum string `json:"checksum"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	node, err := s.Svc.ResolveUploadChecksum(r.Context(), p, pathParam(r, "session"), req.Checksum)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"dedup": node != nil, "node": node})
}

// handleUploadChunk 接收一个分片，请求体是原始分片字节。
//
// 不做 JSON 包装：分片是二进制，包一层 base64 会让传输量膨胀三分之一，
// 而分片大小按 MB 计，这个开销直接体现在上传耗时上。
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// 分片大小在上传会话创建时固定。这里再做一次 HTTP 层上限保护，避免
	// 服务层尚未开始写盘前就让客户端发送任意大的请求体；服务层仍会按会话
	// 实际尺寸做最终校验。
	chunkLimit := s.Settings.Runtime(r.Context()).Upload.ChunkSize
	if chunkLimit <= 0 {
		writeErr(w, http.StatusServiceUnavailable, "服务暂时不可用", "上传分片大小配置无效")
		return
	}
	if r.ContentLength > chunkLimit+1 {
		writeErr(w, http.StatusRequestEntityTooLarge, "分片超过大小限制", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, chunkLimit+1)
	index, err := strconv.Atoi(pathParam(r, "index"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "分片序号不正确", "")
		return
	}
	task, err := s.Svc.UploadChunk(r.Context(), p, pathParam(r, "session"), index, r.Body)
	if err != nil {
		fail(w, err)
		return
	}
	var received []int
	for i := 0; i < task.ChunkTotal; i++ {
		if store.HasBit(task.ReceivedMask, i) {
			received = append(received, i)
		}
	}
	writeData(w, map[string]any{
		"sessionId":     task.ID,
		"received":      received,
		"receivedBytes": task.ReceivedBytes(),
	})
}

// handleUploadComplete 收尾：合并、加密、写入存储并登记节点。
func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	job, err := s.Svc.QueueUploadCompletion(r.Context(), p, pathParam(r, "session"))
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, job)
}

// handleUploadStatus 返回会话进度，供断点续传。
func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	sessionID := pathParam(r, "session")
	if job, found, err := s.Svc.UploadJobStatusForUser(r.Context(), p, sessionID); err != nil {
		fail(w, err)
		return
	} else if found {
		writeData(w, job)
		return
	}
	task, received, err := s.Svc.UploadProgress(r.Context(), p, sessionID)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{
		"sessionId":     task.ID,
		"chunkSize":     task.ChunkSize,
		"chunkTotal":    task.ChunkTotal,
		"received":      received,
		"receivedBytes": task.ReceivedBytes(),
		"expiresAt":     task.ExpiresAt.Unix(),
	})
}

// handleUploadCancel 取消上传并释放配额与临时文件。
func (s *Server) handleUploadCancel(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.Svc.CancelUpload(r.Context(), p, pathParam(r, "session")); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}
