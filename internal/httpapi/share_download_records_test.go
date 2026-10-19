package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

func TestShareAndPickupDownloadsAppearInOwnerAccessHistory(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	token := e.login(t)

	const path = "/share-download-record.txt"
	file, err := e.backend.IngestPlaintext(ctx, bytes.NewReader([]byte("share download records")), 1)
	if err != nil {
		t.Fatalf("ingest file: %v", err)
	}
	if err := store.InsertNode(ctx, e.db.W(), store.Node{
		UserID: 1, LogicalPath: path, NodeType: store.NodeFile, FileChecksum: file.Checksum,
		Name: vpath.Base(path), ParentPath: vpath.Parent(path), SizePlain: file.SizePlain,
		Mtime: store.Now(), CreatedAt: store.Now(),
	}); err != nil {
		t.Fatalf("insert file node: %v", err)
	}
	if _, err := store.AddFileRef(ctx, e.db.W(), file.Checksum); err != nil {
		t.Fatalf("add file reference: %v", err)
	}
	shareID, err := store.NewID("sh")
	if err != nil {
		t.Fatal(err)
	}
	share := store.Share{
		ID: shareID, OwnerID: 1, RootPath: path, Kind: store.ShareFile,
		AccessMode: store.AccessPublic, AllowDownload: true, CreatedAt: store.Now(),
	}
	if err := store.CreateShare(ctx, e.db.W(), share); err != nil {
		t.Fatalf("create share: %v", err)
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

	// Exercise the actual share-download route and the anonymous settlement route.
	status, payload := e.do(t, http.MethodPost, "/api/s/"+share.ID+"/download", map[string]any{
		"password": "", "relPath": "", "clientPublicKey": clientPublicKey,
	}, "")
	if status != http.StatusOK {
		t.Fatalf("prepare share download: status=%d body=%v", status, payload)
	}
	settleTicket(t, e, payload)

	// The pickup flow also writes its separate pickup-use event, then the settled download.
	status, pickupPayload := e.do(t, http.MethodPost, "/api/share/"+share.ID+"/pickup", map[string]any{"maxUses": 1}, token)
	if status != http.StatusOK {
		t.Fatalf("create pickup code: status=%d body=%v", status, pickupPayload)
	}
	pickupData, _ := pickupPayload["data"].(map[string]any)
	pickup, _ := pickupData["pickup"].(map[string]any)
	code, _ := pickup["code"].(string)
	if code == "" {
		t.Fatalf("create pickup code returned no code: %v", pickupPayload)
	}
	status, pickupDownload := e.do(t, http.MethodPost, "/api/p/"+code+"/download", map[string]any{
		"clientPublicKey": clientPublicKey,
	}, "")
	if status != http.StatusOK {
		t.Fatalf("prepare pickup download: status=%d body=%v", status, pickupDownload)
	}
	settleTicket(t, e, pickupDownload)

	rows, total, _, err := store.ListShareAccesses(ctx, e.db.R(), share.ID, 100, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	downloadCount, pickupCount := 0, 0
	for _, row := range rows {
		if row.Action == "download" {
			downloadCount++
			if row.Target != path || row.BytesPlain != file.SizePlain || row.ActorType != store.ActorGuest {
				t.Fatalf("download record has wrong ticket metadata: %+v", row)
			}
		}
		if row.Action == "pickup" {
			pickupCount++
		}
	}
	if downloadCount != 2 || pickupCount != 1 || total != 4 {
		t.Fatalf("share and pickup download history missing events: downloads=%d pickups=%d total=%d rows=%+v", downloadCount, pickupCount, total, rows)
	}

	// Verify the owner-facing endpoint's paged JSON contract, including the snapshot token.
	status, page := e.do(t, http.MethodGet, fmt.Sprintf("/api/share/%s/accesses?limit=1&offset=0", share.ID), nil, token)
	if status != http.StatusOK {
		t.Fatalf("list share access page: status=%d body=%v", status, page)
	}
	pageData, _ := page["data"].(map[string]any)
	pageItems, _ := pageData["items"].([]any)
	snapshotID, _ := pageData["snapshotId"].(float64)
	if len(pageItems) != 1 || pageData["total"] != float64(total) || snapshotID <= 0 {
		t.Fatalf("paged endpoint response mismatch: %v", pageData)
	}
	status, nextPage := e.do(t, http.MethodGet, fmt.Sprintf("/api/share/%s/accesses?limit=1&offset=1&snapshotId=%s", share.ID, strconv.FormatInt(int64(snapshotID), 10)), nil, token)
	if status != http.StatusOK {
		t.Fatalf("list next access page: status=%d body=%v", status, nextPage)
	}
	nextData, _ := nextPage["data"].(map[string]any)
	if nextData["total"] != float64(total) || nextData["snapshotId"] != snapshotID {
		t.Fatalf("next page should preserve the same snapshot: %v", nextData)
	}
}

func settleTicket(t *testing.T, e *testEnv, payload map[string]any) {
	t.Helper()
	data, _ := payload["data"].(map[string]any)
	ticketID, _ := data["ticketId"].(string)
	mode, _ := data["mode"].(string)
	if ticketID == "" || (mode != "direct" && mode != "proxy") {
		t.Fatalf("client-key delivery plan should use client-settled mode and include a ticket: %v", payload)
	}
	status, body := e.do(t, http.MethodPost, "/api/fs/settle", map[string]any{"ticketId": ticketID}, "")
	if status != http.StatusOK {
		t.Fatalf("settle share ticket: status=%d body=%v", status, body)
	}
}
