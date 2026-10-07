package store

import (
	"context"
	"errors"
	"testing"
)

// seedReadMail 直接插一封最小邮件 + 一个归属行，返回邮件 ID。
func seedReadMail(t *testing.T, db *DB, msgID string, uid int64, role, status string) {
	t.Helper()
	ctx := context.Background()
	m := MailMessage{
		ID: msgID, MessageID: msgID + "@rfc",
		FromAddress: "sender@ext.example", Subject: "关于 " + msgID,
		SentAt: 1000, CreatedAt: 900, SizePlain: 12,
	}
	if err := InsertMailMessage(ctx, db.W(), m); err != nil {
		t.Fatal(err)
	}
	if err := InsertMailbox(ctx, db.W(), Mailbox{
		MessageID: msgID, UserID: uid, Role: role, Status: status, ChargedBytes: 12,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGetMailboxForUserAcrossRoles(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)
	seedReadMail(t, db, "mm-cross", 1, MailboxRoleInbox, MailboxStatusNormal)

	b, err := GetMailboxForUser(ctx, db.R(), 1, "mm-cross")
	if err != nil || b.Role != MailboxRoleInbox {
		t.Fatalf("跨角色取归属失败: %+v %v", b, err)
	}
	if _, err := GetMailboxForUser(ctx, db.R(), 2, "mm-cross"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("非属主应 ErrNotFound，得到 %v", err)
	}
}

func TestMailboxCounters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)
	seedReadMail(t, db, "mm-a", 1, MailboxRoleInbox, MailboxStatusNormal) // 未读
	seedReadMail(t, db, "mm-b", 1, MailboxRoleInbox, MailboxStatusNormal)
	if err := SetMailboxRead(ctx, db.W(), 1, "mm-b", MailboxRoleInbox, true); err != nil {
		t.Fatal(err)
	}
	seedReadMail(t, db, "mm-d", 1, MailboxRoleInbox, MailboxStatusArchived) // 归档不计

	c, err := GetMailboxCounters(ctx, db.R(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.UnreadInbox != 1 {
		t.Fatalf("邮件计数错误: %+v", c)
	}
}

func TestOnlyUnreadFilter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)
	seedReadMail(t, db, "mm-u1", 1, MailboxRoleInbox, MailboxStatusNormal)
	seedReadMail(t, db, "mm-u2", 1, MailboxRoleInbox, MailboxStatusNormal)
	if err := SetMailboxRead(ctx, db.W(), 1, "mm-u2", MailboxRoleInbox, true); err != nil {
		t.Fatal(err)
	}
	items, total, err := ListMailboxes(ctx, db.R(), 1,
		MailboxListFilter{Role: MailboxRoleInbox, OnlyUnread: true}, 50, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].MessageID != "mm-u1" {
		t.Fatalf("只看未读失败: items=%d total=%d err=%v", len(items), total, err)
	}

	// 搜索词里的 LIKE 通配符按字面量匹配（mm-u_ 不应命中 mm-u1/mm-u2）。
	if _, total, err := ListMailboxes(ctx, db.R(), 1,
		MailboxListFilter{Role: MailboxRoleInbox, Query: "mm-u_"}, 50, 0); err != nil || total != 0 {
		t.Fatalf("字面量 mm-u_ 应 0 命中，得 total=%d err=%v", total, err)
	}
}

