package store

import (
	"context"
	"testing"
)

func TestDeleteShareAccessesBeforeBatchIsBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO user_groups (name, display_name, created_at) VALUES ('normal', '普通', 1)`)
	mustExec(t, db, `INSERT INTO users (account, password_hash, group_name, status, created_at, updated_at)
		VALUES ('share-owner', 'h', 'normal', 1, 1, 1)`)
	mustExec(t, db, `INSERT INTO shares (id, owner_id, root_path, kind, access_mode, created_at, updated_at)
		VALUES ('share-retention', 1, '/file.bin', 'file', 'public', 1, 1)`)
	for _, occurredAt := range []int64{1, 2, 3} {
		mustExec(t, db, `INSERT INTO share_accesses (share_id, actor_type, client_ip, action, occurred_at)
			VALUES ('share-retention', 'guest', '203.0.113.1', 'open', ?)`, occurredAt)
	}

	removed, err := DeleteShareAccessesBeforeBatch(ctx, db.W(), 3, 1)
	if err != nil || removed != 1 {
		t.Fatalf("首轮应限量删除 1 条过期记录，removed=%d err=%v", removed, err)
	}
	removed, err = DeleteShareAccessesBeforeBatch(ctx, db.W(), 3, 10)
	if err != nil || removed != 1 {
		t.Fatalf("第二轮应删除剩余过期记录，removed=%d err=%v", removed, err)
	}
	var remaining int
	if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM share_accesses WHERE share_id = 'share-retention'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("cutoff 边界上的记录必须保留，剩余 %d 条", remaining)
	}
}
