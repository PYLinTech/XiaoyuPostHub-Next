package store

import (
	"context"
	"errors"
	"testing"
)

// seedMailFixture 准备：两个用户组、两个用户、一个内容池对象。
func seedMailFixture(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('staff', '员工组', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
	                 VALUES (1, 'alice', 'x', 'normal', 0, 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
	                 VALUES (2, 'bob', 'x', 'staff', 0, 0)`)
	mustExec(t, db, `INSERT INTO files (checksum, size_plain, pan_object_name, pan_size_wire,
	                 enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope,
	                 kek_key_id, status, created_at, updated_at)
	                 VALUES ('bodycs', 100, '2026/09/26/b.xph', 120,
	                 'AES-256-GCM', 20, 1, x'00', x'00', 'k1', 1, 0, 0)`)
	mustExec(t, db, `INSERT INTO files (checksum, size_plain, pan_object_name, pan_size_wire,
	                 enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope,
	                 kek_key_id, status, created_at, updated_at)
	                 VALUES ('attcs', 200, '2026/09/26/a.xph', 240,
	                 'AES-256-GCM', 20, 1, x'00', x'00', 'k1', 1, 0, 0)`)
}

// bindDomain 建域名并绑给某组。收件开关按绑定各存各的。
func bindDomain(t *testing.T, db *DB, groupName, domain string, receiveEnabled bool) {
	t.Helper()
	ctx := context.Background()
	if err := EnsureMailDomain(ctx, db.W(), domain); err != nil {
		t.Fatalf("建域名失败: %v", err)
	}
	if err := BindGroupMailDomain(ctx, db.W(), groupName, domain, receiveEnabled); err != nil {
		t.Fatalf("绑定域名失败: %v", err)
	}
}

// TestGroupMailDomainBindings 覆盖组 ↔ 域名的多对多：
// 一个组可绑多个域名，同一个域名可绑多个组，且收件开关互不影响。
func TestGroupMailDomainBindings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)

	// 一个组两个域名。
	bindDomain(t, db, "normal", "example.com", true)
	bindDomain(t, db, "normal", "vip.example.com", true)
	got, err := ListGroupMailDomains(ctx, db.R(), "normal")
	if err != nil || len(got) != 2 {
		t.Fatalf("normal 应有两个域名绑定: %v %+v", err, got)
	}

	// 同一个域名再绑给另一个组——这正是多对多的另一半。
	bindDomain(t, db, "staff", "example.com", true)
	if all, err := ListAllGroupMailDomains(ctx, db.R()); err != nil || len(all) != 3 {
		t.Fatalf("全量绑定应为 3 条: %v %+v", err, all)
	}

	// 开关是绑定的属性：暂停 normal 不影响同样用 example.com 的 staff。
	if err := BindGroupMailDomain(ctx, db.W(), "normal", "example.com", false); err != nil {
		t.Fatal(err)
	}
	normal, _ := GetGroupMailDomain(ctx, db.R(), "normal", "example.com")
	staff, _ := GetGroupMailDomain(ctx, db.R(), "staff", "example.com")
	if normal.ReceiveEnabled || !staff.ReceiveEnabled {
		t.Fatalf("开关应互不影响: normal=%v staff=%v", normal.ReceiveEnabled, staff.ReceiveEnabled)
	}

	// 同一组重复绑同一域名走 upsert，不报错也不产生第二行。
	if err := BindGroupMailDomain(ctx, db.W(), "normal", "example.com", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := ListGroupMailDomains(ctx, db.R(), "normal"); len(got) != 2 {
		t.Fatalf("重复绑定不应新增行，实际 %d 条", len(got))
	}

	if _, err := GetMailDomain(ctx, db.R(), "missing.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在域名应返 ErrNotFound，实际: %v", err)
	}
	if _, err := GetGroupMailDomain(ctx, db.R(), "normal", "missing.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的绑定应返 ErrNotFound，实际: %v", err)
	}

	// 解绑只影响该组，域名本身与其他组不受影响。
	if err := UnbindGroupMailDomain(ctx, db.W(), "normal", "example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetGroupMailDomain(ctx, db.R(), "staff", "example.com"); err != nil {
		t.Fatalf("解绑 A 组不应影响 B 组: %v", err)
	}
	if _, err := GetGroupMailDomain(ctx, db.R(), "normal", "example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("解绑后该绑定应不存在: %v", err)
	}
	if err := UnbindGroupMailDomain(ctx, db.W(), "normal", "example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复解绑应返 ErrNotFound: %v", err)
	}
}

func TestMailAddressLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)
	bindDomain(t, db, "normal", "example.com", true)

	a := MailAddress{Address: "alice@example.com", LocalPart: "alice", Domain: "example.com", UserID: 1}
	if err := CreateMailAddress(ctx, db.W(), a); err != nil {
		t.Fatalf("创建地址失败: %v", err)
	}
	// 全址重复。
	if err := CreateMailAddress(ctx, db.W(), a); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复地址应 ErrConflict: %v", err)
	}
	// 同 local+domain 不同全址写法（大小写归一由 service 层负责，库里仍是唯一约束）。
	// 同域同 local 再插一次直接撞 UNIQUE(local_part, domain)。
	if err := CreateMailAddress(ctx, db.W(), MailAddress{
		Address: "alice2@example.com", LocalPart: "alice", Domain: "example.com", UserID: 2,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("同域同 local 应冲突: %v", err)
	}
	// 外键：不存在的域名。
	if err := CreateMailAddress(ctx, db.W(), MailAddress{
		Address: "x@nope.com", LocalPart: "x", Domain: "nope.com", UserID: 1,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("未知域名应映射 ErrConflict: %v", err)
	}

	n, err := CountMailAddresses(ctx, db.R(), 1, "example.com")
	if err != nil || n != 1 {
		t.Fatalf("活跃地址数 = %d, %v", n, err)
	}
	if err := SetMailAddressStatus(ctx, db.W(), "alice@example.com", MailAddressFrozen); err != nil {
		t.Fatal(err)
	}
	// 冻结只应摘掉「可用地址数」，不能影响管理端的域名占用统计。
	n, _ = CountMailAddresses(ctx, db.R(), 1, "example.com")
	if n != 0 {
		t.Fatalf("冻结后活跃地址应为 0，实际 %d", n)
	}
	if n, _ = CountMailAddressesAll(ctx, db.R(), "example.com"); n != 1 {
		t.Fatalf("冻结地址仍应计入域名占用，实际 %d", n)
	}
	if err := SetMailAddressStatus(ctx, db.W(), "alice@example.com", MailAddressActive); err != nil {
		t.Fatal(err)
	}
	if err := SetMailAddressStatus(ctx, db.W(), "alice@example.com", "weird"); err == nil {
		t.Fatal("非法状态应被拒绝")
	}
}

// TestSyncMailAddressStatusFollowsGroup 覆盖「移组冻结、回组自动恢复」。
//
// 判定依据是「用户当前组是否绑定了该地址所在的域名」（group_mail_domains），
// 不是地址自身被谁手工改过。
func TestSyncMailAddressStatusFollowsGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)
	bindDomain(t, db, "normal", "example.com", true)
	bindDomain(t, db, "staff", "staff.com", true)
	// alice 在 normal 组，normal 绑 example.com；bob 在 staff 组，staff 绑 staff.com。
	if err := CreateMailAddress(ctx, db.W(), MailAddress{
		Address: "alice@example.com", LocalPart: "alice", Domain: "example.com", UserID: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := CreateMailAddress(ctx, db.W(), MailAddress{
		Address: "bob@staff.com", LocalPart: "bob", Domain: "staff.com", UserID: 2,
	}); err != nil {
		t.Fatal(err)
	}

	statusOf := func(addr string) string {
		t.Helper()
		a, err := GetMailAddress(ctx, db.R(), addr)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", addr, err)
		}
		return a.Status
	}

	// alice 移到 staff 组：example.com 不再是她的组 → 冻结。
	// bob 不受影响，仍 active。
	if err := SyncMailAddressStatus(ctx, db.W(), 1, "staff"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf("alice@example.com"); got != MailAddressFrozen {
		t.Fatalf("移组后 alice 的地址应冻结，实际 %q", got)
	}
	if got := statusOf("bob@staff.com"); got != MailAddressActive {
		t.Fatalf("他人地址不应被波及，实际 %q", got)
	}

	// 移回 normal 组 → 自动恢复，无需逐个手工解除。
	if err := SyncMailAddressStatus(ctx, db.W(), 1, "normal"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf("alice@example.com"); got != MailAddressActive {
		t.Fatalf("回组后地址应自动恢复，实际 %q", got)
	}

	// 移到一个没有绑定任何域名的组：地址全部冻结（无授权）。
	if err := SyncMailAddressStatus(ctx, db.W(), 1, "no-mail-group"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf("alice@example.com"); got != MailAddressFrozen {
		t.Fatalf("无绑定域的组应冻结地址，实际 %q", got)
	}

	// 幂等：再跑一次结果不变。
	if err := SyncMailAddressStatus(ctx, db.W(), 1, "no-mail-group"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf("alice@example.com"); got != MailAddressFrozen {
		t.Fatalf("重复同步应幂等，实际 %q", got)
	}
}

func TestCreateReceivedMailTxCascadeAndRefs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)
	bindDomain(t, db, "normal", "example.com", true)

	msg := MailMessage{
		ID: "mm_1", MessageID: "<one@external.example>",
		FromAddress: "outsider@external.example", Subject: "你好",
		SentAt: 100, SizePlain: 300, AttachmentCount: 1, SPFResult: "pass",
	}
	recipients := []MailRecipient{
		{Kind: MailRecipientTo, Address: "alice@example.com", Seq: 0},
		{Kind: MailRecipientCc, Address: "bob@example.com", Seq: 0},
	}
	parts := []MailPart{
		{Seq: 0, Kind: MailPartBody, ContentType: "application/json", SizePlain: 100, FileChecksum: "bodycs"},
		{Seq: 1, Kind: MailPartAttachment, FileName: "a.bin", ContentType: "application/octet-stream", SizePlain: 200, FileChecksum: "attcs"},
	}
	boxes := []Mailbox{
		{UserID: 1, Role: MailboxRoleInbox, ChargedBytes: 300},
		{UserID: 2, Role: MailboxRoleInbox, ChargedBytes: 300},
	}
	if err := CreateReceivedMailTx(ctx, db, msg, recipients, parts, boxes); err != nil {
		t.Fatalf("落信事务失败: %v", err)
	}

	// 部件引用计数 +1（群发物理一份）。
	if rc := fileRefCount(t, db, "bodycs"); rc != 1 {
		t.Fatalf("正文引用计数 = %d，应为 1", rc)
	}
	if rc := fileRefCount(t, db, "attcs"); rc != 1 {
		t.Fatalf("附件引用计数 = %d，应为 1", rc)
	}

	// 收件人与部件读回。
	rs, err := ListMailRecipients(ctx, db.R(), "mm_1")
	if err != nil || len(rs) != 2 || rs[0].Kind != "to" {
		t.Fatalf("收件人读回异常: %v %+v", err, rs)
	}
	ps, err := ListMailParts(ctx, db.R(), "mm_1")
	if err != nil || len(ps) != 2 || ps[0].Kind != MailPartBody {
		t.Fatalf("部件读回异常: %v %+v", err, ps)
	}

	// RFC Message-ID 幂等：重复投递应冲突。
	err = CreateReceivedMailTx(ctx, db, msg, recipients, nil, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重复 Message-ID 应 ErrConflict，实际: %v", err)
	}

	// 列表：alice 能看到，按主题搜索能命中，按无关发件人搜不到。
	items, total, err := ListMailboxes(ctx, db.R(), 1, MailboxListFilter{Role: MailboxRoleInbox}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].Subject != "你好" || items[0].IsRead {
		t.Fatalf("收件箱列表异常: %v total=%d items=%+v", err, total, items)
	}
	if _, total, err := ListMailboxes(ctx, db.R(), 1, MailboxListFilter{
		Role: MailboxRoleInbox, Query: "你好",
	}, 10, 0); err != nil || total != 1 {
		t.Fatalf("主题搜索异常: %v total=%d", err, total)
	}
	if _, total, _ := ListMailboxes(ctx, db.R(), 1, MailboxListFilter{
		Role: MailboxRoleInbox, Query: "nobody",
	}, 10, 0); total != 0 {
		t.Fatalf("无关查询不应命中，total=%d", total)
	}

	// 已读 / 星标 / 归档流转。
	if err := SetMailboxRead(ctx, db.W(), 1, "mm_1", MailboxRoleInbox, true); err != nil {
		t.Fatal(err)
	}
	if err := SetMailboxStarred(ctx, db.W(), 1, "mm_1", MailboxRoleInbox, true); err != nil {
		t.Fatal(err)
	}
	items, _, _ = ListMailboxes(ctx, db.R(), 1, MailboxListFilter{Role: MailboxRoleInbox, OnlyStarred: true}, 10, 0)
	if len(items) != 1 || !items[0].IsRead || !items[0].IsStarred {
		t.Fatalf("已读星标状态异常: %+v", items)
	}
	if err := SetMailboxStatus(ctx, db.W(), 1, "mm_1", MailboxRoleInbox, MailboxStatusArchived, 999); err != nil {
		t.Fatal(err)
	}
	if _, total, _ = ListMailboxes(ctx, db.R(), 1, MailboxListFilter{Role: MailboxRoleInbox}, 10, 0); total != 0 {
		t.Fatalf("归档中的邮件不应出现在正常列表，total=%d", total)
	}
	items, total, _ = ListMailboxes(ctx, db.R(), 1, MailboxListFilter{
		Role: MailboxRoleInbox, Status: MailboxStatusArchived,
	}, 10, 0)
	if total != 1 || items[0].PurgeAt != 999 {
		t.Fatalf("归档视图异常: total=%d %+v", total, items)
	}

	// 级联删除：删掉 message，收件人/部件/归属行全部消失；引用计数由调用方
	// 通过 ReleaseFileRef 管理（这里模拟 service 的释放动作，验证 FK 不挡删除）。
	if err := db.InTx(ctx, func(tx Querier) error {
		if _, err := AddFileRef(ctx, tx, "bodycs"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DELETE FROM mail_messages WHERE id = 'mm_1'`)
	rs, _ = ListMailRecipients(ctx, db.R(), "mm_1")
	ps, _ = ListMailParts(ctx, db.R(), "mm_1")
	if len(rs) != 0 || len(ps) != 0 {
		t.Fatalf("级联删除失败: recipients=%d parts=%d", len(rs), len(ps))
	}
	if _, err := GetMailbox(ctx, db.R(), 1, "mm_1", MailboxRoleInbox); !errors.Is(err, ErrNotFound) {
		t.Fatalf("归属行应级联删除: %v", err)
	}
}

func TestMailStorageCounterIsIndependent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := ReserveCounter(ctx, db.W(), ScopeMailStorage, UserCounterKey(1, ""), 300, 1000); err != nil {
		t.Fatalf("邮件配额预扣失败: %v", err)
	}
	mailUsed, err := GetCounter(ctx, db.R(), ScopeMailStorage, UserCounterKey(1, ""))
	if err != nil || mailUsed != 300 {
		t.Fatalf("邮件计数 = %d, %v", mailUsed, err)
	}
	fileUsed, err := GetCounter(ctx, db.R(), ScopeStorage, UserCounterKey(1, ""))
	if err != nil || fileUsed != 0 {
		t.Fatalf("文件计数不应受邮件影响 = %d, %v", fileUsed, err)
	}
	// 邮件上限独立生效（已用 300，再预扣 700 恰好到顶）。
	if _, err := ReserveCounter(ctx, db.W(), ScopeMailStorage, UserCounterKey(1, ""), 700, 1000); err != nil {
		t.Fatalf("预扣至上限应成功: %v", err)
	}
	if _, err := ReserveCounter(ctx, db.W(), ScopeMailStorage, UserCounterKey(1, ""), 1, 1000); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超出邮件配额应 ErrQuotaExceeded: %v", err)
	}
}

func fileRefCount(t *testing.T, db *DB, checksum string) int64 {
	t.Helper()
	var n int64
	if err := db.R().QueryRowContext(context.Background(),
		`SELECT ref_count FROM files WHERE checksum = ?`, checksum).Scan(&n); err != nil {
		t.Fatalf("读取引用计数失败: %v", err)
	}
	return n
}
