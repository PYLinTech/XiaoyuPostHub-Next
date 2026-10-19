package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
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
		got.ClientIP != f.guest.ClientIP.String() || got.Target != f.target.Path || got.BytesPlain != f.file.SizePlain || got.EvidenceSource != store.ShareEvidenceServerMeasured {
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

func TestShareDirectoryBrowsingRecordsOpenAndListOnceEach(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	share := f.share("/a", store.ShareFolder, store.AccessPublic, "", true)
	if _, _, err := f.svc.ResolveShare(ctx, share.ID, "", f.user); err != nil {
		t.Fatal(err)
	}
	if items, err := f.svc.ListShareDir(ctx, share.ID, "", "", f.user); err != nil || len(items) == 0 {
		t.Fatalf("list shared root: items=%+v err=%v", items, err)
	}
	rows, total, _, err := store.ListShareAccesses(ctx, f.db.R(), share.ID, 100, 0, 0)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("page resolve and one listing should create two history events: rows=%+v total=%d err=%v", rows, total, err)
	}
	actions := map[string]int{}
	for _, row := range rows {
		actions[row.Action]++
	}
	if actions["open"] != 1 || actions["list"] != 1 {
		t.Fatalf("directory list must not duplicate its open event: %+v", actions)
	}
}

func TestClientSettlementEvidenceAndIncompleteOutcome(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyDeliveryDirect:       "false",
		settings.KeyDeliveryProxyDecrypt: "false",
	}, 0); err != nil {
		t.Fatal(err)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPublicKey := base64.StdEncoding.EncodeToString(publicDER)
	prepare := func() DeliveryPlan {
		plan, err := f.svc.PrepareDelivery(ctx, f.guest, f.target, DeliveryRequest{
			Purpose: store.PurposeDownload, ClientPublicKey: clientPublicKey, ForceProxy: true,
		})
		if err != nil {
			t.Fatalf("prepare client-encrypted plan: %v", err)
		}
		return plan
	}

	completed := prepare()
	if err := f.svc.SettleDeliveryFromClient(ctx, f.guest, completed.TicketID, true); err != nil {
		t.Fatalf("settle completed client delivery: %v", err)
	}
	incomplete := prepare()
	if err := f.svc.SettleDeliveryFromClient(ctx, f.guest, incomplete.TicketID, false); err != nil {
		t.Fatalf("settle incomplete client delivery: %v", err)
	}
	rows, total, _, err := store.ListShareAccesses(ctx, f.db.R(), f.share.ID, 100, 0, 0)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("only the client-confirmed complete download should be recorded: rows=%+v total=%d err=%v", rows, total, err)
	}
	if rows[0].EvidenceSource != store.ShareEvidenceClientAck {
		t.Fatalf("client settlement must not be presented as server-measured: %+v", rows[0])
	}
	incompleteTicket, err := store.GetTicket(ctx, f.db.R(), incomplete.TicketID)
	if err != nil || incompleteTicket.SettledBytes == 0 {
		t.Fatalf("incomplete client report must still settle conservative full quota: ticket=%+v err=%v", incompleteTicket, err)
	}
}

func TestShareDeliveryLimitAllowsBrowsingAndCapsPreparedPlans(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	share, err := f.svc.CreateShare(ctx, f.user, CreateShareRequest{
		Path: "/a", Kind: store.ShareFolder, AccessMode: store.AccessPublic,
		AllowDownload: true, AllowPreview: true, AllowSubpath: true, MaxDeliveries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.ResolveShare(ctx, share.ID, "", f.guest); err != nil {
		t.Fatalf("maxDeliveries=1 must allow the initial page resolve: %v", err)
	}
	if items, err := f.svc.ListShareDir(ctx, share.ID, "", "", f.guest); err != nil || len(items) == 0 {
		t.Fatalf("maxDeliveries=1 must allow listing the shared folder: items=%+v err=%v", items, err)
	}

	deliver := func(purpose store.Purpose) (DeliveryPlan, error) {
		target, _, _, err := f.svc.ResolveSharePath(ctx, share.ID, "", "sample.bin", f.guest)
		if err != nil {
			return DeliveryPlan{}, err
		}
		return f.svc.PrepareDelivery(ctx, f.guest, target, DeliveryRequest{Purpose: purpose, ForceProxy: true})
	}
	first, err := deliver(store.PurposePreview)
	if err != nil {
		t.Fatalf("prepare first preview plan: %v", err)
	}
	if first.TicketID == "" {
		t.Fatal("preview plan did not return a ticket")
	}
	if err := f.svc.ClaimShareDelivery(ctx, share.ID); err != nil {
		t.Fatalf("first prepared plan should consume the single delivery allowance: %v", err)
	}
	if items, err := f.svc.ListShareDir(ctx, share.ID, "", "", f.guest); err != nil || len(items) == 0 {
		t.Fatalf("browsing should remain possible after the delivery allowance is used: items=%+v err=%v", items, err)
	}

	second, err := deliver(store.PurposeDownload)
	if err != nil {
		t.Fatalf("plan preparation happens before the atomic cap claim: %v", err)
	}
	if err := f.svc.ClaimShareDelivery(ctx, share.ID); !errors.Is(err, ErrForbidden) {
		_ = f.svc.CancelDelivery(ctx, f.guest, second.TicketID)
		t.Fatalf("second prepared plan must be rejected at the cap, got %v", err)
	}
	if err := f.svc.CancelDelivery(ctx, f.guest, second.TicketID); err != nil {
		t.Fatalf("cancel rejected plan: %v", err)
	}
	current, err := store.GetShare(ctx, f.db.R(), share.ID)
	if err != nil || current.Deliveries != 1 {
		t.Fatalf("delivery counter should remain at one: share=%+v err=%v", current, err)
	}
}
