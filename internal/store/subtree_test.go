package store

import (
	"context"
	"testing"
)

// TestCountSubtreeExcludesThePathItself 钉住统计口径：只数 path 之下的节点。
//
// 这条断言来自一次实机验证：站在一个空目录里，界面显示"1 个文件夹"。
// 原因是查询里带了 `logical_path = ?`，把目录自身也算了进去——对一个
// "这下面有什么"的展示来说，那是错的。
func TestCountSubtreeExcludesThePathItself(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// user_nodes.user_id 有外键约束，先把账号与预设组建好。
	if err := EnsureBuiltinGroups(ctx, db.W()); err != nil {
		t.Fatalf("写入预设组失败: %v", err)
	}
	userID, err := CreateUser(ctx, db.W(), User{
		Account: "subtree", PasswordHash: "x", GroupName: "normal",
		Status: UserEnabled, CreatedAt: Now(), UpdatedAt: Now(),
	})
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}

	mk := func(path string, kind NodeType) {
		t.Helper()
		if err := InsertNode(ctx, db.W(), Node{
			UserID: userID, LogicalPath: path, NodeType: kind,
			Name: path, ParentPath: "/", Mtime: Now(), CreatedAt: Now(),
		}); err != nil {
			t.Fatalf("插入节点 %s 失败: %v", path, err)
		}
	}
	mk("/a", NodeFolder)
	mk("/a/b", NodeFolder)
	mk("/a/c", NodeFolder)
	mk("/ab", NodeFolder) // 与 /a 共享前缀但在树外，用来验证模式带分隔符

	files, folders, _, err := CountSubtree(ctx, db.R(), userID, "/a")
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if folders != 2 {
		t.Fatalf("/a 之下应有 2 个文件夹（b、c），实得 %d", folders)
	}
	if files != 0 {
		t.Fatalf("/a 之下没有文件，实得 %d", files)
	}

	// 空目录的统计必须是 0，而不是"1 个文件夹"。
	mk("/empty", NodeFolder)
	if _, folders, _, err := CountSubtree(ctx, db.R(), userID, "/empty"); err != nil {
		t.Fatalf("统计空目录失败: %v", err)
	} else if folders != 0 {
		t.Fatalf("空目录之下应有 0 个文件夹，实得 %d", folders)
	}

	// 根目录统计全部（根没有对应的节点行，两种口径结果一致）。
	if _, folders, _, err := CountSubtree(ctx, db.R(), userID, "/"); err != nil {
		t.Fatalf("统计根目录失败: %v", err)
	} else if folders != 5 {
		t.Fatalf("根目录之下应有 5 个文件夹，实得 %d", folders)
	}
}
