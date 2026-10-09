package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 测试用主密钥：32 字节全零的 URL 安全 Base64（43 字符，无填充）。
const testKeyring = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// enableEncryption 给 fixture 配上主密钥集合 —— InitUpload 的前置条件之一。
func (f *shareFixture) enableEncryption() {
	f.t.Helper()
	if err := f.svc.Settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCryptokeys: testKeyring,
	}, 0); err != nil {
		f.t.Fatalf("配置主密钥失败: %v", err)
	}
}

// initUpload 以给定校验码发起一次上传初始化。
func (f *shareFixture) initUpload(checksum string) (InitUploadResult, error) {
	f.t.Helper()
	return f.svc.InitUpload(context.Background(), f.user, InitUploadRequest{
		Checksum:   checksum,
		SizePlain:  1024,
		ParentPath: "/a",
		Name:       "sample.bin",
	})
}

// TestInitUploadRejectsMalformedChecksum 校验码格式在入口就被拒绝。
//
// 校验码是内容池主键，格式不对会让去重与秒传全部失去意义。
func TestInitUploadRejectsMalformedChecksum(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()

	for _, bad := range []string{"abc", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		if _, err := f.initUpload(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("校验码 %q 应被拒绝，实得: %v", bad, err)
		}
	}
	// 空摘要现在表示客户端会在传输期间并行计算；初始化应继续到存储就绪校验。
	if _, err := f.initUpload(""); !errors.Is(err, ErrStorageNotReady) {
		t.Errorf("缺省校验码应允许初始化，实得: %v", err)
	}
}

// TestInitUploadRequiresEncryptionKey 未配置主密钥时必须拒绝新上传并说明原因。
//
// 让它失败的代价远小于"写进去一份谁也解不开的数据"。
func TestInitUploadRequiresEncryptionKey(t *testing.T) {
	f := newShareFixture(t)
	// 刻意不调用 enableEncryption。
	_, err := f.initUpload(strings.Repeat("a", 64))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("未配置主密钥时应返回依赖不可用，实得: %v", err)
	}
}

// TestInitUploadFailsFastWithoutStorage 存储未就绪时必须在收到分片之前就失败。
//
// 这条断言守的是"用户不该白传一次"：早先的实现只在收尾写入存储时才失败，
// 于是客户端会把整份文件传完，然后拿到一句"服务暂时不可用"——流量与时间
// 全部白费，而问题的解法（管理员配置凭据）从一开始就已经确定。
func TestInitUploadFailsFastWithoutStorage(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption() // 加密就绪，但测试装置没有配置存储后端

	_, err := f.initUpload(strings.Repeat("a", 64))
	if !errors.Is(err, ErrStorageNotReady) {
		t.Fatalf("存储未就绪时应返回 ErrStorageNotReady，实得: %v", err)
	}
	// 必须与泛化的 ErrUnavailable 区分开：前者会给出"请联系管理员"的提示，
	// 后者只会建议重试。
	if errors.Is(err, ErrUnavailable) {
		t.Fatal("ErrStorageNotReady 不应同时被判定为 ErrUnavailable")
	}
}

func TestCompleteEmptyUpload(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()

	sum := sha256.Sum256(nil)
	result, err := f.svc.InitUpload(context.Background(), f.user, InitUploadRequest{
		Checksum:       hex.EncodeToString(sum[:]),
		SizePlain:      0,
		ParentPath:     "/a",
		Name:           "empty.bin",
		ConflictAction: ConflictReject,
	})
	if err != nil {
		t.Fatalf("空文件初始化失败: %v", err)
	}
	if result.ChunkTotal != 0 {
		t.Fatalf("空文件不应创建分片，实得 %d", result.ChunkTotal)
	}
	node, err := f.svc.CompleteUpload(context.Background(), f.user, result.SessionID)
	if err != nil {
		t.Fatalf("空文件收尾失败: %v", err)
	}
	if node.SizePlain != 0 || node.LogicalPath != "/a/empty.bin" {
		t.Fatalf("空文件节点不正确: %+v", node)
	}
}

