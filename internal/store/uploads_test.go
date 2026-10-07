package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestMarkChunkReceivedMergesConcurrentUpdates verifies that simultaneous chunk
// acknowledgements cannot lose bits from one another.
func TestMarkChunkReceivedMergesConcurrentUpdates(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "uploads.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通用户', 0)`)
	mustExec(t, db, `INSERT INTO users (id, account, password_hash, group_name, created_at, updated_at)
		VALUES (1, 'u1', 'x', 'normal', 0, 0)`)

	const total = 32
	if err := CreateUploadTask(ctx, db.W(), UploadTask{
		ID:               "up-cas",
		UserID:           1,
		Checksum:         "checksum",
		SizePlain:        total,
		ChunkSize:        1,
		ChunkTotal:       total,
		ReceivedMask:     make([]byte, BitmapBytes(total)),
		TargetParentPath: "/",
		TargetName:       "file",
		ConflictAction:   "rename",
		ExpiresAt:        ToTime(Now() + 3600),
	}); err != nil {
		t.Fatalf("创建上传会话失败: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := MarkChunkReceived(ctx, db.W(), "up-cas", index)
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("并发确认分片失败: %v", err)
	}

	task, err := GetUploadTask(ctx, db.R(), "up-cas")
	if err != nil {
		t.Fatalf("读取上传会话失败: %v", err)
	}
	if !task.Complete() {
		t.Fatalf("并发确认后位图丢失分片: mask=%v", task.ReceivedMask)
	}
}
