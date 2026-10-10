package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

type UploadJobStatus struct {
	SessionID     string      `json:"sessionId"`
	FileName      string      `json:"fileName,omitempty"`
	State         string      `json:"state"`
	Message       string      `json:"message"`
	Error         string      `json:"error,omitempty"`
	ProgressBytes int64       `json:"progressBytes"`
	TotalBytes    int64       `json:"totalBytes"`
	Node          *store.Node `json:"node,omitempty"`
}

func (s *Service) uploadQueueLimit(ctx context.Context) int64 {
	limit := int64(s.Settings.Runtime(ctx).Upload.SystemMaxTasks)
	if limit <= 0 {
		return 8
	}
	return limit
}

// ActiveUploadJobsForUser 返回用户尚未结束的上传任务，供新会话恢复传输面板。
func (s *Service) ActiveUploadJobsForUser(ctx context.Context, p auth.Principal) ([]UploadJobStatus, error) {
	jobs, err := store.ListActiveUploadJobsForUser(ctx, s.DB.R(), p.UserID())
	if err != nil {
		return nil, err
	}
	statuses := make([]UploadJobStatus, 0, len(jobs))
	for _, job := range jobs {
		status, err := s.uploadJobStatus(job)
		if err != nil {
			return nil, err
		}
		status.FileName = job.TargetName
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// QueueUploadCompletion 将已收齐的分片放入持久化收尾队列，HTTP 请求不再等待云盘。
func (s *Service) QueueUploadCompletion(ctx context.Context, p auth.Principal, sessionID string) (UploadJobStatus, error) {
	if err := auth.RequirePermission(p, perm.Upload); err != nil {
		return UploadJobStatus{}, err
	}
	if job, err := store.GetUploadJob(ctx, s.DB.R(), sessionID); err == nil {
		if job.UserID != p.UserID() {
			return UploadJobStatus{}, ErrNotFound
		}
		if job.State == "receiving" {
			task, taskErr := s.ownUploadTask(ctx, p, sessionID)
			if taskErr != nil {
				return UploadJobStatus{}, taskErr
			}
			if uploadExpectedChecksum(task) == "" {
				return UploadJobStatus{}, fmt.Errorf("%w: 文件校验尚未完成", ErrConflict)
			}
			if !task.Complete() {
				return UploadJobStatus{}, fmt.Errorf("%w: 分片尚未接收完整", ErrBadRequest)
			}
			if err := s.queueStreamingUploadIfReady(ctx, task); err != nil && !errors.Is(err, store.ErrBusy) {
				return UploadJobStatus{}, err
			}
			s.ingressGate.Release(sessionID)
			job, err = store.GetUploadJob(ctx, s.DB.R(), sessionID)
			if err != nil {
				return UploadJobStatus{}, err
			}
		}
		return s.uploadJobStatus(job)
	} else if !errors.Is(err, store.ErrNotFound) {
		return UploadJobStatus{}, err
	}
	task, err := s.ownUploadTask(ctx, p, sessionID)
	if err != nil {
		return UploadJobStatus{}, err
	}
	if uploadExpectedChecksum(task) == "" {
		return UploadJobStatus{}, fmt.Errorf("%w: 文件校验尚未完成", ErrConflict)
	}
	if !task.Complete() {
		return UploadJobStatus{}, fmt.Errorf("%w: 分片尚未接收完整", ErrBadRequest)
	}
	job, err := store.CreateUploadJob(ctx, s.DB, store.UploadJob{
		SessionID:  sessionID,
		UserID:     p.UserID(),
		ClientIP:   p.ClientIP.String(),
		TotalBytes: task.SizePlain,
	}, s.uploadQueueLimit(ctx))
	if err != nil {
		if errors.Is(err, store.ErrBusy) {
			return UploadJobStatus{}, fmt.Errorf("%w: 服务器处理队列已满，请稍后重试", ErrBusy)
		}
		return UploadJobStatus{}, err
	}
	if job.UserID != p.UserID() {
		return UploadJobStatus{}, ErrNotFound
	}
	s.ingressGate.Release(sessionID)
	return s.uploadJobStatus(job)
}

// UploadJobStatusForUser 查询本人任务；job 状态优先于仍然存在的分片会话。
func (s *Service) UploadJobStatusForUser(ctx context.Context, p auth.Principal, sessionID string) (UploadJobStatus, bool, error) {
	job, err := store.GetUploadJob(ctx, s.DB.R(), sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return UploadJobStatus{}, false, nil
	}
	if err != nil {
		return UploadJobStatus{}, false, err
	}
	if job.UserID != p.UserID() {
		return UploadJobStatus{}, false, ErrNotFound
	}
	if job.State == "receiving" {
		if task, taskErr := store.GetUploadTask(ctx, s.DB.R(), sessionID); taskErr == nil && task.Streaming &&
			task.Complete() && uploadExpectedChecksum(task) != "" {
			if queueErr := s.queueStreamingUploadIfReady(ctx, task); queueErr != nil && !errors.Is(queueErr, store.ErrBusy) {
				return UploadJobStatus{}, false, queueErr
			}
			job, err = store.GetUploadJob(ctx, s.DB.R(), sessionID)
			if err != nil {
				return UploadJobStatus{}, false, err
			}
		} else if taskErr != nil && !errors.Is(taskErr, store.ErrNotFound) {
			return UploadJobStatus{}, false, taskErr
		}
	}
	if task, taskErr := store.GetUploadTask(ctx, s.DB.R(), sessionID); taskErr == nil &&
		(((!task.Complete() || uploadExpectedChecksum(task) == "") && job.State != "error") || job.State == "receiving") {
		// 大文件可能一边接收后续分片、一边后台处理已收齐的卷；恢复客户端
		// 必须继续发送缺失块，不能把中间卷状态误当成最终收尾。
		return UploadJobStatus{}, false, nil
	} else if taskErr != nil && !errors.Is(taskErr, store.ErrNotFound) {
		return UploadJobStatus{}, false, taskErr
	}
	status, err := s.uploadJobStatus(job)
	return status, true, err
}

func (s *Service) uploadJobStatus(job store.UploadJob) (UploadJobStatus, error) {
	status := UploadJobStatus{
		SessionID: job.SessionID, State: job.State, Error: job.Error,
		ProgressBytes: job.ProgressBytes, TotalBytes: job.TotalBytes,
	}
	switch job.State {
	case "queued":
		status.Message = "等待服务器处理"
	case "receiving":
		status.Message = "等待上传分片"
	case "processing":
		status.Message = "服务器正在校验、加密并保存"
	case "done":
		status.Message = "上传完成"
		var node store.Node
		if err := json.Unmarshal([]byte(job.ResultJSON), &node); err != nil {
			return UploadJobStatus{}, err
		}
		status.Node = &node
	case "error":
		status.Message = "服务器处理失败"
		status.Error = "服务器处理上传失败，请稍后重试或联系管理员"
		if job.Error == "已取消" {
			status.Error = "已取消"
		}
	default:
		return UploadJobStatus{}, ErrUnavailable
	}
	return status, nil
}

// RunUploadFinalizers 按系统配置动态调整后台收尾 worker 数。任务状态在 SQLite 中，
// 重启后 processing 会恢复为 queued；按用户轮转可避免一个人的大批文件占住 worker。
func (s *Service) RunUploadFinalizers(ctx context.Context) {
	if err := store.ResetInterruptedUploadJobs(ctx, s.DB.W()); err != nil {
		log.Printf("upload queue: 恢复中断任务失败: %v", err)
	}
	s.finalizerWG.Add(1)
	go s.manageUploadFinalizers(ctx)
}

type uploadFinalizerWorkerHandle struct {
	stop     chan struct{}
	done     chan struct{}
	stopping bool
}

func (s *Service) manageUploadFinalizers(ctx context.Context) {
	defer s.finalizerWG.Done()
	workers := make(map[int]*uploadFinalizerWorkerHandle)
	nextID := 0
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		for id, worker := range workers {
			select {
			case <-worker.done:
				delete(workers, id)
			default:
			}
		}

		desired := int(s.uploadQueueLimit(ctx))
		for len(workers) < desired {
			nextID++
			worker := &uploadFinalizerWorkerHandle{stop: make(chan struct{}), done: make(chan struct{})}
			workers[nextID] = worker
			s.finalizerWG.Add(1)
			go func(id int, handle *uploadFinalizerWorkerHandle) {
				defer s.finalizerWG.Done()
				defer close(handle.done)
				s.uploadFinalizerWorker(ctx, id, handle.stop)
			}(nextID, worker)
		}
		if len(workers) > desired {
			excess := len(workers) - desired
			for _, worker := range workers {
				if worker.stopping {
					excess--
				}
			}
			for id := nextID; id > 0 && excess > 0; id-- {
				worker, ok := workers[id]
				if !ok || worker.stopping {
					continue
				}
				worker.stopping = true
				close(worker.stop)
				excess--
			}
		}

		select {
		case <-ctx.Done():
			for _, worker := range workers {
				if !worker.stopping {
					close(worker.stop)
					worker.stopping = true
				}
			}
			return
		case <-ticker.C:
		}
	}
}

// WaitUploadFinalizers 等待正在收尾的任务在关闭数据库前退出。
func (s *Service) WaitUploadFinalizers(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.finalizerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) uploadFinalizerWorker(ctx context.Context, worker int, stop <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		default:
		}
		job, found, err := s.DB.ClaimNextUploadJob(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("upload queue: worker %d 领取任务失败: %v", worker, err)
			if !waitUploadQueueOrStop(ctx, stop, time.Second) {
				return
			}
			continue
		}
		if !found {
			if !waitUploadQueueOrStop(ctx, stop, 500*time.Millisecond) {
				return
			}
			continue
		}
		s.processUploadJob(ctx, job)
	}
}

