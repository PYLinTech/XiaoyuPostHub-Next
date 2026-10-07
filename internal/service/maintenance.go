package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 维护任务的两组上限。
const (
	// maintenanceTimeout 是整轮清理的时间上限。
	maintenanceTimeout = 30 * time.Second
	// maintenanceBatch 是单轮清理各环节的处理条数。
	//
	// 分批而不是一次性清空：清理任务要与正常请求争抢那唯一的写连接，
	// 一次扫掉几十万行会让全站写入卡住。
	maintenanceBatch = 200
)

// MaintenanceReport 是一次维护的结果。
type MaintenanceReport struct {
	ExpiredUploads   int `json:"expiredUploads"`
	ExpiredTickets   int `json:"expiredTickets"`
	ExpiredSessions  int `json:"expiredSessions"`
	ExpiredThrottles int `json:"expiredThrottles"`
	ArchiveFiles     int `json:"archiveFiles"`
	// ArchiveExpired / ArchivePurged 是归档两个留存期到期的批次数。
	ArchiveExpired int `json:"archiveExpired"`
	ArchivePurged  int `json:"archivePurged"`
	// PurgedMail 是本轮到期彻底删除的邮件归属数（含配额与部件引用释放）。
	PurgedMail int `json:"purgedMail"`
	// PurgedTrafficLogs / PurgedAuditLogs 是保留期清理删掉的行数。
	PurgedTrafficLogs int `json:"purgedTrafficLogs"`
	PurgedAuditLogs   int `json:"purgedAuditLogs"`
	// Failures 是各环节的错误计数，用于判断"清理是否真的在推进"。
	Failures int `json:"failures"`
	// Skipped 为真表示上一轮清理尚未结束、本次触发被跳过。
	// 单飞是必要的：步骤⑥的引用与配额释放不是幂等的，重叠执行会二次释放。
	Skipped bool `json:"skipped"`
}

