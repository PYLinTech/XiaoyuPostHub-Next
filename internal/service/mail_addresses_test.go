package service

import (
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

func TestCreateMyMailAddress(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	// user1 已持有 alice@example.com；默认配额 1，第二个地址应被拒。
	_, err := f.svc.CreateMyMailAddress(ctx, f.user, "second", "example.com")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超出地址配额应 ErrQuotaExceeded，得到 %v", err)
	}

	// 提到 2 后可建，大小写与首尾点号做规范化。
	if err := store.SetGroupQuota(ctx, f.db.W(), "normal", store.QuotaMailAddresses, 2); err != nil {
		t.Fatal(err)
	}
	addr, err := f.svc.CreateMyMailAddress(ctx, f.user, "Second.Name", "EXAMPLE.com")
	if err != nil {
		t.Fatalf("合法创建失败: %v", err)
	}
	if addr.Address != "second.name@example.com" || addr.Status != store.MailAddressActive {
		t.Fatalf("地址规范化错误: %+v", addr)
	}

	// 重名拒绝（配额放宽，确保撞的是唯一约束而不是配额墙）。
	if err := store.SetGroupQuota(ctx, f.db.W(), "normal", store.QuotaMailAddresses, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateMyMailAddress(ctx, f.user, "second.name", "example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("重名应 ErrConflict，得到 %v", err)
	}
}

func TestCreateMyMailAddressRejectsBadPrefixAndForeignDomain(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	if err := store.SetGroupQuota(ctx, f.db.W(), "normal", store.QuotaMailAddresses, 50); err != nil {
		t.Fatal(err)
	}
	// 只挡格式错误。前缀本身没有保留名单：域名由管理员按组分配，用户能填的
	// 空间已经很小，再叠一层黑名单只会让"为什么 admin 不行"变成一个需要
	// 向管理员单独解释的问题。
	for _, bad := range []string{"a/b", "-x", "xn--", "a..b", "", "有中文"} {
		if _, err := f.svc.CreateMyMailAddress(ctx, f.user, bad, "example.com"); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("非法前缀 %q 应 ErrBadRequest，得到 %v", bad, err)
		}
	}
	// 未分配给本组的域名拒绝，且与"域名根本不存在"走同一错误，
	// 不向普通用户暴露后台配置。
	for _, d := range []string{"other.example", "never-existed.example"} {
		if _, err := f.svc.CreateMyMailAddress(ctx, f.user, "ok1", d); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("未分配给本组的域名 %q 应 ErrBadRequest，得到 %v", d, err)
		}
	}
	// 分配给本组但暂停收件的绑定同样不可用。
	if err := store.BindGroupMailDomain(ctx, f.db.W(), "normal", "example.com", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateMyMailAddress(ctx, f.user, "ok1", "example.com"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("暂停收件的域名应拒绝建址，得到 %v", err)
	}
}

func TestCreateMyMailAddressDefaultZeroDenies(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	// 新建一个组配额无行、全局默认 0 的环境：staff 组（bob）默认配额无行，
	// 把全局默认调成 0 后 bob 不得新建。
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyMailAddressesDefault: "0",
	}, 0); err != nil {
		t.Fatal(err)
	}
	bob, err := store.GetUserByID(ctx, f.db.R(), f.uid2)
	if err != nil {
		t.Fatal(err)
	}
	p := adminPrincipal(ctx, t, f.shareFixture, bob)
	if _, err := f.svc.CreateMyMailAddress(ctx, p, "new1", "example.com"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("默认 0 时应 ErrForbidden，得到 %v", err)
	}
}

func TestListMyMailAddresses(t *testing.T) {
	f := newInboundFixture(t)
	addrs, err := f.svc.ListMyMailAddresses(context.Background(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0].Address != "alice@example.com" {
		t.Fatalf("地址列表错误: %+v", addrs)
	}
}

func TestListMyMailDomains(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	// ① 未绑域名：这不是错误，返回空列表让界面提示"去联系管理员"，
	//    而不是弹一个红色报错。管理员组没绑任何域名，用它来覆盖这条。
	//    （不能靠删 normal 组的 example.com 来构造：夹具给它建了地址，
	//    外键会拦住删除，而那正是产品要的约束。）
	admin := f.adminUser("no-domain-admin")
	got, err := f.svc.ListMyMailDomains(ctx, admin)
	if err != nil {
		t.Fatalf("未绑域名不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("未绑域名应返回空列表，实际 %+v", got)
	}

	// ② 已绑且开启收信：下拉里应当出现它。
	got, err = f.svc.ListMyMailDomains(ctx, f.user)
	if err != nil {
		t.Fatalf("读取域名失败: %v", err)
	}
	if len(got) != 1 || got[0].Domain != "example.com" {
		t.Fatalf("应返回 example.com，实际 %+v", got)
	}

	// ③ 关闭收件：必须立刻从列表消失。留着它等于让用户选中一个
	//    必然创建失败的值。
	if err := store.BindGroupMailDomain(ctx, f.db.W(), perm.GroupNormal, "example.com", false); err != nil {
		t.Fatal(err)
	}
	got, err = f.svc.ListMyMailDomains(ctx, f.user)
	if err != nil {
		t.Fatalf("关闭收件后读取失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("关闭收件后应返回空列表，实际 %+v", got)
	}
}

// 收件域名页依赖「每人可建地址上限」能按组调整，而这个键此前不在写入
// 白名单里——后端会读它，管理员却调不了。
func TestMailAddressesQuotaIsWritable(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	p := f.adminUser("quota-admin")
	if _, err := f.svc.AdminSaveGroup(ctx, p, SaveGroupRequest{
		Name:        perm.GroupNormal,
		DisplayName: "普通用户组",
		Permissions: int64(perm.Share),
		Quotas:      map[string]int64{store.QuotaMailAddresses: 5, store.QuotaMailStorageTotal: 1 << 30},
	}); err != nil {
		t.Fatalf("写入邮件配额应被接受: %v", err)
	}
	quotas, err := store.GroupQuotaMap(ctx, f.db.R(), "normal")
	if err != nil {
		t.Fatal(err)
	}
	if quotas[store.QuotaMailAddresses] != 5 {
		t.Fatalf("地址上限未落库: %+v", quotas)
	}
	if quotas[store.QuotaMailStorageTotal] != 1<<30 {
		t.Fatalf("邮件存储上限未落库: %+v", quotas)
	}
}
