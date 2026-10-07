package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestFreshSchemaCreatesAllTables 全新库按最终结构一次性建齐全部业务表。
func TestFreshSchemaCreatesAllTables(t *testing.T) {
	db := openTestDB(t)
	want := []string{
		"announcements", "announcement_reads", "announcement_targets", "audit_logs",
		"download_tickets", "files", "group_quotas", "invite_codes", "invite_uses",
		"pickup_codes", "quota_counters", "sessions", "share_accesses", "shares",
		"system_config", "throttle", "traffic_daily", "traffic_logs", "upload_tasks",
		"user_groups", "user_nodes", "users",
	}
	rows, err := db.R().Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("查询表清单失败: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描表名失败: %v", err)
		}
		got[name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("缺少表 %s", name)
		}
	}

	// 结构初始化必须幂等：重复打开同一文件不应报错。
	reopened, err := Open(db.Path())
	if err != nil {
		t.Fatalf("重复打开数据库失败: %v", err)
	}
	_ = reopened.Close()
}

// TestTickerIsSingular 顶部滚动公告的"有且仅有一条"必须由数据库保证，
// 而不是只在代码里判断。
func TestTickerIsSingular(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insert := func(id string) error {
		_, err := db.W().ExecContext(ctx,
			`INSERT INTO announcements (id, kind, audience, title, body, enabled, created_at, updated_at)
			 VALUES (?, 'ticker', 'all', '标题', '正文', 1, 0, 0)`, id)
		return err
	}
	if err := insert("t1"); err != nil {
		t.Fatalf("首条滚动公告应可写入: %v", err)
	}
	if err := insert("t2"); err == nil {
		t.Fatal("第二条启用中的滚动公告应被唯一索引拒绝")
	}
	if _, err := db.W().ExecContext(ctx, `UPDATE announcements SET enabled = 0 WHERE id = 't1'`); err != nil {
		t.Fatalf("停用滚动公告失败: %v", err)
	}
	if err := insert("t3"); err != nil {
		t.Fatalf("停用后可写入新的滚动公告: %v", err)
	}
	// 非滚动类公告不受该约束。
	if _, err := db.W().ExecContext(ctx,
		`INSERT INTO announcements (id, kind, audience, title, body, enabled, created_at, updated_at)
		 VALUES ('m1', 'message', 'all', '标题', '正文', 1, 0, 0)`); err != nil {
		t.Fatalf("普通消息不受滚动公告唯一约束影响: %v", err)
	}
	if _, err := db.W().ExecContext(ctx,
		`INSERT INTO announcements (id, kind, audience, title, body, enabled, created_at, updated_at)
		 VALUES ('m2', 'message', 'all', '标题', '正文', 1, 0, 0)`); err != nil {
		t.Fatalf("普通消息可以有任意多条: %v", err)
	}
}

