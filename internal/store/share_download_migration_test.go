package store

import (
	"context"
	"testing"
)

// rewindShareDownloadSchema restores the v9 share_accesses table before older migrations run.
func rewindShareDownloadSchema(t *testing.T, db *DB) {
	t.Helper()
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

	rewindShareDownloadSchema(t, db)
	mustExec(t, db, `PRAGMA user_version = 9`)
	if err := db.initSchema(); err != nil {
		t.Fatalf("v9 → v10 migration failed: %v", err)
	}

	rows, total, _, err := ListShareAccesses(ctx, db.R(), share.ID, 100, 0, 0)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("legacy access row must survive migration: rows=%+v total=%d err=%v", rows, total, err)
	}
	if rows[0].Action != "open" || rows[0].Target != "" || rows[0].BytesPlain != 0 {
		t.Fatalf("legacy row should get neutral download fields: %+v", rows[0])
	}

	download := ShareDownloadAccess{
		TicketID: "ticket-1", ShareID: share.ID, ActorType: ActorGuest,
		ClientIP: "198.51.100.8", ResourcePath: "/a/file.txt", BytesPlain: 321,
		OccurredAt: Now() + 1,
	}
	if err := InsertShareDownloadAccess(ctx, db.W(), download); err != nil {
		t.Fatalf("insert download record: %v", err)
	}
	if err := InsertShareDownloadAccess(ctx, db.W(), download); err == nil {
		t.Fatal("ticket id must prevent duplicate download records")
	}
	rows, total, _, err = ListShareAccesses(ctx, db.R(), share.ID, 100, 0, 0)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("download should be visible in access list: rows=%+v total=%d err=%v", rows, total, err)
	}
	if rows[0].Action != "download" || rows[0].Target != download.ResourcePath || rows[0].BytesPlain != download.BytesPlain {
		t.Fatalf("download details not returned: %+v", rows[0])
	}
}
