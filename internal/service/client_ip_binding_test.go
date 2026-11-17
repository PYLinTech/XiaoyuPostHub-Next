package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/netip"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// testPublicKey 生成一枚合法的 RSA-OAEP 临时公钥（base64 SPKI DER），
// 让交付计划走密文通道，从而可以测试客户端结算。
func testPublicKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// TestClientIPDeclarationBindsVisitorPrefix 验证 API Key 代传访客 IP 后，
// 票据按访客网段绑定（而非后端网段），计费归属仍记后端属主；123 回源鉴权
// 放行访客网段、拒绝后端网段与缺失地址。
func TestClientIPDeclarationBindsVisitorPrefix(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyDown, wire*8)

	backend := workspaceKey(f.user, "/") // API Key 身份，ClientIP 仍是 10.1.2.3
	target := DeliveryTarget{OwnerUserID: f.user.UserID(), Path: f.target.Path}
	visitor := "203.0.113.7"

	plan, err := f.svc.PrepareDelivery(ctx, backend, target, DeliveryRequest{
		Purpose: store.PurposeDownload, ForceProxy: true, ClientIP: visitor,
	})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.GetTicket(ctx, f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := f.svc.Auth.IPPrefix(ctx, netip.MustParseAddr(visitor))
	if ticket.IPPrefix != wantPrefix || ticket.ClientIP != visitor {
		t.Fatalf("票据未按访客网段绑定: prefix=%s ip=%s want=%s", ticket.IPPrefix, ticket.ClientIP, wantPrefix)
	}
	if ticket.IPPrefix == backend.IPPrefix {
		t.Fatal("票据仍绑在后端网段")
	}
	if ticket.BilledUserID != f.user.UserID() {
		t.Fatalf("计费归属被改写: %+v", ticket)
	}

	// 同一访客网段（/24）内的地址放行。
	if _, err := f.svc.AuthorizeCDN(ctx, ticket.ID, "203.0.113.99"); err != nil {
		t.Fatalf("访客网段内回源被拒: %v", err)
	}
	// 后端自身网段不在访客前缀内，拒绝。
	if _, err := f.svc.AuthorizeCDN(ctx, ticket.ID, "198.51.100.10"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("后端网段应被拒: %v", err)
	}
	// 缺 remote_addr 一律拒绝。
	if _, err := f.svc.AuthorizeCDN(ctx, ticket.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("缺来源应被拒: %v", err)
	}
}

// TestAPIKeyOwnerSettlesDelegatedTicket 验证票据绑在访客网段时，后端 API Key
// 属主仍能跨网段回执结算（不再要求与申请网段一致）。
func TestAPIKeyOwnerSettlesDelegatedTicket(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyDown, wire*8)
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyDeliveryProxyDecrypt: "false",
	}, 0); err != nil {
		t.Fatal(err)
	}

	backend := workspaceKey(f.user, "/")
	target := DeliveryTarget{OwnerUserID: f.user.UserID(), Path: f.target.Path}
	plan, err := f.svc.PrepareDelivery(ctx, backend, target, DeliveryRequest{
		Purpose: store.PurposeDownload, ForceProxy: true,
		ClientPublicKey: testPublicKey(t), ClientIP: "203.0.113.7",
	})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.GetTicket(ctx, f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.DeliveryMode == store.TicketProxyDecrypt {
		t.Fatalf("应为密文票据才能测客户端结算: %+v", ticket)
	}
	if ticket.IPPrefix == backend.IPPrefix {
		t.Fatal("前置条件失败：票据未绑访客网段")
	}
	if err := f.svc.SettleDeliveryFromClient(ctx, backend, ticket.ID, true); err != nil {
		t.Fatalf("后端属主跨网段结算被拒: %v", err)
	}
}

// TestClientIPInvalidRejected 验证非法 clientIp 返回 400，且不泄漏配额。
func TestClientIPInvalidRejected(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyDown, wire*8)

	backend := workspaceKey(f.user, "/")
	target := DeliveryTarget{OwnerUserID: f.user.UserID(), Path: f.target.Path}
	_, err := f.svc.PrepareDelivery(ctx, backend, target, DeliveryRequest{
		Purpose: store.PurposeDownload, ForceProxy: true, ClientIP: "not-an-ip",
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("非法 clientIp 应返回 400: %v", err)
	}
	if f.used(store.ScopeTrafficDown, f.svc.deliveryQuotaKey(backend, store.Now())) != 0 {
		t.Fatal("非法请求泄漏了预扣配额")
	}
}

// TestInvalidTicketPrefixIsForbidden 验证票据来源前缀损坏时按 403 拒绝；
// 这是部署侧的来源解析/可信代理配置问题，不应返回可重试的 503。
func TestInvalidTicketPrefixIsForbidden(t *testing.T) {
	f := newTrafficFixture(t)
	err := f.svc.authorizeTicketActor(context.Background(), store.Ticket{
		ActorType: store.ActorGuest,
		IPPrefix:  "unknown",
	}, f.guest)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("非法票据来源前缀应返回 403: %v", err)
	}
}

// TestClientIPIgnoredForSession 验证非 API Key 身份代传的 clientIp 被忽略，
// 票据仍按会话自身网段绑定。
func TestClientIPIgnoredForSession(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyDown, wire*8)

	target := DeliveryTarget{OwnerUserID: f.user.UserID(), Path: f.target.Path}
	plan, err := f.svc.PrepareDelivery(ctx, f.user, target, DeliveryRequest{
		Purpose: store.PurposeDownload, ForceProxy: true, ClientIP: "203.0.113.7",
	})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.GetTicket(ctx, f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.IPPrefix != f.user.IPPrefix || ticket.ClientIP != f.user.ClientIP.String() {
		t.Fatalf("会话身份应忽略代传 IP: %+v", ticket)
	}
}
