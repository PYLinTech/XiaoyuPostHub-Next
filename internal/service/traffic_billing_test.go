package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

type trafficFixture struct {
	*shareFixture
	share  store.Share
	target DeliveryTarget
	file   store.File
	guest  auth.Principal
}

func newTrafficFixture(t *testing.T) *trafficFixture {
	t.Helper()
	ctx := context.Background()
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	node := completeUpload(t, f, contentA, "sample.bin")
	file, err := store.GetFile(ctx, f.db.R(), node.FileChecksum)
	if err != nil {
		t.Fatal(err)
	}
	share := f.share(node.LogicalPath, store.ShareFile, store.AccessPublic, "", false)
	group, err := store.GetGroup(ctx, f.db.R(), perm.GroupGuest)
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr("203.0.113.7")
	guest := auth.Principal{Actor: store.ActorGuest, Group: group, ClientIP: ip, IPPrefix: f.svc.Auth.IPPrefix(ctx, ip)}
	return &trafficFixture{shareFixture: f, share: share, target: DeliveryTarget{OwnerUserID: f.user.UserID(), Path: node.LogicalPath, ShareID: share.ID}, file: file, guest: guest}
}
func (f *trafficFixture) quota(group, key string, value int64) {
	f.t.Helper()
	if err := store.SetGroupQuota(context.Background(), f.db.W(), group, key, value); err != nil {
		f.t.Fatal(err)
	}
}
func (f *trafficFixture) prepare(actor auth.Principal, purpose store.Purpose) (store.Ticket, error) {
	plan, err := f.svc.PrepareDelivery(context.Background(), actor, f.target, DeliveryRequest{Purpose: purpose, ForceProxy: true})
	if err != nil {
		return store.Ticket{}, err
	}
	return store.GetTicket(context.Background(), f.db.R(), plan.TicketID)
}
func (f *trafficFixture) used(scope store.CounterScope, key string) int64 {
	f.t.Helper()
	used, err := store.GetCounter(context.Background(), f.db.R(), scope, key)
	if err != nil {
		f.t.Fatal(err)
	}
	return used
}

func TestShareTrafficPriorityAndFallback(t *testing.T) {
	for _, purpose := range []store.Purpose{store.PurposeDownload, store.PurposePreview} {
		t.Run(string(purpose), func(t *testing.T) {
			f := newTrafficFixture(t)
			ctx := context.Background()
			wire := f.file.PanSizeWire
			f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, wire)
			// Guest personal quota = zero; sponsor can still cover the first transfer.
			f.quota(perm.GroupGuest, store.QuotaTrafficDailyDown, 0)
			ticket, err := f.prepare(f.guest, purpose)
			if err != nil {
				t.Fatal(err)
			}
			if ticket.QuotaScope != store.ScopeTrafficShare || ticket.BilledUserID != f.user.UserID() || ticket.UserID != 0 || ticket.ActorType != store.ActorGuest {
				t.Fatalf("incorrect billing/identity: %+v", ticket)
			}
			personalKey := f.svc.deliveryQuotaKey(f.guest, ticket.CreatedAt)
			if f.used(store.ScopeTrafficDown, personalKey) != 0 || f.used(store.ScopeTrafficShare, ticket.QuotaKey) != wire {
				t.Fatal("sponsored traffic charged visitor")
			}
			if _, err := f.prepare(f.guest, purpose); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("both exhausted: %v", err)
			}
			f.quota(perm.GroupGuest, store.QuotaTrafficDailyDown, wire)
			fallback, err := f.prepare(f.guest, purpose)
			if err != nil {
				t.Fatal(err)
			}
			if fallback.QuotaScope != store.ScopeTrafficDown || fallback.BilledUserID != 0 || fallback.QuotaKey != personalKey {
				t.Fatalf("fallback: %+v", fallback)
			}
			if f.used(store.ScopeTrafficShare, ticket.QuotaKey) != wire || f.used(store.ScopeTrafficDown, personalKey) != wire {
				t.Fatal("fallback double charged or wrong bucket")
			}
			if err := f.svc.CancelDelivery(ctx, f.guest, fallback.ID); err != nil {
				t.Fatal(err)
			}
			if f.used(store.ScopeTrafficDown, personalKey) != 0 || f.used(store.ScopeTrafficShare, ticket.QuotaKey) != wire {
				t.Fatal("fallback refund affected sponsor")
			}
			if err := f.svc.CancelDelivery(ctx, f.guest, ticket.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.CancelDelivery(ctx, f.guest, ticket.ID); err != nil {
				t.Fatal(err)
			}
			if f.used(store.ScopeTrafficShare, ticket.QuotaKey) != 0 {
				t.Fatal("sponsor refund failed")
			}
		})
	}
}

