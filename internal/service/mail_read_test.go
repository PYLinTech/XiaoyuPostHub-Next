package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailin"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// receiveRawMail 走真实入站管线写一封信，返回内部邮件 ID。
func receiveRawMail(t *testing.T, f *inboundFixture, rfcID string, recipients []string) string {
	t.Helper()
	ctx := context.Background()
	env := mailin.Envelope{
		RemoteIP:   "9.9.9.9",
		Helo:       "mta.ext.example",
		MailFrom:   "sender@ext.example",
		Recipients: recipients,
		SPFResult:  "pass",
	}
	if err := f.svc.Receive(ctx, env, strings.NewReader(mixedRaw(rfcID))); err != nil {
		t.Fatalf("落信失败: %v", err)
	}
	m, err := store.GetMailMessageByRFCID(ctx, f.db.R(), rfcID)
	if err != nil {
		t.Fatalf("查邮件失败: %v", err)
	}
	return m.ID
}

func TestListAndGetMail(t *testing.T) {
	f := newInboundFixture(t)
	id := receiveRawMail(t, f, "m3-list@ext.example", []string{"alice@example.com", "bob@example.com"})
	ctx := context.Background()

	res, err := f.svc.ListMail(ctx, f.user, ListMailRequest{View: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Items[0].MessageID != id || res.Items[0].IsRead {
		t.Fatalf("列表错误: %+v", res)
	}
	if res.Counters.UnreadInbox != 1 {
		t.Fatalf("未读徽标 = %d，期望 1", res.Counters.UnreadInbox)
	}

	detail, err := f.svc.GetMail(ctx, f.user, id)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Message.Subject != "inbound test" || len(detail.Parts) != 2 ||
		len(detail.Recipients) != 2 {
		t.Fatalf("详情错误: %+v", detail)
	}
	if !detail.Box.IsRead {
		t.Fatal("详情应把邮件标记为已读")
	}
	c, _ := store.GetMailboxCounters(ctx, f.db.R(), f.uid1)
	if c.UnreadInbox != 0 {
		t.Fatalf("未读应归零: %+v", c)
	}

	// 非属主（管理员也不是属主）看不到。
	other := f.adminUser("root2")
	if _, err := f.svc.GetMail(ctx, other, id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非属主应 Forbidden，得到 %v", err)
	}
}

func TestMailStarAndFilters(t *testing.T) {
	f := newInboundFixture(t)
	id := receiveRawMail(t, f, "m3-flag@ext.example", []string{"alice@example.com"})
	ctx := context.Background()

	if err := f.svc.StarMail(ctx, f.user, id, true); err != nil {
		t.Fatal(err)
	}
	res, _ := f.svc.ListMail(ctx, f.user, ListMailRequest{View: "starred"})
	if res.Total != 1 {
		t.Fatalf("星标视图应有 1 封，得到 %d", res.Total)
	}

	// 只看未读：已自动已读后应为 0；置回未读再过滤。
	if err := f.svc.MarkMailRead(ctx, f.user, id, false); err != nil {
		t.Fatal(err)
	}
	res, _ = f.svc.ListMail(ctx, f.user, ListMailRequest{View: "inbox", OnlyUnread: true})
	if res.Total != 1 {
		t.Fatal("未读过滤应有 1 封")
	}
	if err := f.svc.MarkMailRead(ctx, f.user, id, true); err != nil {
		t.Fatal(err)
	}

	// 非属主（管理员也不是属主）改星标不允许。
	other := f.adminUser("root3")
	if err := f.svc.StarMail(ctx, other, id, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非属主操作应 Forbidden: %v", err)
	}
}

func TestMailStarredView(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	// 收两封，标一封星标。
	for _, subject := range []string{"第一封", "第二封"} {
		f.receive(t, subject)
	}
	items, err := f.svc.ListMail(ctx, f.user, ListMailRequest{})
	if err != nil || len(items.Items) != 2 {
		t.Fatalf("收件箱应有 2 封: %v err=%v", len(items.Items), err)
	}
	target := items.Items[0].MessageID
	if err := f.svc.StarMail(ctx, f.user, target, true); err != nil {
		t.Fatalf("标星失败: %v", err)
	}

	res, err := f.svc.ListMail(ctx, f.user, ListMailRequest{View: "starred"})
	if err != nil || res.Total != 1 || res.Items[0].MessageID != target {
		t.Fatalf("星标视图应只含标星的那封: %v %+v", err, res)
	}

	// 取消星标后星标视图为空。
	if err := f.svc.StarMail(ctx, f.user, target, false); err != nil {
		t.Fatalf("取消星标失败: %v", err)
	}
	if res, err = f.svc.ListMail(ctx, f.user, ListMailRequest{View: "starred"}); err != nil || res.Total != 0 {
		t.Fatalf("取消星标后应为空: %v %+v", err, res)
	}
}

func TestMailSearchMetadata(t *testing.T) {
	f := newInboundFixture(t)
	receiveRawMail(t, f, "m3-q@ext.example", []string{"alice@example.com"})
	ctx := context.Background()
	hit, err := f.svc.ListMail(ctx, f.user, ListMailRequest{View: "inbox", Query: "inbound"})
	if err != nil || hit.Total != 1 {
		t.Fatalf("主题搜索应命中: %v total=%d", err, hit.Total)
	}
	miss, err := f.svc.ListMail(ctx, f.user, ListMailRequest{View: "inbox", Query: "html hello"})
	if err != nil {
		t.Fatal(err)
	}
	if miss.Total != 0 {
		t.Fatal("正文内容不应被元数据搜索命中")
	}
}

func TestMailArchiveTwoStage(t *testing.T) {
	f := newInboundFixture(t)
	// 只投递给 alice：她是唯一属主，彻底删除后部件引用应归零。
	id := receiveRawMail(t, f, "m3-trash@ext.example", []string{"alice@example.com"})
	ctx := context.Background()

	usedBefore, _ := store.GetCounter(ctx, f.db.R(), store.ScopeMailStorage,
		store.UserCounterKey(f.uid1, ""))
	if usedBefore == 0 {
		t.Fatal("前置：应已计配额")
	}
	if err := f.svc.ArchiveMyMail(ctx, f.user, id); err != nil {
		t.Fatal(err)
	}
	inbox, _ := f.svc.ListMail(ctx, f.user, ListMailRequest{View: "inbox"})
	if inbox.Total != 0 {
		t.Fatal("归档邮件不应出现在收件箱")
	}
	trash, _ := f.svc.ListMail(ctx, f.user, ListMailRequest{View: "trash"})
	if trash.Total != 1 {
		t.Fatal("归档视图应有 1 封")
	}
	usedIn, _ := store.GetCounter(ctx, f.db.R(), store.ScopeMailStorage,
		store.UserCounterKey(f.uid1, ""))
	if usedIn != usedBefore {
		t.Fatal("归档期间应继续占配额")
	}
	if err := f.svc.RestoreMyMail(ctx, f.user, id); err != nil {
		t.Fatal(err)
	}
	inbox, _ = f.svc.ListMail(ctx, f.user, ListMailRequest{View: "inbox"})
	if inbox.Total != 1 {
		t.Fatal("恢复后应回到收件箱")
	}

	// 恢复后再次回收 → 彻底删除。
	if err := f.svc.ArchiveMyMail(ctx, f.user, id); err != nil {
		t.Fatal(err)
	}
	// normal 状态下不允许直接彻底删除。
	if err := store.SetMailboxStatus(ctx, f.db.W(), f.uid1, id,
		store.MailboxRoleInbox, store.MailboxStatusNormal, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.PurgeMyMail(ctx, f.user, id); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("非归档邮件彻底删除应 BadRequest，得到 %v", err)
	}
	if err := f.svc.ArchiveMyMail(ctx, f.user, id); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.PurgeMyMail(ctx, f.user, id); err != nil {
		t.Fatalf("彻底删除失败: %v", err)
	}
	usedAfter, _ := store.GetCounter(ctx, f.db.R(), store.ScopeMailStorage,
		store.UserCounterKey(f.uid1, ""))
	if usedAfter != 0 {
		t.Fatalf("彻底删除后配额应归零，得到 %d", usedAfter)
	}
	parts, _ := store.ListMailParts(ctx, f.db.R(), id)
	for _, p := range parts {
		file, _ := store.GetFile(ctx, f.db.R(), p.FileChecksum)
		if file.RefCount != 0 {
			t.Fatalf("唯一属主删除后部件 %s 引用应归零，ref=%d", p.FileChecksum, file.RefCount)
		}
	}
}

func TestListMailRequiresPerm(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	if err := store.CreateGroup(ctx, f.db.W(), store.Group{
		Name: "nomail", DisplayName: "nomail", Permissions: 0, CreatedAt: store.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	uid, err := store.CreateUser(ctx, f.db.W(), store.User{
		Account: "dave", DisplayName: "dave", PasswordHash: "x",
		GroupName: "nomail", Status: store.UserEnabled,
		CreatedAt: store.Now(), UpdatedAt: store.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.GetUserByID(ctx, f.db.R(), uid)
	if err != nil {
		t.Fatal(err)
	}
	dave := adminPrincipal(ctx, t, f.shareFixture, user)
	if dave.Can(perm.MailAccess) {
		t.Fatal("前置：dave 不应有 MailAccess")
	}
	if _, err := f.svc.ListMail(ctx, dave, ListMailRequest{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("无 MailAccess 应 Forbidden，得到 %v", err)
	}
}
