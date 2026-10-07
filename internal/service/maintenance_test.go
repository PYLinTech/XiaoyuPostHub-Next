package service

import (
	"context"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// TestMaintenancePurgesExpiredDetailRows 保留期清理必须真的删掉过期明细，
// 同时保留仍在保留期内的数据。
//
// 明细类表只增不减：流量明细记每次传输、审计记每次管理动作。没有这一步，
// 库会随使用时间无限膨胀，而这在早期完全看不出来。
func TestMaintenancePurgesExpiredDetailRows(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	now := store.Now()

	day := int64(24 * 3600)
	// 流量明细默认保留 90 天、审计保留 365 天，因此这里分别越过各自的门槛。
	trafficOld := now - 100*day
	trafficFresh := now - 10*day
	auditOld := now - 400*day
	auditFresh := now - 10*day

	for _, at := range []int64{trafficOld, trafficFresh} {
		if err := store.InsertTrafficLog(ctx, f.db.W(), store.TrafficLog{
			ActorType: store.ActorUser, UserID: f.user.User.ID, GroupName: "normal",
			Action: "download", BytesPlain: 1, BytesWire: 1, OccurredAt: at,
		}); err != nil {
			t.Fatalf("写入流量明细失败: %v", err)
		}
	}
	for _, at := range []int64{auditOld, auditFresh} {
		if err := store.InsertAuditLog(ctx, f.db.W(), store.AuditLog{
			ActorType: "user", ActorID: f.user.User.ID, Action: "test.marker", OccurredAt: at,
		}); err != nil {
			t.Fatalf("写入审计失败: %v", err)
		}
	}

	// 维护自身在结尾也会写一条审计（maintenance.run），因此断言用"标记行"计数，
	// 而不是全表行数。
	report := f.svc.RunMaintenance(ctx)

	if report.PurgedTrafficLogs != 1 {
		t.Fatalf("应清理 1 条过期流量明细，实得 %d（report=%+v）", report.PurgedTrafficLogs, report)
	}
	if report.PurgedAuditLogs != 1 {
		t.Fatalf("应清理 1 条过期审计，实得 %d（report=%+v）", report.PurgedAuditLogs, report)
	}
	if report.Failures != 0 {
		t.Fatalf("维护不应有失败环节: %+v", report)
	}

	var trafficLeft, auditMarkerLeft int
	if err := f.db.R().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM traffic_logs WHERE action = 'download'`).Scan(&trafficLeft); err != nil {
		t.Fatalf("统计剩余流量明细失败: %v", err)
	}
	if trafficLeft != 1 {
		t.Fatalf("保留期内的流量明细不应被删，实得 %d 条", trafficLeft)
	}
	if err := f.db.R().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action = 'test.marker'`).Scan(&auditMarkerLeft); err != nil {
		t.Fatalf("统计剩余审计失败: %v", err)
	}
	if auditMarkerLeft != 1 {
		t.Fatalf("保留期内的审计不应被删，实得 %d 条", auditMarkerLeft)
	}
}

// TestMaintenanceIsIdempotent 重复执行不应报错，也不应重复计量。
func TestMaintenanceIsIdempotent(t *testing.T) {
	f := newShareFixture(t)
	first := f.svc.RunMaintenance(context.Background())
	second := f.svc.RunMaintenance(context.Background())

	if first.Failures != 0 || second.Failures != 0 {
		t.Fatalf("两轮维护都不应有失败: %+v / %+v", first, second)
	}
	// 第二轮没有可清理的东西，因此各项计数应为 0。
	if second.ExpiredUploads+second.ExpiredTickets+second.ExpiredSessions+
		second.ArchiveFiles+second.PurgedTrafficLogs+second.PurgedAuditLogs != 0 {
		t.Fatalf("第二轮维护应无事可做，实得 %+v", second)
	}
}