func TestShareTrafficOwnerAndRecipientBypass(t *testing.T) {
	f := newTrafficFixture(t)
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, 0)
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyDown, wire)
	self, err := f.prepare(f.user, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}
	if self.QuotaScope != store.ScopeTrafficDown {
		t.Fatal("owner must use personal quota")
	}
	if _, err := f.prepare(f.user, store.PurposePreview); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("zero/filled personal quota: %v", err)
	}
	admin := f.adminUser("recipient-admin")
	f.quota(perm.GroupAdmin, store.QuotaTrafficDailyDown, 0)
	fallback, err := f.prepare(admin, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.QuotaScope != store.ScopeTrafficDown || fallback.BilledUserID != admin.UserID() {
		t.Fatalf("recipient bypass fallback: %+v", fallback)
	}
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, wire)
	covered, err := f.prepare(admin, store.PurposePreview)
	if err != nil {
		t.Fatal(err)
	}
	if covered.QuotaScope != store.ScopeTrafficShare || covered.BilledUserID != f.user.UserID() {
		t.Fatal("recipient bypass must not change sponsor priority")
	}
	// Missing share quota means unlimited, independently of owner's personal quota.
	if err := store.DeleteGroupQuota(context.Background(), f.db.W(), perm.GroupNormal, store.QuotaTrafficDailyShare); err != nil {
		t.Fatal(err)
	}
	f.quota(perm.GroupGuest, store.QuotaTrafficDailyDown, 0)
	if _, err := f.prepare(f.guest, store.PurposeDownload); err != nil {
		t.Fatalf("unlimited sponsor: %v", err)
	}
}

func TestShareTrafficSettlementKeepsActorAndBilling(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, wire)
	ticket, err := f.prepare(f.guest, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}
	// A ticket remains authorized to the visitor, never to the payer.
	if err := f.svc.SettleDelivery(ctx, f.user, ticket.ID, 500); !errors.Is(err, ErrForbidden) {
		t.Fatalf("payer used recipient's ticket: %v", err)
	}
	if err := f.svc.SettleDelivery(ctx, f.guest, ticket.ID, 500); err != nil {
		t.Fatal(err)
	}
	used := f.used(store.ScopeTrafficShare, ticket.QuotaKey)
	if used <= 0 || used >= wire {
		t.Fatalf("partial refund: %d/%d", used, wire)
	}
	if err := f.svc.SettleDelivery(ctx, f.guest, ticket.ID, 500); err != nil {
		t.Fatal(err)
	}
	if f.used(store.ScopeTrafficShare, ticket.QuotaKey) != used {
		t.Fatal("settlement not idempotent")
	}
	if err := f.db.Traffic().Flush(ctx); err != nil {
		t.Fatal(err)
	}
	logs, total, err := store.ListTrafficLogs(ctx, f.db.R(), store.TrafficFilter{Action: "download", BilledUserID: f.user.UserID()})
	if err != nil || total != 1 {
		t.Fatalf("audit: %+v %d %v", logs, total, err)
	}
	l := logs[0]
	if l.ActorType != store.ActorGuest || l.UserID != 0 || l.BilledUserID != f.user.UserID() || l.BilledGroupName != perm.GroupNormal || l.GroupName != perm.GroupGuest || l.QuotaScope != store.ScopeTrafficShare || l.ShareID != f.share.ID || l.ResourcePath != f.target.Path {
		t.Fatalf("audit conflates visitor and payer: %+v", l)
	}
	var ownerDown, guestDown int64
	if err := f.db.R().QueryRowContext(ctx, `SELECT down_wire FROM traffic_daily WHERE actor_key=? AND day=?`, store.UserCounterKey(f.user.UserID(), ""), store.DayKey(store.Now())).Scan(&ownerDown); err != nil {
		t.Fatal(err)
	}
	if err := f.db.R().QueryRowContext(ctx, `SELECT COALESCE(SUM(down_wire),0) FROM traffic_daily WHERE actor_key=?`, f.guest.ActorKey()).Scan(&guestDown); err != nil {
		t.Fatal(err)
	}
	if ownerDown != used || guestDown != 0 {
		t.Fatalf("daily charged twice/wrong: owner=%d guest=%d", ownerDown, guestDown)
	}
}

