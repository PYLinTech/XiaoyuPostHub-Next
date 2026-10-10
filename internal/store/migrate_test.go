package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func testContext() context.Context { return context.Background() }

// schemaSnapshot 把库文件里的结构对象取出来，作为两个库可比对的指纹。
//
// 只取 type/name/sql 三列且排除 sqlite_ 开头的内部表：内容一致才说明
// "这两个库长得一样"，而 sql 文本里 CREATE IF NOT EXISTS 与 CREATE 的
// 写法差异不该被当成结构差异——SQLite 存的是原始语句，两条建库路径
// （schema.sql 直建 vs 迁移累积）写法本就不同，比文本会永远报错。
func schemaSnapshot(t *testing.T, db *DB) map[string]string {
	t.Helper()
	rows, err := db.read.Query(`
		SELECT type, name FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("读取结构失败: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("扫描结构失败: %v", err)
		}
		// 逐对象比对列与索引定义，这才是"结构一致"的实质。
		out[typ+" "+name] = objectFingerprint(t, db, typ, name)
	}
	return out
}

// objectFingerprint 把一个表/索引/视图的规范化定义取成字符串。
func objectFingerprint(t *testing.T, db *DB, typ, name string) string {
	t.Helper()
	switch typ {
	case "table":
		return tableFingerprint(t, db, name)
	case "index":
		var sqlText sql.NullString
		if err := db.read.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, name,
		).Scan(&sqlText); err != nil {
			t.Fatalf("读取索引 %s 失败: %v", name, err)
		}
		return normalizeSQL(sqlText.String)
	default:
		var sqlText sql.NullString
		if err := db.read.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type=? AND name=?`, typ, name,
		).Scan(&sqlText); err != nil {
			t.Fatalf("读取 %s %s 失败: %v", typ, name, err)
		}
		return normalizeSQL(sqlText.String)
	}
}

// tableFingerprint 汇总一张表的列定义、外键与索引，忽略书写格式差异。
func tableFingerprint(t *testing.T, db *DB, table string) string {
	t.Helper()
	var b []byte
	cols, err := db.read.Query(fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table)))
	if err != nil {
		t.Fatalf("读取表 %s 的列失败: %v", table, err)
	}
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫描列失败: %v", err)
		}
		b = append(b, fmt.Sprintf("col %s %s notnull=%d pk=%d;",
			name, strings.ToUpper(ctype), notnull, pk)...)
	}
	cols.Close()

	fks, err := db.read.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", quoteIdent(table)))
	if err != nil {
		t.Fatalf("读取表 %s 的外键失败: %v", table, err)
	}
	for fks.Next() {
		var id, seq int
		var target, from, to string
		var onUpdate, onDelete, match string
		if err := fks.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("扫描外键失败: %v", err)
		}
		b = append(b, fmt.Sprintf("fk %s.%s->%s; ", from, to, target)...)
	}
	fks.Close()

	idx, err := db.read.Query(fmt.Sprintf("PRAGMA index_list(%s)", quoteIdent(table)))
	if err != nil {
		t.Fatalf("读取表 %s 的索引失败: %v", table, err)
	}
	for idx.Next() {
		var seqNo int
		var iname, origin string
		var unique, partial int
		if err := idx.Scan(&seqNo, &iname, &unique, &origin, &partial); err != nil {
			t.Fatalf("扫描索引失败: %v", err)
		}
		b = append(b, fmt.Sprintf("idx %s unique=%d; ", iname, unique)...)
	}
	idx.Close()
	return string(b)
}

// normalizeSQL 压掉空白差异，让"写法不同但结构相同"不被误判为结构差异。
func normalizeSQL(s string) string {
	var out []rune
	space := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			space = true
			continue
		}
		if space && len(out) > 0 {
			out = append(out, ' ')
		}
		space = false
		out = append(out, r)
	}
	return strings.ToUpper(string(out))
}

