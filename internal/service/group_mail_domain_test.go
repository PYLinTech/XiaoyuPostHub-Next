package service

import (
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 收件域名并入用户组之后的契约：它是组的一个列表项，与组行同事务写入。
//
// 组 ↔ 域名是多对多：一个组可托管多个域名，同一个域名也可同时托管给多个组；
// 每个「组 × 域名」绑定自带收件开关。夹具里 example.com 绑在 normal 组上，
// 挂着 alice/bob 两个地址——正好覆盖"本组有地址的域名不能移除"这条边界。

// ptr 让用例能区分"不管域名"与"解绑全部域名"这两种截然不同的请求。
func ptr[T any](v T) *T { return &v }

// bindFixture 建域名并绑给某组，夹具与用例共用同一套写法。
func bindFixture(t *testing.T, f *inboundFixture, groupName, domain string, enabled bool) {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureMailDomain(ctx, f.db.W(), domain); err != nil {
		t.Fatalf("建域名失败: %v", err)
	}
	if err := store.BindGroupMailDomain(ctx, f.db.W(), groupName, domain, enabled); err != nil {
		t.Fatalf("绑定域名失败: %v", err)
	}
}

// domainsOf 取某组当前的域名绑定列表。
func domainsOf(t *testing.T, f *inboundFixture, groupName string) []store.GroupMailDomain {
	t.Helper()
	got, err := store.ListGroupMailDomains(context.Background(), f.db.R(), groupName)
	if err != nil {
		t.Fatalf("读取组域名失败: %v", err)
	}
	return got
}

func TestAdminListGroupsCarriesMailDomains(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")
	// 给 normal 再加一个域名：列表必须把两个都带出来，而不是只留一个。
	bindFixture(t, f, perm.GroupNormal, "vip.example.com", true)

	groups, err := f.svc.AdminListGroups(ctx, admin)
	if err != nil {
		t.Fatalf("列出用户组失败: %v", err)
	}
	var normal GroupDetail
	for _, g := range groups {
		if g.Group.Name == perm.GroupNormal {
			normal = g
		}
	}
	if len(normal.MailDomains) != 2 {
		t.Fatalf("normal 应带出 2 个域名，得 %d: %+v", len(normal.MailDomains), normal.MailDomains)
	}
	// 列表按域名排序，example.com 在前。
	byDomain := map[string]GroupMailDomain{}
	for _, d := range normal.MailDomains {
		byDomain[d.Domain] = d
	}
	if !byDomain["example.com"].ReceiveEnabled {
		t.Fatal("夹具里的域名是开启收件的，列表里却报关闭")
	}
	if byDomain["vip.example.com"].Domain != "vip.example.com" {
		t.Fatalf("第二个域名缺失: %+v", normal.MailDomains)
	}
	// 地址数决定管理员能不能移除，必须跟着域名一起给出来。
	if byDomain["example.com"].AddressCount != 2 {
		t.Fatalf("example.com 应报 2 个地址，得 %d", byDomain["example.com"].AddressCount)
	}
	if byDomain["vip.example.com"].AddressCount != 0 {
		t.Fatalf("vip.example.com 应报 0 个地址，得 %d", byDomain["vip.example.com"].AddressCount)
	}

	// 没绑域名的组要给出空数组而不是 null：前端会直接对它取 .length。
	for _, g := range groups {
		if g.Group.Name != perm.GroupNormal && len(g.MailDomains) != 0 {
			t.Fatalf("组 %s 没有域名，mailDomains 应为空数组，得 %+v", g.Group.Name, g.MailDomains)
		}
	}

	if _, err := f.svc.AdminListGroups(ctx, f.user); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户应 403，得 %v", err)
	}
}

