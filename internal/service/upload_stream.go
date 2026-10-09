package service

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// queueStreamingUploadIfReady 仅在下一卷需要的输入分片全部到齐后才占用公平 worker。
func (s *Service) queueStreamingUploadIfReady(ctx context.Context, task store.UploadTask) error {
	if !task.Streaming {
		return nil
	}
	state, err := store.GetUploadStreamState(ctx, s.DB.R(), task.ID)
	if err != nil {
		return err
	}
	if !streamWindowReady(task, state.NextPlainOffset, task.VolumeSize, s.BlockSize(ctx)) {
		return nil
	}
	if streamFinalWindowWaitingForChecksum(task, state.NextPlainOffset, task.VolumeSize, s.BlockSize(ctx)) {
		// 最后一卷要用完整摘要做服务端交叉校验，必须等前端哈希完成并绑定摘要。
		return nil
	}
	return s.DB.InTx(ctx, func(tx store.Querier) error {
		return store.QueueReceivingUploadJob(ctx, tx, task.ID)
	})
}

func uploadStagingAdmission(ctx context.Context, q store.Querier, stagingLimit, proposedVolume int64) (int64, error) {
	if stagingLimit <= 0 || proposedVolume <= xph.HeaderSize {
		return 0, fmt.Errorf("%w: 上传暂存或分卷配置无效", ErrUnavailable)
	}
	var activeVolume int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(volume_size), 0) FROM upload_tasks WHERE streaming = 1`).Scan(&activeVolume); err != nil {
		return 0, err
	}
	if proposedVolume > activeVolume {
		activeVolume = proposedVolume
	}
	if activeVolume >= stagingLimit/2 {
		return 0, fmt.Errorf("%w: 分卷上限必须小于暂存空间的一半", ErrUnavailable)
	}
	// 队列同一时刻只领取一个流式任务，因此只需为一个最大密文卷保留空间。
	return stagingLimit - activeVolume, nil
}

func streamWindowSize(task store.UploadTask, maxVolume, blockSize int64) int64 {
	if maxVolume <= xph.HeaderSize || blockSize <= 0 || task.ChunkSize <= 0 {
		return 0
	}
	unit := blockSize + xph.TagSize
	blocksPerChunk := task.ChunkSize / blockSize
	if blocksPerChunk <= 0 {
		return 0
	}
	blocks := (maxVolume - xph.HeaderSize) / unit
	blocks = blocks / blocksPerChunk * blocksPerChunk
	if blocks <= 0 || blocks > int64(^uint64(0)>>1)/blockSize {
		return 0
	}
	return blocks * blockSize
}

func streamWindowReady(task store.UploadTask, offset, maxVolume, blockSize int64) bool {
	if offset < 0 || offset >= task.SizePlain {
		return task.SizePlain == 0 && offset == 0
	}
	window := streamWindowSize(task, maxVolume, blockSize)
	if window <= 0 {
		return false
	}
	length := window
	if remain := task.SizePlain - offset; remain < length {
		length = remain
		if !task.Complete() {
			return false
		}
	}
	first := int(offset / task.ChunkSize)
	last := int((offset + length - 1) / task.ChunkSize)
	for i := first; i <= last; i++ {
		if !store.HasBit(task.ReceivedMask, i) {
			return false
		}
	}
	return true
}

func uploadExpectedChecksum(task store.UploadTask) string {
	if task.ExpectedChecksum != "" {
		return task.ExpectedChecksum
	}
	if !strings.HasPrefix(task.Checksum, "pending:") {
		return task.Checksum
	}
	return ""
}

func streamFinalWindowWaitingForChecksum(task store.UploadTask, offset, maxVolume, blockSize int64) bool {
	if !task.Complete() || uploadExpectedChecksum(task) != "" || offset < 0 || offset > task.SizePlain {
		return false
	}
	return streamWindowSize(task, maxVolume, blockSize) >= task.SizePlain-offset
}

// processStreamingUploadWindow 每次只处理一卷，处理后将任务重新排队或退回收片态，
// 使超大文件不会独占 worker，也不会把全部明文或密文堆在服务器暂存盘。
func (s *Service) processStreamingUploadWindow(ctx context.Context, p auth.Principal, task store.UploadTask) error {
	if !task.Streaming {
		return fmt.Errorf("流式上传任务标志不匹配")
	}
	state, err := store.GetUploadStreamState(ctx, s.DB.R(), task.ID)
	if err != nil {
		return err
	}
	if !streamWindowReady(task, state.NextPlainOffset, task.VolumeSize, s.BlockSize(ctx)) {
		return s.DB.InTx(ctx, func(tx store.Querier) error {
			latest, err := store.GetUploadTask(ctx, tx, task.ID)
			if err != nil {
				return err
			}
			if streamWindowReady(latest, state.NextPlainOffset, latest.VolumeSize, s.BlockSize(ctx)) {
				_, err := tx.ExecContext(ctx, `UPDATE upload_jobs SET state = 'queued', updated_at = ? WHERE session_id = ? AND state = 'processing'`, store.Now(), task.ID)
				return err
			}
			return store.SetUploadJobReceiving(ctx, tx, task.ID)
		})
	}
	if streamFinalWindowWaitingForChecksum(task, state.NextPlainOffset, task.VolumeSize, s.BlockSize(ctx)) {
		return s.DB.InTx(ctx, func(tx store.Querier) error {
			return store.SetUploadJobReceiving(ctx, tx, task.ID)
		})
	}

	window := streamWindowSize(task, task.VolumeSize, s.BlockSize(ctx))
	plainSize := window
	if remain := task.SizePlain - state.NextPlainOffset; remain < plainSize {
		plainSize = remain
	}
	if plainSize <= 0 || state.NextPlainOffset%s.BlockSize(ctx) != 0 {
		return fmt.Errorf("流式上传窗口没有对齐加密块")
	}
	parts, err := store.ListUploadParts(ctx, s.DB.R(), task.ID)
	if err != nil {
		return err
	}
	partNo := len(parts) + 1
	var wireOffset int64
	for _, part := range parts {
		if part.WireOffset+part.WireSize > wireOffset {
			wireOffset = part.WireOffset + part.WireSize
		}
	}

	cipherPath, cipherMD5, nextHashState, plainDigest, err := s.encryptUploadWindow(ctx, task, state, partNo, plainSize)
	if err != nil {
		return err
	}
	defer os.Remove(cipherPath)
	cipherFile, err := os.Open(cipherPath)
	if err != nil {
		return fmt.Errorf("%w: 打开密文卷失败", ErrUnavailable)
	}
	defer cipherFile.Close()
	info, err := cipherFile.Stat()
	if err != nil {
		return err
	}
	panDir, panName, err := s.ObjectName(ctx, task.Checksum)
	if err != nil {
		return err
	}
	partName := fmt.Sprintf("%s.xph-%05d", panName, partNo)
	var progressMu sync.Mutex
	lastProgressWrite := time.Time{}
	put, err := s.Backend.Put(ctx, backend.PutRequest{
		LogicalName: partName,
		ParentDir:   panDir,
		SizePlain:   plainSize,
		SizeWire:    info.Size(),
		CipherMD5:   cipherMD5,
		Source:      cipherFile,
		BlockSize:   s.BlockSize(ctx),
		MaxPartSize: task.VolumeSize,
		OnProgress: func(uploaded int64) {
			progressMu.Lock()
			defer progressMu.Unlock()
			now := time.Now()
			if !lastProgressWrite.IsZero() && now.Sub(lastProgressWrite) < time.Second && uploaded < info.Size() {
				return
			}
			lastProgressWrite = now
			fraction := float64(uploaded) / float64(info.Size())
			plainDone := int64(float64(plainSize) * fraction)
			if plainDone > plainSize {
				plainDone = plainSize
			}
			_ = store.SetUploadJobProgress(ctx, s.DB.W(), task.ID, state.NextPlainOffset+plainDone)
		},
	})
	if err != nil {
		return fmt.Errorf("%w: 上传密文卷 %d 失败: %w", ErrUnavailable, partNo, err)
	}

	part := store.UploadPart{
		SessionID: task.ID, PartNo: partNo, ObjectRef: put.ObjectRef,
		ObjectName: panDir + "/" + partName, PlainOffset: state.NextPlainOffset,
		PlainSize: plainSize, WireOffset: wireOffset, WireSize: info.Size(), CipherMD5: cipherMD5,
	}
	final := state.NextPlainOffset+plainSize == task.SizePlain
	expectedChecksum := uploadExpectedChecksum(task)
	if final {
		if expectedChecksum == "" {
			return errors.Join(fmt.Errorf("%w: 文件校验尚未完成", ErrBadRequest), s.Backend.Delete(ctx, put.ObjectRef))
		}
		if !strings.EqualFold(plainDigest, expectedChecksum) {
			deleteErr := s.Backend.Delete(ctx, put.ObjectRef)
			return errors.Join(fmt.Errorf("%w: 明文校验码与声明不一致", ErrBadRequest), deleteErr)
		}
	}
	if err := cipherFile.Close(); err != nil {
		return errors.Join(fmt.Errorf("关闭密文卷失败: %w", err), s.Backend.Delete(ctx, put.ObjectRef))
	}
	if err := os.Remove(cipherPath); err != nil && !os.IsNotExist(err) {
		return errors.Join(fmt.Errorf("移除已上传密文卷暂存失败: %w", err), s.Backend.Delete(ctx, put.ObjectRef))
	}

	if final {
		// 已上传的当前卷必须可重新定位，后续任一事务失败都会进入统一清理路径。
		if err := s.removeConsumedChunks(ctx, task, state.NextPlainOffset+plainSize); err != nil {
			return errors.Join(err, s.Backend.Delete(ctx, put.ObjectRef))
		}
	}

	var node store.Node
	err = s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := store.AdvanceUploadStream(ctx, tx, task.ID, state.NextPlainOffset,
			state.NextPlainOffset+plainSize, nextHashState, part); err != nil {
			return err
		}
		if final {
			allParts, err := store.ListUploadParts(ctx, tx, task.ID)
			if err != nil {
				return err
			}
			objectParts := make([]backend.ObjectPart, 0, len(allParts))
			var totalWire int64
			for _, item := range allParts {
				objectParts = append(objectParts, backend.ObjectPart{Ref: item.ObjectRef, Offset: item.WireOffset, Size: item.WireSize})
				if item.WireOffset+item.WireSize > totalWire {
					totalWire = item.WireOffset + item.WireSize
				}
			}
			ref, err := backend.ComposeObjectRef(objectParts)
			if err != nil {
				return err
			}
			if err := s.requireFolderTx(ctx, tx, p.UserID(), task.TargetParentPath); err != nil {
				return err
			}
			if err := store.FinalizeFile(ctx, tx, expectedChecksum, ref, panDir+"/"+panName, totalWire); err != nil {
				return err
			}
			if task.Checksum != expectedChecksum {
				if err := store.DeleteFileRow(ctx, tx, task.Checksum); err != nil {
					return err
				}
			}
			if _, err := store.AddFileRef(ctx, tx, expectedChecksum); err != nil {
				return err
			}
			file, err := store.GetFile(ctx, tx, expectedChecksum)
			if err != nil {
				return err
			}
			node, err = s.placeFileNodeTx(ctx, tx, p, task.TargetParentPath, task.TargetName, task.ConflictAction, file)
			if err != nil {
				return err
			}
			if err := store.DeleteUploadTask(ctx, tx, task.ID); err != nil {
				return err
			}
			result, err := json.Marshal(node)
			if err != nil {
				return err
			}
			return store.CompleteUploadJobInTx(ctx, tx, task.ID, string(result))
		}
		if err := store.SetUploadJobProgress(ctx, tx, task.ID, state.NextPlainOffset+plainSize); err != nil {
			return err
		}
		latestTask, err := store.GetUploadTask(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		if streamWindowReady(latestTask, state.NextPlainOffset+plainSize, latestTask.VolumeSize, s.BlockSize(ctx)) {
			_, err := tx.ExecContext(ctx, `UPDATE upload_jobs SET state = 'queued', updated_at = ? WHERE session_id = ? AND state = 'processing'`, store.Now(), task.ID)
			return err
		}
		return store.SetUploadJobReceiving(ctx, tx, task.ID)
	})
	if err != nil {
		return errors.Join(err, s.Backend.Delete(ctx, put.ObjectRef))
	}
	if final {
		if cleanupErr := s.cleanupSession(task.ID); cleanupErr != nil {
			log.Printf("service: 流式上传 %s 已完成，但临时目录清理失败: %v", task.ID, cleanupErr)
		}
		s.recordUpload(ctx, p, task, wireOffset+info.Size())
		return nil
	}
	if err := s.removeConsumedChunks(ctx, task, state.NextPlainOffset+plainSize); err != nil {
		log.Printf("service: 流式上传 %s 已提交卷 %d，但释放输入暂存失败: %v", task.ID, partNo, err)
	}
	return nil
}

func (s *Service) encryptUploadWindow(ctx context.Context, task store.UploadTask, state store.UploadStreamState, partNo int, plainSize int64) (path, cipherMD5 string, nextHash []byte, digest string, err error) {
	fileRow, err := store.GetFile(ctx, s.DB.R(), task.Checksum)
	if err != nil {
		return "", "", nil, "", err
	}
	kek, err := s.KEKFor(ctx, fileRow.KEKKeyID)
	if err != nil {
		return "", "", nil, "", err
	}
	dek, err := xph.UnwrapDEK(kek, fileRow.DEKEnvelope)
	if err != nil {
		return "", "", nil, "", fmt.Errorf("%w: 解开内容密钥失败", ErrUnavailable)
	}
	hdr := xph.Header{Algo: xph.AlgoAESGCM, BlockLog2: byte(fileRow.EncChunkLog2), PlainSize: task.SizePlain, NoncePrefix: uint32(fileRow.EncNoncePrefix)}
	copy(hdr.FileSalt[:], fileRow.EncSalt)
	hasher := sha256.New()
	if state.NextPlainOffset > 0 {
		unmarshal, ok := hasher.(encoding.BinaryUnmarshaler)
		if !ok {
			return "", "", nil, "", fmt.Errorf("%w: SHA-256 状态无法恢复", ErrUnavailable)
		}
		if err := unmarshal.UnmarshalBinary(state.SHA256State); err != nil {
			return "", "", nil, "", fmt.Errorf("%w: SHA-256 上传状态损坏", ErrUnavailable)
		}
	}
	if err := os.MkdirAll(filepath.Join(s.TempDir, "cipher"), 0o700); err != nil {
		return "", "", nil, "", fmt.Errorf("%w: 创建密文暂存目录失败", ErrUnavailable)
	}
	out, err := os.CreateTemp(filepath.Join(s.TempDir, "cipher"), task.ID+"-*.xphpart")
	if err != nil {
		return "", "", nil, "", fmt.Errorf("%w: 创建密文卷暂存文件失败", ErrUnavailable)
	}
	path = out.Name()
	ok := false
	defer func() {
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = closeErr
			ok = false
		}
		if !ok {
			_ = os.Remove(path)
		}
	}()
	md5Hash := md5.New()
	w := io.MultiWriter(out, md5Hash)
	if partNo == 1 {
		if _, err := w.Write(hdr.Marshal()); err != nil {
			return "", "", nil, "", err
		}
	}
	reader := &chunkRangeSequenceReader{task: task, pathFor: s.chunkPath, startChunk: int(state.NextPlainOffset / task.ChunkSize), remaining: plainSize}
	defer reader.Close()
	blockSize := hdr.BlockSize()
	block := make([]byte, blockSize)
	aad := hdr.AAD()
	index := state.NextPlainOffset / blockSize
	remaining := plainSize
	for remaining > 0 {
		want := blockSize
		if remaining < want {
			want = remaining
		}
		if _, err := io.ReadFull(reader, block[:want]); err != nil {
			return "", "", nil, "", fmt.Errorf("%w: 流式上传分片读取失败: %v", ErrUnavailable, err)
		}
		if _, err := hasher.Write(block[:want]); err != nil {
			return "", "", nil, "", err
		}
		cipherBlock, err := xph.ChunkEncrypt(dek, aad, xph.BlockNonce(hdr.NoncePrefix, index), block[:want])
		if err != nil {
			return "", "", nil, "", err
		}
		if _, err := w.Write(cipherBlock); err != nil {
			return "", "", nil, "", err
		}
		remaining -= want
		index++
	}
	if err := out.Sync(); err != nil {
		return "", "", nil, "", fmt.Errorf("%w: 密文卷落盘失败", ErrUnavailable)
	}
	stateMarshaler, ok := hasher.(encoding.BinaryMarshaler)
	if !ok {
		return "", "", nil, "", fmt.Errorf("%w: SHA-256 状态无法保存", ErrUnavailable)
	}
	nextHash, err = stateMarshaler.MarshalBinary()
	if err != nil {
		return "", "", nil, "", err
	}
	digest = hex.EncodeToString(hasher.Sum(nil))
	cipherMD5 = hex.EncodeToString(md5Hash.Sum(nil))
	ok = true
	return path, cipherMD5, nextHash, digest, nil
}

func (s *Service) removeConsumedChunks(ctx context.Context, task store.UploadTask, endOffset int64) error {
	last := int((endOffset + task.ChunkSize - 1) / task.ChunkSize)
	for i := 0; i < last; i++ {
		if err := os.Remove(s.chunkPath(task.ID, i)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("移除已处理分片 %d 失败: %w", i, err)
		}
		if err := store.ReleaseUploadStagingChunk(ctx, s.DB.W(), task.ID, i); err != nil {
			return err
		}
	}
	return nil
}

type chunkRangeSequenceReader struct {
	task       store.UploadTask
	pathFor    func(string, int) string
	startChunk int
	remaining  int64
	current    *os.File
	chunkLeft  int64
	index      int
}

func (r *chunkRangeSequenceReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	for {
		if r.current == nil {
			idx := r.startChunk + r.index
			if idx >= r.task.ChunkTotal {
				return 0, io.ErrUnexpectedEOF
			}
			file, err := os.Open(r.pathFor(r.task.ID, idx))
			if err != nil {
				return 0, err
			}
			r.current = file
			r.chunkLeft = r.task.ChunkSize
			if idx == r.task.ChunkTotal-1 {
				r.chunkLeft = r.task.SizePlain - int64(idx)*r.task.ChunkSize
			}
		}
		want := int64(len(p))
		if want > r.remaining {
			want = r.remaining
		}
		if want > r.chunkLeft {
			want = r.chunkLeft
		}
		n, err := r.current.Read(p[:int(want)])
		r.remaining -= int64(n)
		r.chunkLeft -= int64(n)
		if r.remaining == 0 {
			return n, nil
		}
		if err == io.EOF && r.chunkLeft > 0 {
			return n, io.ErrUnexpectedEOF
		}
		if r.chunkLeft == 0 {
			if closeErr := r.current.Close(); closeErr != nil {
				return n, closeErr
			}
			r.current = nil
			r.index++
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			return n, err
		}
		if n == 0 {
			return 0, io.ErrNoProgress
		}
		return n, nil
	}
}

func (r *chunkRangeSequenceReader) Close() error {
	if r.current == nil {
		return nil
	}
	err := r.current.Close()
	r.current = nil
	return err
}
