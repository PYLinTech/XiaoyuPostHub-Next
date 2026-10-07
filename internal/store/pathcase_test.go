package store

import (
	"context"
	"strings"
	"testing"
)

// 路径子树谓词的大小写正确性。
//
// 回归背景：子树范围原先用 `logical_path LIKE ? ESCAPE '\'` 表达，而
// SQLite 的 LIKE 对 ASCII 默认大小写不敏感（没有开 case_sensitive_like），
// 路径比较（=、主键）却是区分大小写的。两者不一致会造出静默的数据丢失：
// `/Photos` 与 `/photos` 是两棵互不相干的树，却会被同一条 LIKE 同时命中。
// 于是删除 `/photos` 会连 `/Photos` 一起删掉，移动 `/photos` 会把两棵树合并。

func seedCaseVariantTree(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
	                 VALUES (1, 'alice', 'x', 'normal', 0, 0)`)
	// 文件节点要挂内容池对象（外键指向 files）。
	mustExec(t, db, `INSERT INTO files (checksum, size_plain, pan_object_name, pan_size_wire,
	                 enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope,
	                 kek_key_id, status, created_at, updated_at)
	                 VALUES ('cs1', 10, 'o/1.xph', 20, 'AES-256-GCM', 20, 1, x'00', x'00', 'k1', 1, 0, 0)`)
	for _, p := range []string{
		"/photos", "/photos/a.txt", "/photos/sub", "/photos/sub/b.txt",
		"/Photos", "/Photos/a.txt", "/Photos/sub", "/Photos/sub/b.txt",
	} {
		nt := 1
		if len(p) > 5 && p[len(p)-4:] == ".txt" {
			nt = 0
		}
		var parent, name string
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] == '/' {
				name = p[i+1:]
				if i == 0 {
					parent = "/"
				} else {
					parent = p[:i]
				}
				break
			}
		}
		if nt == 0 {
			mustExec(t, db, `INSERT INTO user_nodes (user_id, logical_path, node_type, file_checksum,
			                name, parent_path, created_at)
			                VALUES (1, ?, 0, 'cs1', ?, ?, 0)`, p, name, parent)
		} else {
			mustExec(t, db, `INSERT INTO user_nodes (user_id, logical_path, node_type, name,
			                parent_path, created_at)
			                VALUES (1, ?, 1, ?, ?, 0)`, p, name, parent)
		}
	}
}

// countPaths 用 GLOB 计数：GLOB 恒为大小写敏感，用它才能真正断言
// "只有这一棵树"。
func countPaths(t *testing.T, db *DB, prefix string) int {
	t.Helper()
	var n int
	if err := db.R().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM user_nodes WHERE user_id = 1 AND logical_path GLOB ?`,
		prefix+"*").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDeleteSubtreeIsCaseExact 删除 /photos 必须只删 /photos。
func TestDeleteSubtreeIsCaseExact(t *testing.T) {
	db := openTestDB(t)
	seedCaseVariantTree(t, db)
	ctx := context.Background()

	if got := countPaths(t, db, "/"); got != 8 {
		t.Fatalf("造数后应有 8 个节点，实际 %d", got)
	}
	if _, err := DeleteSubtree(ctx, db.W(), 1, "/photos"); err != nil {
		t.Fatal(err)
	}
	if n := countPaths(t, db, "/photos"); n != 0 {
		t.Fatalf("/photos 应被删空，实际残留 %d", n)
	}
	if n := countPaths(t, db, "/Photos"); n != 4 {
		t.Fatalf("/Photos 必须完好无损，实际剩余 %d（大小写不敏感的 LIKE 把它一起删了）", n)
	}
}

// TestMoveSubtreeIsCaseExact 移动 /photos 必须只影响 /photos。
func TestMoveSubtreeIsCaseExact(t *testing.T) {
	db := openTestDB(t)
	seedCaseVariantTree(t, db)
	ctx := context.Background()

	if err := MoveSubtree(ctx, db.W(), 1, "/photos", "/Archive/photos", "/Archive", "photos"); err != nil {
		t.Fatal(err)
	}
	if n := countPaths(t, db, "/Archive/photos"); n != 4 {
		t.Fatalf("/Archive/photos 应有 4 个节点，实际 %d", n)
	}
	if n := countPaths(t, db, "/Photos"); n != 4 {
		t.Fatalf("/Photos 必须原地不动，实际 %d（被错误合并进来了）", n)
	}
	if n := countPaths(t, db, "/photos"); n != 0 {
		t.Fatalf("原 /photos 应已移走，实际残留 %d", n)
	}
}

// TestEnumerateSubtreeIsCaseExact 归档快照同样不能跨大小写抓取。
func TestEnumerateSubtreeIsCaseExact(t *testing.T) {
	db := openTestDB(t)
	seedCaseVariantTree(t, db)
	ctx := context.Background()

	nodes, err := EnumerateSubtree(ctx, db.R(), 1, "/photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 {
		t.Fatalf("/photos 子树应有 4 个节点，实际 %d（把 /Photos 也算进来了）", len(nodes))
	}
	for _, n := range nodes {
		if n.LogicalPath != "/photos" && !strings.HasPrefix(n.LogicalPath, "/photos/") {
			t.Fatalf("枚举结果混入了其它树: %s", n.LogicalPath)
		}
	}
}

// TestCountSubtreeIsCaseExact 统计同样必须区分大小写。
func TestCountSubtreeIsCaseExact(t *testing.T) {
	db := openTestDB(t)
	seedCaseVariantTree(t, db)
	ctx := context.Background()

	files, folders, _, err := CountSubtree(ctx, db.R(), 1, "/photos")
	if err != nil {
		t.Fatal(err)
	}
	// CountSubtree 刻意不含自身：/photos 下是 a.txt、sub/b.txt 两个文件与
	// sub 一个文件夹。
	if files != 2 || folders != 1 {
		t.Fatalf("/photos 应统计到 2 文件 1 文件夹，实际 %d/%d", files, folders)
	}
}