func TestAdminSaveGroupBindsMultipleMailDomains(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")

	// 新建组的同时绑两个域名：组行与绑定必须一起落库。
	want := []GroupMailDomainInput{
		{Domain: "vip.example.com", ReceiveEnabled: true},
		{Domain: "ops.example.com", ReceiveEnabled: true},
	}
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "vip", DisplayName: "会员组", ReceiveDomains: &want,
	}); err != nil {
		t.Fatalf("建组并绑域名失败: %v", err)
	}
	got := domainsOf(t, f, "vip")
	if len(got) != 2 {
		t.Fatalf("vip 应有两个域名，得 %+v", got)
	}
	for _, d := range got {
		if !d.ReceiveEnabled {
			t.Fatalf("新建的域名默认应开启收件: %+v", d)
		}
	}

	// 大小写与空白要归一：同一个域名写成两种样子会在库里变成两行。
	again := []GroupMailDomainInput{
		{Domain: "  VIP.Example.COM ", ReceiveEnabled: true},
		{Domain: "OPS.EXAMPLE.COM", ReceiveEnabled: true},
	}
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "vip", DisplayName: "会员组", ReceiveDomains: &again,
	}); err != nil {
		t.Fatalf("归一化保存失败: %v", err)
	}
	got = domainsOf(t, f, "vip")
	if len(got) != 2 || got[0].Domain != "ops.example.com" || got[1].Domain != "vip.example.com" {
		t.Fatalf("域名应被归一: %+v", got)
	}

	// 列表是集合语义：少写一项等于不再托管那一个。
	shrunk := []GroupMailDomainInput{{Domain: "vip.example.com", ReceiveEnabled: true}}
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "vip", DisplayName: "会员组", ReceiveDomains: &shrunk,
	}); err != nil {
		t.Fatalf("缩减域名列表失败: %v", err)
	}
	got = domainsOf(t, f, "vip")
	if len(got) != 1 || got[0].Domain != "vip.example.com" {
		t.Fatalf("应只剩一个域名: %+v", got)
	}
	// 没人再绑的域名连行一起消失，不留只存在于"域名存在"里的孤儿。
	if _, err := store.GetMailDomain(ctx, f.db.R(), "ops.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("无人绑定的域名应被清理，得 %v", err)
	}

	// 非法域名直接拒，不写库。
	for _, bad := range [][]GroupMailDomainInput{
		{{Domain: "不是域名"}},
		{{Domain: "ok.example.com"}, {Domain: "  "}},
		{{Domain: "dup.example.com"}, {Domain: "DUP.example.com"}},
	} {
		if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
			Name: "vip", DisplayName: "会员组", ReceiveDomains: &bad,
		}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("非法域名列表 %v 应 ErrBadRequest，得 %v", bad, err)
		}
	}

	// 普通用户无权。
	if _, err := f.svc.AdminSaveGroup(ctx, f.user, SaveGroupRequest{
		Name: "x", DisplayName: "x",
		ReceiveDomains: ptr([]GroupMailDomainInput{{Domain: "x.example.com", ReceiveEnabled: true}}),
	}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户应 403，得 %v", err)
	}
}

