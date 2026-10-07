package service

import (
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// seedAdminMailbox 直写一封属主为 uid 的邮件归属，返回 mailbox 行 ID。
// 只用于全局管理编排测试，不经过收件管道。
func seedAdminMailbox(t *testing.T, f *inboundFixture, msgID string, uid int64, role, status string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := store.InsertMailMessage(ctx, f.db.W(), store.MailMessage{
		ID: msgID, MessageID: msgID + "@rfc",
		FromAddress: "ext@x.test", FromName: "外部", Subject: "管理测试 " + msgID,
		SentAt: f.svc.Now(), CreatedAt: f.svc.Now(), SizePlain: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMailbox(ctx, f.db.W(), store.Mailbox{
		MessageID: msgID, UserID: uid, Role: role, Status: status, ChargedBytes: 12,
	}); err != nil {
		t.Fatal(err)
	}
	box, err := store.GetMailboxForUser(ctx, f.db.R(), uid, msgID)
	if err != nil {
		t.Fatal(err)
	}
	return box.ID
}

func TestAdminListAndGetMail(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")
	boxID := seedAdminMailbox(t, f, "adm-list-1", f.uid1, store.MailboxRoleInbox, store.MailboxStatusNormal)

	// 普通用户无权。
	if _, err := f.svc.AdminListMail(ctx, f.user, AdminListMailRequest{}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户列表应 403，得 %v", err)
	}

	res, err := f.svc.AdminListMail(ctx, admin, AdminListMailRequest{Limit: 50})
	if err != nil {
		t.Fatalf("管理列表失败: %v", err)
	}
	if res.Total != 1 || len(res.Items) != 1 {
		t.Fatalf("应 1 行，得 total=%d len=%d", res.Total, len(res.Items))
	}
	row := res.Items[0]
	if row.OwnerAccount != "alice" || row.ID != boxID {
		t.Fatalf("行数据错误: %+v", row)
	}

	// 详情不自动已读。
	if box0, _ := store.GetMailboxByID(ctx, f.db.R(), row.ID); box0.IsRead {
		t.Fatal("前置状态不应已读")
	}
	d, err := f.svc.AdminGetMail(ctx, admin, row.ID)
	if err != nil {
		t.Fatalf("管理详情失败: %v", err)
	}
	if d.Message.Subject == "" || d.OwnerAccount != "alice" || d.Box.ID != row.ID {
		t.Fatalf("详情字段错误: %+v", d)
	}
	if box1, _ := store.GetMailboxByID(ctx, f.db.R(), row.ID); box1.IsRead {
		t.Fatal("管理员查看详情不得改动用户已读状态")
	}

	// 未知归属 → NotFound；普通用户 403；非法筛选 → BadRequest。
	if _, err := f.svc.AdminGetMail(ctx, admin, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知归属应 ErrNotFound，得 %v", err)
	}
	if _, err := f.svc.AdminGetMail(ctx, f.user, row.ID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户详情应 403，得 %v", err)
	}
	if _, err := f.svc.AdminListMail(ctx, admin, AdminListMailRequest{Role: "bogus"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("非法 role 应 ErrBadRequest，得 %v", err)
	}

	// 管理员跨用户查看邮件元数据属敏感操作，必须留审计。
	logs, n, err := store.ListAuditLogs(ctx, f.db.R(), "admin.mail.message_view", "", 0, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || logs[0].Target != "adm-list-1" || logs[0].ActorID != admin.UserID() {
		t.Fatalf("详情查看应留 1 条审计，得 n=%d logs=%+v", n, logs)
	}
}

func TestAdminListMailNormalizesPaging(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")
	seedAdminMailbox(t, f, "adm-page-1", f.uid1, store.MailboxRoleInbox, store.MailboxStatusNormal)

	// 非法/超大 limit 由编排层归一为 50（与 store 实际取值一致），回显不得撒谎；
	// 负 offset 归零。
	res, err := f.svc.AdminListMail(ctx, admin, AdminListMailRequest{Limit: 99999, Offset: -5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Limit != 50 || res.Offset != 0 || res.Total != 1 {
		t.Fatalf("分页归一化错误: %+v", res)
	}

	// 合法值原样透传。
	res, err = f.svc.AdminListMail(ctx, admin, AdminListMailRequest{Limit: 1, Offset: 0})
	if err != nil || res.Limit != 1 || len(res.Items) != 1 {
		t.Fatalf("合法分页应透传: %+v err=%v", res, err)
	}

	// 负数属主 ID 是非法请求，不能被静默吞成"不过滤"。
	if _, err := f.svc.AdminListMail(ctx, admin, AdminListMailRequest{UserID: -3}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("负 userId 应 ErrBadRequest，得 %v", err)
	}

	// 属主账号过滤直达 store。
	res, err = f.svc.AdminListMail(ctx, admin, AdminListMailRequest{Owner: "alice"})
	if err != nil || res.Total != 1 || res.Items[0].OwnerAccount != "alice" {
		t.Fatalf("owner 过滤错误: %+v err=%v", res, err)
	}
}

func TestAdminMailArchiveRestorePurge(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")
	boxID := seedAdminMailbox(t, f, "adm-life-1", f.uid1, store.MailboxRoleInbox, store.MailboxStatusNormal)

	// 回收：normal → archived，带计划清理时间；重复回收幂等。
	if err := f.svc.AdminArchiveMail(ctx, admin, boxID); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if err := f.svc.AdminArchiveMail(ctx, admin, boxID); err != nil {
		t.Fatalf("重复回收应幂等，得 %v", err)
	}
	box, _ := store.GetMailboxByID(ctx, f.db.R(), boxID)
	if box.Status != store.MailboxStatusArchived || box.PurgeAt == 0 {
		t.Fatalf("回收状态错误: %+v", box)
	}

	// 恢复回 normal。
	if err := f.svc.AdminRestoreMail(ctx, admin, boxID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	box, _ = store.GetMailboxByID(ctx, f.db.R(), boxID)
	if box.Status != store.MailboxStatusNormal || box.PurgeAt != 0 {
		t.Fatalf("恢复状态错误: %+v", box)
	}
	// normal 态再恢复 → BadRequest；normal 态直接销毁 → BadRequest。
	if err := f.svc.AdminRestoreMail(ctx, admin, boxID); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("normal 态恢复应 ErrBadRequest，得 %v", err)
	}
	if err := f.svc.AdminPurgeMail(ctx, admin, boxID); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("normal 态彻底删除应 ErrBadRequest，得 %v", err)
	}

	// 再回收 → 彻底删除：released；released 行再删 → NotFound（影响行数 0）。
	if err := f.svc.AdminArchiveMail(ctx, admin, boxID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AdminPurgeMail(ctx, admin, boxID); err != nil {
		t.Fatalf("彻底删除失败: %v", err)
	}
	box, _ = store.GetMailboxByID(ctx, f.db.R(), boxID)
	if box.Status != store.MailboxStatusReleased {
		t.Fatalf("应 released: %+v", box)
	}
	if err := f.svc.AdminPurgeMail(ctx, admin, boxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released 行再删应 ErrNotFound，得 %v", err)
	}
	if err := f.svc.AdminArchiveMail(ctx, admin, boxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released 行回收应 ErrNotFound，得 %v", err)
	}
	if err := f.svc.AdminRestoreMail(ctx, admin, boxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released 行恢复应 ErrNotFound，得 %v", err)
	}

	// 普通用户全部 403。
	if err := f.svc.AdminArchiveMail(ctx, f.user, boxID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("普通用户回收应 403，得 %v", err)
	}
}

// TestAdminListMailStatsIgnoreFilters 锁住统计条不受筛选影响这条契约。
//
// 统计条回答的是"整个系统里有多少邮件"。一旦它跟着筛选条件走，它就只是
// 表格上方的一个副本，对不上任何问题——而「全站」正是这一页存在的理由。
func TestAdminListMailStatsIgnoreFilters(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	admin := f.adminUser("root")
	seedAdminMailbox(t, f, "adm-stat-1", f.uid1, store.MailboxRoleInbox, store.MailboxStatusNormal)
	seedAdminMailbox(t, f, "adm-stat-2", f.uid2, store.MailboxRoleInbox, store.MailboxStatusArchived)

	all, err := f.svc.AdminListMail(ctx, admin, AdminListMailRequest{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if all.Stats.Total != 2 || all.Stats.Archived != 1 || all.Stats.UserCount != 2 {
		t.Fatalf("全站统计不对: %+v", all.Stats)
	}

	// 筛到只剩一封：Total 跟着变，Stats 不能变。
	filtered, err := f.svc.AdminListMail(ctx, admin, AdminListMailRequest{
		Status: store.MailboxStatusArchived, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 1 {
		t.Fatalf("筛选后应命中 1 行，得 %d", filtered.Total)
	}
	if filtered.Stats != all.Stats {
		t.Fatalf("统计不应随筛选变化: %+v vs %+v", filtered.Stats, all.Stats)
	}

	// 按不存在的属主筛到 0 行时，统计仍是全站的。
	none, err := f.svc.AdminListMail(ctx, admin, AdminListMailRequest{Owner: "ghost", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if none.Total != 0 {
		t.Fatalf("无命中应 total=0，得 %d", none.Total)
	}
	if none.Stats.Total != 2 {
		t.Fatalf("无命中时统计仍应是全站值，得 %+v", none.Stats)
	}
}