func TestPurgeMailboxTx(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db)

	// 群发邮件 mm-p：两个属主，共享一个部件（引用夹具里的 bodycs 文件）。
	if err := InsertMailMessage(ctx, db.W(), MailMessage{
		ID: "mm-p", MessageID: "mm-p@rfc",
		FromAddress: "sender@ext.example", Subject: "群发", SentAt: 1000, SizePlain: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddFileRef(ctx, db.W(), "bodycs"); err != nil {
		t.Fatal(err)
	}
	if err := InsertMailParts(ctx, db.W(), []MailPart{{
		MessageID: "mm-p", Seq: 0, Kind: MailPartBody, SizePlain: 12, FileChecksum: "bodycs",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := InsertMailbox(ctx, db.W(), Mailbox{
		MessageID: "mm-p", UserID: 1, Role: MailboxRoleInbox,
		Status: MailboxStatusArchived, ChargedBytes: 12, PurgeAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := InsertMailbox(ctx, db.W(), Mailbox{
		MessageID: "mm-p", UserID: 2, Role: MailboxRoleInbox,
		Status: MailboxStatusArchived, ChargedBytes: 12, PurgeAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveCounter(ctx, db.W(), ScopeMailStorage, UserCounterKey(1, ""), 12, 1000); err != nil {
		t.Fatal(err)
	}

	due, err := ListDueArchivedMailboxes(ctx, db.R(), 2000, 50)
	if err != nil || len(due) != 2 {
		t.Fatalf("到期列表 = %d 封，期望 2: err=%v", len(due), err)
	}
	live, err := CountLiveMailboxes(ctx, db.R(), "mm-p")
	if err != nil || live != 2 {
		t.Fatalf("live=%d err=%v，期望 2", live, err)
	}

	var first DueMailbox
	for _, d := range due {
		if d.UserID == 1 {
			first = d
		}
	}
	dueGuard := PurgeGuard{RequireArchived: true, RequireDue: true, DueBefore: 2000}

	// 先清 user1：仍有 live 属主，不释放部件引用。
	if err := PurgeMailboxTx(ctx, db, first, dueGuard); err != nil {
		t.Fatalf("首次 PurgeMailboxTx: %v", err)
	}
	used, _ := GetCounter(ctx, db.R(), ScopeMailStorage, UserCounterKey(1, ""))
	if used != 0 {
		t.Fatalf("释放后配额应归零，得到 %d", used)
	}
	file, _ := GetFile(ctx, db.R(), "bodycs")
	if file.RefCount != 1 {
		t.Fatalf("还有属主时不应释放部件引用，ref=%d", file.RefCount)
	}
	b1, err := GetMailboxForUser(ctx, db.R(), 1, "mm-p")
	if err != nil || b1.Status != MailboxStatusReleased {
		t.Fatalf("user1 归属应为 released: %+v %v", b1, err)
	}

	// 最后一个属主释放并释放部件引用。
	var second DueMailbox
	for _, d := range due {
		if d.UserID == 2 {
			second = d
		}
	}
	if err := PurgeMailboxTx(ctx, db, second, dueGuard); err != nil {
		t.Fatalf("末次 PurgeMailboxTx: %v", err)
	}
	file, _ = GetFile(ctx, db.R(), "bodycs")
	if file.RefCount != 0 || file.Status != FileArchive {
		t.Fatalf("末次释放后引用应归零且进入待回收，ref=%d status=%d", file.RefCount, file.Status)
	}

	// 幂等：重复销毁已 released 的行必须是无副作用的空操作。
	// 旧实现只按 id 匹配，SQLite 对值未变的 UPDATE 同样计入 affected，
	// 于是会二次退还配额（把别的邮件占用的额度凭空还回去）。
	if _, err := ReserveCounter(ctx, db.W(), ScopeMailStorage, UserCounterKey(2, ""), 800, 1000); err != nil {
		t.Fatal(err)
	}
	err = PurgeMailboxTx(ctx, db, second, PurgeGuard{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复销毁应返回 ErrNotFound，实际 %v", err)
	}
	used, _ = GetCounter(ctx, db.R(), ScopeMailStorage, UserCounterKey(2, ""))
	if used != 800 {
		t.Fatalf("重复销毁不得改动配额，期望仍为 800，实际 %d", used)
	}
	file, _ = GetFile(ctx, db.R(), "bodycs")
	if file.RefCount != 0 {
		t.Fatalf("重复销毁不得再次释放部件引用，ref=%d", file.RefCount)
	}

	// 恢复后不得被到期清理误删：ListDueArchivedMailboxes 的结果是事务外
	// 快照，用户可能在批次扫过之后把邮件恢复出来。
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('guest', '访客组', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
	                 VALUES (3, 'carol', 'x', 'guest', 0, 0)`)
	if err := InsertMailbox(ctx, db.W(), Mailbox{
		MessageID: "mm-p", UserID: 3, Role: MailboxRoleInbox,
		Status: MailboxStatusArchived, ChargedBytes: 12, PurgeAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	restored := DueMailbox{ID: 0, MessageID: "mm-p", UserID: 3, Role: MailboxRoleInbox}
	if err := db.R().QueryRowContext(ctx,
		`SELECT id FROM mailboxes WHERE message_id = 'mm-p' AND user_id = 3`).Scan(&restored.ID); err != nil {
		t.Fatal(err)
	}
	if err := SetMailboxStatus(ctx, db.W(), 3, "mm-p", MailboxRoleInbox, MailboxStatusNormal, 0); err != nil {
		t.Fatal(err)
	}
	if err := PurgeMailboxTx(ctx, db, restored, dueGuard); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已恢复的邮件不应被到期清理销毁，实际 %v", err)
	}
	box3, err := GetMailboxForUser(ctx, db.R(), 3, "mm-p")
	if err != nil || box3.Status != MailboxStatusNormal {
		t.Fatalf("user3 归属应保持 normal: %+v %v", box3, err)
	}
}

// insertAdminMsg 直写一封最小邮件（不经过收件管道），供全局列表测试造数。
func insertAdminMsg(t *testing.T, db *DB, id, from, subject string, sentAt int64) {
	t.Helper()
	ctx := context.Background()
	if err := InsertMailMessage(ctx, db.W(), MailMessage{
		ID: id, MessageID: id + "@rfc",
		FromAddress: from, Subject: subject, SentAt: sentAt, CreatedAt: sentAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func insertAdminBox(t *testing.T, db *DB, msgID string, uid int64, role, status string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := InsertMailbox(ctx, db.W(), Mailbox{
		MessageID: msgID, UserID: uid, Role: role, Status: status, ChargedBytes: 10,
	}); err != nil {
		t.Fatal(err)
	}
	b, err := GetMailboxForUser(ctx, db.R(), uid, msgID)
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func TestListAllMailboxesForAdmin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMailFixture(t, db) // uid1=alice(normal) uid2=bob(staff)

	insertAdminMsg(t, db, "m1", "ext@x.test", "第一封", 100)
	insertAdminMsg(t, db, "m2", "ext2@x.test", "第二封", 200)
	insertAdminMsg(t, db, "m3", "spam@x.test", "广告", 90)
	id1 := insertAdminBox(t, db, "m1", 1, MailboxRoleInbox, MailboxStatusNormal)
	insertAdminBox(t, db, "m2", 1, MailboxRoleInbox, MailboxStatusNormal)
	insertAdminBox(t, db, "m3", 2, MailboxRoleInbox, MailboxStatusArchived)

	// 默认（status 空）只看 normal，跨两个用户，按 sent_at DESC。
	items, total, err := ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{}, 50, 0)
	if err != nil {
		t.Fatalf("全局列表失败: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("normal 应 2 行，得 total=%d len=%d", total, len(items))
	}
	if items[0].MessageID != "m2" {
		t.Fatalf("应按 sent_at DESC 排序，得 %s", items[0].MessageID)
	}
	if items[0].OwnerAccount != "alice" {
		t.Fatalf("首行属主应是 alice，得 %q", items[0].OwnerAccount)
	}

	// status=archived 显式可查，带属主信息。
	items, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{
		Status: MailboxStatusArchived,
	}, 50, 0)
	if err != nil || total != 1 || items[0].MessageID != "m3" || items[0].OwnerAccount != "bob" {
		t.Fatalf("archived 过滤错误: %v items=%+v", err, items)
	}

	// 收件人地址子串。
	if err := InsertMailRecipients(ctx, db.W(), []MailRecipient{{
		MessageID: "m1", Kind: MailRecipientTo, Address: "alice@x.test", Seq: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	items, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{Query: "alice@x.test"}, 50, 0)
	if err != nil || total != 1 || items[0].MessageID != "m1" {
		t.Fatalf("query 过滤错误: %v items=%+v", err, items)
	}

	// role 过滤 + 按属主过滤 + 分页。
	items, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{
		UserID: 1,
	}, 1, 0)
	if err != nil || total != 2 || len(items) != 1 || items[0].MessageID != "m2" {
		t.Fatalf("userID 过滤/分页错误: %v total=%d items=%+v", err, total, items)
	}

	// 按属主账号子串过滤（管理员通常只记得账号而不是数字 ID）。
	_, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{
		Owner: "ali",
	}, 50, 0)
	if err != nil || total != 2 {
		t.Fatalf("owner 账号过滤错误: %v total=%d", err, total)
	}

	_, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{
		Owner: "nobody",
	}, 50, 0)
	if err != nil || total != 0 {
		t.Fatalf("owner 无命中应 0 行: %v total=%d", err, total)
	}

	// 关键词应覆盖内部 ID 与 RFC Message-ID（运维拿退信/日志排查）。
	for _, q := range []string{"m1", "m1@rfc"} {
		items, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{Query: q}, 50, 0)
		if err != nil || total != 1 || items[0].MessageID != "m1" {
			t.Fatalf("关键词 %q 应命中 m1: %v total=%d items=%+v", q, err, total, items)
		}
	}

	// 搜索词里的 LIKE 通配符按字面量处理：m_ 不得命中 m1/m2/m3，% 不得命中全部。
	_, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{Query: "m_"}, 50, 0)
	if err != nil || total != 0 {
		t.Fatalf("字面量 m_ 应 0 命中，得 total=%d err=%v", total, err)
	}
	_, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{Owner: "_"}, 50, 0)
	if err != nil || total != 0 {
		t.Fatalf("owner 字面量 _ 应 0 命中，得 total=%d", total)
	}
	_, total, err = ListAllMailboxes(ctx, db.R(), AdminMailboxFilter{Query: "%"}, 50, 0)
	if err != nil || total != 0 {
		t.Fatalf("字面量 %% 应 0 命中，得 total=%d", total)
	}

	// GetMailboxByID 往返与不存在。
	box, err := GetMailboxByID(ctx, db.R(), id1)
	if err != nil || box.MessageID != "m1" || box.UserID != 1 {
		t.Fatalf("按 ID 取归属错误: %v box=%+v", err, box)
	}
	if _, err := GetMailboxByID(ctx, db.R(), 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的归属应 ErrNotFound，得 %v", err)
	}
}