func TestShareTrafficRefundFrozenAfterGroupChangeAndMidnight(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, wire)
	ticket, err := f.prepare(f.guest, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ticket.QuotaKey
	// Move the payer to a different group before refund; change must not recalculate
	// the charged account, source, or the reservation's day.
	if _, err := f.db.W().ExecContext(ctx, `UPDATE users SET group_name=? WHERE id=?`, perm.GroupAdmin, f.user.UserID()); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().UTC().Add(-24 * time.Hour).Unix()
	oldKey = store.UserCounterKey(f.user.UserID(), store.DayKey(yesterday))
	if err := f.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.ReleaseCounter(ctx, q, store.ScopeTrafficShare, ticket.QuotaKey, wire); err != nil {
			return err
		}
		if _, err := store.AddCounter(ctx, q, store.ScopeTrafficShare, oldKey, wire); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `UPDATE download_tickets SET quota_key=?,created_at=? WHERE id=?`, oldKey, yesterday, ticket.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelDelivery(ctx, f.guest, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if f.used(store.ScopeTrafficShare, oldKey) != 0 {
		t.Fatal("refund must return to original date")
	}
	// Older tickets without billing metadata retain personal-bucket refund semantics.
	legacy := ticket
	legacy.ID = "legacy-refund"
	legacy.QuotaScope = ""
	legacy.QuotaKey = ""
	legacy.CreatedAt = yesterday
	key := f.svc.deliveryQuotaKey(f.guest, yesterday)
	if err := f.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.AddCounter(ctx, q, store.ScopeTrafficDown, key, wire); err != nil {
			return err
		}
		return store.CreateTicket(ctx, q, legacy)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelDelivery(ctx, f.guest, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if f.used(store.ScopeTrafficDown, key) != 0 {
		t.Fatal("legacy personal refund")
	}
}

func TestShareTrafficConcurrentReservationAndFallback(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, wire)
	f.quota(perm.GroupGuest, store.QuotaTrafficDailyDown, wire)
	// Separate links from the same owner must share one per-owner allowance.
	second := f.shareFixture.share(f.target.Path, store.ShareFile, store.AccessPublic, "", false)
	targets := []DeliveryTarget{f.target, {OwnerUserID: f.user.UserID(), Path: f.target.Path, ShareID: second.ID}}
	var wg sync.WaitGroup
	results := make(chan store.Ticket, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plan, err := f.svc.PrepareDelivery(ctx, f.guest, targets[i%2], DeliveryRequest{Purpose: store.PurposeDownload, ForceProxy: true})
			if err != nil {
				errs <- err
				return
			}
			ticket, err := store.GetTicket(ctx, f.db.R(), plan.TicketID)
			if err != nil {
				errs <- err
				return
			}
			results <- ticket
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	counts := map[store.CounterScope]int{}
	for ticket := range results {
		counts[ticket.QuotaScope]++
	}
	rejected := 0
	for err := range errs {
		if !errors.Is(err, ErrQuotaExceeded) {
			t.Fatal(err)
		}
		rejected++
	}
	if counts[store.ScopeTrafficShare] != 1 || counts[store.ScopeTrafficDown] != 1 || rejected != 10 {
		t.Fatalf("non-atomic quota fallback: %v rejected=%d", counts, rejected)
	}
	for scope, key := range map[store.CounterScope]string{store.ScopeTrafficShare: store.UserCounterKey(f.user.UserID(), store.DayKey(store.Now())), store.ScopeTrafficDown: f.svc.deliveryQuotaKey(f.guest, store.Now())} {
		if f.used(scope, key) != wire {
			t.Fatal(fmt.Sprintf("counter %s exceeded", scope))
		}
	}
}

func TestShareTrafficRegisteredRecipientAndPreparationRollback(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	uid, err := store.CreateUser(ctx, f.db.W(), store.User{Account: "bob", DisplayName: "Bob", PasswordHash: "test", GroupName: perm.GroupNormal, Status: store.UserEnabled, CreatedAt: store.Now(), UpdatedAt: store.Now()})
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.GetUserByID(ctx, f.db.R(), uid)
	if err != nil {
		t.Fatal(err)
	}
	recipient := f.user
	recipient.User = user
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyDown, 0)
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, wire)
	ticket, err := f.prepare(recipient, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.UserID != uid || ticket.BilledUserID != f.user.UserID() || ticket.QuotaScope != store.ScopeTrafficShare {
		t.Fatalf("registered sponsorship: %+v", ticket)
	}
	if err := f.svc.CancelDelivery(ctx, recipient, ticket.ID); err != nil {
		t.Fatal(err)
	}
	for _, sponsorQuota := range []int64{wire, 0} {
		f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, sponsorQuota)
		f.quota(perm.GroupNormal, store.QuotaTrafficDailyDown, wire)
		_, err := f.svc.PrepareDelivery(ctx, recipient, f.target, DeliveryRequest{Purpose: store.PurposeDownload, ClientPublicKey: "invalid-public-key"})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid key: %v", err)
		}
		if f.used(store.ScopeTrafficShare, ticket.QuotaKey) != 0 || f.used(store.ScopeTrafficDown, f.svc.deliveryQuotaKey(recipient, store.Now())) != 0 {
			t.Fatal("prepare rollback leaked quota")
		}
	}
	fallback, err := f.prepare(recipient, store.PurposeDownload)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.BilledUserID != uid || fallback.QuotaScope != store.ScopeTrafficDown {
		t.Fatalf("registered fallback: %+v", fallback)
	}
}

