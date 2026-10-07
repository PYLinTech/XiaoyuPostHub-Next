package service

import (
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// adminPrincipal 以指定用户构造带其所属组身份的 Principal。
func adminPrincipal(ctx context.Context, t *testing.T, f *shareFixture, user store.User) auth.Principal {
	t.Helper()
	group, err := store.GetGroup(ctx, f.db.R(), user.GroupName)
	if err != nil {
		t.Fatalf("读取用户组失败: %v", err)
	}
	return auth.Principal{Actor: store.ActorUser, User: user, Group: group}
}

// TestAdminGroupAlwaysKeepsOneEnabledAdmin 管理员组必须永远留有至少一个
// 启用的账号：把最后一个管理员移出组或禁用掉，站点就失去了唯一能管理
// 自身的身份，只能靠手工改库救回。移出组与禁用是同一条不变量的两个入口，
// 禁用的成员不算数。
func TestAdminGroupAlwaysKeepsOneEnabledAdmin(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()

	createUser := func(account, group string) store.User {
		t.Helper()
		id, err := store.CreateUser(ctx, f.db.W(), store.User{
			Account:      account,
			DisplayName:  account,
			PasswordHash: "not-a-real-hash",
			GroupName:    group,
			Status:       store.UserEnabled,
		})
		if err != nil {
			t.Fatalf("创建账号 %s 失败: %v", account, err)
		}
		user, err := store.GetUserByID(ctx, f.db.R(), id)
		if err != nil {
			t.Fatalf("读取账号失败: %v", err)
		}
		return user
	}

	adminA := createUser("admin-a", perm.GroupAdmin)
	adminB := createUser("admin-b", perm.GroupAdmin)
	pA := adminPrincipal(ctx, t, f, adminA)

	// 组里还有 A 时，B 移出管理员组应当成功；随后拉回来进入管理员组永远允许。
	if err := f.svc.AdminSetUserGroup(ctx, adminPrincipal(ctx, t, f, adminB), adminB.ID, perm.GroupNormal); err != nil {
		t.Fatalf("组内还有其他管理员时移出应成功: %v", err)
	}
	if err := f.svc.AdminSetUserGroup(ctx, pA, adminB.ID, perm.GroupAdmin); err != nil {
		t.Fatalf("移入管理员组应成功: %v", err)
	}

	// 禁用 B 之后 B 不再算"可用的管理员"：此时 A 不能把自己移出管理员组。
	if err := f.svc.AdminSetUserStatus(ctx, pA, adminB.ID, false); err != nil {
		t.Fatalf("禁用另一名管理员应成功: %v", err)
	}
	err := f.svc.AdminSetUserGroup(ctx, pA, adminA.ID, perm.GroupNormal)
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("只剩禁用成员时移出唯一启用管理员应被拒绝，实得: %v", err)
	}

	// 重新启用 B 后，A 就可以移出管理员组了。
	if err := f.svc.AdminSetUserStatus(ctx, pA, adminB.ID, true); err != nil {
		t.Fatalf("重新启用应成功: %v", err)
	}
	if err := f.svc.AdminSetUserGroup(ctx, pA, adminA.ID, perm.GroupNormal); err != nil {
		t.Fatalf("组内还有其他启用管理员时移出应成功: %v", err)
	}

	// 现在组里只剩 B：B 不能把自己移出管理员组。
	users, err := store.ListUsers(ctx, f.db.R(), 100, 0)
	if err != nil {
		t.Fatalf("列出账号失败: %v", err)
	}
	var adminBNow store.User
	for _, u := range users {
		if u.Account == "admin-b" {
			adminBNow = u
			break
		}
	}
	if adminBNow.ID == 0 {
		t.Fatal("找不到留在管理员组的 admin-b")
	}
	pB := adminPrincipal(ctx, t, f, adminBNow)
	if err := f.svc.AdminSetUserGroup(ctx, pB, adminBNow.ID, perm.GroupNormal); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("最后一个管理员不能移出管理员组，实得: %v", err)
	}
}