// TestNodeTypeInvariant 文件必须有校验码、文件夹必须没有——这条不变式由
// CHECK 约束守，不能只靠应用层。
func TestNodeTypeInvariant(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
	                 VALUES (1, 'alice', 'x', 'normal', 0, 0)`)

	_, err := db.W().ExecContext(ctx,
		`INSERT INTO user_nodes (user_id, logical_path, node_type, file_checksum, name, parent_path, created_at)
		 VALUES (1, '/f', 1, 'somechecksum', 'f', '/', 0)`)
	if err == nil {
		t.Fatal("文件夹携带校验码应被 CHECK 约束拒绝")
	}

	// 文件节点必须指向存在的内容池行（外键）。
	_, err = db.W().ExecContext(ctx,
		`INSERT INTO user_nodes (user_id, logical_path, node_type, file_checksum, name, parent_path, created_at)
		 VALUES (1, '/a.txt', 0, 'not-in-files', 'a.txt', '/', 0)`)
	if err == nil {
		t.Fatal("文件节点指向不存在的内容池行应被外键拒绝")
	}

	mustExec(t, db, `INSERT INTO files (checksum, size_plain, pan_object_name, pan_size_wire,
	                 enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id, status, created_at, updated_at)
	                 VALUES ('ck1', 10, '2026/09/21/x.xph', 90, 'AES-256-GCM', 20, 1, x'00', x'00', 'k1', 1, 0, 0)`)
	mustExec(t, db, `INSERT INTO user_nodes (user_id, logical_path, node_type, file_checksum, name, parent_path, created_at)
	                 VALUES (1, '/a.txt', 0, 'ck1', 'a.txt', '/', 0)`)
	mustExec(t, db, `INSERT INTO user_nodes (user_id, logical_path, node_type, name, parent_path, created_at)
	                 VALUES (1, '/docs', 1, 'docs', '/', 0)`)

	// 同一用户同一路径不可重复；不同用户互不影响。
	if _, err := db.W().ExecContext(ctx,
		`INSERT INTO user_nodes (user_id, logical_path, node_type, name, parent_path, created_at)
		 VALUES (1, '/docs', 1, 'docs', '/', 0)`); err == nil {
		t.Fatal("同一用户的同一逻辑路径应重复插入失败")
	}
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
	                 VALUES (2, 'bob', 'x', 'normal', 0, 0)`)
	mustExec(t, db, `INSERT INTO user_nodes (user_id, logical_path, node_type, name, parent_path, created_at)
	                 VALUES (2, '/docs', 1, 'docs', '/', 0)`)
}

// TestQuotaCounterAtomicReserve 验证预扣的条件更新语义：并发下不得击穿。
func TestQuotaCounterAtomicReserve(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const limit = 100

	reserve := func(n int64) bool {
		res, err := db.W().ExecContext(ctx, `
			INSERT INTO quota_counters (scope, key, used, updated_at) VALUES ('storage', 'user:1', ?, 0)
			ON CONFLICT(scope, key) DO UPDATE SET used = used + excluded.used, updated_at = excluded.updated_at
			WHERE quota_counters.used + excluded.used <= ?`, n, limit)
		if err != nil {
			t.Fatalf("预扣失败: %v", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("读取影响行数失败: %v", err)
		}
		return affected > 0
	}

	if !reserve(60) {
		t.Fatal("首次预扣 60 应成功")
	}
	if reserve(50) {
		t.Fatal("预扣 50 会超出上限 100，应被拒绝")
	}
	if !reserve(40) {
		t.Fatal("预扣 40 恰好达到上限，应成功")
	}
	if reserve(1) {
		t.Fatal("已达上限后任何预扣都应失败")
	}

	var used int64
	if err := db.R().QueryRowContext(ctx,
		`SELECT used FROM quota_counters WHERE scope = 'storage' AND key = 'user:1'`).Scan(&used); err != nil {
		t.Fatalf("读取计数器失败: %v", err)
	}
	if used != limit {
		t.Fatalf("已用量=%d，期望 %d", used, limit)
	}
}

// TestInviteCodeAtomicConsume 邀请码用量必须原子递增，天然防超发。
func TestInviteCodeAtomicConsume(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO invite_codes (code_hash, code_hint, group_name, max_uses, created_at)
	                 VALUES ('h1', 'AB12', 'normal', 2, 0)`)

	consume := func() bool {
		res, err := db.W().ExecContext(ctx, `
			UPDATE invite_codes SET used_count = used_count + 1
			WHERE code_hash = 'h1' AND used_count < max_uses
			  AND disabled = 0 AND (expires_at = 0 OR expires_at > ?)`, Now())
		if err != nil {
			t.Fatalf("消费邀请码失败: %v", err)
		}
		n, _ := res.RowsAffected()
		return n > 0
	}

	// 两次调用各消费一次名额，副作用不同，分开断言以明确语义。
	if !consume() {
		t.Fatal("第一次消费应成功")
	}
	if !consume() {
		t.Fatal("第二次消费应成功")
	}
	if consume() {
		t.Fatal("超出 max_uses 后应失败")
	}
}

func TestBitHelpers(t *testing.T) {
	mask := make([]byte, BitmapBytes(10))
	for _, i := range []int{0, 3, 7, 8, 9} {
		mask = SetBit(mask, i)
	}
	for _, i := range []int{0, 3, 7, 8, 9} {
		if !HasBit(mask, i) {
			t.Errorf("第 %d 位应为已置位", i)
		}
	}
	for _, i := range []int{1, 2, 4, 5, 6} {
		if HasBit(mask, i) {
			t.Errorf("第 %d 位应为未置位", i)
		}
	}
	if HasBit(mask, 10) {
		t.Error("越界位应返回 false")
	}
	mask = SetBit(mask, 100)
	if !HasBit(mask, 100) {
		t.Error("超出初始容量的位置应能自动扩容")
	}
}

// TestThrottleKeyIsDimensionFree 退避按任意键维度记录，因此"同一 IP 刷大量
// 账号"这一维度无法被账号维度的退避绕过。
func TestThrottleKeyIsDimensionFree(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, key := range []string{"ip:10.0.0.1", "account:alice", "share:abc", "pickup:XY7Z"} {
		for i := 0; i < 2; i++ {
			if _, err := db.W().ExecContext(ctx,
				`INSERT INTO throttle (key, fail_count, updated_at) VALUES (?, 1, 0)
				 ON CONFLICT(key) DO UPDATE SET fail_count = fail_count + 1, updated_at = excluded.updated_at`,
				key); err != nil {
				t.Fatalf("写入退避状态失败(%s): %v", key, err)
			}
		}
	}
	var n int
	if err := db.R().QueryRowContext(ctx, `SELECT fail_count FROM throttle WHERE key = 'ip:10.0.0.1'`).Scan(&n); err != nil {
		t.Fatalf("读取退避状态失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("同键累加应得 2，实得 %d", n)
	}
	var keys int
	if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM throttle`).Scan(&keys); err != nil {
		t.Fatalf("统计退避键失败: %v", err)
	}
	if keys != 4 {
		t.Fatalf("不同维度的键应各自独立，实得 %d 个", keys)
	}
}

