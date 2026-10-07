package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailin"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/spf"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// inboundFixture 准备一套可收件环境：alice/bob 在 normal 组 example.com。
type inboundFixture struct {
	*shareFixture
	stub *storageStub
	uid1 int64
	uid2 int64
}

func newInboundFixture(t *testing.T) *inboundFixture {
	t.Helper()
	f := newShareFixture(t)
	f.enableEncryption()
	stub := newStorageStub()
	f.svc.Backend = stub

	ctx := context.Background()
	now := store.Now()
	uid2, err := store.CreateUser(ctx, f.db.W(), store.User{
		Account: "bob", DisplayName: "bob", PasswordHash: "x",
		GroupName: perm.GroupNormal, Status: store.UserEnabled,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureMailDomain(ctx, f.db.W(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindGroupMailDomain(ctx, f.db.W(), perm.GroupNormal, "example.com", true); err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		addr, local string
		uid         int64
	}{
		{"alice@example.com", "alice", f.user.UserID()},
		{"bob@example.com", "bob", uid2},
	} {
		if err := store.CreateMailAddress(ctx, f.db.W(), store.MailAddress{
			Address: a.addr, LocalPart: a.local, Domain: "example.com",
			UserID: a.uid, Status: store.MailAddressActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &inboundFixture{shareFixture: f, stub: stub, uid1: f.user.UserID(), uid2: uid2}
}

func mixedRaw(messageID string) string {
	att := base64.StdEncoding.EncodeToString([]byte("ATTACH"))
	return "From: Sender <sender@ext.example>\r\n" +
		"To: alice@example.com\r\n" +
		"Cc: Bob <bob@example.com>\r\n" +
		"Subject: inbound test\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		"Date: Thu, 25 Sep 2025 12:00:00 +0000\r\n" +
		"Content-Type: multipart/mixed; boundary=\"MIX\"\r\n\r\n" +
		"--MIX\r\nContent-Type: multipart/alternative; boundary=\"ALT\"\r\n\r\n" +
		"--ALT\r\nContent-Type: text/plain\r\n\r\nplain hello\r\n" +
		"--ALT\r\nContent-Type: text/html\r\n\r\n<p>html hello</p>\r\n--ALT--\r\n" +
		"--MIX\r\nContent-Type: application/octet-stream; name=\"a.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\n" +
		att + "\r\n--MIX--\r\n"
}

// receive 走完整入站链路投递一封邮件，subject 唯一（同时用作 Message-ID）。
func (f *inboundFixture) receive(t *testing.T, subject string) {
	t.Helper()
	raw := "From: Sender <sender@ext.example>\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + subject + "@ext.example>\r\n" +
		"Date: Thu, 25 Sep 2025 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"正文 " + subject + "\r\n"
	env := mailin.Envelope{
		RemoteIP:   "9.9.9.9",
		Helo:       "mta.ext.example",
		MailFrom:   "sender@ext.example",
		Recipients: []string{"alice@example.com"},
		SPFResult:  "pass",
	}
	if err := f.svc.Receive(context.Background(), env, strings.NewReader(raw)); err != nil {
		t.Fatalf("落信失败: %v", err)
	}
}

func countRows(t *testing.T, f *inboundFixture, table string) int {
	t.Helper()
	var n int
	if err := f.db.R().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReceiveInboundFullFlow(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	env := mailin.Envelope{
		RemoteIP:   "9.9.9.9",
		Helo:       "mta.ext.example",
		MailFrom:   "sender@ext.example",
		Recipients: []string{"alice@example.com", "bob@example.com"},
		SPFResult:  "pass",
	}
	if err := f.svc.Receive(ctx, env, strings.NewReader(mixedRaw("recv-1@ext.example"))); err != nil {
		t.Fatalf("落信失败: %v", err)
	}

	msg, err := store.GetMailMessageByRFCID(ctx, f.db.R(), "recv-1@ext.example")
	if err != nil {
		t.Fatalf("查邮件失败: %v", err)
	}
	if msg.FromAddress != "sender@ext.example" ||
		msg.SPFResult != "pass" || msg.AttachmentCount != 1 {
		t.Fatalf("邮件元数据错误: %+v", msg)
	}

	// 收件人头：to 1、cc 1。
	rs, err := store.ListMailRecipients(ctx, f.db.R(), msg.ID)
	if err != nil || len(rs) != 2 || rs[0].Kind != store.MailRecipientTo ||
		rs[0].Address != "alice@example.com" || rs[1].Kind != store.MailRecipientCc {
		t.Fatalf("收件人表错误: %+v err=%v", rs, err)
	}

	// 部件：正文容器 + 附件；各自 ref_count=1。
	ps, err := store.ListMailParts(ctx, f.db.R(), msg.ID)
	if err != nil || len(ps) != 2 {
		t.Fatalf("部件数错误: %+v err=%v", ps, err)
	}
	bodyExpect, err := EncodeMailBody(MailBody{HTML: "<p>html hello</p>", Text: "plain hello"})
	if err != nil {
		t.Fatal(err)
	}
	bodySum := sha256.Sum256(bodyExpect)
	if ps[0].Kind != store.MailPartBody || ps[0].FileChecksum != hex.EncodeToString(bodySum[:]) {
		t.Fatalf("正文部件错误: %+v", ps[0])
	}
	if ps[1].Kind != store.MailPartAttachment || ps[1].FileName != "a.bin" ||
		ps[1].SizePlain != int64(len("ATTACH")) {
		t.Fatalf("附件部件错误: %+v", ps[1])
	}
	for _, p := range ps {
		file, ferr := store.GetFile(ctx, f.db.R(), p.FileChecksum)
		if ferr != nil || file.RefCount != 1 || file.Status != store.FileNormal {
			t.Fatalf("部件 %d 文件行错误: %+v err=%v", p.Seq, file, ferr)
		}
	}

	// 总明文字节 = 正文容器 + 附件。
	totalPlain := int64(len(bodyExpect)) + int64(len("ATTACH"))
	if msg.SizePlain != totalPlain {
		t.Fatalf("size_plain = %d，期望 %d", msg.SizePlain, totalPlain)
	}

	// 两个收件人各有 inbox 归属行、各计一份配额。
	for _, uid := range []int64{f.uid1, f.uid2} {
		box, berr := store.GetMailbox(ctx, f.db.R(), uid, msg.ID, store.MailboxRoleInbox)
		if berr != nil || box.ChargedBytes != totalPlain || box.Status != store.MailboxStatusNormal {
			t.Fatalf("uid %d 归属行错误: %+v err=%v", uid, box, berr)
		}
		used, cerr := store.GetCounter(ctx, f.db.R(),
			store.ScopeMailStorage, store.UserCounterKey(uid, ""))
		if cerr != nil || used != totalPlain {
			t.Fatalf("uid %d 配额 used=%d err=%v，期望 %d", uid, used, cerr, totalPlain)
		}
	}

	// 物理对象只入池一次：2 个部件 = 2 次 Put，不随收件人数翻倍。
	if f.stub.next != 2 {
		t.Fatalf("后端 Put 次数 = %d，期望 2（物理去重）", f.stub.next)
	}
}

func TestReceiveInboundIdempotent(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	env := mailin.Envelope{
		RemoteIP: "9.9.9.9", Recipients: []string{"alice@example.com"},
	}
	raw := mixedRaw("recv-dup@ext.example")
	if err := f.svc.Receive(ctx, env, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Receive(ctx, env, strings.NewReader(raw)); err != nil {
		t.Fatalf("重复投递应幂等成功，得到 %v", err)
	}
	if n := countRows(t, f, "mail_messages"); n != 1 {
		t.Fatalf("重复投递后邮件数 = %d，期望 1", n)
	}
	if n := countRows(t, f, "mailboxes"); n != 1 {
		t.Fatalf("重复投递后归属行数 = %d，期望 1", n)
	}
	bodyExpect, _ := EncodeMailBody(MailBody{HTML: "<p>html hello</p>", Text: "plain hello"})
	totalPlain := int64(len(bodyExpect)) + int64(len("ATTACH"))
	used, _ := store.GetCounter(ctx, f.db.R(),
		store.ScopeMailStorage, store.UserCounterKey(f.uid1, ""))
	if used != totalPlain {
		t.Fatalf("重复投递后配额 = %d，期望 %d", used, totalPlain)
	}
}

func TestReceiveInboundQuotaRollback(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	// 建一个独立组 + 独立域名，配额只给 1 字节：carol 必然装不下。
	if err := store.CreateGroup(ctx, f.db.W(), store.Group{
		Name: "tiny", DisplayName: "tiny", Permissions: 0, CreatedAt: store.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupQuota(ctx, f.db.W(), "tiny", store.QuotaMailStorageTotal, 1); err != nil {
		t.Fatal(err)
	}
	carol, err := store.CreateUser(ctx, f.db.W(), store.User{
		Account: "carol", DisplayName: "carol", PasswordHash: "x",
		GroupName: "tiny", Status: store.UserEnabled,
		CreatedAt: store.Now(), UpdatedAt: store.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureMailDomain(ctx, f.db.W(), "tiny.example"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindGroupMailDomain(ctx, f.db.W(), "tiny", "tiny.example", true); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMailAddress(ctx, f.db.W(), store.MailAddress{
		Address: "carol@tiny.example", LocalPart: "carol", Domain: "tiny.example",
		UserID: carol, Status: store.MailAddressActive,
	}); err != nil {
		t.Fatal(err)
	}

	msgsBefore := countRows(t, f, "mail_messages")
	usedBefore, _ := store.GetCounter(ctx, f.db.R(),
		store.ScopeMailStorage, store.UserCounterKey(f.uid1, ""))

	env := mailin.Envelope{Recipients: []string{"alice@example.com", "carol@tiny.example"}}
	err = f.svc.Receive(ctx, env, strings.NewReader(mixedRaw("recv-q@ext.example")))
	se := mailin.AsStatusError(err)
	if se == nil || se.Code != 452 {
		t.Fatalf("超额应 452，得到 %v", err)
	}
	if n := countRows(t, f, "mail_messages"); n != msgsBefore {
		t.Fatal("失败后不应留下邮件行")
	}
	if n := countRows(t, f, "mail_parts"); n != 0 {
		t.Fatalf("失败后不应留下部件行，得到 %d", n)
	}
	usedAfter, _ := store.GetCounter(ctx, f.db.R(),
		store.ScopeMailStorage, store.UserCounterKey(f.uid1, ""))
	if usedAfter != usedBefore {
		t.Fatalf("中途失败应回退 alice 的预留：%d ≠ %d", usedAfter, usedBefore)
	}
}

func TestVerifyInboundRecipient(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	if err := f.svc.VerifyRecipient(ctx, "alice@example.com", 0); err != nil {
		t.Fatalf("正常地址应通过: %v", err)
	}
	if err := f.svc.VerifyRecipient(ctx, "<alice@example.com>", 10); err != nil {
		t.Fatalf("带尖括号地址应通过: %v", err)
	}
	assertCode := func(err error, want int, label string) {
		se := mailin.AsStatusError(err)
		if se == nil || se.Code != want {
			t.Fatalf("%s: 期望 %d，得到 %v", label, want, err)
		}
	}
	assertCode(f.svc.VerifyRecipient(ctx, "ghost@example.com", 0), 550, "不存在地址")
	assertCode(f.svc.VerifyRecipient(ctx, "alice@other.example", 0), 550, "未托管域")
	assertCode(f.svc.VerifyRecipient(ctx, "alice@example.com", 2<<30), 452, "超出 1GiB 邮箱")

	if err := store.SetMailAddressStatus(ctx, f.db.W(), "bob@example.com", store.MailAddressFrozen); err != nil {
		t.Fatal(err)
	}
	assertCode(f.svc.VerifyRecipient(ctx, "bob@example.com", 0), 550, "冻结地址")

	if err := store.EnsureMailDomain(ctx, f.db.W(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindGroupMailDomain(ctx, f.db.W(), perm.GroupNormal, "example.com", false); err != nil {
		t.Fatal(err)
	}
	assertCode(f.svc.VerifyRecipient(ctx, "alice@example.com", 0), 550, "域名停收")
}

// TestInboundRejectsAddressOfDepartedGroup 收件人校验必须独立于地址的 status 缓存。
//
// 只信 status 等于把授权交给一份可能陈旧的派生数据：运维直接改
// users.group_name、或迁移前的老库里残留着没跑过同步的地址，都会让已移出
// 域绑定组的用户继续收信。这里刻意绕开同步直接改组，验证复核仍然生效。
func TestInboundRejectsAddressOfDepartedGroup(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	if err := store.CreateGroup(ctx, f.db.W(), store.Group{
		Name: "archived", DisplayName: "已归档组",
	}); err != nil {
		t.Fatal(err)
	}
	assertCode := func(err error, want int, label string) {
		t.Helper()
		se := mailin.AsStatusError(err)
		if se == nil || se.Code != want {
			t.Fatalf("%s: 期望 %d，得到 %v", label, want, err)
		}
	}
	statusOf := func(addr string) string {
		t.Helper()
		a, err := store.GetMailAddress(ctx, f.db.R(), addr)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", addr, err)
		}
		return a.Status
	}

	// 只改 users.group_name，不动 mail_addresses.status：地址仍是 active。
	if err := store.UpdateUserGroup(ctx, f.db.W(), f.uid2, "archived"); err != nil {
		t.Fatal(err)
	}
	if got := statusOf("bob@example.com"); got != store.MailAddressActive {
		t.Fatalf("前置条件不成立：地址应仍为 active，实际 %q", got)
	}
	assertCode(f.svc.VerifyRecipient(ctx, "bob@example.com", 0), 550, "已离开域绑定组的用户")
	// 同组用户不受牵连。
	if err := f.svc.VerifyRecipient(ctx, "alice@example.com", 0); err != nil {
		t.Fatalf("同组用户不应被波及: %v", err)
	}
}

// TestAdminGroupChangeFreezesMailAddresses 端到端覆盖「移组冻结 / 回组恢复」。
//
// 与上一个测试互补：那里绕开同步验证复核，这里验证同步本身真的被移组操作
// 触发——否则 frozen 状态仍然没有生产者。
func TestAdminGroupChangeFreezesMailAddresses(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()

	adminID, err := store.CreateUser(ctx, f.db.W(), store.User{
		Account: "root", DisplayName: "root", PasswordHash: "x",
		GroupName: perm.GroupAdmin, Status: store.UserEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.GetUserByID(ctx, f.db.R(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	p := adminPrincipal(ctx, t, f.shareFixture, admin)

	if err := store.CreateGroup(ctx, f.db.W(), store.Group{
		Name: "archived", DisplayName: "已归档组",
	}); err != nil {
		t.Fatal(err)
	}
	statusOf := func(addr string) string {
		t.Helper()
		a, err := store.GetMailAddress(ctx, f.db.R(), addr)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", addr, err)
		}
		return a.Status
	}

	// alice 移出 normal 组 → 地址冻结，收信被拒。
	if err := f.svc.AdminSetUserGroup(ctx, p, f.uid1, "archived"); err != nil {
		t.Fatalf("移组失败: %v", err)
	}
	if got := statusOf("alice@example.com"); got != store.MailAddressFrozen {
		t.Fatalf("移组后地址应被冻结，实际 %q", got)
	}
	if se := mailin.AsStatusError(f.svc.VerifyRecipient(ctx, "alice@example.com", 0)); se == nil || se.Code != 550 {
		t.Fatalf("冻结后仍可收信: %v", f.svc.VerifyRecipient(ctx, "alice@example.com", 0))
	}
	// 地址没有被删除：历史邮件仍要可读。
	if _, err := store.GetMailAddress(ctx, f.db.R(), "alice@example.com"); err != nil {
		t.Fatalf("移组不应删除地址: %v", err)
	}
	// 冻结地址不再占用组配额。
	if n, err := store.CountMailAddresses(ctx, f.db.R(), f.uid1, "example.com"); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("冻结地址不应计入可用配额，实际 %d", n)
	}

	// 移回 normal 组 → 自动恢复，无需逐个手工解冻。
	if err := f.svc.AdminSetUserGroup(ctx, p, f.uid1, perm.GroupNormal); err != nil {
		t.Fatalf("移回组失败: %v", err)
	}
	if got := statusOf("alice@example.com"); got != store.MailAddressActive {
		t.Fatalf("回组后地址应自动恢复，实际 %q", got)
	}
	if err := f.svc.VerifyRecipient(ctx, "alice@example.com", 0); err != nil {
		t.Fatalf("恢复后应可收信: %v", err)
	}
}

// fakeSPFResolver 是 service 测试用的 SPF 假 DNS。
type fakeSPFResolver struct {
	txt map[string][]string
	err error
}

func (r *fakeSPFResolver) LookupTXT(_ context.Context, domain string) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.txt[domain], nil
}
func (r *fakeSPFResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	return nil, nil
}
func (r *fakeSPFResolver) LookupMXHosts(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func TestCheckSenderSPF(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	f.svc.spfResolver = &fakeSPFResolver{txt: map[string][]string{
		"hard.example": {"v=spf1 -all"},
	}}

	// 默认 hard：fail 直接 550。
	res, err := f.svc.CheckSender(ctx, "1.2.3.4", "mx.hard.example", "p@hard.example")
	se := mailin.AsStatusError(err)
	if se == nil || se.Code != 550 || res != string(spf.Fail) {
		t.Fatalf("hard fail 应 550 且返回 fail，res=%s err=%v", res, err)
	}

	// mark：只记录不拒收。
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyMailSPFPolicy: settings.SPFMark,
	}, 0); err != nil {
		t.Fatal(err)
	}
	res, err = f.svc.CheckSender(ctx, "1.2.3.4", "mx.hard.example", "p@hard.example")
	if err != nil || res != string(spf.Fail) {
		t.Fatalf("mark 策略不应拒收，res=%s err=%v", res, err)
	}

	// off：不校验。
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyMailSPFPolicy: settings.SPFOff,
	}, 0); err != nil {
		t.Fatal(err)
	}
	res, err = f.svc.CheckSender(ctx, "1.2.3.4", "mx.hard.example", "p@hard.example")
	if err != nil || res != "" {
		t.Fatalf("off 策略应跳过，res=%q err=%v", res, err)
	}

	// 空 MAIL FROM 时身份退回 HELO 域（重新置回 hard）。
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyMailSPFPolicy: settings.SPFHard,
	}, 0); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CheckSender(ctx, "1.2.3.4", "hard.example", "")
	if mailin.AsStatusError(err) == nil || mailin.AsStatusError(err).Code != 550 {
		t.Fatalf("空反向路径应按 HELO 域校验，得到 %v", err)
	}

	// DNS 临时故障 → 451。
	f.svc.spfResolver = &fakeSPFResolver{err: errors.New("dns timeout")}
	_, err = f.svc.CheckSender(ctx, "1.2.3.4", "mx.x.example", "p@x.example")
	if se = mailin.AsStatusError(err); se == nil || se.Code != 451 {
		t.Fatalf("DNS 临时故障应 451，得到 %v", err)
	}
}