// TestAdminSaveGroupAllowsDomainSharedWithOtherGroup 是本次改造的核心用例：
// 同一个域名必须能被多个组同时托管，且两边的收件开关互不影响。
//
// 这正是旧模型报「域名已绑定到用户组 normal」的地方——那个约束是反的。
func TestAdminSaveGroupAllowsDomainSharedWithOtherGroup(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")

	// normal 已经托管着 example.com，新组直接用同一个域名——不再冲突。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "vip", DisplayName: "会员组",
		ReceiveDomains: ptr([]GroupMailDomainInput{{Domain: "example.com", ReceiveEnabled: true}, {Domain: "vip.example.com", ReceiveEnabled: true}}),
	}); err != nil {
		t.Fatalf("共用域名应被允许，得 %v", err)
	}
	got := domainsOf(t, f, "vip")
	if len(got) != 2 {
		t.Fatalf("vip 应有两个域名，得 %+v", got)
	}

	// 暂停 vip 组在共享域名上的收件，不能动到 normal 组的那条绑定。
	bindFixture(t, f, "vip", "example.com", false)
	normalBinding, err := store.GetGroupMailDomain(ctx, f.db.R(), perm.GroupNormal, "example.com")
	if err != nil {
		t.Fatalf("读取 normal 绑定失败: %v", err)
	}
	if !normalBinding.ReceiveEnabled {
		t.Fatal("vip 组暂停收件不应改掉 normal 组共用的那条绑定")
	}
	vipBinding, _ := store.GetGroupMailDomain(ctx, f.db.R(), "vip", "example.com")
	if vipBinding.ReceiveEnabled {
		t.Fatal("vip 组自己的绑定应当是暂停的")
	}

	// 移除阻断的口径是「本组用户的地址」：normal 在 example.com 上挂着两个
	// 地址，但那是 normal 用户的数据。vip 组在同一个域名上一个地址都没有，
	// 因此它必须能自行移除——否则共用一个域名会让谁都走不掉。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "vip", DisplayName: "会员组", ReceiveDomains: ptr([]GroupMailDomainInput{{Domain: "vip.example.com", ReceiveEnabled: true}}),
	}); err != nil {
		t.Fatalf("vip 移除共享域名失败（normal 的地址不该构成阻断）: %v", err)
	}
	if _, err := store.GetGroupMailDomain(ctx, f.db.R(), perm.GroupNormal, "example.com"); err != nil {
		t.Fatalf("vip 移除不应影响 normal: %v", err)
	}
	if n, _ := store.CountMailAddressesAll(ctx, f.db.R(), "example.com"); n != 2 {
		t.Fatalf("normal 的地址应完好，实得 %d", n)
	}

	// 反过来：normal 自己有地址，移除要被挡住并说清原因。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: perm.GroupNormal, DisplayName: "普通用户", ReceiveDomains: &[]GroupMailDomainInput{},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("移除含本组地址的域名应 ErrConflict，得 %v", err)
	}
}