// RunMaintenance 执行一轮清理：过期上传会话、过期票据、过期会话、待回收对象、保留期清理。
//
// 两个刻意的设计：
//
//  1. 上下文用 `context.WithoutCancel` 派生。维护由后台定时器或关机前的收尾
//     流程触发，调用方的取消会让清理停在半途，留下"记录已删、临时文件还在"
//     这类半成品；保留值（追踪）但断开取消，再套一个自己的超时。
//
//  2. 每个环节的错误只累加到 Failures，不中断其它环节：票据清理失败不该
//     阻止临时目录清理，否则磁盘会被慢慢填满。
func (s *Service) RunMaintenance(ctx context.Context) MaintenanceReport {
	// 单飞：定时器与管理端手动触发会重叠。绝大多数步骤靠 WHERE 条件幂等，
	// 但步骤⑥的引用与配额释放不是幂等的（同一批次做两次就会二次释放，
	// 在共享内容上把别人的对象打到 ref_count=0 并最终物理删除）。
	// 这里直接跳过重叠的那一轮：维护是尽力而为的后台任务，下一轮再补。
	if !s.maintenanceMu.TryLock() {
		log.Printf("maintenance: 上一轮尚未结束，跳过本次触发")
		return MaintenanceReport{Skipped: true}
	}
	defer s.maintenanceMu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maintenanceTimeout)
	defer cancel()

	var report MaintenanceReport
	now := s.Now()

	// ① 过期上传会话：必须连带删除临时目录。库里删掉会话而留下分片文件，
	//    这些文件既不会被续传命中，也没有任何记录能再定位到它们。
	tasks, err := store.ListExpiredUploadTasksBatch(ctx, s.DB.R(), now, maintenanceBatch)
	if err != nil {
		report.Failures++
	}
	for _, task := range tasks {
		dir := s.tempDir("uploads", task.ID)
		if err := os.RemoveAll(dir); err != nil {
			report.Failures++
			// 保留数据库记录和预扣额度，下一轮维护可以重试；否则
			// 临时文件删除失败会变成无法追踪的磁盘垃圾。
			continue
		}
		deleted := false
		if err := s.DB.InTx(ctx, func(tx store.Querier) error {
			var err error
			var current store.UploadTask
			current, deleted, err = store.DeleteExpiredUploadTask(ctx, tx, task.ID, now)
			if err != nil || !deleted {
				return err
			}
			// 建立上传会话时已预扣存储配额；会话删除与归还必须
			// 同一笔事务完成，否则数据库故障会永久吞掉额度。
			return s.releaseStorageQuotaQ(ctx, tx, current.UserID, current.SizePlain)
		}); err != nil {
			report.Failures++
			continue
		}
		if deleted {
			report.ExpiredUploads++
		}
	}

	// ② 过期票据。删除前取出尚未结算的快照，释放它们占用的流量预扣；
	// 否则票据记录虽然消失，配额计数会永久偏高。
	expiredTickets, err := store.ListExpiredTicketsBatch(ctx, s.DB.R(), now, maintenanceBatch)
	if err != nil {
		report.Failures++
	}
	for _, snapshot := range expiredTickets {
		var deleted bool
		var needsRefund bool
		err := s.DB.InTx(ctx, func(tx store.Querier) error {
			_, removed, refund, err := store.DeleteExpiredTicket(ctx, tx, snapshot.ID, now)
			if err != nil {
				return err
			}
			deleted, needsRefund = removed, refund
			if !needsRefund {
				return nil
			}
			return s.releaseDeliveryQuotaByTicketQ(ctx, tx, snapshot, snapshot.ReservedBytes)
		})
		if err != nil {
			report.Failures++
			continue
		}
		if deleted {
			report.ExpiredTickets++
		}
	}

	// ③ 过期会话。
	if n, err := store.DeleteExpiredSessionsBatch(ctx, s.DB.W(), now, maintenanceBatch); err != nil {
		report.Failures++
	} else {
		report.ExpiredSessions = int(n)
	}

	// ④ 失败退避状态。不存在账号的撞库尝试没有成功登录路径可触发
	// ClearThrottle，必须由维护任务回收，否则攻击者可以让该表无限增长。
	if n, err := store.DeleteStaleThrottles(ctx, s.DB.W(), now-int64((24*time.Hour).Seconds()), maintenanceBatch); err != nil {
		report.Failures++
	} else {
		report.ExpiredThrottles = int(n)
	}

	// ⑤ 待回收对象：引用计数归零且到达计划清理时间的内容。
	//    先补记"批次流转之外"归零对象的清理时间，再执行到期清理。
	report.Failures = s.markStaleArchiveFiles(ctx, report.Failures)
	report.ArchiveFiles, report.Failures = s.purgeDueFileObjects(ctx, report.Failures)

	// ⑥ 归档留存期：暂存满用户留存期 → 归档删除（释放引用与配额）。
	archive := s.Settings.Runtime(ctx).Archive
	if archive.UserRetention > 0 {
		staged, err := store.ListExpiredStagedBatches(ctx, s.DB.R(),
			now-int64(archive.UserRetention.Seconds()), maintenanceBatch)
		if err != nil {
			report.Failures++
		}
		for _, stale := range staged {
			// stale 是读池上的快照，事务外可能已被用户"清空归档"处理过。
			// 必须在事务内按 id 重读：只有事务内读到的批次才代表当前状态
			// （ClearArchiveBatch / ClearAllArchive 都是这么做的）。
			if err := s.DB.InTx(ctx, func(tx store.Querier) error {
				batch, err := store.GetArchiveBatch(ctx, tx, stale.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return store.ErrNotFound
					}
					return err
				}
				return s.transitionBatchToArchiveDeleted(ctx, tx, batch, archive.AdminRetention)
			}); err != nil {
				// 已被并发清空是预期内的竞态结果，不是失败。
				if !errors.Is(err, store.ErrNotFound) {
					report.Failures++
				}
				continue
			}
			report.ArchiveExpired++
		}
	}

	// ⑦ 归档管理留存期：到期批次真实删除远端对象，记录保留。
	dueBatches, err := store.ListPurgeDueBatches(ctx, s.DB.R(), now, maintenanceBatch)
	if err != nil {
		report.Failures++
	} else if len(dueBatches) > 0 {
		batchesDone, objectsPurged, failures := s.purgeArchiveStorage(ctx, dueBatches)
		report.ArchivePurged += batchesDone
		report.ArchiveFiles += objectsPurged
		report.Failures += failures
	}

	// ⑦b 邮件归档到期：逐属主释放邮件配额；最后存活属主时释放部件引用，
	//     归零对象交给 ⑤ 在后续轮次物理删除。群发邮件各属主到期时间独立，
	//     一个人删信不影响其他人继续读。
	dueMail, err := store.ListDueArchivedMailboxes(ctx, s.DB.R(), now, maintenanceBatch)
	if err != nil {
		report.Failures++
	}
	for _, dm := range dueMail {
		// 存活属主数与"是否仍属归档且已到期"都由 store 在同一事务内判定：
		// ListDueArchivedMailboxes 是事务外快照，期间用户可能已恢复该邮件，
		// 限定条件里带上 status/purge_at 才不会把刚恢复的邮件误删。
		if err := store.PurgeMailboxTx(ctx, s.DB, dm, store.PurgeGuard{
			RequireArchived: true, RequireDue: true, DueBefore: now,
		}); err != nil {
			// ErrNotFound = 这条归属在本批扫过之后已被并发销毁或被用户恢复，
			// 属于预期内的竞态结果，不计为失败（计费与引用都已在那一路结清）。
			if !errors.Is(err, store.ErrNotFound) {
				report.Failures++
			}
			continue
		}
		report.PurgedMail++
	}

	// ⑧ 自然过期的分享：吊销在途票据。没人再访问过期链接时，已签出的
	//    CDN/中转票据不能活过分享的时效。窗口取两个维护周期，短暂停机
	//    也不会漏；更早过期的分享，其票据早已被自身 TTL 回收。
	report.Failures = s.revokeExpiredShareTickets(ctx, int64(now), report.Failures)

	// ⑥⑦ 保留期清理。明细类表只增不减——流量明细记每次传输、审计记每次管理
	//     动作——不设上限会让库无限膨胀。日聚合与文件记录不受影响，
	//     对账依赖它们，必须长期保留。
	ops := s.Settings.Runtime(ctx).Ops
	if ops.TrafficRetention > 0 {
		if n, err := store.DeleteTrafficLogsBeforeBatch(ctx, s.DB.W(), now-int64(ops.TrafficRetention.Seconds()), maintenanceBatch); err != nil {
			report.Failures++
		} else {
			report.PurgedTrafficLogs = int(n)
		}
	}
	if ops.AuditRetention > 0 {
		if n, err := store.DeleteAuditLogsBeforeBatch(ctx, s.DB.W(), now-int64(ops.AuditRetention.Seconds()), maintenanceBatch); err != nil {
			report.Failures++
		} else {
			report.PurgedAuditLogs = int(n)
		}
	}

	// 维护没有操作者，审计里的 actor 记 0 表示系统自身；失败计数一并落库，
	// 便于事后回答"这轮清理到底有没有在推进"。
	_ = store.InsertAuditLog(ctx, s.DB.W(), store.AuditLog{
		ActorType: "system",
		Action:    "maintenance.run",
		Detail: fmt.Sprintf("uploads=%d tickets=%d sessions=%d throttles=%d archived=%d mail=%d traffic=%d audit=%d failures=%d",
			report.ExpiredUploads, report.ExpiredTickets, report.ExpiredSessions,
			report.ExpiredThrottles, report.ArchiveFiles, report.PurgedMail,
			report.PurgedTrafficLogs, report.PurgedAuditLogs, report.Failures),
		OccurredAt: now,
	})
	return report
}

