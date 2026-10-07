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
