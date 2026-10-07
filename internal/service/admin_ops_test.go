package service

// 直链空间开关（配置勾选即指令）的编排测试。探针本体的链路测试在
// internal/backend/probe_test.go（驱动层）。

import (
	"context"
	"errors"
	"testing"
)

// TestApplyDirectLinkSettingTogglesBackend：勾选/取消勾选必须原样转成后端的
// 启用/禁用调用——配置项本身就是开关指令。
func TestApplyDirectLinkSettingTogglesBackend(t *testing.T) {
	f := newShareFixture(t)
	stub := newStorageStub()
	f.svc.Backend = stub
	admin := f.adminUser("direct-link-admin")

	if _, err := f.svc.AdminApplyDirectLink(context.Background(), admin, true); err != nil {
		t.Fatalf("启用直链空间失败: %v", err)
	}
	if _, err := f.svc.AdminApplyDirectLink(context.Background(), admin, false); err != nil {
		t.Fatalf("关闭直链空间失败: %v", err)
	}
	if got := stub.directLinkToggles(); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("开关调用序列应为 [true false]，实得 %v", got)
	}
}

// TestAdminApplyDirectLinkRejectsNonAdmin：直链开关从系统配置入口触发，
// 只认 AdminSystem 权限，普通用户不得调用。
func TestAdminApplyDirectLinkRejectsNonAdmin(t *testing.T) {
	f := newShareFixture(t)
	f.svc.Backend = newStorageStub()
	if _, err := f.svc.AdminApplyDirectLink(context.Background(), f.user, true); err == nil {
		t.Fatal("普通用户不应能切换直链空间")
	}
}

// TestApplyDirectLinkSettingRequiresReadyBackend：未配置后端时启用必须报错，
// 关闭是无代价 no-op；后端执行失败必须如实上抛，不能谎报成功。
func TestApplyDirectLinkSettingRequiresReadyBackend(t *testing.T) {
	f := newShareFixture(t)

	if _, err := f.svc.ApplyDirectLinkSetting(context.Background(), true); err == nil {
		t.Fatal("存储未就绪时启用直链空间必须失败")
	}
	if _, err := f.svc.ApplyDirectLinkSetting(context.Background(), false); err != nil {
		t.Fatalf("存储未就绪时关闭直链空间应为 no-op，实得错误: %v", err)
	}

	stub := newStorageStub()
	stub.toggleErr = errors.New("stub: 开关接口不可用")
	f.svc.Backend = stub
	if _, err := f.svc.ApplyDirectLinkSetting(context.Background(), true); err == nil {
		t.Fatal("后端开关失败必须向上返回错误")
	}
	if got := stub.directLinkToggles(); len(got) != 0 {
		t.Fatalf("失败的开关调用不应被记录，实得 %v", got)
	}
}
