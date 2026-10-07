package settings

import (
	"context"
	"testing"
	"time"
)

func TestMailRuntimeDefaults(t *testing.T) {
	rt := buildRuntime(nil)
	m := rt.Mail
	if m.ReceiveEnabled {
		t.Fatal("收件默认应关闭")
	}
	if m.Listen != ":25" {
		t.Fatalf("默认监听地址 = %q，应为 :25", m.Listen)
	}
	if m.MaxMessageSize != 25<<20 {
		t.Fatalf("入站单封上限 = %d，应为 25MiB", m.MaxMessageSize)
	}
	if m.SPFPolicy != SPFHard {
		t.Fatalf("SPF 默认策略 = %q，应为 hard", m.SPFPolicy)
	}
	if m.AddressesDefault != 1 {
		t.Fatalf("默认地址数 = %d，应为 1", m.AddressesDefault)
	}
	if m.ArchiveRetention != 720*time.Hour {
		t.Fatalf("邮件归档留存 = %v，应为 720h", m.ArchiveRetention)
	}
}

func TestMailRuntimeOverrides(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.SetMany(ctx, map[Key]string{
		KeyMailReceiveEnabled:   "true",
		KeyMailAddressesDefault: "3",
		KeyMailSPFPolicy:        SPFMark,
		KeyMailArchiveRetention: "48h",
	}, 1); err != nil {
		t.Fatalf("写入邮件配置失败: %v", err)
	}
	m := s.Runtime(ctx).Mail
	if !m.ReceiveEnabled || m.AddressesDefault != 3 || m.SPFPolicy != SPFMark ||
		m.ArchiveRetention != 48*time.Hour {
		t.Fatalf("覆盖后快照不符预期: %+v", m)
	}
}

func TestMailSizeInvalidFallsBackToSentinel(t *testing.T) {
	// 损坏的大小配置不能被当成 0（不限）放行：返回 -1 由业务层拒绝。
	rt := buildRuntime(map[Key]string{KeyMailMaxMessageSize: "abc"})
	if rt.Mail.MaxMessageSize != -1 {
		t.Fatalf("非法入站上限兜底 = %d，应为 -1", rt.Mail.MaxMessageSize)
	}
}