func TestPickupAndExpiryUseFrozenSharingQuota(t *testing.T) {
	f := newTrafficFixture(t)
	ctx := context.Background()
	wire := f.file.PanSizeWire
	f.quota(perm.GroupNormal, store.QuotaTrafficDailyShare, wire)
	f.quota(perm.GroupGuest, store.QuotaTrafficDailyDown, 0)
	code, err := f.svc.CreatePickupCode(ctx, f.user, f.share.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := f.svc.PeekPickupCode(ctx, code.Code, f.guest)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.svc.PrepareDelivery(ctx, f.guest, target, DeliveryRequest{Purpose: store.PurposeDownload, ForceProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.GetTicket(ctx, f.db.R(), plan.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.ShareID != f.share.ID || ticket.PickupCode != code.Code || ticket.QuotaScope != store.ScopeTrafficShare {
		t.Fatalf("pickup billing context lost: %+v", ticket)
	}
	if err := f.svc.ClaimPickupUseForActor(ctx, code.Code, f.guest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W().ExecContext(ctx, `UPDATE download_tickets SET expires_at=? WHERE id=?`, store.Now()-1, ticket.ID); err != nil {
		t.Fatal(err)
	}
	// The maintenance path must refund the sponsor after the share was deleted;
	// billing must survive without looking up the share or the owner's new group.
	if err := f.svc.DeleteShare(ctx, f.user, f.share.ID); err != nil {
		t.Fatal(err)
	}
	report := f.svc.RunMaintenance(ctx)
	if report.Failures != 0 || report.ExpiredTickets != 1 {
		t.Fatalf("maintenance: %+v", report)
	}
	if f.used(store.ScopeTrafficShare, ticket.QuotaKey) != 0 {
		t.Fatal("expired pickup sponsor refund")
	}
}
