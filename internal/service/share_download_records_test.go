package service

import (
	"context"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

func TestShareDownloadRecordIsWrittenOnceAfterCompleteSettlement(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	if err := store.InsertShareAccess(ctx, f.db.W(), f.share.ID, store.ActorUser, f.user.UserID(), "192.0.2.5", "open"); err != nil {
		t.Fatal(err)
	}
	ticket, err := f.prepare(f.guest, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}

	rows, total, _, err := store.ListShareAccesses(ctx, f.db.R(), f.share.ID, 100, 0, 0)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Action != "open" {
		t.Fatalf("preparing a ticket must not create a download record: rows=%+v total=%d err=%v", rows, total, err)
	}

	if err := f.svc.SettleDelivery(ctx, f.guest, ticket.ID, f.file.SizePlain); err != nil {
		t.Fatalf("settle complete download: %v", err)
	}
	if err := f.svc.SettleDelivery(ctx, f.guest, ticket.ID, f.file.SizePlain); err != nil {
		t.Fatalf("idempotent second settlement: %v", err)
	}

	rows, total, snapshotID, err := f.svc.ListShareAccesses(ctx, f.user, f.share.ID, 1, 0, 0)
	if err != nil || total != 2 || len(rows) != 1 {
		t.Fatalf("owner should see a paginated access list: rows=%+v total=%d err=%v", rows, total, err)
	}
	got := rows[0]
	if got.Action != "download" || got.ActorType != store.ActorGuest || got.ActorID != 0 ||
		got.ClientIP != f.guest.ClientIP.String() || got.Target != f.target.Path || got.BytesPlain != f.file.SizePlain {
		t.Fatalf("download event fields mismatch: %+v", got)
	}
	older, olderTotal, olderSnapshotID, err := f.svc.ListShareAccesses(ctx, f.user, f.share.ID, 1, 1, snapshotID)
	if err != nil || olderTotal != 2 || olderSnapshotID != snapshotID || len(older) != 1 || older[0].Action != "open" {
		t.Fatalf("offset should retrieve the preceding access entry: rows=%+v total=%d err=%v", older, olderTotal, err)
	}
	if _, _, _, err := f.svc.ListShareAccesses(ctx, f.guest, f.share.ID, 1, 0, snapshotID); err == nil {
		t.Fatal("guest must not be allowed to inspect owner records")
	}
}

func TestShareDownloadRecordExcludesPreviewAndPartialTransfer(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()

	preview, err := f.prepare(f.guest, store.PurposePreview)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SettleDelivery(ctx, f.guest, preview.ID, f.file.SizePlain); err != nil {
		t.Fatalf("settle preview: %v", err)
	}

	partialDownload, err := f.prepare(f.guest, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}
	partialBytes := f.file.SizePlain / 2
	if partialBytes == f.file.SizePlain {
		t.Fatal("fixture must have a non-empty file to test partial delivery")
	}
	if err := f.svc.SettleDelivery(ctx, f.guest, partialDownload.ID, partialBytes); err != nil {
		t.Fatalf("settle partial download: %v", err)
	}

	rows, total, _, err := store.ListShareAccesses(ctx, f.db.R(), f.share.ID, 100, 0, 0)
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("preview and incomplete transfer must not be shown as completed downloads: rows=%+v total=%d err=%v", rows, total, err)
	}
}
