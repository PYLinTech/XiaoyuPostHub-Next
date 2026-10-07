package store

import (
	"context"
	"testing"
)

func TestListAuditLogsActionPrefix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	actions := []string{
		"admin.mail.message_archive", "admin.mail.message_purge",
		"admin.mail.message_restore", "user.status",
	}
	for i, a := range actions {
		if err := InsertAuditLog(ctx, db.W(), AuditLog{
			ActorType: "user", ActorID: 1, Action: a,
			OccurredAt: int64(100 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 前缀匹配全部 admin.mail.* 三行。
	rows, total, err := ListAuditLogs(ctx, db.R(), "", "admin.mail", 0, 0, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("admin.mail 前缀应命中 3 行，得 total=%d len=%d", total, len(rows))
	}

	// 精确与前缀同时给时优先精确（只 1 行）。
	_, total, err = ListAuditLogs(ctx, db.R(), "user.status", "admin.mail", 0, 0, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("精确动作优先时应 1 行，得 %d", total)
	}

	// 前缀中的 LIKE 通配符按字面量处理，不会扩大匹配。
	_, total, err = ListAuditLogs(ctx, db.R(), "", "admin.mail%", 0, 0, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("含 %% 的前缀应无字面匹配，得 %d", total)
	}
}
