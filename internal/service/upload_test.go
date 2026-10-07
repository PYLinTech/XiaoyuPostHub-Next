package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
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

	for _, bad := range []string{"", "abc", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		if _, err := f.initUpload(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("校验码 %q 应被拒绝，实得: %v", bad, err)
		}
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