// rewindToBaselineSchema 把全新库降回已发布的 v2 结构，供测试完整上传平台迁移。
func rewindToBaselineSchema(t *testing.T, db *DB) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE shares DROP COLUMN show_sharer_name`,
		`ALTER TABLE upload_tasks DROP COLUMN expected_checksum`,
		`DROP TABLE upload_job_parts`,
		`DROP TABLE upload_stream_state`,
		`DROP TABLE upload_staging_chunks`,
		`DROP TABLE upload_staging_reservations`,
		`ALTER TABLE upload_tasks DROP COLUMN volume_size`,
		`ALTER TABLE upload_tasks DROP COLUMN streaming`,
		`DROP INDEX idx_upload_jobs_queue`,
		`DROP INDEX idx_upload_jobs_user_state`,
		`DROP TABLE upload_jobs`,
		`ALTER TABLE user_groups DROP COLUMN resource_scheduling_priority`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := db.write.Exec(stmt); err != nil {
			t.Fatalf("回退到 v2 结构失败 (%s): %v", stmt, err)
		}
	}
}

// TestMigrationFromBaseline 把带有基线业务数据的旧库升到当前版本。
//
// 测试先按终版 schema 建库并写入业务数据，再降为真实 v2 结构，验证当前迁移
// 能在有数据的库上运行且不会动到既有数据。
func TestMigrationFromBaseline(t *testing.T) {
	db := openTestDB(t) // 全新库，已是当前版本

	// 塞一点业务数据，证明迁移前后数据不受影响。
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, is_builtin, permissions, priority, created_at)
		VALUES ('normal', '普通用户', 1, 32831, 100, 1)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
		VALUES (1, 'u1', 'x', 'normal', 1, 1)`)
	mustExec(t, db, `INSERT INTO mail_domains (domain, created_at) VALUES ('pylin.cn', 1)`)
	if err := CreateUploadTask(context.Background(), db.W(), UploadTask{
		ID: "legacy-upload", UserID: 1, Checksum: "legacy-checksum", SizePlain: 1024,
		ChunkSize: 512, ChunkTotal: 2, ReceivedMask: make([]byte, BitmapBytes(2)),
		TargetParentPath: "/", TargetName: "legacy.bin", ConflictAction: "rename",
		ExpiresAt: ToTime(Now() + 3600),
	}); err != nil {
		t.Fatalf("创建迁移前上传会话失败: %v", err)
	}

	// 回退到已发布的 v2 结构，模拟已有站点升级。
	rewindToBaselineSchema(t, db)
	if err := db.initSchema(); err != nil {
		t.Fatalf("从 v2 升级失败: %v", err)
	}

	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Fatalf("升级后版本应为 %d，得 %d", currentSchemaVersion, got)
	}
	var groups, domains, uploads int
	mustQuery(t, db, `SELECT COUNT(*) FROM user_groups`, &groups)
	mustQuery(t, db, `SELECT COUNT(*) FROM mail_domains`, &domains)
	mustQuery(t, db, `SELECT COUNT(*) FROM upload_tasks WHERE id = 'legacy-upload'`, &uploads)
	if groups != 1 || domains != 1 || uploads != 1 {
		t.Fatalf("迁移不应动既有数据，groups=%d domains=%d uploads=%d", groups, domains, uploads)
	}
	var stagingReservation int64
	if err := db.read.QueryRow(`SELECT bytes FROM upload_staging_reservations WHERE session_id = 'legacy-upload' AND kind = 'legacy'`).Scan(&stagingReservation); err != nil {
		t.Fatalf("读取旧上传暂存预留失败: %v", err)
	}
	if stagingReservation != 2144 {
		t.Fatalf("旧上传暂存预留应覆盖明文与密文开销，得 %d", stagingReservation)
	}
	var resourcePriority int
	if err := db.read.QueryRow(`SELECT resource_scheduling_priority FROM user_groups WHERE name = 'normal'`).Scan(&resourcePriority); err != nil {
		t.Fatalf("读取迁移后资源调度优先级失败: %v", err)
	}
	if resourcePriority != 0 {
		t.Fatalf("迁移后资源调度优先级应默认 0，得 %d", resourcePriority)
	}
}

