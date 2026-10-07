package store

import (
	"errors"
	"testing"
)

// seedUnbindFixture 造一个能提交解绑申请的最小环境：一个用户组、一个域名、
// 一个用户、两个地址。
func seedUnbindFixture(t *testing.T, db *DB) (userID int64, addrA, addrB string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, is_builtin, permissions, priority, created_at)
		VALUES ('normal', '普通用户', 1, 32831, 100, 1)`)
	mustExec(t, db, `INSERT INTO mail_domains (domain, created_at) VALUES ('pylin.cn', 1)`)
	mustExec(t, db, `INSERT INTO users (account, password_hash, group_name, status, created_at, updated_at)
		VALUES ('alice', 'x', 'normal', 1, 1, 1)`)
	mustExec(t, db, `INSERT INTO group_mail_domains (group_name, domain, receive_enabled, created_at, updated_at)
		VALUES ('normal', 'pylin.cn', 1, 1, 1)`)

	var id int64
	mustQuery(t, db, `SELECT id FROM users WHERE account = 'alice'`, &id)

	for _, local := range []string{"alice", "alice.alt"} {
		a := local + "@pylin.cn"
		if err := CreateMailAddress(testContext(), db.W(), MailAddress{
			Address: a, LocalPart: local, Domain: "pylin.cn", UserID: id,
		}); err != nil {
			t.Fatalf("创建地址 %s 失败: %v", a, err)
		}
		if local == "alice" {
			addrA = a
		} else {
			addrB = a
		}
	}
	return id, addrA, addrB
}

// TestUnbindApproveDeletesAddress 批准后地址真的消失，且申请单留存。
//
// 这条是整条链路的目的。地址必须**真删除**（不是冻结），否则别人申请
// 同一个前缀时会被占住；而申请单必须留下，否则"这个地址当初为什么没了"
// 无人能答。
func TestUnbindApproveDeletesAddress(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()

	if err := CreateUnbindRequest(ctx, db.W(), MailUnbindRequest{
		Address: addr, UserID: userID, Reason: "不再使用",
	}); err != nil {
		t.Fatalf("创建解绑申请失败: %v", err)
	}
	pending, err := PendingUnbindForAddress(ctx, db.R(), addr)
	if err != nil {
		t.Fatalf("读取待审申请失败: %v", err)
	}
	if pending.Status != UnbindPending || pending.Reason != "不再使用" {
		t.Fatalf("待审申请内容不对: %+v", pending)
	}

	if _, err := ApproveUnbindRequest(ctx, db, pending.ID, 999, "同意"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}

	// 地址没了。
	if _, err := GetMailAddress(ctx, db.R(), addr); !errors.Is(err, ErrNotFound) {
		t.Fatalf("批准后地址应不存在，得 %v", err)
	}
	// 单子留下，且是已批准。
	got, err := GetUnbindRequest(ctx, db.R(), pending.ID)
	if err != nil {
		t.Fatalf("申请单应留存: %v", err)
	}
	if got.Status != UnbindApproved || got.ReviewedBy != 999 || got.Note != "同意" {
		t.Fatalf("申请单状态不对: %+v", got)
	}
	if got.ReviewedAt == 0 {
		t.Fatal("审核时间未写入")
	}
}

// TestUnbindApproveIsIdempotentGuard 同一张单不能批两次，也不能重复删地址。
func TestUnbindApproveTwiceRejected(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))
	pending, err := PendingUnbindForAddress(ctx, db.R(), addr)
	if err != nil {
		t.Fatalf("读取待审申请失败: %v", err)
	}
	if _, err := ApproveUnbindRequest(ctx, db, pending.ID, 1, ""); err != nil {
		t.Fatalf("首次批准失败: %v", err)
	}
	if _, err := ApproveUnbindRequest(ctx, db, pending.ID, 1, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("二次批准应报冲突，得 %v", err)
	}
}

// TestUnbindApproveLostAddress 审批时地址已被别处删除：必须报冲突，不动数据。
//
// 这是"待审却已失效"的真实场景——管理员手工删过地址，或用户被注销连带
// 清掉了地址。此时静默通过等于放走一张指向空处的单子。
func TestUnbindApproveLostAddress(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))
	pending, err := PendingUnbindForAddress(ctx, db.R(), addr)
	if err != nil {
		t.Fatalf("读取待审申请失败: %v", err)
	}
	// 地址被外部删掉。
	if _, err := db.W().ExecContext(ctx, `DELETE FROM mail_addresses WHERE address = ?`, addr); err != nil {
		t.Fatalf("预置删除地址失败: %v", err)
	}

	if _, err := ApproveUnbindRequest(ctx, db, pending.ID, 1, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("地址已失效时应报冲突，得 %v", err)
	}
	// 单子必须仍是 pending，不能被半途改成已批准。
	got, err := GetUnbindRequest(ctx, db.R(), pending.ID)
	if err != nil {
		t.Fatalf("读取申请失败: %v", err)
	}
	if got.Status != UnbindPending {
		t.Fatalf("失败后单子状态应仍为 pending，实为 %s", got.Status)
	}
}

// TestUnbindOnePendingPerAddress 同一地址只能有一条待审申请。
func TestUnbindOnePendingPerAddress(t *testing.T) {
	db := openTestDB(t)
	userID, addr, other := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID, Reason: "第一次"}))
	if err := CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID, Reason: "第二次"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复申请应报冲突，得 %v", err)
	}
	// 另一个地址不受影响。
	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: other, UserID: userID}))

	var n int64
	mustQuery(t, db, `SELECT COUNT(*) FROM mail_unbind_requests WHERE status = 'pending'`, &n)
	if n != 2 {
		t.Fatalf("待审申请应为 2 条（两个地址各一），得 %d", n)
	}
}

// TestUnbindCancel 撤销后可以重新申请。
func TestUnbindCancel(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))
	pending, err := PendingUnbindForAddress(ctx, db.R(), addr)
	if err != nil {
		t.Fatalf("读取待审申请失败: %v", err)
	}
	if _, err := CancelUnbindRequest(ctx, db.W(), pending.ID, userID); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	// 再撤一次应报冲突。
	if _, err := CancelUnbindRequest(ctx, db.W(), pending.ID, userID); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复撤销应报冲突，得 %v", err)
	}
	// 撤销后应能重新申请。
	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID, Reason: "再申请"}))
}

// TestUnbindCancelOthersRejected 不能撤销别人的申请。
func TestUnbindCancelOthersRejected(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))
	pending, err := PendingUnbindForAddress(ctx, db.R(), addr)
	if err != nil {
		t.Fatalf("读取待审申请失败: %v", err)
	}
	if _, err := CancelUnbindRequest(ctx, db.W(), pending.ID, userID+777); !errors.Is(err, ErrConflict) {
		t.Fatalf("撤销他人申请应报冲突，得 %v", err)
	}
}

// TestUnbindRejectKeepsAddress 驳回后地址必须在。
func TestUnbindRejectKeepsAddress(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))
	pending, err := PendingUnbindForAddress(ctx, db.R(), addr)
	if err != nil {
		t.Fatalf("读取待审申请失败: %v", err)
	}
	if _, err := RejectUnbindRequest(ctx, db.W(), pending.ID, 42, "暂不解绑"); err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	if _, err := GetMailAddress(ctx, db.R(), addr); err != nil {
		t.Fatalf("驳回后地址应仍存在: %v", err)
	}
	// 驳回后可重新申请。
	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))
}

// TestUnbindFreesAddressForReuse 批准后地址可被重新申请。
//
// 这是"真删除"相对于冻结的全部意义：腾出来的前缀能再次注册。
func TestUnbindFreesAddressForReuse(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))
	pending, _ := PendingUnbindForAddress(ctx, db.R(), addr)
	if _, err := ApproveUnbindRequest(ctx, db, pending.ID, 1, ""); err != nil {
		t.Fatalf("批准失败: %v", err)
	}

	// 同前缀换个用户，应能重新注册。
	mustExec(t, db, `INSERT INTO users (account, password_hash, group_name, status, created_at, updated_at)
		VALUES ('bob', 'x', 'normal', 1, 1, 1)`)
	var bob int64
	mustQuery(t, db, `SELECT id FROM users WHERE account = 'bob'`, &bob)
	if err := CreateMailAddress(ctx, db.W(), MailAddress{
		Address: addr, LocalPart: "alice", Domain: "pylin.cn", UserID: bob,
	}); err != nil {
		t.Fatalf("地址释放后应能重新注册: %v", err)
	}
}

// TestUnbindListAndStats 管理端列表与统计条。
func TestUnbindListAndStats(t *testing.T) {
	db := openTestDB(t)
	userID, addrA, addrB := seedUnbindFixture(t, db)
	ctx := testContext()

	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addrA, UserID: userID, Reason: "a"}))
	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addrB, UserID: userID, Reason: "b"}))

	pa, _ := PendingUnbindForAddress(ctx, db.R(), addrA)
	if _, err := RejectUnbindRequest(ctx, db.W(), pa.ID, 1, "x"); err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	pb, _ := PendingUnbindForAddress(ctx, db.R(), addrB)
	if _, err := ApproveUnbindRequest(ctx, db, pb.ID, 1, ""); err != nil {
		t.Fatalf("批准失败: %v", err)
	}

	// 全部三类都出现过，但已处理的现在不在 pending 里。
	all, err := ListUnbindRequests(ctx, db.R(), UnbindListFilter{})
	if err != nil {
		t.Fatalf("列全部失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("应列出 2 条历史单，得 %d", len(all))
	}
	stats, err := CountUnbindStats(ctx, db.R(), 0)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if stats.Pending != 0 || stats.Approved != 1 || stats.Rejected != 1 {
		t.Fatalf("统计不对: %+v", stats)
	}
}

// TestUnbindListEscapeLike 模糊搜索里的 % 与 _ 必须按字面匹配。
//
// 不转义的话，管理员搜 "%" 会列出全部申请——这在几百条数据时还只是难看，
// 在几万条时是拖垮管理页的查询。
func TestUnbindListEscapeLike(t *testing.T) {
	db := openTestDB(t)
	userID, addr, _ := seedUnbindFixture(t, db)
	ctx := testContext()
	mustExecOK(t, db, CreateUnbindRequest(ctx, db.W(),
		MailUnbindRequest{Address: addr, UserID: userID}))

	all, err := ListUnbindRequests(ctx, db.R(), UnbindListFilter{})
	if err != nil || len(all) != 1 {
		t.Fatalf("前置失败: %v %d", err, len(all))
	}
	hits, err := ListUnbindRequests(ctx, db.R(), UnbindListFilter{Search: "%"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("%% 应按字面匹配（得 0 条），实得 %d 条", len(hits))
	}
	real, err := ListUnbindRequests(ctx, db.R(), UnbindListFilter{Search: "alice"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(real) != 1 {
		t.Fatalf("alice 应命中 1 条，得 %d", len(real))
	}
}

func mustExecOK(t *testing.T, db *DB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("操作失败: %v", err)
	}
}
