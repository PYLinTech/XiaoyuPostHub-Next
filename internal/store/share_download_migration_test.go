package store

import (
	"context"
	"testing"
)

// rewindShareDownloadSchema restores the v9 share/share_accesses structures before older migrations run.
func rewindShareDownloadSchema(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `ALTER TABLE share_accesses DROP COLUMN evidence_source`)
	mustExec(t, db, `ALTER TABLE shares DROP COLUMN delivery_count`)
	mustExec(t, db, `ALTER TABLE shares RENAME COLUMN max_deliveries TO max_visits`)
	mustExec(t, db, `DROP INDEX idx_share_accesses_ticket`)
	for _, column := range []string{"ticket_id", "bytes_plain", "resource_path"} {
		mustExec(t, db, "ALTER TABLE share_accesses DROP COLUMN "+column)
	}
}

func TestShareDownloadMigrationPreservesAccessesAndAddsTicketDedupe(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	share := seededShare(t, db)
	if err := InsertShareAccess(ctx, db.W(), share.ID, ActorUser, share.OwnerID, "192.0.2.4", "open"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE shares SET max_deliveries = 5 WHERE id = ?`, share.ID)
	mustExec(t, db, `INSERT INTO share_accesses (share_id, actor_type, action, occurred_at)
		VALUES (?, 'guest', 'download', ?)`, share.ID, Now()-1)

	rewindShareDownloadSchema(t, db)
	mustExec(t, db, `PRAGMA user_version = 9`)
	if err := db.initSchema(); err != nil {
		t.Fatalf("v9 → v11 migration failed: %v", err)
	}

	migrated, err := GetShare(ctx, db.R(), share.ID)
	if err != nil || migrated.MaxDeliveries != 5 || migrated.Deliveries != 1 {
		t.Fatalf("v10 limit/count migration mismatch: share=%+v err=%v", migrated, err)
	}
	rows, total, _, err := ListShareAccesses(ctx, db.R(), share.ID, 100, 0, 0)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("legacy access rows must survive migration: rows=%+v total=%d err=%v", rows, total, err)
	}
	for _, row := range rows {
		if row.Action == "open" && (row.Target != "" || row.BytesPlain != 0 || row.EvidenceSource != ShareEvidenceLegacyUnknown) {
			t.Fatalf("legacy open row should remain neutral: %+v", row)
		}
		if row.Action == "download" && row.EvidenceSource != ShareEvidenceLegacyUnknown {
			t.Fatalf("legacy download source must be marked unknown: %+v", row)
		}
	}

	download := ShareDownloadAccess{
		TicketID: "ticket-1", ShareID: share.ID, ActorType: ActorGuest,
		ClientIP: "198.51.100.8", ResourcePath: "/a/file.txt", BytesPlain: 321,
		EvidenceSource: ShareEvidenceClientAck, OccurredAt: Now() + 1,
	}
	if err := InsertShareDownloadAccess(ctx, db.W(), download); err != nil {
		t.Fatalf("insert download record: %v", err)
	}
	if err := InsertShareDownloadAccess(ctx, db.W(), download); err == nil {
		t.Fatal("ticket id must prevent duplicate download records")
	}
	rows, total, _, err = ListShareAccesses(ctx, db.R(), share.ID, 100, 0, 0)
	if err != nil || total != 3 || len(rows) != 3 {
		t.Fatalf("download should be visible in access list: rows=%+v total=%d err=%v", rows, total, err)
	}
	if rows[0].Action != "download" || rows[0].Target != download.ResourcePath || rows[0].BytesPlain != download.BytesPlain || rows[0].EvidenceSource != ShareEvidenceClientAck {
		t.Fatalf("download details not returned: %+v", rows[0])
	}
}
