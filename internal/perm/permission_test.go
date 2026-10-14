package perm

import "testing"

func TestMailPermissionsAreAppendedBits(t *testing.T) {
	// 新增权限只能追加：位值必须接续 AdminAudit，改动旧位会让存量组语义漂移。
	if MailAccess != AdminAudit<<1 {
		t.Fatalf("MailAccess = %d, 应为 AdminAudit<<1 = %d", MailAccess, AdminAudit<<1)
	}
	if AdminMail != MailAccess<<1 {
		t.Fatalf("AdminMail = %d, 应为 MailAccess<<1", AdminMail)
	}
	for _, b := range []Bit{MailAccess, AdminMail} {
		if !Has(int64(All), b) {
			t.Fatalf("All 未包含权限位 %d", b)
		}
	}
}

func TestBuiltinGroupsMailDefaults(t *testing.T) {
	groups := map[string]BuiltinGroup{}
	for _, g := range BuiltinGroups() {
		groups[g.Name] = g
	}
	normal := groups[GroupNormal]
	if !Has(normal.Permissions, MailAccess) {
		t.Fatal("普通用户默认应具备 MailAccess")
	}
	if Has(normal.Permissions, AdminMail) {
		t.Fatal("普通用户默认不应具备 AdminMail")
	}
	guest := groups[GroupGuest]
	if Has(guest.Permissions, MailAccess) {
		t.Fatal("访客不应具备 MailAccess")
	}
	admin := groups[GroupAdmin]
	if !HasAll(admin.Permissions, MailAccess, AdminMail) {
		t.Fatal("管理员应通过 All 具备全部邮件权限")
	}
}

func TestDirectLinkIsAppendedBit(t *testing.T) {
	// 新增权限只能追加：位值必须接续 AdminUnbind。改动旧位会让存量组里
	// 已经赋过的每一位都平移，语义全体漂移且不会立刻暴露。
	if DirectLink != AdminUnbind<<1 {
		t.Fatalf("DirectLink = %d, 应为 AdminUnbind<<1 = %d", DirectLink, AdminUnbind<<1)
	}
	// All 必须同步并入：service 层保存用户组时会用 `req &^ perm.All` 裁剪，
	// 漏加这一位会让管理员在表单里怎么勾都存不进去。
	if !Has(int64(All), DirectLink) {
		t.Fatal("All 未包含 DirectLink")
	}
}

func TestBuiltinGroupDirectLinkDefaults(t *testing.T) {
	groups := map[string]BuiltinGroup{}
	for _, g := range BuiltinGroups() {
		groups[g.Name] = g
	}
	// 与 Share 同一类用户能力，默认给普通用户。
	if !Has(groups[GroupNormal].Permissions, DirectLink) {
		t.Fatal("普通用户默认应具备 DirectLink")
	}
	// 访客不持有创建权限：直链只能由登录用户签发。
	if Has(groups[GroupGuest].Permissions, DirectLink) {
		t.Fatal("访客不应具备 DirectLink")
	}
	if !Has(groups[GroupAdmin].Permissions, DirectLink) {
		t.Fatal("管理员应通过 All 具备 DirectLink")
	}
}
