package service

import (
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

func TestDedupPlacementAndDeleteKeepStorageQuotaBalanced(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.putFile("/a/source.bin", "quota-dedup-checksum")
	file, err := store.GetFile(ctx, f.db.R(), "quota-dedup-checksum")
	if err != nil {
		t.Fatalf("读取内容池记录失败: %v", err)
	}

	node, err := f.svc.placeFileNode(ctx, f.user, "/a", "copy.bin", ConflictRename, file)
	if err != nil {
		t.Fatalf("放置秒传引用失败: %v", err)
	}
	used, err := store.GetCounter(ctx, f.db.R(), store.ScopeStorage, store.UserCounterKey(f.user.User.ID, ""))
	if err != nil {
		t.Fatalf("读取存储配额失败: %v", err)
	}
	if used != file.SizePlain {
		t.Fatalf("秒传引用应计入一次存储配额，实得 %d", used)
	}

	if err := f.svc.DeleteNode(ctx, f.user, node.LogicalPath); err != nil {
		t.Fatalf("删除秒传引用失败: %v", err)
	}
	// 删除只是进入归档暂存：引用与配额仍然占用，数据可恢复。
	used, err = store.GetCounter(ctx, f.db.R(), store.ScopeStorage, store.UserCounterKey(f.user.User.ID, ""))
	if err != nil {
		t.Fatalf("读取删除后的存储配额失败: %v", err)
	}
	if used != file.SizePlain {
		t.Fatalf("暂存期配额应保持占用，实得 %d", used)
	}

	// 清除后（归档删除）才真正归还配额。
	if err := f.svc.ClearArchiveBatch(ctx, f.user, "nonexistent-batch"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的批次应返回 ErrNotFound，实得: %v", err)
	}
	// 找到删除产生的批次并清除。
	batches, total, err := f.svc.ListUserArchive(ctx, f.user, 10, 0)
	if err != nil {
		t.Fatalf("列出归档失败: %v", err)
	}
	if total != 1 {
		t.Fatalf("删除后归档应有 1 个批次，实得 %d", total)
	}
	if err := f.svc.ClearArchiveBatch(ctx, f.user, batches[0].ID); err != nil {
		t.Fatalf("清除批次失败: %v", err)
	}
	used, err = store.GetCounter(ctx, f.db.R(), store.ScopeStorage, store.UserCounterKey(f.user.User.ID, ""))
	if err != nil {
		t.Fatalf("读取清除后的存储配额失败: %v", err)
	}
	if used != 0 {
		t.Fatalf("归档删除后应归还配额，实得 %d", used)
	}
}

func TestMoveNodeRewritesParentPaths(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.mkdir("/src")
	f.mkdir("/src/sub")
	f.mkdir("/dest")

	path, err := f.svc.MoveNode(ctx, f.user, "/src", "/dest")
	if err != nil {
		t.Fatalf("移动目录失败: %v", err)
	}
	if path != "/dest/src" {
		t.Fatalf("移动后的路径错误: %q", path)
	}
	node, err := store.GetNode(ctx, f.db.R(), f.user.User.ID, "/dest/src/sub")
	if err != nil {
		t.Fatalf("移动后的子目录不存在: %v", err)
	}
	if node.ParentPath != "/dest/src" {
		t.Fatalf("子目录 parent_path 未同步更新: %q", node.ParentPath)
	}
	if _, err := store.GetNode(ctx, f.db.R(), f.user.User.ID, "/src/sub"); err == nil {
		t.Fatal("旧路径下不应保留子目录")
	}
}
