package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newOpsTestDB 单独建一个临时库，不复用 store_test.go 里的 helper：
// ops 这一组测试要独占一个空库来断言"表清单包含哪些表"，共用一个全局库
// 会让断言随其它测试的写入而漂移。
func newOpsTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedOpsTestData 写入最小可用数据，让行数统计有非零值可断言。
// 先建组再建用户：foreign_keys 是打开的，顺序错了会直接报外键错误。
func seedOpsTestData(t *testing.T, db *DB) {
	t.Helper()
	now := Now()
	if _, err := db.W().Exec(`
		INSERT INTO user_groups (name, display_name, is_builtin, permissions, priority, created_at)
		VALUES ('ops_test', 'Ops Test', 0, 0, 0, ?)`, now); err != nil {
		t.Fatalf("写入用户组失败: %v", err)
	}
	if _, err := db.W().Exec(`
		INSERT INTO users (account, display_name, password_hash, group_name, status, created_at, updated_at)
		VALUES ('ops_test_user', '', 'x', 'ops_test', 1, ?, ?)`, now, now); err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
}

// opsTableRows 把 Tables 还原成"表名 -> 行数"，方便断言；
// 被跳过统计的表在 Name 上带标记，这里剥掉标记再入表。
func opsTableRows(stats []TableStat) map[string]int64 {
	out := make(map[string]int64, len(stats))
	for _, s := range stats {
		name := strings.TrimSuffix(strings.TrimSpace(s.Name), " (skipped)")
		out[name] = s.Rows
	}
	return out
}

func TestOpsStats(t *testing.T) {
	db := newOpsTestDB(t)
	seedOpsTestData(t, db)
	ctx := context.Background()

	got, err := db.Stats(ctx, true)
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}

	if got.PageSize <= 0 {
		t.Errorf("PageSize 应大于 0，实际 %d", got.PageSize)
	}
	if got.PageCount <= 0 {
		t.Errorf("PageCount 应大于 0，实际 %d", got.PageCount)
	}
	if got.InUseBytes <= 0 {
		t.Errorf("InUseBytes 应大于 0，实际 %d", got.InUseBytes)
	}
	if got.FreeBytes != got.FreelistCount*got.PageSize {
		t.Errorf("FreeBytes 与 freelistCount*pageSize 不一致: %d vs %d",
			got.FreeBytes, got.FreelistCount*got.PageSize)
	}
	if got.FileBytes <= 0 {
		t.Errorf("FileBytes 应大于 0（库文件已存在），实际 %d", got.FileBytes)
	}
	if got.Path != db.Path() {
		t.Errorf("Path 不一致: %q vs %q", got.Path, db.Path())
	}
	if !strings.EqualFold(got.JournalMode, "wal") {
		t.Errorf("JournalMode 应为 wal，实际 %q", got.JournalMode)
	}
	if !got.ForeignKeys {
		t.Error("ForeignKeys 应为 true（DSN 里显式开启）")
	}
	if got.BusyTimeout <= 0 {
		t.Errorf("BusyTimeout 应大于 0，实际 %d", got.BusyTimeout)
	}
	if got.AutoVacuum == "" {
		t.Error("AutoVacuum 不应为空")
	}

	rows := opsTableRows(got.Tables)
	for _, want := range []string{"files", "users", "shares", "user_groups"} {
		if _, ok := rows[want]; !ok {
			t.Errorf("Tables 缺少表 %q，实际表: %v", want, rows)
		}
	}
	if n := rows["users"]; n != 1 {
		t.Errorf("users 行数应为 1，实际 %d", n)
	}
	if n := rows["files"]; n != 0 {
		t.Errorf("files 行数应为 0，实际 %d", n)
	}

	// includeTables=false 时不应付逐表统计的代价。
	light, err := db.Stats(ctx, false)
	if err != nil {
		t.Fatalf("Stats(false) 失败: %v", err)
	}
	if len(light.Tables) != 0 {
		t.Errorf("includeTables=false 时 Tables 应为空，实际 %d 项", len(light.Tables))
	}
	if light.PageSize != got.PageSize {
		t.Errorf("两次 Stats 的 PageSize 应一致: %d vs %d", light.PageSize, got.PageSize)
	}
}

func TestOpsIntegrityCheck(t *testing.T) {
	db := newOpsTestDB(t)
	seedOpsTestData(t, db)
	ctx := context.Background()

	for _, quick := range []bool{true, false} {
		problems, err := db.IntegrityCheck(ctx, quick)
		if err != nil {
			t.Fatalf("IntegrityCheck(quick=%v) 失败: %v", quick, err)
		}
		if len(problems) != 0 {
			t.Errorf("IntegrityCheck(quick=%v) 应无问题，实际: %v", quick, problems)
		}
	}
}