// TestAdminSaveGroupAppliesReceiveEnabledPerBinding 覆盖收件开关的归属：
// 每条绑定按这次提交的值写入，同一个域名在不同组上互不影响。
//
// 开关必须能跟着表单一路走到库里——管理端在域名那一行上勾的就是意图，
// 后端若"保留原值"就会让管理员明明改了却看不到任何变化。
func TestAdminSaveGroupAppliesReceiveEnabledPerBinding(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")

	// 先建组再绑域名：绑定的外键指向 user_groups(name)。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "ops", DisplayName: "运维组",
	}); err != nil {
		t.Fatal(err)
	}

	// 一次提交里两条绑定，开关一开一关。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "ops", DisplayName: "运维组",
		ReceiveDomains: ptr([]GroupMailDomainInput{
			{Domain: "ops.example.com", ReceiveEnabled: false},
			{Domain: "new.example.com", ReceiveEnabled: true},
		}),
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got := domainsOf(t, f, "ops")
	if len(got) != 2 {
		t.Fatalf("ops 应有两个域名，得 %+v", got)
	}
	// 按域名取值，不按下标：列表是按域名排序的，位置会随域名改名而变。
	enabled := map[string]bool{}
	for _, d := range got {
		enabled[d.Domain] = d.ReceiveEnabled
	}
	if enabled["ops.example.com"] {
		t.Fatalf("ops.example.com 应按提交值关闭，得 %+v", got)
	}
	if !enabled["new.example.com"] {
		t.Fatalf("new.example.com 应按提交值开启，得 %+v", got)
	}

	// 再存一次把 ops.example.com 打开：开关必须是这次提交说了算。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "ops", DisplayName: "运维组",
		ReceiveDomains: ptr([]GroupMailDomainInput{
			{Domain: "ops.example.com", ReceiveEnabled: true},
			{Domain: "new.example.com", ReceiveEnabled: true},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := store.GetGroupMailDomain(ctx, f.db.R(), "ops", "ops.example.com"); !b.ReceiveEnabled {
		t.Fatal("再次提交开启后应收件打开")
	}

	// 同一域名在另一个组上仍是独立的一条，互不牵动。
	bindFixture(t, f, perm.GroupNormal, "ops.example.com", false)
	if b, _ := store.GetGroupMailDomain(ctx, f.db.R(), "ops", "ops.example.com"); !b.ReceiveEnabled {
		t.Fatal("normal 组自己的开关不应改到 ops 组")
	}
}

// TestAdminSaveGroupRefusesRemovingDomainThatHasOwnAddresses 覆盖移除阻断：
// 只有**本组用户**的地址会构成阻断，别组的地址不该把本组钉住。
func TestAdminSaveGroupRefusesRemovingDomainThatHasOwnAddresses(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")

	// example.com 挂着 normal 组两个用户（alice/bob）的地址：移除要挡住并说清原因。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: perm.GroupNormal, DisplayName: "普通用户", ReceiveDomains: &[]GroupMailDomainInput{},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("有本组地址的域名移除应 ErrConflict，得 %v", err)
	}
	if _, err := store.GetGroupMailDomain(ctx, f.db.R(), perm.GroupNormal, "example.com"); err != nil {
		t.Fatalf("被拒后绑定不应消失: %v", err)
	}

	// 只切开关是允许的：关闭收件不丢地址，重新打开即刻恢复。
	if err := store.BindGroupMailDomain(ctx, f.db.W(), perm.GroupNormal, "example.com", false); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.AdminListGroups(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := store.GetGroupMailDomain(ctx, f.db.R(), perm.GroupNormal, "example.com")
	if b.ReceiveEnabled {
		t.Fatal("暂停收件未生效")
	}
	if err := store.BindGroupMailDomain(ctx, f.db.W(), perm.GroupNormal, "example.com", true); err != nil {
		t.Fatal(err)
	}
}

// TestAdminSaveGroupWithoutDomainFieldKeepsBindings 请求里没有 receiveDomains 字段时，
// 域名必须原样不动。只改权限或配额的调用很常见，让它顺手解绑域名既意外又
// 会连带把整次保存挡下来（域名下有地址时移除会被拒绝）。
func TestAdminSaveGroupWithoutDomainFieldKeepsBindings(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")
	bindFixture(t, f, perm.GroupNormal, "vip.example.com", true)

	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: perm.GroupNormal, DisplayName: "普通用户",
		Quotas: map[string]int64{store.QuotaMailAddresses: 3},
	}); err != nil {
		t.Fatalf("只调配额不应被域名挡下: %v", err)
	}
	got := domainsOf(t, f, perm.GroupNormal)
	if len(got) != 2 {
		t.Fatalf("域名不该被动到，得 %+v", got)
	}
	for _, d := range got {
		if !d.ReceiveEnabled {
			t.Fatalf("域名 %s 的开关应保持原样", d.Domain)
		}
	}
}

func TestAdminDeleteGroupBlockedByMailDomains(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")

	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "ops", DisplayName: "运维组",
		ReceiveDomains: ptr([]GroupMailDomainInput{{Domain: "ops.example.com", ReceiveEnabled: true}, {Domain: "sre.example.com", ReceiveEnabled: true}}),
	}); err != nil {
		t.Fatal(err)
	}
	// 组和绑定在库里分属两行，绑定会随组级联消失——但错误里必须点名是哪些域名，
	// 否则管理员只看到一个没有信息量的 409。
	err := f.svc.AdminDeleteGroup(ctx, admin, "ops")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("托管着域名的组应 ErrConflict，得 %v", err)
	}
	if _, gerr := store.GetGroup(ctx, f.db.R(), "ops"); gerr != nil {
		t.Fatalf("删除被拒后组不应消失: %v", gerr)
	}

	// 先移除域名再删就通了。
	if _, err := f.svc.AdminSaveGroup(ctx, admin, SaveGroupRequest{
		Name: "ops", DisplayName: "运维组", ReceiveDomains: &[]GroupMailDomainInput{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AdminDeleteGroup(ctx, admin, "ops"); err != nil {
		t.Fatalf("移除域名后应可删除: %v", err)
	}
}
