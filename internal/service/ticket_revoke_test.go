package service

// 票据吊销与来源校验的回归测试：
//   - CDN 回源鉴权在来源缺失时必须拒绝（防伪造），不再"默认放行"；
//   - 分享过期（即时改短或自然到期）必须吊销在途票据；
//   - 提取码更改必须吊销在途票据；
//   - 取件码为六位。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// prepareShareDelivery 为分享根签发一张真实票据。
func prepareShareDelivery(t *testing.T, f *shareFixture, shareID string) store.Ticket {
	t.Helper()
	plan, err := f.svc.PrepareDelivery(context.Background(), f.user, DeliveryTarget{
		OwnerUserID: f.user.User.ID,
		Path:        "/a/sample.bin",
		ShareID:     shareID,
	}, DeliveryRequest{Purpose: store.PurposeDownload, ForceProxy: true})
	if err != nil {
		t.Fatalf("准备交付失败: %v", err)
	}
	ticket, err := store.GetTicket(context.Background(), f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatalf("读取票据失败: %v", err)
	}
	return ticket
}

// TestAuthorizeCDNDeniesUnknownSource：票据绑定了来源前缀而上游无法告知
// 原始地址时，回源鉴权必须拒绝，而不是放行。
func TestAuthorizeCDNDeniesUnknownSource(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	completeUpload(t, f, contentA, "sample.bin")

	// 来源一致：放行。
	ok := prepareShareDelivery(t, f, "")
	if _, err := f.svc.AuthorizeCDN(context.Background(), ok.ID, "10.1.2.99"); err != nil {
		t.Fatalf("来源一致应放行: %v", err)
	}
	// 来源缺失：拒绝。
	unknown := prepareShareDelivery(t, f, "")
	if _, err := f.svc.AuthorizeCDN(context.Background(), unknown.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("来源缺失应拒绝，实得: %v", err)
	}
	// 来源不一致：拒绝。
	other := prepareShareDelivery(t, f, "")
	if _, err := f.svc.AuthorizeCDN(context.Background(), other.ID, "10.9.9.9"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("来源不一致应拒绝，实得: %v", err)
	}
}

// TestUpdateShareToExpiredRevokesTickets：把有效期改到过去必须立即吊销
// 该分享的在途票据——否则改短有效期形同虚设。
func TestUpdateShareToExpiredRevokesTickets(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	completeUpload(t, f, contentA, "sample.bin")

	share := f.share("/a/sample.bin", store.ShareFile, store.AccessPublic, "", false)
	ticket := prepareShareDelivery(t, f, share.ID)

	past := store.Now() - 1
	if _, err := f.svc.UpdateShare(context.Background(), f.user, share.ID,
		UpdateShareRequest{ExpiresAt: &past}); err != nil {
		t.Fatalf("更新有效期失败: %v", err)
	}
	current, err := store.GetTicket(context.Background(), f.db.R(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Revoked {
		t.Fatal("有效期改短为已过期后，在途票据应被吊销")
	}
}

// TestUpdateSharePasswordChangeRevokesTickets：提取码更改必须吊销在途
// 票据——旧提取码通过的那批票据不能在新提取码生效后继续取数。
func TestUpdateSharePasswordChangeRevokesTickets(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	completeUpload(t, f, contentA, "sample.bin")

	share := f.share("/a/sample.bin", store.ShareFile, store.AccessPassword, "ABCD", false)
	ticket := prepareShareDelivery(t, f, share.ID)

	newPwd := "EFGH"
	if _, err := f.svc.UpdateShare(context.Background(), f.user, share.ID,
		UpdateShareRequest{Password: &newPwd}); err != nil {
		t.Fatalf("更新提取码失败: %v", err)
	}
	current, err := store.GetTicket(context.Background(), f.db.R(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Revoked {
		t.Fatal("提取码更改后，在途票据应被吊销")
	}
}

// TestMaintenanceRevokesExpiredShareTickets：自然过期的分享由维护任务
// 兜底吊销在途票据——没人访问过期链接时，票据也不能活过分享的时效。
func TestMaintenanceRevokesExpiredShareTickets(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	completeUpload(t, f, contentA, "sample.bin")

	share, err := f.svc.CreateShare(context.Background(), f.user, CreateShareRequest{
		Path:          "/a/sample.bin",
		Kind:          store.ShareFile,
		AccessMode:    store.AccessPublic,
		AllowDownload: true,
		ExpiresAt:     store.Now() + 3600,
	})
	if err != nil {
		t.Fatalf("创建分享失败: %v", err)
	}
	ticket := prepareShareDelivery(t, f, share.ID)

	// 绕过服务层把有效期拨到过去（模拟自然到期）。
	if _, err := f.db.W().ExecContext(context.Background(),
		`UPDATE shares SET expires_at = ? WHERE id = ?`, store.Now()-1, share.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.RunMaintenance(context.Background())

	current, err := store.GetTicket(context.Background(), f.db.R(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Revoked {
		t.Fatal("维护应吊销已过期分享的在途票据")
	}
}

// TestPickupCodeIsSixChars：取件码必须是六位，且只能取自剔除易混字符后的
// 28 字符规范字母表（数字 2-8 + 大写字母去掉 G/I/L/O/Q）。
func TestPickupCodeIsSixChars(t *testing.T) {
	f := newShareFixture(t)
	share := f.share("/a", store.ShareFolder, store.AccessPublic, "", false)
	const allowed = "2345678ABCDEFHJKMNPRSTUVWXYZ"
	// 连续生成一批，既校验位长，也钉住随机采样不会吐出表外字符。
	for i := 0; i < 50; i++ {
		pickup, err := f.svc.CreatePickupCode(context.Background(), f.user, share.ID, 1)
		if err != nil {
			t.Fatalf("创建取件码失败: %v", err)
		}
		if len(pickup.Code) != 6 {
			t.Fatalf("取件码应为 6 位，实得 %d 位（%q）", len(pickup.Code), pickup.Code)
		}
		for _, r := range pickup.Code {
			if !strings.ContainsRune(allowed, r) {
				t.Fatalf("取件码 %q 含规范字符集外的字符 %q（禁止 0/1/9 与 O/I/L/Q/G）", pickup.Code, r)
			}
		}
	}
}