// revokeExpiredShareTickets 吊销窗口内自然过期分享的在途票据。
//
// 窗口下界 = 上一次维护时刻（间隔 × 2 取冗余）。票据吊销按分享根的子树
// 内容定位（票据模型没有分享外键，这是能表达的最窄边界），幂等可重入。
func (s *Service) revokeExpiredShareTickets(ctx context.Context, now int64, failures int) int {
	interval := s.Settings.Runtime(ctx).Ops.MaintenanceInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	since := now - int64(2*interval.Seconds())
	shares, err := store.ListSharesExpiredSince(ctx, s.DB.R(), now, since, maintenanceBatch)
	if err != nil {
		return failures + 1
	}
	for _, share := range shares {
		if err := s.DB.InTx(ctx, func(tx store.Querier) error {
			_, err := store.RevokeTicketsByShareRoot(ctx, tx, share.OwnerID, share.RootPath)
			return err
		}); err != nil {
			failures++
		}
	}
	return failures
}

// markStaleArchiveFiles 为"引用归零但从未排定清理时间"的待回收记录补记
// archive_purge_at。这类行来自归档批次流转之外的引用释放——覆盖同名文件
// 是最常见的来源——没有任何批次会为它们排定清理，若不补记，远端对象与记录
// 就会永久滞留。清理时间按管理留存期计，与批次流转语义保持一致。
func (s *Service) markStaleArchiveFiles(ctx context.Context, failures int) int {
	checksums, err := store.ListArchiveFilesWithoutPurgeAt(ctx, s.DB.R(), maintenanceBatch)
	if err != nil {
		return failures + 1
	}
	if len(checksums) == 0 {
		return failures
	}
	retention := s.Settings.Runtime(ctx).Archive.AdminRetention
	at := s.Now()
	if retention > 0 {
		at += int64(retention.Seconds())
	}
	for _, cs := range checksums {
		if err := store.SetFileArchivePurgeAt(ctx, s.DB.W(), cs, at); err != nil {
			failures++
		}
	}
	return failures
}

// purgeDueFileObjects 真实删除到达计划时间的远端对象。
//
// 顺序与失败处理是这里的关键：**先删远端对象、成功后再改库里的记录**。
// 反过来先改记录再删对象，一旦远端删除失败，这个对象就再没有任何线索能被
// 找到，变成永远无法回收的孤儿。因此删除失败时保留记录，下一轮重试。
// archive_purge_at 由归档流转写入：引用归零并不立即清理，用户还可能恢复。
func (s *Service) purgeDueFileObjects(ctx context.Context, failures int) (int, int) {
	if s.Backend == nil {
		// 没有配置存储后端时无事可做（例如本地开发）。
		return 0, failures
	}
	files, err := store.ListPurgeDueFiles(ctx, s.DB.R(), s.Now(), maintenanceBatch)
	if err != nil {
		return 0, failures + 1
	}
	reclaimed := 0
	for _, f := range files {
		// 没有存储侧定位符说明这行记录从未对应过真实对象（上传失败留下的
		// 占位行），直接置为已删除即可，不存在需要回收的远端对象。
		if f.PanFileID != "" {
			if err := s.Backend.Delete(ctx, f.PanFileID); err != nil {
				// 保留记录，等待下一轮重试。
				failures++
				continue
			}
		}
		if err := store.MarkFilePurged(ctx, s.DB.W(), f.Checksum); err != nil {
			failures++
			continue
		}
		reclaimed++
	}
	return reclaimed, failures
}
