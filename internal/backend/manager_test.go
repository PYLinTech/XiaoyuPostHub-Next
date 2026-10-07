package backend

import (
	"context"
	"errors"
	"io"
	"testing"
)

// switcherFake 是同时实现 Backend 与 DirectLinkSwitcher 的测试后端，
// 用于验证 Manager 对可选能力的转发。
type switcherFake struct {
	toggles []bool
	err     error
}

func (f *switcherFake) Kind() string { return "fake" }
func (f *switcherFake) Put(context.Context, PutRequest) (PutResult, error) {
	return PutResult{}, errors.New("fake: 未实现")
}
func (f *switcherFake) Open(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, errors.New("fake: 未实现")
}
func (f *switcherFake) Delete(context.Context, string) error { return errors.New("fake: 未实现") }
func (f *switcherFake) Stat(context.Context, string) (int64, error) {
	return 0, errors.New("fake: 未实现")
}
func (f *switcherFake) Presign(context.Context, string, PresignOptions) (string, error) {
	return "", errors.New("fake: 未实现")
}
func (f *switcherFake) PresignReady() bool    { return true }
func (f *switcherFake) SliceMD5Enabled() bool { return false }

func (f *switcherFake) SetDirectLink(_ context.Context, enabled bool) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.toggles = append(f.toggles, enabled)
	return "fake-dir", nil
}

// TestManagerForwardsSetDirectLink：生产装配里 Service.Backend 持有 *Manager，
// 直链开关必须经 Manager 转发到当前后端；该转发若缺失，配置保存会直接 501。
func TestManagerForwardsSetDirectLink(t *testing.T) {
	m := NewManager()
	fake := &switcherFake{}
	m.build = func(Pan123Config) (Backend, error) { return fake, nil }
	if err := m.Reconfigure(Pan123Config{ClientID: "cid", ClientSecret: "secret"}); err != nil {
		t.Fatalf("Reconfigure 失败: %v", err)
	}

	name, err := m.SetDirectLink(context.Background(), true)
	if err != nil || name != "fake-dir" {
		t.Fatalf("启用转发结果不正确: name=%q err=%v", name, err)
	}
	if _, err := m.SetDirectLink(context.Background(), false); err != nil {
		t.Fatalf("关闭转发失败: %v", err)
	}
	if got := fake.toggles; len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("开关序列应为 [true false]，实得 %v", got)
	}
}

// TestManagerSetDirectLinkUnconfigured：未配置后端时开关请求必须得到
// ErrNotSupported，而不是 panic 或静默成功。
func TestManagerSetDirectLinkUnconfigured(t *testing.T) {
	m := NewManager()
	if _, err := m.SetDirectLink(context.Background(), true); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("未配置时应返回 ErrNotSupported，实得 %v", err)
	}
}
