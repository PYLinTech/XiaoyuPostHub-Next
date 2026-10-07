package service

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// checksumPattern 校验明文 SHA-256 的十六进制表示。校验码是内容池主键，
// 格式不对会让去重与秒传全部失去意义，因此在入口就拒绝。
var checksumPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// 同名冲突处置方式。
const (
	ConflictRename    = "rename"
	ConflictOverwrite = "overwrite"
	ConflictReject    = "reject"
)

// InitUploadRequest 是一次上传的初始化请求。
type InitUploadRequest struct {
	// Checksum 是**明文** SHA-256（十六进制）。
	//
	// 加密在服务端进行、内容密钥每文件随机，因此跨用户唯一稳定的标识只有
	// 明文哈希——这也是秒传能成立的前提。
	Checksum string
	// SizePlain 是明文长度。
	SizePlain int64
	// ParentPath 与 Name 是落点。
	ParentPath string
	Name       string
	// ConflictAction 是同名处置方式。
	ConflictAction string
}

// InitUploadResult 描述初始化结果：要么秒传命中、要么给出上传会话。
type InitUploadResult struct {
	Dedup bool        `json:"dedup"`
	Node  *store.Node `json:"node,omitempty"`

	SessionID  string `json:"sessionId,omitempty"`
	ChunkSize  int64  `json:"chunkSize,omitempty"`
	ChunkTotal int    `json:"chunkTotal,omitempty"`
	// Received 是已接收的分片序号，用于断点续传。
	Received  []int `json:"received,omitempty"`
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// InitUpload 初始化上传：先做秒传探测，未命中则建立上传会话并预扣配额。
func (s *Service) InitUpload(ctx context.Context, p auth.Principal, req InitUploadRequest) (InitUploadResult, error) {
	if err := auth.RequirePermission(p, perm.Upload); err != nil {
		return InitUploadResult{}, err
	}
	checksum := strings.ToLower(strings.TrimSpace(req.Checksum))
	if !checksumPattern.MatchString(checksum) {
		return InitUploadResult{}, fmt.Errorf("%w: 校验码必须是 64 位十六进制 SHA-256", ErrBadRequest)
	}
	if req.SizePlain < 0 {
		return InitUploadResult{}, fmt.Errorf("%w: 文件大小非法", ErrBadRequest)
	}
	rt := s.Settings.Runtime(ctx)
	if !s.EncryptionReady(ctx) {
		return InitUploadResult{}, fmt.Errorf("%w: 服务端未配置加密密钥", ErrUnavailable)
	}
	// 存储未就绪必须在**收到任何分片之前**就拦下。放行的话，客户端会把整份
	// 文件传完，然后在收尾写入存储时收到失败——用户的流量与时间白花了，
	// 而问题的解法（管理员填凭据）从一开始就已经确定。
	if !s.StorageReady() {
		return InitUploadResult{}, fmt.Errorf("%w: 需要在管理界面配置存储凭据后才能上传", ErrStorageNotReady)
	}

	// 组配额在入口一次性读出：单文件上限、并发会话数与存储总量都在其中。
	// BypassQuota 的身份跳过组配额判定（但仍受全局上限与记账约束）。
	quotas, quotaBypass, err := s.groupQuotaMap(ctx, p)
	if err != nil {
		return InitUploadResult{}, err
	}

	maxFile := rt.Upload.MaxFileSize
	if maxFile < 0 {
		return InitUploadResult{}, fmt.Errorf("%w: 单文件上限配置无效", ErrUnavailable)
	}
	// 组配额可以把单文件上限收得更紧。配额语义：无行沿用全局值，行值为 0
	// 表示该组完全禁止上传（与"0 是禁止、无行是无限"的配额文档一致）。
	fileMaxForbidden := false
	if !quotaBypass {
		if fm, ok := quotas[store.QuotaFileMax]; ok {
			if fm == 0 {
				fileMaxForbidden = true
			} else if maxFile <= 0 || fm < maxFile {
				maxFile = fm
			}
		}
	}
	if fileMaxForbidden || (maxFile > 0 && req.SizePlain > maxFile) {
		return InitUploadResult{}, fmt.Errorf("%w: 单文件上限 %d 字节", ErrTooLarge, maxFile)
	}

	parent, err := vpath.Normalize(req.ParentPath)
	if err != nil {
		return InitUploadResult{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if err := s.requireFolder(ctx, p.UserID(), parent); err != nil {
		return InitUploadResult{}, err
	}
	name, err := vpath.ValidateName(req.Name)
	if err != nil {
		return InitUploadResult{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	action := req.ConflictAction
	switch action {
	case "":
		action = ConflictRename
	case ConflictRename, ConflictOverwrite, ConflictReject:
	default:
		return InitUploadResult{}, fmt.Errorf("%w: 未知的同名冲突处理方式 %q", ErrBadRequest, action)
	}

	// ① 秒传探测。作用域默认仅同组：全局秒传等于"知道明文哈希即可拿到可
	// 下载的引用"，对私有部署没有必要承担这个泄露面。
	scope, err := s.dedupScope(ctx)
	if err != nil {
		return InitUploadResult{}, err
	}
	if scope != settings.DedupOff {
		file, err := store.GetDedupCandidate(ctx, s.DB.R(), checksum, req.SizePlain)
		if err == nil {
			visible := true
			if scope == settings.DedupGroup {
				visible, err = store.ChecksumReferencedByGroup(ctx, s.DB.R(), checksum, p.GroupName())
				if err != nil {
					return InitUploadResult{}, err
				}
			}
			if visible {
				node, err := s.placeFileNode(ctx, p, parent, name, action, file)
				if err != nil {
					return InitUploadResult{}, err
				}
				return InitUploadResult{Dedup: true, Node: &node}, nil
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return InitUploadResult{}, err
		}
	}

	// ② 进行中的上传会话数上限与配额预扣会在同一写事务中完成。上传会把
	// 分片落到临时盘，不设上限等于把磁盘交给客户端支配；秒传命中不建会话，
	// 不会被这个上限拦住。组配额行存在时覆盖默认并发数（0 = 禁止新建会话）。
	uploadRT := s.Settings.Runtime(ctx).Upload
	pendingLimit := uploadRT.MaxPending
	pendingLimited := false
	if !quotaBypass {
		if pl, ok := quotas[store.QuotaPendingUploads]; ok {
			// 组配额行存在时覆盖默认并发数。行值为 0 表示该组完全禁止
			// 新建会话，与文件上限/存储配额的"0 是禁止、无行是无限"语义一致。
			pendingLimit, pendingLimited = int(pl), true
		}
	}
	if pendingLimited && pendingLimit <= 0 {
		return InitUploadResult{}, fmt.Errorf("%w: 当前用户组已被禁止创建上传会话", ErrForbidden)
	}
	chunkSize := uploadRT.ChunkSize
	if chunkSize <= 0 {
		return InitUploadResult{}, fmt.Errorf("%w: 分片大小配置无效", ErrUnavailable)
	}
	// 分片边界必须与加密块边界对齐：错位会让密文流无法跨片连续解密。
	block := s.BlockSize(ctx)
	if block <= 0 || chunkSize%block != 0 {
		return InitUploadResult{}, fmt.Errorf("%w: 分片大小与加密块大小不兼容", ErrUnavailable)
	}
	if uploadRT.SessionTTL <= 0 {
		return InitUploadResult{}, fmt.Errorf("%w: 上传会话有效期配置无效", ErrUnavailable)
	}

	// ③ 建立上传会话。配额预扣与会话创建在同一事务中提交：如果进程在
	// 两步之间退出，维护任务仍能根据会话回收额度，不会留下无主的预扣。
	total := 0
	if req.SizePlain > 0 {
		total = int((req.SizePlain + chunkSize - 1) / chunkSize)
	}

	sessionID, err := store.NewID("up")
	if err != nil {
		return InitUploadResult{}, err
	}

	task := store.UploadTask{
		ID:               sessionID,
		UserID:           p.UserID(),
		Checksum:         checksum,
		SizePlain:        req.SizePlain,
		ChunkSize:        chunkSize,
		ChunkTotal:       total,
		ReceivedMask:     make([]byte, store.BitmapBytes(total)),
		TargetParentPath: parent,
		TargetName:       name,
		ConflictAction:   action,
		ExpiresAt:        time.Now().UTC().Add(uploadRT.SessionTTL),
		CreatedAt:        s.Now(),
	}
	storageKey := store.UserCounterKey(p.UserID(), "")
	// 存储总量配额：无行 = 不受限；行值 0 = 完全禁止，交给 ReserveCounter
	// 的上限判定拒绝（其 limit<=0 分支与配额语义一致）。
	storageLimitValue, storageLimited := int64(0), false
	if !quotaBypass {
		if v, ok := quotas[store.QuotaStorageTotal]; ok {
			storageLimitValue, storageLimited = v, true
		}
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		var err error
		if storageLimited {
			_, err = store.ReserveCounter(ctx, tx, store.ScopeStorage, storageKey, req.SizePlain, storageLimitValue)
		} else {
			_, err = store.AddCounter(ctx, tx, store.ScopeStorage, storageKey, req.SizePlain)
		}
		if errors.Is(err, store.ErrQuotaExceeded) {
			return fmt.Errorf("%w: 存储空间不足", ErrQuotaExceeded)
		}
		if err != nil {
			return err
		}
		if pendingLimit > 0 {
			return store.CreateUploadTaskLimited(ctx, tx, task, int64(pendingLimit))
		}
		return store.CreateUploadTask(ctx, tx, task)
	}); err != nil {
		return InitUploadResult{}, err
	}

	// ④ 占位内容池并冻结加密参数。会话先落库后再占位，所有并发上传者
	// 都能被 CountLiveUploadsByChecksum 看到，避免两个请求同时接管同一占位行。
	if _, err := s.ensureContentPlaceholder(ctx, checksum, req.SizePlain, p.UserID()); err != nil {
		if abortErr := s.abortUpload(ctx, task); abortErr != nil {
			return InitUploadResult{}, errors.Join(err, abortErr)
		}
		return InitUploadResult{}, err
	}
	if err := os.MkdirAll(s.sessionDir(task.ID), 0o700); err != nil {
		if abortErr := s.abortUpload(ctx, task); abortErr != nil {
			return InitUploadResult{}, errors.Join(err, abortErr)
		}
		return InitUploadResult{}, err
	}

	return InitUploadResult{
		SessionID:  task.ID,
		ChunkSize:  chunkSize,
		ChunkTotal: total,
		ExpiresAt:  task.ExpiresAt.Unix(),
	}, nil
}

// ensureContentPlaceholder 确保内容池中存在该内容的占位行，并返回带完整
// 加密参数的那一行。
//
// 内容池主键本身就是并发上传的锁。返回 ErrBusy 表示同一内容正在被上传，
// 调用方应稍后重试或直接复用——重复上传同一份内容没有价值。
//
// 既有行的处置：
//   - uploading → 同校验码已有上传在途（含本请求自己的会话），返回 ErrBusy；
//     僵死的占位行由调用方 abort 时顺手清理，下一轮 init 即可正常占位；
//   - archive（引用归零待回收）→ 先删掉旧物理对象与记录后重新占位。远端
//     删除在**写事务外**进行：网络调用不能独占 SQLite 写锁；
//   - purged（远端对象已真实删除，记录仅存审计）→ 翻新为全新占位，
//     让同校验码内容可以再次上传；
//   - disabled（被拉黑）→ 明确拒绝，而不是让违规内容用同一份数据反复上架。
//
// ref=0 的回收态行不存在"恢复竞态"：恢复只允许暂存中的批次，而暂存期的
// 引用仍被持有，ref 不可能为 0。因此事务外删除窗口内没有能把对象重新
// 占用的路径。
func (s *Service) ensureContentPlaceholder(ctx context.Context, checksum string, sizePlain, userID int64) (store.File, error) {
	fresh, err := s.newContentRecord(ctx, checksum, sizePlain, userID)
	if err != nil {
		return store.File{}, err
	}
	// 远端删除拆成三段：事务内出计划 → 事务外删对象 → 事务内收尾。
	// 计划与收尾之间行可能被并发请求推进（例如维护任务恰好 purge 了它），
	// 因此收尾前重新决策，最多三轮。
	for attempt := 0; attempt < 3; attempt++ {
		plan, err := s.planContentPlaceholder(ctx, fresh)
		if err != nil {
			return store.File{}, err
		}
		if plan.deleteRef == "" {
			return plan.file, nil
		}
		if err := s.Backend.Delete(ctx, plan.deleteRef); err != nil {
			// 删除失败可能只是因为并发请求已经删过同一对象：行一旦不再
			// 持有该定位符，就按新状态重新决策；仍然持有才是真失败。
			current, getErr := store.GetFile(ctx, s.DB.R(), checksum)
			if getErr == nil && current.Status == store.FileArchive && current.PanFileID == plan.deleteRef {
				return store.File{}, fmt.Errorf("%w: 回收旧对象失败: %v", ErrUnavailable, err)
			}
			continue
		}
		// 清空定位符表示物理对象已不存在：并发接管者据此不再重复删除；
		// 行若已被推进（更新 0 行），下一轮按新状态重新决策。
		if _, err := store.ClearArchiveObjectRef(ctx, s.DB.W(), checksum, plan.deleteRef); err != nil {
			return store.File{}, err
		}
	}
	return store.File{}, ErrBusy
}

// placeholderPlan 是一轮占位决策的结果。
type placeholderPlan struct {
	// file 是就绪的占位行；deleteRef 非空时它尚不可用。
	file store.File
	// deleteRef 非空表示需要先在写事务外删除的远端对象。
	deleteRef string
}

// planContentPlaceholder 在一个写事务内决策占位行的处置。除"删旧对象"
// 之外的变更都在这里落库，保证并发请求看到的状态是串行化的。
func (s *Service) planContentPlaceholder(ctx context.Context, fresh store.File) (placeholderPlan, error) {
	var plan placeholderPlan
	err := s.DB.InTx(ctx, func(tx store.Querier) error {
		created, err := store.InsertFilePlaceholder(ctx, tx, fresh)
		if err != nil {
			return err
		}
		if created {
			plan.file = fresh
			return nil
		}
		existing, err := store.GetFile(ctx, tx, fresh.Checksum)
		if err != nil {
			return err
		}
		switch existing.Status {
		case store.FileUploading:
			// 本请求的会话在进入这里之前已经落库，live 计数恒 ≥ 1，
			// 因此正常流程不会走到"无活跃会话的接管"：僵死占位行由
			// 本轮 abort 清理，下一轮 init 用全新参数重新占位。
			return ErrBusy
		case store.FileNormal:
			// 秒传探测已经处理过正常对象；走到这里说明明文长度不一致或秒传作用域
			// 判定不同，两者都属于数据异常，按占用处理。
			return ErrBusy
		case store.FileDisabled:
			return fmt.Errorf("%w: 该内容已被拉黑，无法上传", ErrForbidden)
		case store.FileArchive:
			if existing.PanFileID != "" {
				// 旧对象仍是唯一可定位的物理副本。先出事务删除远端并
				// 清空定位符，成功后才允许重建占位；反过来先删数据库
				// 会制造永远无法定位的孤儿。
				plan.deleteRef = existing.PanFileID
				return nil
			}
			return s.rebuildPlaceholder(ctx, tx, fresh, &plan)
		case store.FilePurged:
			// 远端对象已被真实删除，记录只剩审计价值；翻新为全新占位，
			// 解除对该校验码的永久占用。
			return s.rebuildPlaceholder(ctx, tx, fresh, &plan)
		default:
			return ErrBusy
		}
	})
	if err != nil {
		return placeholderPlan{}, err
	}
	return plan, nil
}

// rebuildPlaceholder 删除失去物理对象的旧记录，并以全新加密参数重建占位。
func (s *Service) rebuildPlaceholder(ctx context.Context, tx store.Querier, fresh store.File, plan *placeholderPlan) error {
	// 同校验码重传意味着旧密文对象即将被新对象替换：指向它的旧票据已不可能
	// 成功交付。必须在删除内容行之前（1）给未结算票据逐张释放预扣额度，
	// （2）物理删除全部票据行——tickets 对 files 有外键约束，只做软吊销
	// 会让下面的 ForceDeleteFileRow 触发 FOREIGN KEY 失败。
	active, err := store.ListActiveTicketsByChecksum(ctx, tx, fresh.Checksum)
	if err != nil {
		return err
	}
	for _, tk := range active {
		if err := s.releaseDeliveryQuotaByTicketQ(ctx, tx, tk, tk.ReservedBytes); err != nil {
			return err
		}
	}
	if err := store.DeleteTicketsByChecksum(ctx, tx, fresh.Checksum); err != nil {
		return err
	}
	if err := store.ForceDeleteFileRow(ctx, tx, fresh.Checksum); err != nil {
		return err
	}
	created, err := store.InsertFilePlaceholder(ctx, tx, fresh)
	if err != nil {
		return err
	}
	if !created {
		return ErrBusy
	}
	plan.file = fresh
	return nil
}

// newContentRecord 生成一条带随机加密参数的内容池记录。
func (s *Service) newContentRecord(ctx context.Context, checksum string, sizePlain, userID int64) (store.File, error) {
	hdr, err := xph.NewHeaderRandom(sizePlain, s.BlockLog2(ctx))
	if err != nil {
		return store.File{}, err
	}
	dek, err := xph.NewDEK()
	if err != nil {
		return store.File{}, err
	}
	kekID, kek, err := s.PrimaryKEK(ctx)
	if err != nil {
		return store.File{}, err
	}
	envelope, err := xph.WrapDEK(kek, dek)
	if err != nil {
		return store.File{}, err
	}
	_, panName, err := s.ObjectName(ctx, checksum)
	if err != nil {
		return store.File{}, err
	}
	now := s.Now()
	return store.File{
		Checksum:       checksum,
		SizePlain:      sizePlain,
		PanObjectName:  panName,
		EncAlgo:        "AES-256-GCM",
		EncChunkLog2:   int(hdr.BlockLog2),
		EncNoncePrefix: int64(hdr.NoncePrefix),
		EncSalt:        append([]byte(nil), hdr.FileSalt[:]...),
		DEKEnvelope:    envelope,
		KEKKeyID:       kekID,
		Status:         store.FileUploading,
		CreatedBy:      userID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// UploadChunk 接收一个分片。
//
// 分片先落临时文件而不是常驻内存：并发分片数乘以分片大小就是内存占用，
// 直接把内存峰值交给客户端控制是危险的。
func (s *Service) UploadChunk(ctx context.Context, p auth.Principal, sessionID string, index int, body io.Reader) (store.UploadTask, error) {
	if err := auth.RequirePermission(p, perm.Upload); err != nil {
		return store.UploadTask{}, err
	}
	task, err := s.ownUploadTask(ctx, p, sessionID)
	if err != nil {
		return store.UploadTask{}, err
	}
	if index < 0 || index >= task.ChunkTotal {
		return store.UploadTask{}, fmt.Errorf("%w: 分片序号越界", ErrBadRequest)
	}
	// 已确认的分片不再允许重复写入。重复请求常见于客户端重试；继续
	// 截断目标文件会与收尾读取产生竞态，甚至把一份完整分片变成半片。
	if store.HasBit(task.ReceivedMask, index) {
		return task, nil
	}

	expect := task.ChunkSize
	if index == task.ChunkTotal-1 {
		expect = task.SizePlain - int64(index)*task.ChunkSize
	}
	if expect < 0 {
		expect = 0
	}

	path := s.chunkPath(task.ID, index)
	if err := s.writeChunkFile(path, body, expect); err != nil {
		return store.UploadTask{}, err
	}
	updated, err := store.MarkChunkReceived(ctx, s.DB.W(), task.ID, index)
	if err != nil {
		// 会话可能恰好在写盘后过期或被取消；位图没有记录这片时，
		// 留下的分片只会变成不可定位的临时垃圾。
		_ = os.Remove(path)
		return store.UploadTask{}, err
	}
	return updated, nil
}

// writeChunkFile 写入分片文件，并校验长度与声明一致。
func (s *Service) writeChunkFile(path string, body io.Reader, expect int64) error {
	if body == nil {
		return fmt.Errorf("%w: 缺少分片数据", ErrBadRequest)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: 创建分片目录失败", ErrUnavailable)
	}
	// 先写同目录临时文件，再原子替换目标。并发重试即使发生，也只会看到
	// 完整的新旧文件，不会让收尾过程读到截断中的内容。
	tmp, err := os.CreateTemp(filepath.Dir(path), ".chunk-*")
	if err != nil {
		return fmt.Errorf("%w: 创建分片文件失败", ErrUnavailable)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	// 多读 1 字节以发现超长输入：只按声明长度截断会把"客户端多发数据"
	// 静默吞掉，最终表现为文件内容错误而不是上传失败。
	written, err := io.Copy(tmp, io.LimitReader(body, expect+1))
	if err != nil {
		cleanup()
		return fmt.Errorf("%w: 接收分片失败", ErrUnavailable)
	}
	if written != expect {
		cleanup()
		return fmt.Errorf("%w: 分片长度 %d，期望 %d", ErrBadRequest, written, expect)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: 写入分片失败", ErrUnavailable)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: 保存分片失败", ErrUnavailable)
	}
	return nil
}

// UploadProgress 返回会话当前进度。
func (s *Service) UploadProgress(ctx context.Context, p auth.Principal, sessionID string) (store.UploadTask, []int, error) {
	task, err := s.ownUploadTask(ctx, p, sessionID)
	if err != nil {
		return store.UploadTask{}, nil, err
	}
	var received []int
	for i := 0; i < task.ChunkTotal; i++ {
		if store.HasBit(task.ReceivedMask, i) {
			received = append(received, i)
		}
	}
	return task, received, nil
}

// CompleteUpload 完成上传：合并分片、流式加密、校验、写入存储后端、登记节点。
func (s *Service) CompleteUpload(ctx context.Context, p auth.Principal, sessionID string) (store.Node, error) {
	if err := auth.RequirePermission(p, perm.Upload); err != nil {
		return store.Node{}, err
	}
	task, err := s.ownUploadTask(ctx, p, sessionID)
	if err != nil {
		return store.Node{}, err
	}
	if !task.Complete() {
		return store.Node{}, fmt.Errorf("%w: 分片尚未接收完整", ErrBadRequest)
	}
	abort := func(cause error) error {
		if cleanupErr := s.abortUpload(ctx, task); cleanupErr != nil {
			return errors.Join(cause, cleanupErr)
		}
		return cause
	}

	cipherPath, plainSHA, cipherMD5, err := s.buildCiphertext(ctx, task)
	if err != nil {
		return store.Node{}, abort(err)
	}
	defer os.Remove(cipherPath)

	// 明文哈希与声明不一致：客户端发来的内容不是它声称的那份。此时密文已经
	// 生成但尚未上传，直接中止即可，不需要清理远端。
	if !strings.EqualFold(plainSHA, task.Checksum) {
		return store.Node{}, abort(fmt.Errorf("%w: 明文校验码与声明不一致", ErrBadRequest))
	}

	cipherFile, err := os.Open(cipherPath)
	if err != nil {
		return store.Node{}, abort(fmt.Errorf("%w: 打开密文失败", ErrUnavailable))
	}
	defer cipherFile.Close()

	// 密文上后端与邮件入库共用同一条原语（对象命名、长度口径、MD5 一致）。
	rec, err := store.GetFile(ctx, s.DB.R(), task.Checksum)
	if err != nil {
		return store.Node{}, abort(err)
	}
	put, err := s.putCipherObject(ctx, rec, cipherFile, cipherMD5)
	if err != nil {
		// putCipherObject 已保留 ErrUnavailable 与底层错误链：存储后端会给出
		// 需要区分处置的专用信号（例如"上行带宽不足"），链断了 HTTP 层
		// 就只能给泛化文案。
		return store.Node{}, abort(err)
	}

	var node store.Node
	err = s.DB.InTx(ctx, func(tx store.Querier) error {
		// 上传初始化时的父目录检查只是早期反馈；收尾必须在同一写事务
		// 内再次确认，避免目录在上传过程中被删除后仍登记出孤儿节点。
		if err := s.requireFolderTx(ctx, tx, p.UserID(), task.TargetParentPath); err != nil {
			return err
		}
		if err := store.FinalizeFile(ctx, tx, task.Checksum, put.ObjectRef, put.objectPath, put.sizeWire); err != nil {
			return err
		}
		if _, err := store.AddFileRef(ctx, tx, task.Checksum); err != nil {
			return err
		}
		file, err := store.GetFile(ctx, tx, task.Checksum)
		if err != nil {
			return err
		}
		n, err := s.placeFileNodeTx(ctx, tx, p, task.TargetParentPath, task.TargetName, task.ConflictAction, file)
		if err != nil {
			return err
		}
		if err := store.DeleteUploadTask(ctx, tx, task.ID); err != nil {
			return err
		}
		node = n
		return nil
	})
	if err != nil {
		// 并发收尾时，只有仍处于“上传中”的内容才属于本次失败。另一条
		// 请求可能已经完成并登记节点；此时不能把正常对象改成待回收。
		var cleanupErr error
		if current, readErr := store.GetFile(ctx, s.DB.R(), task.Checksum); readErr == nil {
			switch {
			case current.Status == store.FileUploading:
				// 登记失败：密文对象可能已经写上远端，而引用它的库内记录
				// 随事务回滚。先尽力删除远端对象，成功后抹掉占位行；删除
				// 失败则把定位符落库并排定清理时间，交给维护任务重试——
				// 绝不能让对象变成无人能定位的孤儿。
				if deleteErr := s.Backend.Delete(ctx, put.ObjectRef); deleteErr != nil {
					cleanupErr = errors.Join(cleanupErr, deleteErr)
					// 排定时间为当前时刻：下一轮维护立即重试删除，直到成功。
					// 这类对象没有任何恢复路径，留存期不适用。
					if markErr := store.MarkRegistrationFailed(ctx, s.DB.W(), task.Checksum,
						put.ObjectRef, put.objectPath, put.sizeWire, s.Now()); markErr != nil {
						cleanupErr = errors.Join(cleanupErr, markErr)
					}
				} else if deleteRowErr := store.DeleteFileRow(ctx, s.DB.W(), task.Checksum); deleteRowErr != nil {
					cleanupErr = errors.Join(cleanupErr, deleteRowErr)
				}
			case current.Status == store.FileNormal && put.ObjectRef != current.PanFileID:
				// 本次请求已经把重复对象写到远端，但数据库已由另一请求
				// 收尾；删掉孤儿对象，不触碰共享内容池记录。
				if deleteErr := s.Backend.Delete(ctx, put.ObjectRef); deleteErr != nil {
					cleanupErr = errors.Join(cleanupErr, deleteErr)
				}
			}
		} else if readErr != nil {
			cleanupErr = errors.Join(cleanupErr, readErr)
		}
		if abortErr := s.abortUpload(ctx, task); abortErr != nil {
			cleanupErr = errors.Join(cleanupErr, abortErr)
		}
		return store.Node{}, errors.Join(err, cleanupErr)
	}

	if err := s.cleanupSession(task.ID); err != nil {
		// 数据库收尾已经提交，不能把一次成功上传对外报告成失败，
		// 否则客户端会重试并制造重复对象。临时目录由运维/下一次
		// 启动清理，错误必须留在日志里而不是静默吞掉。
		log.Printf("service: 上传 %s 已完成，但清理临时目录失败: %v", task.ID, err)
	}
	s.recordUpload(ctx, p, task, put.sizeWire)
	return node, nil
}

// buildCiphertext 把分片合并并流式加密到临时文件，同时得到明文 SHA-256 与密文 MD5。
//
// 之所以要先落盘而不是直接把加密流喂给存储后端：123 的 create 需要
// **密文 MD5** 作为 etag，而这个值只有读完整份密文才知道。把密文缓存下来
// 换来的是一个可寻址、可分片并发上传的来源。
func (s *Service) buildCiphertext(ctx context.Context, task store.UploadTask) (path, plainSHA, cipherMD5 string, err error) {
	reader := &chunkSequenceReader{task: task, pathFor: s.chunkPath, index: -1}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("%w: 关闭分片读取器失败", ErrUnavailable)
		}
	}()

	tmpDir, err := s.tempSubdir("cipher")
	if err != nil {
		return "", "", "", err
	}
	file, err := os.CreateTemp(tmpDir, task.ID+"-*.xph")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: 创建密文临时文件失败", ErrUnavailable)
	}
	path = file.Name()
	success := false
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("%w: 关闭密文临时文件失败", ErrUnavailable)
			success = false
		}
		if !success {
			os.Remove(path)
		}
	}()

	plainHash := sha256.New()
	cipherHash := md5.New()

	// 加密参数取自会话创建时冻结的那一组：盐与前缀存在内容池占位行里。
	fileRow, err := store.GetFile(ctx, s.DB.R(), task.Checksum)
	if err != nil {
		return "", "", "", err
	}
	// 按对象记录的 keyId 取主密钥：主密钥可能已经轮换过，
	// 用当前主密钥去解旧信封会直接失败。
	kek, err := s.KEKFor(ctx, fileRow.KEKKeyID)
	if err != nil {
		return "", "", "", err
	}
	dek, err := xph.UnwrapDEK(kek, fileRow.DEKEnvelope)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: 解开内容密钥失败", ErrUnavailable)
	}
	hdr := xph.Header{
		Algo:        xph.AlgoAESGCM,
		BlockLog2:   byte(fileRow.EncChunkLog2),
		PlainSize:   task.SizePlain,
		NoncePrefix: uint32(fileRow.EncNoncePrefix),
	}
	copy(hdr.FileSalt[:], fileRow.EncSalt)

	tee := io.TeeReader(reader, plainHash)
	enc, err := xph.NewEncryptor(tee, dek, hdr)
	if err != nil {
		return "", "", "", err
	}
	written, err := io.Copy(io.MultiWriter(file, cipherHash), enc)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: 加密失败: %v", ErrUnavailable, err)
	}
	if want := hdr.CipherSize(); written != want {
		return "", "", "", fmt.Errorf("%w: 密文长度 %d，期望 %d", ErrBadRequest, written, want)
	}
	if err := file.Sync(); err != nil {
		return "", "", "", fmt.Errorf("%w: 落盘失败", ErrUnavailable)
	}

	success = true
	return path, hex.EncodeToString(plainHash.Sum(nil)), hex.EncodeToString(cipherHash.Sum(nil)), nil
}

