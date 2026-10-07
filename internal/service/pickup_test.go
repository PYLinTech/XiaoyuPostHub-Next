package service

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// TestPickupPeekDoesNotConsumeUse 钉住取件码的记账语义：查看不扣次数，取数才扣。
//
// 这条断言看似琐碎，但它对应一个会让功能完全不可用的缺陷：如果"查看"也扣一次，
// 那么 maxUses=1（创建接口的默认值）的取件码在用户点下下载之前就已经用尽，
// 表现为"提取成功但永远下载不了"。用户视角里"提取"就是一个动作，
// 记账因此也只能有一次。
func TestPickupPeekDoesNotConsumeUse(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.putFile("/a/f.txt", "checksum-pickup-peek")
	share := f.share("/a/f.txt", store.ShareFile, store.AccessPublic, "", true)

	pc, err := f.svc.CreatePickupCode(ctx, f.user, share.ID, 1)
	if err != nil {
		t.Fatalf("创建取件码失败: %v", err)
	}
	if pc.MaxUses != 1 {
		t.Fatalf("前置条件不成立：期望 maxUses=1，实得 %d", pc.MaxUses)
	}

	guest := auth.Principal{
		Actor:    store.ActorGuest,
		ClientIP: netip.MustParseAddr("203.0.113.7"),
	}
	guest.IPPrefix = f.svc.Auth.IPPrefix(ctx, guest.ClientIP)

	// 查看若干次都不应消耗额度。
	for i := 0; i < 3; i++ {
		if _, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest); err != nil {
			t.Fatalf("第 %d 次查看应当成功: %v", i+1, err)
		}
	}
	if used := f.pickupUsedCount(pc.Code); used != 0 {
		t.Fatalf("查看不应消耗使用次数，实得 used_count=%d", used)
	}

	// 真正取数时由 Claim 核销一次（生产路径：Peek → PrepareDelivery → Claim）。
	if err := f.svc.ClaimPickupUseForActor(ctx, pc.Code, guest); err != nil {
		t.Fatalf("首次取数核销应当成功: %v", err)
	}
	if used := f.pickupUsedCount(pc.Code); used != 1 {
		t.Fatalf("取数应消耗一次使用次数，实得 used_count=%d", used)
	}

	// 额度用尽后，核销与查看都必须被拒——查看不能变成绕过额度的手段。
	if err := f.svc.ClaimPickupUseForActor(ctx, pc.Code, guest); err == nil {
		t.Fatal("额度已用尽时取数核销应当失败")
	}
	if _, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest); err == nil {
		t.Fatal("额度已用尽时查看也应当失败")
	}
}

func (f *shareFixture) pickupUsedCount(code string) int {
	f.t.Helper()
	pc, err := store.GetPickupCode(context.Background(), f.db.R(), code)
	if err != nil {
		f.t.Fatalf("读取取件码失败: %v", err)
	}
	return pc.UsedCount
}

// TestPickupUseIsNotBurnedByFailedDelivery 交付失败不得消耗取件码额度。
//
// 交付流程是 Peek（校验）→ PrepareDelivery（准备）→ ClaimPickupUseForActor（核销）。
// 这条断言守的是"核销必须在最后"：曾实现过一次"先核销再准备"，结果是
// 对目录分享（对象不可交付）发起下载，用户什么都没拿到，取件码却已作废——
// 而 maxUses 的默认值就是 1，等价于一次误点就毁掉一张码。
func TestPickupUseIsNotBurnedByFailedDelivery(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	// 分享一个目录：目录没有可交付的内容，准备阶段必定失败。
	f.mkdir("/d")
	share := f.share("/d", store.ShareFolder, store.AccessPublic, "", true)

	pc, err := f.svc.CreatePickupCode(ctx, f.user, share.ID, 1)
	if err != nil {
		t.Fatalf("创建取件码失败: %v", err)
	}

	guest := auth.Principal{Actor: store.ActorGuest, ClientIP: netip.MustParseAddr("203.0.113.9")}
	guest.IPPrefix = f.svc.Auth.IPPrefix(ctx, guest.ClientIP)

	target, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest)
	if err != nil {
		t.Fatalf("校验应当通过: %v", err)
	}
	if _, err := f.svc.PrepareDelivery(ctx, guest, target, DeliveryRequest{Purpose: store.PurposeDownload}); err == nil {
		t.Fatal("前置条件不成立：目录分享的交付准备应当失败")
	}

	if used := f.pickupUsedCount(pc.Code); used != 0 {
		t.Fatalf("交付失败不应消耗额度，实得 used_count=%d", used)
	}

	// 真正核销之后才扣一次。
	if err := f.svc.ClaimPickupUseForActor(ctx, pc.Code, guest); err != nil {
		t.Fatalf("核销应当成功: %v", err)
	}
	if used := f.pickupUsedCount(pc.Code); used != 1 {
		t.Fatalf("核销后 used_count 应为 1，实得 %d", used)
	}
	// 额度已满，再核销必须失败（并发下只有一次能命中）。
	if err := f.svc.ClaimPickupUseForActor(ctx, pc.Code, guest); err == nil {
		t.Fatal("额度用尽后核销应当失败")
	}
}