func TestStreamingUploadSupportsFileLargerThanStagingLimit(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	ctx := context.Background()
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyUploadChunkSize:       "1M",
		settings.KeyUploadMaxStagingBytes: "5M",
		settings.KeyUploadMaxVolumeBytes:  "2M",
	}, 0); err != nil {
		t.Fatalf("设置小型流式测试参数失败: %v", err)
	}

	// 首卷在摘要绑定前到齐：流式路径必须允许先提交非末卷。
	payload := bytes.Repeat([]byte("stream"), (4<<20)/len("stream"))
	digest := sha256.Sum256(payload)
	result, err := f.svc.InitUpload(ctx, f.user, InitUploadRequest{
		SizePlain:  int64(len(payload)),
		ParentPath: "/a", Name: "larger-than-staging.bin",
	})
	if err != nil {
		t.Fatalf("初始化超暂存上限文件失败: %v", err)
	}
	task, err := store.GetUploadTask(ctx, f.db.R(), result.SessionID)
	if err != nil || !task.Streaming {
		t.Fatalf("超过暂存窗口的文件应进入流式模式，task=%+v err=%v", task, err)
	}

	workerCtx, stopWorkers := context.WithCancel(ctx)
	f.svc.RunUploadFinalizers(workerCtx)
	defer func() {
		stopWorkers()
		waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := f.svc.WaitUploadFinalizers(waitCtx); err != nil {
			t.Errorf("等待收尾 worker 退出失败: %v", err)
		}
	}()

	for index := 0; index < 1; index++ {
		start := int64(index) * result.ChunkSize
		end := start + result.ChunkSize
		if end > int64(len(payload)) {
			end = int64(len(payload))
		}
		if _, err := f.svc.UploadChunk(ctx, f.user, result.SessionID, index,
			bytes.NewReader(payload[start:end])); err != nil {
			t.Fatalf("接收第 %d 片失败: %v", index, err)
		}
	}
	// 等待第一个非末卷真正完成；旧逻辑会因摘要尚未绑定而把整项任务终止。
	streamDeadline := time.After(10 * time.Second)
	for {
		state, stateErr := store.GetUploadStreamState(ctx, f.db.R(), result.SessionID)
		if stateErr == nil && state.NextPlainOffset > 0 {
			break
		}
		if _, taskErr := store.GetUploadTask(ctx, f.db.R(), result.SessionID); taskErr != nil {
			status, _, _ := f.svc.UploadJobStatusForUser(ctx, f.user, result.SessionID)
			t.Fatalf("摘要绑定前处理首卷应成功，任务已消失，状态=%+v err=%v", status, stateErr)
		}
		select {
		case <-streamDeadline:
			t.Fatalf("等待首个流式分卷超时：state=%+v err=%v", state, stateErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	for index := 1; index < result.ChunkTotal; index++ {
		start := int64(index) * result.ChunkSize
		end := start + result.ChunkSize
		if end > int64(len(payload)) {
			end = int64(len(payload))
		}
		if _, err := f.svc.UploadChunk(ctx, f.user, result.SessionID, index,
			bytes.NewReader(payload[start:end])); err != nil {
			t.Fatalf("接收第 %d 片失败: %v", index, err)
		}
	}
	if _, err := f.svc.ResolveUploadChecksum(ctx, f.user, result.SessionID, hex.EncodeToString(digest[:])); err != nil {
		t.Fatalf("绑定并行计算出的校验码失败: %v", err)
	}
	if _, err := f.svc.QueueUploadCompletion(ctx, f.user, result.SessionID); err != nil {
		t.Fatalf("提交流式收尾失败: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		status, found, err := f.svc.UploadJobStatusForUser(ctx, f.user, result.SessionID)
		if err != nil {
			t.Fatalf("读取收尾状态失败: %v", err)
		}
		if found && status.State == "done" {
			break
		}
		if found && status.State == "error" {
			t.Fatalf("流式收尾失败: %s", status.Error)
		}
		select {
		case <-deadline:
			t.Fatalf("流式收尾超时，最近状态：%+v", status)
		case <-time.After(10 * time.Millisecond):
		}
	}

	file, err := store.GetFile(ctx, f.db.R(), hex.EncodeToString(digest[:]))
	if err != nil || file.Status != store.FileNormal {
		t.Fatalf("超大文件内容池登记失败：file=%+v err=%v", file, err)
	}
	parts, err := backend.SplitObjectRef(file.PanFileID, file.PanSizeWire)
	if err != nil || len(parts) != 4 {
		t.Fatalf("4 个密文分卷应合成为一个对象，parts=%+v err=%v", parts, err)
	}
	if used, err := store.UploadStagingUsage(ctx, f.db.R()); err != nil || used != 0 {
		t.Fatalf("完成后应释放全部暂存记录，used=%d err=%v", used, err)
	}
}

func TestParallelChecksumReupload(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(map[bool]string{false: "archive", true: "purged"}[purge], func(t *testing.T) {
			f := newShareFixture(t)
			f.enableEncryption()
			f.enableStorage()
			ctx := context.Background()
			completeUpload(t, f, contentA, "old.bin")
			batch := stagedThenArchiveDeleted(t, f, "/a/old.bin")
			if purge {
				purgeBatch(t, f, batch)
			}
			result, err := f.svc.InitUpload(ctx, f.user, InitUploadRequest{
				SizePlain: int64(len(contentA)), ParentPath: "/a", Name: "new.bin",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.UploadChunk(ctx, f.user, result.SessionID, 0, bytes.NewReader(contentA)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.ResolveUploadChecksum(ctx, f.user, result.SessionID, csA); err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.CompleteUpload(ctx, f.user, result.SessionID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAbortUploadUsesLatestChecksum(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	ctx := context.Background()
	result, err := f.initUpload("")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.GetUploadTask(ctx, f.db.R(), result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ResolveUploadChecksum(ctx, f.user, result.SessionID, csA); err != nil {
		t.Fatal(err)
	}
	if n, err := store.CountLiveUploadsByChecksum(ctx, f.db.R(), csA, f.svc.Now()); err != nil || n != 1 {
		t.Fatalf("bound checksum must retain its owner: count=%d err=%v", n, err)
	}
	if _, err := f.initUpload(csA); !errors.Is(err, ErrBusy) {
		t.Fatalf("known-checksum upload must not replace the bound placeholder: %v", err)
	}
	if _, err := store.GetFile(ctx, f.db.R(), csA); err != nil {
		t.Fatalf("competing upload deleted bound placeholder: %v", err)
	}
	if err := f.svc.abortUpload(ctx, stale); err != nil {
		t.Fatal(err)
	}
	for _, checksum := range []string{csA, stale.Checksum} {
		if _, err := store.GetFile(ctx, f.db.R(), checksum); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("placeholder %s remains: %v", checksum, err)
		}
	}
}

func TestParallelDedupIsRecoverable(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	ctx := context.Background()
	completeUpload(t, f, contentA, "existing.bin")
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{settings.KeyDedupScope: "global"}, 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"existing.bin", "copy.bin"} {
		result, err := f.svc.InitUpload(ctx, f.user, InitUploadRequest{
			SizePlain: int64(len(contentA)), ParentPath: "/a", Name: name, ConflictAction: ConflictReject,
		})
		if err != nil {
			t.Fatal(err)
		}
		node, err := f.svc.ResolveUploadChecksum(ctx, f.user, result.SessionID, csA)
		if name == "existing.bin" {
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("expected name conflict: %v", err)
			}
			if _, err := store.GetUploadTask(ctx, f.db.R(), result.SessionID); err != nil {
				t.Fatalf("conflict destroyed resumable session: %v", err)
			}
			if err := f.svc.CancelUpload(ctx, f.user, result.SessionID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || node == nil {
			t.Fatalf("dedup failed: node=%+v err=%v", node, err)
		}
		retry, err := f.svc.ResolveUploadChecksum(ctx, f.user, result.SessionID, csA)
		if err != nil || retry == nil || retry.LogicalPath != node.LogicalPath {
			t.Fatalf("lost response must return same node: %+v %v", retry, err)
		}
	}
}

func TestChecksumPendingJobStillAcceptsResume(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	ctx := context.Background()
	result, err := f.initUpload("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UploadChunk(ctx, f.user, result.SessionID, 0, bytes.NewReader(contentA)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUploadJob(ctx, f.db, store.UploadJob{SessionID: result.SessionID, UserID: f.user.UserID()}, 0); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.svc.UploadJobStatusForUser(ctx, f.user, result.SessionID); err != nil || found {
		t.Fatalf("pending checksum must resume client work: found=%v err=%v", found, err)
	}
}

func TestHeadStreamDoesNotOpenOrConsume(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	ctx := context.Background()
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyDeliveryDirect: "false", settings.KeyDeliveryProxyDecrypt: "false",
	}, 0); err != nil {
		t.Fatal(err)
	}
	completeUpload(t, f, contentA, "sample.bin")
	plan := proxyCipherPlan(t, f)
	f.svc.Backend.(*storageStub).openErr = errors.New("HEAD must not open storage")
	stream, err := f.svc.HeadServerStreamForActor(ctx, plan.TicketID, f.user, "bytes=0-9")
	if err != nil || stream.Reader != nil || stream.Limit != 10 {
		t.Fatalf("invalid HEAD metadata: %+v %v", stream, err)
	}
	ticket, err := store.GetTicket(ctx, f.db.R(), plan.TicketID)
	if err != nil || ticket.UseCount != 0 || ticket.SettledBytes != 0 {
		t.Fatalf("HEAD consumed ticket: %+v %v", ticket, err)
	}
}

func TestParallelEmptyUploadRequiresChecksum(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	ctx := context.Background()
	result, err := f.svc.InitUpload(ctx, f.user, InitUploadRequest{ParentPath: "/a", Name: "empty-parallel.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.QueueUploadCompletion(ctx, f.user, result.SessionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unbound checksum must not enter completion queue: %v", err)
	}
	if _, err := f.svc.ResolveUploadChecksum(ctx, f.user, result.SessionID, payloadChecksum(nil)); err != nil {
		t.Fatal(err)
	}
	if node, err := f.svc.CompleteUpload(ctx, f.user, result.SessionID); err != nil || node.SizePlain != 0 {
		t.Fatalf("empty parallel upload failed: %+v %v", node, err)
	}
}
