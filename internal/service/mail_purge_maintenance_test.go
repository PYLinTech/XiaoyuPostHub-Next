package service

import (
	"context"
	"testing"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// forceMailPurgeAt 直接改写归属行的计划清理时间（测试辅助）。
func (f *inboundFixture) forceMailPurgeAt(t *testing.T, id string, uid int64, at int64) {
	t.Helper()
	if _, err := f.db.W().ExecContext(context.Background(),
		`UPDATE mailboxes SET purge_at = ? WHERE user_id = ? AND message_id = ?`, at, uid, id); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenancePurgesExpiredMail(t *testing.T) {
	f := newInboundFixture(t)
	ctx := context.Background()
	if err := f.svc.Settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyMailArchiveRetention: "1h",
	}, 0); err != nil {
		t.Fatal(err)
	}
	id := receiveRawMail(t, f, "m3-expire@ext.example", []string{"alice@example.com"})
	if err := f.svc.ArchiveMyMail(ctx, f.user, id); err != nil {
		t.Fatal(err)
	}

	used, _ := store.GetCounter(ctx, f.db.R(), store.ScopeMailStorage,
		store.UserCounterKey(f.uid1, ""))
	if used == 0 {
		t.Fatal("前置：归档期间应占配额")
	}
	// 未到期不清。
	if r := f.svc.RunMaintenance(ctx); r.PurgedMail != 0 {
		t.Fatalf("未到期不应清理，PurgedMail=%d failures=%d", r.PurgedMail, r.Failures)
	}

	// 排到过去后下一轮清理。
	f.forceMailPurgeAt(t, id, f.uid1, time.Now().Add(-time.Minute).Unix())
	r := f.svc.RunMaintenance(ctx)
	if r.Failures != 0 {
		t.Fatalf("清理不应有失败: %+v", r)
	}
	if r.PurgedMail != 1 {
		t.Fatalf("应清理 1 封，得到 %d", r.PurgedMail)
	}
	used, _ = store.GetCounter(ctx, f.db.R(), store.ScopeMailStorage,
		store.UserCounterKey(f.uid1, ""))
	if used != 0 {
		t.Fatalf("清理后配额应归零，得到 %d", used)
	}
	box, err := store.GetMailboxForUser(ctx, f.db.R(), f.uid1, id)
	if err != nil || box.Status != store.MailboxStatusReleased {
		t.Fatalf("归属应为 released: %+v err=%v", box, err)
	}
	// 幂等：第二轮不再处理。
	if r2 := f.svc.RunMaintenance(ctx); r2.PurgedMail != 0 {
		t.Fatalf("第二轮不应重复清理，PurgedMail=%d", r2.PurgedMail)
	}
}
