package store

import (
	"context"
	"strings"
	"testing"
)

// TestFreshDatabaseAtCurrentVersion 全新库直接落在当前版本，邮件表齐全、
// traffic_logs 用的是新 CHECK。
func TestFreshDatabaseAtCurrentVersion(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var version int
	if err := db.R().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读取 user_version 失败: %v", err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("全新库版本 = %d，应为 %d", version, currentSchemaVersion)
	}

	for _, table := range []string{
		"mail_domains", "mail_addresses", "mail_messages",
		"mail_message_recipients", "mail_parts", "mailboxes",
	} {
		var name string
		err := db.R().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Fatalf("邮件表 %s 未创建: %v", table, err)
		}
	}

	if _, err := db.W().ExecContext(ctx, `
		INSERT INTO traffic_logs (actor_type, user_id, group_name, action, occurred_at)
		VALUES ('user', 1, 'normal', 'mail_send', 100)`); err != nil {
		t.Fatalf("新库应允许 mail_send 动作: %v", err)
	}
	if _, err := db.W().ExecContext(ctx, `
		INSERT INTO traffic_logs (actor_type, user_id, group_name, action, occurred_at)
		VALUES ('user', 1, 'normal', 'bogus', 100)`); err == nil {
		t.Fatal("非法 action 应被 CHECK 拒绝")
	}
}

// TestSchemaInitIsIdempotentAndStateless 覆盖 initSchema 现在唯一的契约：
// 执行最终结构脚本 + 戳版本，没有分支、没有迁移、没有版本判断。
// 同一个库文件反复打开都应成功，版本始终落在 currentSchemaVersion 上，
// 缺失的对象被幂等补齐——这是"重复执行 CREATE ... IF NOT EXISTS"的结果，
// 不是任何兼容逻辑。
func TestSchemaInitIsIdempotentAndStateless(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 连开三次：每次都应成功，且版本号稳定。
	for i := 0; i < 3; i++ {
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次重开应成功（schema.sql 幂等）: %v", i+1, err)
		}
		var version int
		if err := reopened.R().QueryRowContext(ctx,
			"PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("第 %d 次读取版本失败: %v", i+1, err)
		}
		if version != currentSchemaVersion {
			t.Fatalf("第 %d 次重开后版本 = %d，应为 %d", i+1, version, currentSchemaVersion)
		}
		// 缺索引时应被幂等补建——这正是"不写版本分支"的代价与好处。
		var idxSQL string
		if err := reopened.R().QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_mailboxes_purge'`).Scan(&idxSQL); err != nil {
			t.Fatalf("第 %d 次重开后归档索引应存在: %v", i+1, err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("第 %d 次关库失败: %v", i+1, err)
		}
	}

	// 删掉一个对象后重开：应被补回来，证明恢复能力来自 schema.sql 而非
	// 任何针对特定版本的修补分支。
	setup, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.W().ExecContext(ctx, `DROP INDEX idx_mailboxes_purge`); err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	healed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = healed.Close() })
	var n int
	if err := healed.R().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_mailboxes_purge'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("缺失索引应在重开时被幂等补建")
	}
}

// TestMailMessageIDUnique 非空 Message-ID 全表唯一（入站幂等的库级保证）；
// 空 Message-ID 不受唯一索引约束。
func TestMailMessageIDUnique(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insert := func(id, messageID string) error {
		_, err := db.W().ExecContext(ctx, `
		INSERT INTO mail_messages (id, message_id, created_at)
		VALUES (?, ?, 1)`, id, messageID)
		return err
	}
	if err := insert("m1", ""); err != nil {
		t.Fatalf("空 Message-ID 应成功: %v", err)
	}
	if err := insert("m2", ""); err != nil {
		t.Fatalf("第二封空 Message-ID 应成功: %v", err)
	}
	if err := insert("m3", "<a@example.com>"); err != nil {
		t.Fatalf("非空 Message-ID 首插应成功: %v", err)
	}
	// 入站幂等：同一 Message-ID 第二封必须被唯一索引拒绝。
	if err := insert("m4", "<a@example.com>"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("重复非空 Message-ID 应触发唯一冲突，实际: %v", err)
	}
}
