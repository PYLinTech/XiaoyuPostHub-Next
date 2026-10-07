package store

import (
	"context"
	"database/sql"
	"testing"
)

// 全站文件检索的回归测试。
//
// 重点是 LEFT JOIN 的那条边界：文件夹在内容池里没有行，状态过滤如果
// 直接打在 f.status 上，会把所有文件夹一起筛掉——而"只看已停用对象"
// 本来就不该把文件夹算进来，可"不加任何过滤"时文件夹必须正常出现。

func seedAdminNodes(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('g1', '组一', 1)`)
	uid, err := CreateUser(ctx, db.W(), User{
		Account: "alice", DisplayName: "Alice", PasswordHash: "h",
		GroupName: "g1", Status: UserEnabled, CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	uid2, err := CreateUser(ctx, db.W(), User{
		Account: "bob", DisplayName: "Bob", PasswordHash: "h",
		GroupName: "g1", Status: UserEnabled, CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 用命名参数而不是混排 ?：SQLite 允许 ?NNN 复用位置，VALUES 里连续的
	// 字面 1 会被当成 ?1 占位符，导致 created_by 静默取错值（created_by=0
	// 传 0 时，前面的 ref_count=1 正好顶上了 ?1，测试会以"拆分不生效"的形式
	// 失败，而不是报错，极难定位）。
	insFile := func(ck string, status int, size int64, createdBy int64) {
		mustExec(t, db, `INSERT INTO files (
		                 checksum, size_plain, pan_object_name, pan_size_wire,
		                 enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt,
		                 dek_envelope, kek_key_id,
		                 status, ref_count, created_by, created_at, updated_at)
			VALUES (:ck, :size, 'p/' || :ck, 90, 'AES-256-GCM', 20, 1,
		                x'00', x'00', 'k1', :status, 1, :created_by, 1, 1)`,
			sql.Named("ck", ck), sql.Named("size", size),
			sql.Named("status", status), sql.Named("created_by", createdBy))
	}
	insFile("ck-report", int(FileNormal), 500, 1)
	insFile("ck-secret", int(FileDisabled), 900, 0)

	// 文件夹没有 file_checksum（表上的 CHECK 约束会拒绝）。
	mustExec(t, db, `INSERT INTO user_nodes (user_id, logical_path, node_type, name, parent_path,
	                 size_plain, mtime, created_at)
		VALUES (?, '/docs', 1, 'docs', '/', 0, 200, 1)`, uid)

	insNode := func(u int64, path, name, parent, ck string, mtime int64) {
		var checksum any
		if ck != "" {
			checksum = ck
		}
		mustExec(t, db, `INSERT INTO user_nodes (user_id, logical_path, node_type, file_checksum,
		                 name, parent_path, size_plain, mtime, created_at)
			VALUES (?, ?, 0, ?, ?, ?, 500, ?, 1)`, u, path, checksum, name, parent, mtime)
	}
	insNode(uid, "/docs/2026年度报告.pdf", "2026年度报告.pdf", "/docs", "ck-report", 300)
	insNode(uid, "/secret.txt", "secret.txt", "/", "ck-secret", 100)
	// 同一个对象被第二个用户引用：检索按节点走，两条都要出现。
	insNode(uid2, "/copy-of-report.pdf", "copy-of-report.pdf", "/", "ck-report", 250)
}

func TestListAllNodesNoFilter(t *testing.T) {
	db := openTestDB(t)
	seedAdminNodes(t, db)

	items, total, err := ListAllNodes(context.Background(), db.R(), AdminNodeFilter{}, 50, 0)
	if err != nil {
		t.Fatalf("全站检索失败: %v", err)
	}
	// 1 个文件夹 + 3 个文件节点。
	if total != 4 || len(items) != 4 {
		t.Fatalf("应返回 4 条，实际 total=%d len=%d: %+v", total, len(items), items)
	}
	// 按 mtime 降序：300 / 250 / 200 / 100。
	if items[0].Name != "2026年度报告.pdf" {
		t.Fatalf("应按修改时间倒序，实际首行 %q", items[0].Name)
	}
	// 文件夹在内容池里没有行，状态必须是"不适用"而不是 0（上传中）。
	for _, it := range items {
		if it.NodeType == "folder" {
			if it.FileStatus != int(FileUnknown) {
				t.Fatalf("文件夹状态应为不适用，实际 %d", it.FileStatus)
			}
			if it.Checksum != "" {
				t.Fatalf("文件夹不应带校验码，实际 %q", it.Checksum)
			}
		}
	}
}

func TestListAllNodesByNameAndPath(t *testing.T) {
	db := openTestDB(t)
	seedAdminNodes(t, db)
	ctx := context.Background()

	// 文件名中间片段命中。
	items, total, err := ListAllNodes(ctx, db.R(), AdminNodeFilter{Query: "报告"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || items[0].Name != "2026年度报告.pdf" {
		t.Fatalf("按文件名片段检索失败: total=%d %+v", total, items)
	}

	// 只给路径片段也应命中——管理员手上通常只有目录片段。
	items, total, err = ListAllNodes(ctx, db.R(), AdminNodeFilter{Query: "/docs"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("按路径片段应命中 2 条，实际 %d: %+v", total, items)
	}

	// LIKE 通配符必须被转义，否则 % 会匹配全表。
	items, total, err = ListAllNodes(ctx, db.R(), AdminNodeFilter{Query: "%"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("%% 应被当作普通字符，实际命中 %d 条", total)
	}
}

func TestListAllNodesStatusFilterExcludesFolders(t *testing.T) {
	db := openTestDB(t)
	seedAdminNodes(t, db)
	ctx := context.Background()

	disabled := FileDisabled
	items, total, err := ListAllNodes(ctx, db.R(), AdminNodeFilter{FileStatus: &disabled}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || items[0].Name != "secret.txt" {
		t.Fatalf("按已停用过滤应只命中 secret.txt，实际 total=%d %+v", total, items)
	}

	// 文件夹在 LEFT JOIN 后状态为 NULL，COALESCE 出来的 -1 与任何一个
	// 真实状态都不相等——这是"过滤状态时天然排除文件夹"的关键。
	if items[0].NodeType != "file" {
		t.Fatalf("状态过滤不应命中文件夹，实际命中 %q", items[0].NodeType)
	}
}

func TestListAllNodesByOwnerAndType(t *testing.T) {
	db := openTestDB(t)
	seedAdminNodes(t, db)
	ctx := context.Background()

	// 同一个对象被两人引用，按属主分开。
	items, total, err := ListAllNodes(ctx, db.R(), AdminNodeFilter{Owner: "bob"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || items[0].Account != "bob" {
		t.Fatalf("按属主过滤失败: total=%d %+v", total, items)
	}
	// 引用计数来自内容池，用于判断这条引用是不是最后一条。
	if items[0].RefCount != 1 {
		t.Fatalf("引用计数不符: %+v", items[0])
	}

	folder := NodeFolder
	items, total, err = ListAllNodes(ctx, db.R(), AdminNodeFilter{NodeType: &folder}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || items[0].Name != "docs" {
		t.Fatalf("按类型过滤失败: total=%d %+v", total, items)
	}
}

func TestListAllNodesPaginationIsStable(t *testing.T) {
	db := openTestDB(t)
	seedAdminNodes(t, db)
	ctx := context.Background()

	first, _, err := ListAllNodes(ctx, db.R(), AdminNodeFilter{}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := ListAllNodes(ctx, db.R(), AdminNodeFilter{}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || len(second) != 1 {
		t.Fatalf("分页条数不对: %d / %d", len(first), len(second))
	}
	// 两页不得出现同一行：mtime 相同的节点靠 logical_path 兜底排序。
	seen := map[string]bool{}
	for _, it := range append(first, second...) {
		key := it.Account + "|" + it.Path
		if seen[key] {
			t.Fatalf("分页出现重复行: %s", key)
		}
		seen[key] = true
	}
}

// TestGetAdminNodeStatsCountsNodesAndObjectsSeparately 锁住统计条的两种口径。
//
// 夹具里 ck-report 被两个用户各存了一份：它是 1 个对象，却是 2 个文件节点。
// 早期这一页把内容池对象计数直接摆在节点表格上方，管理员数表格行数永远
// 对不上顶上的「对象总数」——因为两者根本不是同一批东西。
func TestGetAdminNodeStatsCountsNodesAndObjectsSeparately(t *testing.T) {
	db := openTestDB(t)
	seedAdminNodes(t, db)
	ctx := context.Background()

	stats, err := GetAdminNodeStats(ctx, db.R())
	if err != nil {
		t.Fatalf("取统计失败: %v", err)
	}
	// 3 个文件节点（alice 两份 + bob 一份）+ 1 个文件夹。
	if stats.FileNodes != 3 || stats.FolderNodes != 1 || stats.NodeTotal != 4 {
		t.Fatalf("节点口径应 3/1/4，得 %d/%d/%d",
			stats.FileNodes, stats.FolderNodes, stats.NodeTotal)
	}
	// 2 个对象按来源拆开：ck-report 是用户上传(created_by=1)，
	// ck-secret 是系统入库(created_by=0，邮件正文走的就是这条路)。
	if stats.FileObjectTotal != 1 || stats.MailObjectTotal != 1 {
		t.Fatalf("对象来源口径应 1/1，得 %d/%d",
			stats.FileObjectTotal, stats.MailObjectTotal)
	}
	if stats.ObjectsByStatus["normal"] != 1 || stats.ObjectsByStatus["disabled"] != 1 {
		t.Fatalf("对象状态分布不对: %+v", stats.ObjectsByStatus)
	}
	// 键必须是 ParseFileStatus 认得的字符串，前端据此映射中文标签。
	if _, ok := stats.ObjectsByStatus["normal"]; !ok {
		t.Fatalf("状态键应是可解析的字符串: %+v", stats.ObjectsByStatus)
	}

	// 空库：全是 0，且 map 不是 nil——nil 会在 JSON 里变成 null，
	// 前端 Object.entries 拿到 null 会直接抛错。
	empty, err := GetAdminNodeStats(ctx, openTestDB(t).R())
	if err != nil {
		t.Fatalf("空库统计失败: %v", err)
	}
	if empty.NodeTotal != 0 || empty.FileObjectTotal != 0 || empty.MailObjectTotal != 0 ||
		empty.ObjectsByStatus == nil {
		t.Fatalf("空库统计应全零且 map 非 nil: %+v", empty)
	}
}
