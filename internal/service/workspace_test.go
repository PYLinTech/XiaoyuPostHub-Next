package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/apikey"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// workspaceKey 把普通会话身份换成绑定工作区的 Key 身份。
func workspaceKey(p auth.Principal, root string) auth.Principal {
	p.APIKeyID = "key-test"
	p.APIKeyName = "test"
	p.APIKeyScopes = int64(apikey.All)
	p.APIKeyWorkspace = root
	return p
}

// TestWorkspaceScopesFileTree 验证文件树操作全部被限制在工作区内：
// 路径按工作区相对解释、响应改回相对路径、根目录不可改名/移动/删除。
func TestWorkspaceScopesFileTree(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	keyed := workspaceKey(f.user, "/a/b")

	items, err := f.svc.ListDir(ctx, keyed, "/")
	if err != nil {
		t.Fatalf("列工作区根失败: %v", err)
	}
	if len(items) != 1 || items[0].Path != "/c" || items[0].Name != "c" {
		t.Fatalf("工作区根只应暴露 /c，实际 %+v", items)
	}

	node, err := f.svc.StatNode(ctx, keyed, "/")
	if err != nil {
		t.Fatalf("读取工作区根失败: %v", err)
	}
	if node.LogicalPath != "/" || node.Name != "b" || node.ParentPath != "" {
		t.Fatalf("工作区根应映射成 /，实际 %+v", node)
	}

	// 同前缀的 /a/bc 不能被当成工作区内容；带 .. 的路径直接拒绝。
	if _, err := f.svc.StatNode(ctx, keyed, "/a/bc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("工作区外路径应 404，实际 %v", err)
	}
	if _, err := f.svc.StatNode(ctx, keyed, "/../a/bc"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("含 .. 的路径应 400，实际 %v", err)
	}

	created, err := f.svc.MakeDir(ctx, keyed, "/", "inside")
	if err != nil {
		t.Fatalf("在工作区内建目录失败: %v", err)
	}
	if created.LogicalPath != "/inside" {
		t.Fatalf("新目录应返回相对路径 /inside，实际 %s", created.LogicalPath)
	}
	if _, err := store.GetNode(ctx, f.db.R(), f.user.UserID(), "/a/b/inside"); err != nil {
		t.Fatalf("新目录应落在工作区绝对路径下: %v", err)
	}

	if _, err := f.svc.RenameNode(ctx, keyed, "/", "nope"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("重命名工作区根应 400，实际 %v", err)
	}
	if _, err := f.svc.MoveNode(ctx, keyed, "/", "/c"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("移动工作区根应 400，实际 %v", err)
	}
	if err := f.svc.DeleteNode(ctx, keyed, "/"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("删除工作区根应 400，实际 %v", err)
	}

	renamed, err := f.svc.RenameNode(ctx, keyed, "/inside", "renamed")
	if err != nil {
		t.Fatalf("重命名工作区内目录失败: %v", err)
	}
	if renamed != "/renamed" {
		t.Fatalf("重命名应返回 /renamed，实际 %s", renamed)
	}
	moved, err := f.svc.MoveNode(ctx, keyed, "/renamed", "/c")
	if err != nil {
		t.Fatalf("移动工作区内目录失败: %v", err)
	}
	if moved != "/c/renamed" {
		t.Fatalf("移动应返回 /c/renamed，实际 %s", moved)
	}
	if err := f.svc.DeleteNode(ctx, keyed, "/c/renamed"); err != nil {
		t.Fatalf("删除工作区内目录失败: %v", err)
	}
}