func waitUploadQueueOrStop(ctx context.Context, stop <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) processUploadJob(ctx context.Context, job store.UploadJob) {
	jobCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	task, err := store.GetUploadTask(jobCtx, s.DB.R(), job.SessionID)
	taskLoaded := err == nil
	if err == nil {
		user, userErr := store.GetUserByID(jobCtx, s.DB.R(), job.UserID)
		if userErr != nil {
			err = userErr
		} else if user.Status != store.UserEnabled {
			err = auth.ErrForbidden
		} else {
			group, groupErr := store.GetGroup(jobCtx, s.DB.R(), user.GroupName)
			if groupErr != nil {
				err = groupErr
			} else {
				clientIP, _ := netip.ParseAddr(job.ClientIP)
				p := auth.Principal{Actor: store.ActorUser, User: user, Group: group, ClientIP: clientIP}
				if perm.Has(group.Permissions, perm.Upload) {
					if task.Streaming {
						err = s.processStreamingUploadWindow(jobCtx, p, task)
					} else {
						_, err = s.CompleteUpload(jobCtx, p, job.SessionID)
					}
				} else {
					err = auth.ErrForbidden
				}
			}
		}
	}
	if err != nil {
		message := err.Error()
		if taskLoaded {
			if _, finishErr := store.GetUploadTask(context.WithoutCancel(jobCtx), s.DB.R(), job.SessionID); finishErr == nil {
				// CompleteUpload 的业务失败会自行回收任务；身份被撤销等前置失败则在此回收。
				_ = s.abortUpload(context.WithoutCancel(jobCtx), task)
			}
		}
		if finishErr := store.FinishUploadJob(context.WithoutCancel(jobCtx), s.DB.W(), job.SessionID, "error", message, ""); finishErr != nil {
			log.Printf("upload queue: 记录任务 %s 失败状态失败: %v", job.SessionID, finishErr)
		}
		log.Printf("upload queue: 任务 %s 处理失败: %v", job.SessionID, err)
	}
}