// TestPickupTTLIsGlobalAndRealtime 钉住取件码有效期的全局、动态语义：
//
// 有效期由管理员统一配置（默认 8 小时、0 表示永久），不落库、按创建时间动态
// 计算。因此从永久缩短为 8 小时后，创建超过 8 小时的存量码必须立即失效；
// 再放宽回永久，同一批码立即恢复可用。这条测试守的就是"实时生效、无需迁移"。
func TestPickupTTLIsGlobalAndRealtime(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.putFile("/a/f.txt", "checksum-pickup-ttl")
	share := f.share("/a/f.txt", store.ShareFile, store.AccessPublic, "", true)
	guest := auth.Principal{Actor: store.ActorGuest, ClientIP: netip.MustParseAddr("203.0.113.11")}
	guest.IPPrefix = f.svc.Auth.IPPrefix(ctx, guest.ClientIP)

	setTTL := func(hours int64) {
		t.Helper()
		if err := f.svc.Settings.Set(ctx, settings.KeySharePickupTTLHours, strconv.FormatInt(hours, 10), 0); err != nil {
			t.Fatalf("设置取件码有效期为 %d 小时失败: %v", hours, err)
		}
	}

	// 默认 8 小时：创建响应里给出按创建时间算出的绝对到期点。
	pc, err := f.svc.CreatePickupCode(ctx, f.user, share.ID, 1)
	if err != nil {
		t.Fatalf("创建取件码失败: %v", err)
	}
	if want := pc.CreatedAt + int64((8*time.Hour)/time.Second); pc.ExpiresAt != want {
		t.Fatalf("默认 8 小时：期望到期点 %d，实得 %d", want, pc.ExpiresAt)
	}
	if _, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest); err != nil {
		t.Fatalf("有效期内的取件码查看应当成功: %v", err)
	}

	// 切到永久：列表里该码不再显示到期时间，查看仍可用。
	setTTL(0)
	items, err := f.svc.ListPickupCodes(ctx, f.user, share.ID)
	if err != nil {
		t.Fatalf("列出取件码失败: %v", err)
	}
	var listed store.PickupCode
	for _, item := range items {
		if item.Code == pc.Code {
			listed = item
		}
	}
	if listed.Code == "" {
		t.Fatal("列表中找不到刚创建的取件码")
	}
	if listed.ExpiresAt != 0 {
		t.Fatalf("永久模式下列表到期点应为 0，实得 %d", listed.ExpiresAt)
	}
	if _, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest); err != nil {
		t.Fatalf("永久有效期下查看应当成功: %v", err)
	}

	// 把创建时间回拨到 10 小时前，模拟"缩短配置之前就已存在的老码"。
	oldCreated := store.Now() - int64((10*time.Hour)/time.Second)
	if _, err := f.db.W().ExecContext(ctx,
		`UPDATE pickup_codes SET created_at = ? WHERE code = ?`, oldCreated, pc.Code); err != nil {
		t.Fatalf("回拨取件码创建时间失败: %v", err)
	}

	// 永久时超龄码仍可用；从永久切到 8 小时后立即失效（查看与核销都拒绝）。
	if _, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest); err != nil {
		t.Fatalf("永久有效期下老码仍应可用: %v", err)
	}
	setTTL(8)
	if _, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest); err == nil {
		t.Fatal("缩短为 8 小时后，创建已超 10 小时的取件码必须立即失效")
	}
	if err := f.svc.ClaimPickupUseForActor(ctx, pc.Code, guest); err == nil {
		t.Fatal("超龄取件码的核销也必须失败，且不得消耗任何额度")
	}
	if used := f.pickupUsedCount(pc.Code); used != 0 {
		t.Fatalf("超龄码核销失败后不应有记账，实得 used_count=%d", used)
	}

	// 放宽回永久：有效性始终跟随当前配置，同一码立即恢复可用。
	setTTL(0)
	if _, _, err := f.svc.PeekPickupCode(ctx, pc.Code, guest); err != nil {
		t.Fatalf("恢复永久有效期后老码应当重新可用: %v", err)
	}
}

// TestPickupCodeIsCaseInsensitive 钉住取件码大小写不区分的入口语义：
//
// 码值统一以大写存储，访客无论抄成小写、混写还是带了首尾空白，都必须等价命中
// 同一个码；码值里只可能出现剔除易混字符后的规范字母表，因此用户输入被剔除的
// 字符（0、1、9、O、I、L、Q、G 及其小写）时按"码不存在"拒绝，而不是模糊放行。
func TestPickupCodeIsCaseInsensitive(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.putFile("/a/f.txt", "checksum-pickup-case")
	share := f.share("/a/f.txt", store.ShareFile, store.AccessPublic, "", true)
	guest := auth.Principal{Actor: store.ActorGuest, ClientIP: netip.MustParseAddr("203.0.113.22")}
	guest.IPPrefix = f.svc.Auth.IPPrefix(ctx, guest.ClientIP)

	pc, err := f.svc.CreatePickupCode(ctx, f.user, share.ID, 1)
	if err != nil {
		t.Fatalf("创建取件码失败: %v", err)
	}
	if pc.Code != strings.ToUpper(pc.Code) {
		t.Fatalf("取件码必须以大写规范形式存储，实得 %q", pc.Code)
	}

	for _, variant := range []string{
		strings.ToLower(pc.Code),
		strings.ToLower(pc.Code[:3]) + pc.Code[3:],
		"  " + strings.ToLower(pc.Code) + "\t",
	} {
		target, _, err := f.svc.PeekPickupCode(ctx, variant, guest)
		if err != nil {
			t.Fatalf("取件码变体 %q 应当等价命中 %q，却失败: %v", variant, pc.Code, err)
		}
		if target.PickupCode != pc.Code {
			t.Fatalf("命中后应回传规范大写码 %q，实得 %q", pc.Code, target.PickupCode)
		}
	}
}