// TestWorkspaceScopesUploadSharesLinksAndTickets 验证上传、分享、直链、
// 归档与交付票据都按工作区隔离，而不是只限制文件树浏览。
func TestWorkspaceScopesUploadSharesLinksAndTickets(t *testing.T) {
	f := newDirectLinkFixture(t)
	ctx := context.Background()
	keyed := workspaceKey(f.user, "/a/b")
	other := workspaceKey(f.user, "/a/bc")

	init, err := f.svc.InitUpload(ctx, keyed, InitUploadRequest{
		Checksum:       payloadChecksum(contentB),
		SizePlain:      int64(len(contentB)),
		ParentPath:     "/",
		Name:           "keyed.bin",
		ConflictAction: ConflictOverwrite,
	})
	if err != nil {
		t.Fatalf("初始化上传失败: %v", err)
	}
	if _, err := f.svc.UploadChunk(ctx, keyed, init.SessionID, 0, bytes.NewReader(contentB)); err != nil {
		t.Fatalf("上传分片失败: %v", err)
	}
	node, err := f.svc.CompleteUpload(ctx, keyed, init.SessionID)
	if err != nil {
		t.Fatalf("上传收尾失败: %v", err)
	}
	if node.LogicalPath != "/keyed.bin" {
		t.Fatalf("上传结果应返回相对路径 /keyed.bin，实际 %s", node.LogicalPath)
	}
	if _, err := store.GetNode(ctx, f.db.R(), f.user.UserID(), "/a/b/keyed.bin"); err != nil {
		t.Fatalf("上传应落在工作区绝对路径下: %v", err)
	}

	// 工作区外目录按相对路径解释后不存在，上传不能借绝对路径落出去。
	if _, err := f.svc.InitUpload(ctx, keyed, InitUploadRequest{
		Checksum:       payloadChecksum(contentA),
		SizePlain:      int64(len(contentA)),
		ParentPath:     "/a/bc",
		Name:           "escape.bin",
		ConflictAction: ConflictRename,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("上传到工作区外应 404，实际 %v", err)
	}

	share, err := f.svc.CreateShare(ctx, keyed, CreateShareRequest{
		Path:          "/keyed.bin",
		Kind:          store.ShareFile,
		AccessMode:    store.AccessPublic,
		AllowDownload: true,
		AllowPreview:  true,
	})
	if err != nil {
		t.Fatalf("创建分享失败: %v", err)
	}
	if share.RootPath != "/keyed.bin" {
		t.Fatalf("分享应返回相对路径，实际 %s", share.RootPath)
	}
	storedShare, err := store.GetShare(ctx, f.db.R(), share.ID)
	if err != nil {
		t.Fatalf("读取分享记录失败: %v", err)
	}
	if storedShare.RootPath != "/a/b/keyed.bin" {
		t.Fatalf("分享应落库为绝对路径，实际 %s", storedShare.RootPath)
	}

	if _, err := f.svc.CreateShare(ctx, f.user, CreateShareRequest{
		Path:       "/a/bc",
		Kind:       store.ShareFolder,
		AccessMode: store.AccessPublic,
	}); err != nil {
		t.Fatalf("创建另一工作区的分享失败: %v", err)
	}
	listed, err := f.svc.ListShares(ctx, keyed, 100, 0)
	if err != nil {
		t.Fatalf("列出分享失败: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != share.ID || listed[0].RootPath != "/keyed.bin" {
		t.Fatalf("Key 只能看到自己工作区的分享，实际 %+v", listed)
	}
	otherShares, err := f.svc.ListShares(ctx, other, 100, 0)
	if err != nil {
		t.Fatalf("另一工作区列分享失败: %v", err)
	}
	if len(otherShares) != 1 || otherShares[0].RootPath != "/" {
		t.Fatalf("另一工作区只应看到自己的分享，实际 %+v", otherShares)
	}

	link, err := f.svc.CreateDirectLink(ctx, keyed, CreateDirectLinkRequest{Path: "/keyed.bin"})
	if err != nil {
		t.Fatalf("创建直链失败: %v", err)
	}
	if link.SourcePath != "/keyed.bin" {
		t.Fatalf("直链应返回相对路径，实际 %s", link.SourcePath)
	}
	storedLink, err := store.GetDirectLink(ctx, f.db.R(), link.ID)
	if err != nil {
		t.Fatalf("读取直链记录失败: %v", err)
	}
	if storedLink.SourcePath != "/a/b/keyed.bin" {
		t.Fatalf("直链应落库为绝对路径，实际 %s", storedLink.SourcePath)
	}
	if _, err := f.svc.DirectLinkTarget(ctx, keyed, link.ID); err != nil {
		t.Fatalf("工作区内读取直链目标失败: %v", err)
	}
	// 工作区外有同内容副本且路径排序更靠前时，直链仍应解析到工作区内文件。
	f.ingest("/a/aa/keyed.bin", contentB)
	target, err := f.svc.DirectLinkTarget(ctx, keyed, link.ID)
	if err != nil || target != "/keyed.bin" {
		t.Fatalf("直链应命中工作区内文件，实际 %q, %v", target, err)
	}
	if _, err := f.svc.DirectLinkTarget(ctx, other, link.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨工作区读取直链目标应 404，实际 %v", err)
	}
	if err := f.svc.DeleteDirectLink(ctx, other, link.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨工作区删除直链应 404，实际 %v", err)
	}
	otherLinks, err := f.svc.ListDirectLinks(ctx, other, 100, 0)
	if err != nil {
		t.Fatalf("另一工作区列直链失败: %v", err)
	}
	if len(otherLinks) != 0 {
		t.Fatalf("另一工作区不应看到直链，实际 %+v", otherLinks)
	}

	// 归档：Key 删掉的文件只出现在自己的工作区归档里。
	f.ingest("/a/b/old.bin", contentA)
	if err := f.svc.DeleteNode(ctx, keyed, "/old.bin"); err != nil {
		t.Fatalf("删除工作区内文件失败: %v", err)
	}
	batches, _, err := f.svc.ListUserArchive(ctx, keyed, 100, 0)
	if err != nil {
		t.Fatalf("列出归档失败: %v", err)
	}
	if len(batches) != 1 || batches[0].RootPath != "/old.bin" {
		t.Fatalf("归档应映射成相对路径，实际 %+v", batches)
	}
	if err := f.svc.RestoreArchiveBatch(ctx, other, batches[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨工作区恢复归档应 404，实际 %v", err)
	}
	if err := f.svc.RestoreArchiveBatch(ctx, keyed, batches[0].ID); err != nil {
		t.Fatalf("工作区内恢复归档失败: %v", err)
	}
	if _, err := store.GetNode(ctx, f.db.R(), f.user.UserID(), "/a/b/old.bin"); err != nil {
		t.Fatalf("恢复后的文件应回到工作区原路径: %v", err)
	}

	// 票据：另一工作区的 Key 不能结算工作区内签发的票据。
	ticketPayload := bytes.Repeat([]byte{0x11}, 4096)
	f.ingest("/a/bc/ticket.bin", ticketPayload)
	plan, err := f.svc.PrepareDelivery(ctx, other, DeliveryTarget{
		OwnerUserID: f.user.UserID(),
		Path:        "/ticket.bin",
	}, DeliveryRequest{Purpose: store.PurposeDownload})
	if err != nil {
		t.Fatalf("准备工作区票据失败: %v", err)
	}
	if err := f.svc.SettleDelivery(ctx, keyed, plan.TicketID, int64(len(ticketPayload))); !errors.Is(err, ErrForbidden) {
		t.Fatalf("跨工作区结算票据应 403，实际 %v", err)
	}
	if err := f.svc.SettleDelivery(ctx, other, plan.TicketID, int64(len(ticketPayload))); err != nil {
		t.Fatalf("工作区内结算票据失败: %v", err)
	}

	// 工作区目录改名后，Key 的可见范围随目录迁移，已有直链与归档不会因此失踪。
	f.ingest("/a/b/keep.bin", contentA)
	if err := f.svc.DeleteNode(ctx, keyed, "/keep.bin"); err != nil {
		t.Fatalf("删除待归档文件失败: %v", err)
	}
	if _, err := f.svc.RenameNode(ctx, f.user, "/a/b", "renamed-ws"); err != nil {
		t.Fatalf("重命名工作区目录失败: %v", err)
	}
	renamedKey := workspaceKey(f.user, "/a/renamed-ws")
	links, err := f.svc.ListDirectLinks(ctx, renamedKey, 100, 0)
	if err != nil {
		t.Fatalf("改名后列直链失败: %v", err)
	}
	if len(links) != 1 || links[0].ID != link.ID || links[0].SourcePath != "/keyed.bin" {
		t.Fatalf("改名后仍应看到原直链，实际 %+v", links)
	}
	storedLink, err = store.GetDirectLink(ctx, f.db.R(), link.ID)
	if err != nil {
		t.Fatalf("读取改名后的直链失败: %v", err)
	}
	if storedLink.SourcePath != "/a/renamed-ws/keyed.bin" {
		t.Fatalf("直链来源路径应随目录迁移，实际 %s", storedLink.SourcePath)
	}
	batches, _, err = f.svc.ListUserArchive(ctx, renamedKey, 100, 0)
	if err != nil {
		t.Fatalf("改名后列归档失败: %v", err)
	}
	if len(batches) != 1 || batches[0].RootPath != "/keep.bin" {
		t.Fatalf("改名后仍应看到原归档，实际 %+v", batches)
	}
	if err := f.svc.RestoreArchiveBatch(ctx, renamedKey, batches[0].ID); err != nil {
		t.Fatalf("改名后恢复归档失败: %v", err)
	}
	if _, err := store.GetNode(ctx, f.db.R(), f.user.UserID(), "/a/renamed-ws/keep.bin"); err != nil {
		t.Fatalf("归档应恢复到改名后的工作区路径: %v", err)
	}
}
