package store

import (
	"context"
	"testing"
)

func TestShareAccessPaginationSnapshotDoesNotShiftWhenNewRowsArrive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	share := seededShare(t, db)
	for _, action := range []string{"open", "list"} {
		if err := InsertShareAccess(ctx, db.W(), share.ID, ActorGuest, 0, "192.0.2.10", action); err != nil {
			t.Fatal(err)
		}
	}

	first, total, snapshotID, err := ListShareAccesses(ctx, db.R(), share.ID, 1, 0, 0)
	if err != nil || total != 2 || len(first) != 1 || first[0].Action != "list" || snapshotID == 0 {
		t.Fatalf("first page should capture current snapshot: rows=%+v total=%d snapshot=%d err=%v", first, total, snapshotID, err)
	}
	if err := InsertShareAccess(ctx, db.W(), share.ID, ActorGuest, 0, "192.0.2.11", "pickup"); err != nil {
		t.Fatal(err)
	}

	second, secondTotal, returnedSnapshotID, err := ListShareAccesses(ctx, db.R(), share.ID, 1, 1, snapshotID)
	if err != nil || secondTotal != 2 || returnedSnapshotID != snapshotID || len(second) != 1 || second[0].Action != "open" {
		t.Fatalf("new rows must not shift the next page: rows=%+v total=%d snapshot=%d err=%v", second, secondTotal, returnedSnapshotID, err)
	}

	withoutSnapshot, currentTotal, _, err := ListShareAccesses(ctx, db.R(), share.ID, 1, 1, 0)
	if err != nil || currentTotal != 3 || len(withoutSnapshot) != 1 || withoutSnapshot[0].Action != "list" {
		t.Fatalf("control query should show the offset shift: rows=%+v total=%d err=%v", withoutSnapshot, currentTotal, err)
	}
}