func TestOpsBackup(t *testing.T) {
	db := newOpsTestDB(t)
	seedOpsTestData(t, db)
	ctx := context.Background()

	// 目标目录故意不存在，顺便验证 Backup 会自己建目录。
	dest := filepath.Join(t.TempDir(), "snapshots", "snap.db")

	n, err := db.Backup(ctx, dest)
	if err != nil {
		t.Fatalf("Backup 失败: %v", err)
	}
	if n <= 0 {
		t.Errorf("备份字节数应大于 0，实际 %d", n)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	if info.Size() != n {
		t.Errorf("返回字节数 %d 与文件实际大小 %d 不一致", n, info.Size())
	}
	st, err := db.Stats(ctx, false)
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}
	if n < st.PageSize {
		t.Errorf("备份文件 %d 字节，小于一个页 %d 字节，明显不完整", n, st.PageSize)
	}

	// VACUUM INTO 拒绝覆盖已存在的文件，这是它自身的语义，不能悄悄变成覆盖。
	if _, err := db.Backup(ctx, dest); err == nil {
		t.Fatal("对已存在的目标再次备份应当报错")
	}

	if _, err := db.Backup(ctx, "  "); err == nil {
		t.Fatal("空路径应当报错")
	}
}

func TestOpsOptimize(t *testing.T) {
	db := newOpsTestDB(t)
	seedOpsTestData(t, db)
	ctx := context.Background()

	got, err := db.Optimize(ctx, 0)
	if err != nil {
		t.Fatalf("Optimize 失败: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Optimize 结果不应为空")
	}
	if _, ok := got["optimize"]; !ok {
		t.Errorf("Optimize 结果缺少 optimize 键，实际: %v", got)
	}
	if _, ok := got["optimize"]; ok && !strings.HasPrefix(got["optimize"], "ok") &&
		!strings.Contains(got["optimize"], "skipped") {
		t.Errorf("optimize 结果既非成功也非跳过: %q", got["optimize"])
	}
	// vacuumPages<=0 必须明确记为 skipped，而不是假装回收过。
	if v := got["incremental_vacuum"]; !strings.HasPrefix(v, "skipped") {
		t.Errorf("vacuumPages=0 时 incremental_vacuum 应标记 skipped，实际 %q", v)
	}
	if _, ok := got["wal_checkpoint"]; !ok {
		t.Errorf("Optimize 结果缺少 wal_checkpoint 键，实际: %v", got)
	}

	// 带上页数再跑一次：默认库是 auto_vacuum=none，应当如实上报为 skipped
	// 而不是谎报回收成功。
	got2, err := db.Optimize(ctx, 64)
	if err != nil {
		t.Fatalf("Optimize(64) 失败: %v", err)
	}
	if v := got2["incremental_vacuum"]; !strings.HasPrefix(v, "ok") && !strings.HasPrefix(v, "skipped") {
		t.Errorf("incremental_vacuum 应为 ok 或 skipped，实际 %q", v)
	}
}

func TestOpsCheckpointWAL(t *testing.T) {
	db := newOpsTestDB(t)
	seedOpsTestData(t, db)
	ctx := context.Background()

	if err := db.CheckpointWAL(ctx, false); err != nil {
		t.Fatalf("CheckpointWAL(PASSIVE) 失败: %v", err)
	}
	if err := db.CheckpointWAL(ctx, true); err != nil {
		t.Fatalf("CheckpointWAL(TRUNCATE) 失败: %v", err)
	}
	// 截断后应把 -wal 收回 0（若它存在的话）。
	if n := fileSizeOrZero(db.Path() + "-wal"); n != 0 {
		t.Errorf("TRUNCATE 检查点后 WAL 应为 0 字节，实际 %d", n)
	}
}

func TestOpsLockErrorDetection(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"database is locked", true},
		{"SQLITE_BUSY: database is locked", true},
		{"database table is locked: users", true},
		{"no such table: nope", false},
	}
	for _, c := range cases {
		if got := isLockedErr(errString(c.msg)); got != c.want {
			t.Errorf("isLockedErr(%q) = %v，期望 %v", c.msg, got, c.want)
		}
	}
	if isLockedErr(nil) {
		t.Error("isLockedErr(nil) 应为 false")
	}
}

// errString 是最小的 error 实现，避免为一条断言引入 errors.New 之外的东西。
type errString string

func (e errString) Error() string { return string(e) }