// placeFileNode 在目录下放置文件节点（含同名冲突处理）。
func (s *Service) placeFileNode(ctx context.Context, p auth.Principal, parent, name, action string, file store.File) (store.Node, error) {
	// 秒传没有 InitUpload 的预扣；它仍然必须按当前用户的逻辑占用计入
	// 存储配额，否则删除一个重复引用会把原有额度错误地扣回去。
	limit, limited, err := s.storageLimit(ctx, p)
	if err != nil {
		return store.Node{}, err
	}
	var node store.Node
	err = s.DB.InTx(ctx, func(tx store.Querier) error {
		var err error
		if limited {
			_, err = store.ReserveCounter(ctx, tx, store.ScopeStorage,
				store.UserCounterKey(p.UserID(), ""), file.SizePlain, limit)
			if errors.Is(err, store.ErrQuotaExceeded) {
				return fmt.Errorf("%w: 存储空间不足", ErrQuotaExceeded)
			}
		} else {
			_, err = store.AddCounter(ctx, tx, store.ScopeStorage,
				store.UserCounterKey(p.UserID(), ""), file.SizePlain)
		}
		if err != nil {
			return err
		}
		if _, err := store.AddFileRef(ctx, tx, file.Checksum); err != nil {
			return err
		}
		n, err := s.placeFileNodeTx(ctx, tx, p, parent, name, action, file)
		if err != nil {
			return err
		}
		node = n
		return nil
	})
	if err != nil {
		return store.Node{}, err
	}
	return node, nil
}

