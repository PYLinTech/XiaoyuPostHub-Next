package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

func TestMailBodyContainerRoundTrip(t *testing.T) {
	cases := []MailBody{
		{HTML: "<p>hi</p>", Text: "hi"},
		{HTML: "", Text: "only text"},
		{HTML: "<p>only html</p>", Text: ""},
		{}, // 极端邮件：只有附件、没有正文，容器仍要存在
	}
	for i, want := range cases {
		raw, err := EncodeMailBody(want)
		if err != nil {
			t.Fatalf("case %d 编码失败: %v", i, err)
		}
		got, err := DecodeMailBody(raw)
		if err != nil {
			t.Fatalf("case %d 解码失败: %v", i, err)
		}
		if got != want {
			t.Fatalf("case %d 往返不一致: %+v ≠ %+v", i, got, want)
		}
	}
	if _, err := DecodeMailBody([]byte("{not json")); err == nil {
		t.Fatal("非法容器字节应返回错误")
	}
}

func TestIngestPlaintextDedupAndRefCount(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	stub := newStorageStub()
	f.svc.Backend = stub

	ctx := context.Background()
	payload := bytes.Repeat([]byte("邮件正文-"), 2000)

	first, err := f.svc.IngestPlaintext(ctx, bytes.NewReader(payload), f.user.UserID())
	if err != nil {
		t.Fatalf("首次入库失败: %v", err)
	}
	if first.Status != store.FileNormal {
		t.Fatalf("入库记录状态 = %d，应为 normal", first.Status)
	}
	if first.RefCount != 0 {
		t.Fatalf("入库记录引用计数 = %d，应为 0（由调用方 AddFileRef）", first.RefCount)
	}
	if first.SizePlain != int64(len(payload)) {
		t.Fatalf("明文长度 = %d，应为 %d", first.SizePlain, len(payload))
	}
	if stub.next != 1 {
		t.Fatalf("首次入库应上传 1 个后端对象，实际 %d", stub.next)
	}

	// 同样内容再入一次：物理对象复用，不再上传后端。
	second, err := f.svc.IngestPlaintext(ctx, bytes.NewReader(payload), f.user.UserID())
	if err != nil {
		t.Fatalf("二次入库失败: %v", err)
	}
	if second.Checksum != first.Checksum {
		t.Fatalf("相同内容应命中同一内容池对象: %s ≠ %s", second.Checksum, first.Checksum)
	}
	if stub.next != 1 {
		t.Fatalf("重复内容不应再次上传后端，Put 次数 = %d", stub.next)
	}
	if second.RefCount != 0 {
		t.Fatalf("复用对象不应改变引用计数 = %d", second.RefCount)
	}

	// 不同内容应真正产生第二个对象。
	other, err := f.svc.IngestPlaintext(ctx, bytes.NewReader([]byte("完全不同的另一封邮件内容xxxxxxxx")), f.user.UserID())
	if err != nil {
		t.Fatalf("不同内容入库失败: %v", err)
	}
	if other.Checksum == first.Checksum {
		t.Fatal("不同内容不应复用对象")
	}
	if stub.next != 2 {
		t.Fatalf("不同内容应再次上传后端，Put 次数 = %d", stub.next)
	}
}

func TestIngestPlaintextRequiresReadyBackends(t *testing.T) {
	f := newShareFixture(t)
	// 只配密钥、不装后端：应明确报存储未就绪。
	f.enableEncryption()
	_, err := f.svc.IngestPlaintext(context.Background(), bytes.NewReader([]byte("x")), f.user.UserID())
	if !errors.Is(err, ErrStorageNotReady) {
		t.Fatalf("存储未就绪应返 ErrStorageNotReady，实际: %v", err)
	}
}