func mustExec(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.W().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("执行失败: %v\n语句: %s", err, query)
	}
}

// TestGetFileStatusesBatch 批量状态查询：一次取回整批，且不把查不到的
// 校验码当成错误（调用方要把"缺失"和"禁用"区分开）。
func TestGetFileStatusesBatch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ins := func(ck string, status int, reason string) {
		mustExec(t, db, `INSERT INTO files (checksum, size_plain, pan_object_name, pan_size_wire,
		                 enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id,
		                 status, disable_reason, created_at, updated_at)
		                 VALUES (?, 10, 'p/' || ? , 90, 'AES-256-GCM', 20, 1, x'00', x'00', 'k1', ?, ?, 0, 0)`,
			ck, ck, status, reason)
	}
	ins("ck-normal", int(FileNormal), "")
	ins("ck-disabled", int(FileDisabled), "违规内容")

	// 空入参必须直接返回空 map，不能拼出非法的 `IN ()`。
	empty, err := GetFileStatuses(ctx, db.R(), nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空 map: %v %+v", err, empty)
	}

	got, err := GetFileStatuses(ctx, db.R(),
		[]string{"ck-normal", "ck-disabled", "ck-absent"})
	if err != nil {
		t.Fatalf("批量查询失败: %v", err)
	}
	if got["ck-normal"].Status != FileNormal || got["ck-normal"].DisableReason != "" {
		t.Fatalf("正常对象状态不符: %+v", got["ck-normal"])
	}
	if got["ck-disabled"].Status != FileDisabled ||
		got["ck-disabled"].DisableReason != "违规内容" {
		t.Fatalf("禁用对象状态不符: %+v", got["ck-disabled"])
	}
	// 查不到的不进 map：由调用方按"内容池记录缺失"处理。
	if _, ok := got["ck-absent"]; ok {
		t.Fatal("不存在的校验码不应出现在结果里")
	}
	if len(got) != 2 {
		t.Fatalf("应只返回 2 行，得 %d", len(got))
	}
}