// placeFileNodeTx 在事务内插入文件节点。
func (s *Service) placeFileNodeTx(ctx context.Context, tx store.Querier, p auth.Principal, parent, name, action string, file store.File) (store.Node, error) {
	// 该函数同时用于秒传和上传收尾；两条路径都必须在写事务内确认
	// 目标目录仍然存在，不能依赖调用方更早的只读检查。
	if err := s.requireFolderTx(ctx, tx, p.UserID(), parent); err != nil {
		return store.Node{}, err
	}
	resolved := name
	switch action {
	case ConflictReject:
		target, err := vpath.Join(parent, name)
		if err != nil {
			return store.Node{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
		}
		exists, err := store.NodeExists(ctx, tx, p.UserID(), target)
		if err != nil {
			return store.Node{}, err
		}
		if exists {
			return store.Node{}, fmt.Errorf("%w: %s 已存在", ErrConflict, target)
		}
	case ConflictOverwrite:
		if err := s.overwriteExisting(ctx, tx, p.UserID(), parent, name); err != nil {
			return store.Node{}, err
		}
	case ConflictRename:
		free, err := store.FreeName(ctx, tx, p.UserID(), parent, name)
		if err != nil {
			return store.Node{}, err
		}
		resolved = free
	default:
		return store.Node{}, fmt.Errorf("%w: 未知的同名冲突处理方式 %q", ErrBadRequest, action)
	}

	full, err := vpath.Join(parent, resolved)
	if err != nil {
		return store.Node{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	node := store.Node{
		UserID:       p.UserID(),
		LogicalPath:  full,
		NodeType:     store.NodeFile,
		FileChecksum: file.Checksum,
		Name:         resolved,
		ParentPath:   parent,
		SizePlain:    file.SizePlain,
		Mtime:        s.Now(),
		CreatedAt:    s.Now(),
	}
	if err := store.InsertNode(ctx, tx, node); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.Node{}, fmt.Errorf("%w: %s 已存在", ErrConflict, full)
		}
		return store.Node{}, err
	}
	return node, nil
}

// overwriteExisting 覆盖同名节点：先释放旧引用，再删除旧节点。
func (s *Service) overwriteExisting(ctx context.Context, tx store.Querier, userID int64, parent, name string) error {
	full, err := vpath.Join(parent, name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	node, err := store.GetNode(ctx, tx, userID, full)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if node.IsFolder() {
		return fmt.Errorf("%w: %s 是目录，不能覆盖", ErrConflict, full)
	}
	refs, err := store.SubtreeRefCounts(ctx, tx, userID, full)
	if err != nil {
		return err
	}
	if _, err := store.DeleteSubtree(ctx, tx, userID, full); err != nil {
		return err
	}
	for checksum, times := range refs {
		file, err := store.GetFile(ctx, tx, checksum)
		if err != nil {
			return err
		}
		// 覆盖会移除旧节点；已签发的票据不能继续绕过新的节点树访问
		// 旧内容。内容池是共享的，按校验码吊销是当前票据模型能表达的
		// 最窄安全边界。
		if _, err := store.RevokeTicketsByChecksum(ctx, tx, checksum); err != nil {
			return err
		}
		for i := int64(0); i < times; i++ {
			if _, _, err := store.ReleaseFileRef(ctx, tx, checksum); err != nil {
				return err
			}
		}
		if file.SizePlain > 0 {
			if _, err := store.ReleaseCounter(ctx, tx, store.ScopeStorage,
				store.UserCounterKey(userID, ""), file.SizePlain*times); err != nil {
				return err
			}
		}
	}
	return nil
}

// CancelUpload 主动取消上传并释放配额与临时文件。
func (s *Service) CancelUpload(ctx context.Context, p auth.Principal, sessionID string) error {
	task, err := s.ownUploadTask(ctx, p, sessionID)
	if err != nil {
		return err
	}
	return s.abortUpload(ctx, task)
}

// abortUpload 清理一次失败的上传：临时文件、上传会话、内容池占位行与配额预扣。
//
// 四处残留必须一起清掉：任何一处遗留都会造成"内容永远处于上传中"或"磁盘被
// 悄悄占满"这类难以归因的问题。
func (s *Service) abortUpload(ctx context.Context, task store.UploadTask) error {
	// 先清理临时文件；失败时保留会话，让下一轮维护仍能定位并重试，
	// 避免留下无法追踪的磁盘垃圾。
	if err := s.cleanupSession(task.ID); err != nil {
		return err
	}

	return s.DB.InTx(ctx, func(tx store.Querier) error {
		// 只有成功删除会话的调用拥有这笔预扣。并发收尾中后到的
		// abort 会得到 ErrNotFound，不能删除共享内容池行或再次回退额度。
		if err := store.DeleteUploadTask(ctx, tx, task.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}

		// 内容池占位行可能已被并发收尾请求推进为可用，或仍有其它
		// 上传会话在使用。只有确实还是无引用的上传中占位才允许删除。
		file, err := store.GetFile(ctx, tx, task.Checksum)
		if err == nil {
			live, liveErr := store.CountLiveUploadsByChecksum(ctx, tx, task.Checksum, s.Now())
			if liveErr != nil {
				return liveErr
			}
			if file.Status == store.FileUploading && file.RefCount == 0 && live == 0 {
				if err := store.DeleteFileRow(ctx, tx, task.Checksum); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}

		return s.releaseStorageQuotaQ(ctx, tx, task.UserID, task.SizePlain)
	})
}

func (s *Service) releaseStorageQuotaQ(ctx context.Context, q store.Querier, userID, size int64) error {
	if size <= 0 || userID == 0 {
		return nil
	}
	_, err := store.ReleaseCounter(ctx, q, store.ScopeStorage,
		store.UserCounterKey(userID, ""), size)
	return err
}

// ownUploadTask 取本人的上传会话。
func (s *Service) ownUploadTask(ctx context.Context, p auth.Principal, sessionID string) (store.UploadTask, error) {
	if err := auth.RequireUser(p); err != nil {
		return store.UploadTask{}, err
	}
	task, err := store.GetUploadTask(ctx, s.DB.R(), sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.UploadTask{}, ErrNotFound
		}
		return store.UploadTask{}, err
	}
	if task.UserID != p.UserID() {
		// 不暴露"该会话存在但不属于你"，统一按不存在处理。
		return store.UploadTask{}, ErrNotFound
	}
	if time.Now().UTC().After(task.ExpiresAt) {
		return store.UploadTask{}, fmt.Errorf("%w: 上传会话已过期", ErrNotFound)
	}
	return task, nil
}

func (s *Service) dedupScope(ctx context.Context) (string, error) {
	scope := s.Settings.Runtime(ctx).Storage.DedupScope
	switch scope {
	case settings.DedupGroup, settings.DedupGlobal, settings.DedupOff:
		return scope, nil
	default:
		return "", fmt.Errorf("%w: 秒传作用域配置无效", ErrUnavailable)
	}
}

// groupQuotaMap 返回该身份所在组的配额快照；BypassQuota 的身份返回 bypass=true。
func (s *Service) groupQuotaMap(ctx context.Context, p auth.Principal) (map[string]int64, bool, error) {
	if p.UnlimitedQuota() {
		return nil, true, nil
	}
	quotas, err := store.GroupQuotaMap(ctx, s.DB.R(), p.GroupName())
	if err != nil {
		return nil, false, fmt.Errorf("%w: 读取存储配额失败", ErrUnavailable)
	}
	return quotas, false, nil
}

// storageLimit 返回该身份的存储总量上限；不受限时返回 false。
// 组配额行值为 0 表示完全禁止，交由 ReserveCounter 的上限判定拒绝；
// 无行才是不受限——与建配额表时的文档语义一致。
func (s *Service) storageLimit(ctx context.Context, p auth.Principal) (int64, bool, error) {
	quotas, bypass, err := s.groupQuotaMap(ctx, p)
	if err != nil || bypass {
		return 0, false, err
	}
	limit, ok := quotas[store.QuotaStorageTotal]
	if !ok {
		return 0, false, nil
	}
	return limit, true, nil
}

func (s *Service) recordUpload(ctx context.Context, p auth.Principal, task store.UploadTask, wire int64) {
	// 明文与密文双口径记账：用户侧看明文，存储侧看密文（含每块 16 字节 tag）。
	detail := store.TrafficLog{
		ActorType:  p.Actor,
		UserID:     p.UserID(),
		ClientIP:   p.ClientIP.String(),
		GroupName:  p.GroupName(),
		Action:     "upload",
		BytesPlain: task.SizePlain,
		BytesWire:  wire,
	}
	daily := store.TrafficDaily{
		ActorKey:  p.ActorKey(),
		Day:       s.DayKey(),
		GroupName: p.GroupName(),
		UpPlain:   task.SizePlain,
		UpWire:    wire,
	}
	// 流量明细不是额度判定依据，交给缓冲批量写入，避免每次上传完成
	// 连续抢两次 SQLite 写锁。关闭数据库时缓冲会主动刷完。
	s.DB.Traffic().Record(detail, daily)
}

// ---------------------------------------------------------------- 临时文件

func (s *Service) sessionDir(sessionID string) string {
	return filepath.Join(s.TempDir, "uploads", sessionID)
}

func (s *Service) chunkPath(sessionID string, index int) string {
	return filepath.Join(s.sessionDir(sessionID), fmt.Sprintf("%06d.part", index))
}

func (s *Service) cleanupSession(sessionID string) error {
	return os.RemoveAll(s.sessionDir(sessionID))
}

func (s *Service) tempSubdir(name string) (string, error) {
	dir := filepath.Join(s.TempDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("%w: 创建临时目录失败", ErrUnavailable)
	}
	return dir, nil
}

// chunkSequenceReader 按序读取全部分片文件，形成一个连续的明文流。
//
// 同一时刻只打开一个分片：一次性打开全部分片会在大文件上撞上文件描述符上限，
// 而且会被误判成"分片缺失"。
type chunkSequenceReader struct {
	pathFor func(string, int) string
	task    store.UploadTask
	current *os.File
	// index 为 -1 表示尚未打开任何分片。
	index int
}

func (r *chunkSequenceReader) Read(p []byte) (int, error) {
	if r.task.ChunkTotal == 0 {
		return 0, io.EOF
	}
	for {
		if r.current == nil {
			// index 是最后打开的分片序号，下一个该打开 index+1。全部读完的
			// 判定必须是 index+1 越界：写成 index >= ChunkTotal 会让最后一个
			// 分片读完后继续去开一个不存在的分片，任何上传都无法收尾。
			if r.index+1 >= r.task.ChunkTotal {
				return 0, io.EOF
			}
			next := r.index + 1
			file, err := os.Open(r.pathFor(r.task.ID, next))
			if err != nil {
				return 0, fmt.Errorf("%w: 分片 %d 缺失", ErrUnavailable, next)
			}
			r.current = file
			r.index = next
		}
		n, err := r.current.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			if closeErr := r.current.Close(); closeErr != nil {
				r.current = nil
				return 0, fmt.Errorf("%w: 关闭分片 %d 失败", ErrUnavailable, r.index)
			}
			r.current = nil
			continue
		}
		return 0, err
	}
}

func (r *chunkSequenceReader) Close() error {
	if r.current != nil {
		err := r.current.Close()
		r.current = nil
		return err
	}
	return nil
}