// TestSchemaMatchesMigrated 是整套机制里最重要的一条：
// 「从基线升上来的库」必须与「全新直建的库」结构完全一致。
//
// 同一段 DDL 要在 schema.sql 与 migrations/ 两处各写一份，迟早会漂移。
// 漂移不会让程序启动失败，只会让某个特定查询返回错结果——那时已经上线了。
// 这条测试把漂移挡在提交之前。
func TestSchemaMatchesMigrated(t *testing.T) {
	fresh := openTestDB(t) // v=0 直建终版

	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	mig, err := Open(migratedPath)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer mig.Close()
	// 降为已发布的 v2 结构，让第二次 initSchema 真正走迁移路径。
	rewindToBaselineSchema(t, mig)
	if err := mig.initSchema(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	freshSnap := schemaSnapshot(t, fresh)
	migratedSnap := schemaSnapshot(t, mig)
	assertSameSchema(t, freshSnap, migratedSnap)

	// 终版结构里的每张表，两个库都得有。
	for key := range freshSnap {
		if _, ok := migratedSnap[key]; !ok {
			t.Errorf("迁移后缺少对象: %s", key)
		}
	}
}

// TestNewerDBRejected 库比程序新时必须拒绝启动。
//
// 这是 fail fast 的核心：让旧代码对着新结构跑，它会在缺列上安静地失败，
// 而不是明确地拒绝启动。宁可起不来，也不要起来之后损坏数据。
func TestNewerDBRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.write.Exec(fmt.Sprintf("PRAGMA user_version = %d",
		currentSchemaVersion+1)); err != nil {
		t.Fatalf("抬高版本失败: %v", err)
	}
	if err := db.initSchema(); err == nil {
		t.Fatalf("库版本高于代码时应当拒绝启动，却成功了")
	}
	// 拒绝启动不能顺手把版本改回去：那会让下次启动悄悄"通过"。
	if got := mustUserVersion(t, db); got != currentSchemaVersion+1 {
		t.Fatalf("被拒绝后版本不应被改动，应仍为 %d，得 %d", currentSchemaVersion+1, got)
	}
}

// TestMigrationRollback 中途失败必须回滚，不能留下建了一半的库。
//
// 这是"可回退"的全部含义：失败的迁移要把库还原到迁移前，而不是留一个
// 上不去也下不来的中间态。SQLite 的 DDL 是事务性的，所以回滚是能做到的。
func TestMigrationRollback(t *testing.T) {
	db := openTestDB(t)

	// 一个"建了张表、又插了行、最后失败"的迁移：
	// 目标是证明连已写入的表和数据都一并消失。
	bad := migration{
		Version: currentSchemaVersion,
		Name:    "rollback_probe",
		SQL: `CREATE TABLE rollback_probe (id INTEGER PRIMARY KEY);
		      INSERT INTO rollback_probe (id) VALUES (1);
		      CREATE TABLE definitely_not_valid_syntax (`,
	}
	if err := db.applyMigration(testContext(), bad); err == nil {
		t.Fatalf("预期迁移失败，却成功了")
	}

	// 回滚后：那两张表都不该存在，版本号也不该动。
	for _, tbl := range []string{"rollback_probe", "definitely_not_valid_syntax"} {
		var n int
		mustQuery(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, &n, tbl)
		if n != 0 {
			t.Errorf("回滚后表 %s 仍存在", tbl)
		}
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Fatalf("回滚后版本号不应改变，应仍为 %d，得 %d", currentSchemaVersion, got)
	}
}

// TestLoadMigrationsValidatesRegistry 迁移清单自身要能查出问题。
//
// 注册表与文件目录对不上（例如提交时漏了一个 .sql）如果不在启动时暴露，
// 就要等某个用户升级到那一代才炸——那时服务已经停不下来了。
func TestLoadMigrationsValidatesRegistry(t *testing.T) {
	got, err := loadMigrations()
	if err != nil {
		t.Fatalf("加载迁移清单失败: %v", err)
	}
	if len(got) != len(migrations) {
		t.Fatalf("加载出 %d 条，注册表有 %d 条", len(got), len(migrations))
	}
	for _, m := range got {
		if m.SQL == "" {
			t.Errorf("迁移 %d（%s）没有读到 SQL", m.Version, m.Name)
		}
		if m.Version <= 1 {
			t.Errorf("迁移版本号必须大于基线 1，实为 %d", m.Version)
		}
	}
}

func mustUserVersion(t *testing.T, db *DB) int {
	t.Helper()
	var v int
	if err := db.write.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("读取版本号失败: %v", err)
	}
	return v
}

func mustQuery(t *testing.T, db *DB, query string, dest any, args ...any) {
	t.Helper()
	if err := db.read.QueryRow(query, args...).Scan(dest); err != nil {
		t.Fatalf("查询失败 (%s): %v", query, err)
	}
}

func assertSameSchema(t *testing.T, want, got map[string]string) {
	t.Helper()
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("缺少对象 %s", key)
			continue
		}
		if w != g {
			t.Errorf("对象 %s 定义不一致:\n  期望 %s\n  实得 %s", key, w, g)
		}
	}
}
