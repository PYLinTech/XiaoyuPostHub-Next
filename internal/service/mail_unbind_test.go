package service

import (
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// unbindAdmin 造一个带 AdminUnbind 权限的管理员 Principal。
func unbindAdmin(ctx context.Context, t *testing.T, f *shareFixture) auth.Principal {
	t.Helper()
	now := store.Now()
	uid, err := store.CreateUser(ctx, f.db.W(), store.User{
		Account: "root", DisplayName: "root", PasswordHash: "x",
		GroupName: perm.GroupAdmin, Status: store.UserEnabled,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	user, err := store.GetUserByID(ctx, f.db.R(), uid)
	if err != nil {
		t.Fatalf("读取管理员失败: %v", err)
	}
	return adminPrincipal(ctx, t, f, user)
}

// TestRequestMyUnbindHappyPath 申请、撤销、再申请的完整用户流程。
func TestRequestMyUnbindHappyPath(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	addr := "alice@example.com"

	req, err := f.svc.RequestMyUnbind(ctx, f.user, addr, "  不再使用了  ")
	if err != nil {
		t.Fatalf("提交申请失败: %v", err)
	}
	if req.Status != store.UnbindPending {
		t.Fatalf("新申请应为 pending，实为 %s", req.Status)
	}
	if req.Reason != "不再使用了" {
		t.Fatalf("理由应去除首尾空白，实为 %q", req.Reason)
	}
	if req.UserID != f.user.UserID() {
		t.Fatalf("申请人应是本人")
	}

	// 同一地址不能重复申请。
	if _, err := f.svc.RequestMyUnbind(ctx, f.user, addr, "再来一次"); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复申请应 ErrConflict，实得 %v", err)
	}

	// 撤销后可再申请。
	if err := f.svc.CancelMyUnbind(ctx, f.user, req.ID); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if _, err := f.svc.RequestMyUnbind(ctx, f.user, addr, "换个理由"); err != nil {
		t.Fatalf("撤销后应能重新申请: %v", err)
	}
}

// TestRequestMyUnbindRejectsForeignAddress 不能申请不属于自己的地址。
//
// 关键点：错误必须是 BadRequest 而不是 NotFound 之外的东西——但**也不能**
// 泄露"这个地址存在"。地址不存在与不属于本人走同一条分支。
func TestRequestMyUnbindRejectsForeignAddress(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	if _, err := f.svc.RequestMyUnbind(ctx, f.user, "nobody@example.com", ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("他人地址应 BadRequest，实得 %v", err)
	}
	if _, err := f.svc.RequestMyUnbind(ctx, f.user, "", ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("空地址应 BadRequest，实得 %v", err)
	}
}

// TestRequestMyUnbindRejectsFrozen 冻结地址不给申请。
func TestRequestMyUnbindRejectsFrozen(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	addr := "alice@example.com"

	if err := store.SetMailAddressStatus(ctx, f.db.W(), addr, store.MailAddressFrozen); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RequestMyUnbind(ctx, f.user, addr, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("冻结地址应 BadRequest，实得 %v", err)
	}
}

// TestApproveUnbindByAdmin 管理员批准后地址消失、收件箱保留。
func TestApproveUnbindByAdmin(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := unbindAdmin(ctx, t, f.shareFixture)
	addr := "alice@example.com"

	if _, err := f.svc.RequestMyUnbind(ctx, f.user, addr, ""); err != nil {
		t.Fatalf("提交申请失败: %v", err)
	}
	list, err := f.svc.ListUnbindsForAdmin(ctx, admin, store.UnbindPending, "", "")
	if err != nil {
		t.Fatalf("管理端列表失败: %v", err)
	}
	if len(list.Items) != 1 || list.Stats.Pending != 1 {
		t.Fatalf("管理端应看到 1 条待审，实为 %+v", list)
	}

	approved, err := f.svc.ApproveUnbind(ctx, admin, list.Items[0].ID, "同意")
	if err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	if approved.Status != store.UnbindApproved {
		t.Fatalf("批准后状态应为 approved，实为 %s", approved.Status)
	}
	if _, err := store.GetMailAddress(ctx, f.db.R(), addr); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("批准后地址应消失，实得 %v", err)
	}
	// 审计应留下一条。
	var n int64
	f.db.R().QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action = 'mail.unbind.approve'`).Scan(&n)
	if n != 1 {
		t.Fatalf("应有 1 条批准审计，实得 %d", n)
	}
}

// TestUnbindAdminEndpointsRequirePermission 没有 AdminUnbind 就不能审核。
func TestUnbindAdminEndpointsRequirePermission(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	// f.user 是普通用户，没有 AdminUnbind。auth.RequirePermission 返回的是
	// auth 包的 ErrForbidden，不是 service 的——两层各有各的语义错误。
	if _, err := f.svc.ListUnbindsForAdmin(ctx, f.user, "", "", ""); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户列管理端应 ErrForbidden，实得 %v", err)
	}
	if _, err := f.svc.ApproveUnbind(ctx, f.user, 1, ""); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户批准应 ErrForbidden，实得 %v", err)
	}
	if _, err := f.svc.RejectUnbind(ctx, f.user, 1, ""); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户驳回应 ErrForbidden，实得 %v", err)
	}
}

// TestRejectUnbindByAdmin 驳回后地址还在。
func TestRejectUnbindByAdmin(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := unbindAdmin(ctx, t, f.shareFixture)
	addr := "alice@example.com"

	if _, err := f.svc.RequestMyUnbind(ctx, f.user, addr, ""); err != nil {
		t.Fatal(err)
	}
	list, _ := f.svc.ListUnbindsForAdmin(ctx, admin, store.UnbindPending, "", "")
	if _, err := f.svc.RejectUnbind(ctx, admin, list.Items[0].ID, "暂不解绑"); err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	if _, err := store.GetMailAddress(ctx, f.db.R(), addr); err != nil {
		t.Fatalf("驳回后地址应仍在: %v", err)
	}
}

// TestApproveUnbindTwice 同一张单不能批两次。
func TestApproveUnbindTwice(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := unbindAdmin(ctx, t, f.shareFixture)

	if _, err := f.svc.RequestMyUnbind(ctx, f.user, "alice@example.com", ""); err != nil {
		t.Fatal(err)
	}
	list, _ := f.svc.ListUnbindsForAdmin(ctx, admin, store.UnbindPending, "", "")
	if _, err := f.svc.ApproveUnbind(ctx, admin, list.Items[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ApproveUnbind(ctx, admin, list.Items[0].ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("二次批准应 ErrConflict，实得 %v", err)
	}
}

// TestCancelMyUnbindOnlyOwn 撤不了别人的单。
func TestCancelMyUnbindOnlyOwn(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	req, err := f.svc.RequestMyUnbind(ctx, f.user, "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.GetUserByID(ctx, f.db.R(), f.uid2)
	if err != nil {
		t.Fatalf("读取 bob 失败: %v", err)
	}
	other := auth.Principal{Actor: store.ActorUser, User: bob, Group: f.user.Group}
	if err := f.svc.CancelMyUnbind(ctx, other, req.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("撤销他人申请应 ErrConflict，实得 %v", err)
	}
}
