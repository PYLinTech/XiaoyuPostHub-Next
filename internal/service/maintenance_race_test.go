package service

import (
	"context"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// TestStaleBatchSnapshotDoesNotDoubleRelease 回归维护任务步骤⑥的并发缺陷。
//
// 旧实现把读池上的批次快照（stale）直接传进事务，状态守卫因此形同虚设：
// 维护定时器与管理端手动触发重叠时，两路都会看到 state=staged 并各执行一次
// transitionBatchToArchiveDeleted。引用释放是"按量减"，第二次在共享内容上仍
// 会命中（ref_count >= delta），把 ref_count 打到 0 → 对象被推进待回收 →
// 下一轮物理删除，而另一个用户的节点仍指向它。配额也会被重复退还。
//
// 现在批次必须在事务内按 id 重读，状态已推进的批次会被守卫拒绝。
func TestStaleBatchSnapshotDoesNotDoubleRelease(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	ctx := context.Background()

	payload := []byte("shared content for archive race")
	node := completeUpload(t, f, payload, "a.txt")

	// 删掉节点 → 进入暂存。
	if err := f.svc.DeleteNode(ctx, f.user, node.LogicalPath); err != nil {
		t.Fatalf("删除节点失败: %v", err)
	}
	batches, _, err := store.ListArchiveBatchesByUser(ctx, f.db.R(), f.user.User.ID, 10, 0)
	if err != nil || len(batches) == 0 {
		t.Fatalf("读取归档批次失败: %v", err)
	}
	stale := batches[len(batches)-1] // 这就是旧实现会原样传进事务的那份快照

	// 第一路：正常推进。
	if err := f.svc.DB.InTx(ctx, func(tx store.Querier) error {
		return f.svc.transitionBatchToArchiveDeleted(ctx, tx, stale, 0)
	}); err != nil {
		t.Fatalf("首次推进失败: %v", err)
	}
	file, err := store.GetFile(ctx, f.db.R(), node.FileChecksum)
	if err != nil {
		t.Fatal(err)
	}
	refAfterFirst := file.RefCount
	usedAfterFirst, _ := store.GetCounter(ctx, f.db.R(), store.ScopeStorage,
		store.UserCounterKey(f.user.User.ID, ""))

	// 第二路：模拟重叠的维护轮，带着**同一份事务外快照**再进一次。
	// 事务内重读会看到状态已不是 staged，守卫必须拒绝。
	err = f.svc.DB.InTx(ctx, func(tx store.Querier) error {
		return f.svc.transitionBatchToArchiveDeleted(ctx, tx, stale, 0)
	})
	if err == nil {
		t.Fatal("重复推进同一批次必须失败：引用与配额不应被释放第二次")
	}

	file, err = store.GetFile(ctx, f.db.R(), node.FileChecksum)
	if err != nil {
		t.Fatal(err)
	}
	if file.RefCount != refAfterFirst {
		t.Fatalf("引用计数被二次释放: %d -> %d", refAfterFirst, file.RefCount)
	}
	usedAfterSecond, _ := store.GetCounter(ctx, f.db.R(), store.ScopeStorage,
		store.UserCounterKey(f.user.User.ID, ""))
	if usedAfterSecond != usedAfterFirst {
		t.Fatalf("存储配额被二次退还: %d -> %d", usedAfterFirst, usedAfterSecond)
	}
}

// TestConcurrentMaintenanceIsSingleFlight 确认 RunMaintenance 同一时刻只跑一轮。
//
// 步骤⑥的引用释放不是幂等的，重叠执行就是上面那条数据丢失路径。维护是
// 尽力而为的后台任务，重叠时跳过本轮即可，不应阻塞调用方。
func TestConcurrentMaintenanceIsSingleFlight(t *testing.T) {
	f := newShareFixture(t)
	f.enableEncryption()
	f.enableStorage()
	completeUpload(t, f, []byte("maintenance single flight"), "a.txt")

	const runs = 8
	reports := make(chan MaintenanceReport, runs)
	start := make(chan struct{})
	for i := 0; i < runs; i++ {
		go func() {
			<-start
			reports <- f.svc.RunMaintenance(context.Background())
		}()
	}
	close(start)

	skipped, executed := 0, 0
	for i := 0; i < runs; i++ {
		if (<-reports).Skipped {
			skipped++
		} else {
			executed++
		}
	}
	if executed > 1 {
		t.Fatalf("并发触发了 %d 轮实际执行，应至多 1 轮（其余 %d 轮跳过）", executed, skipped)
	}
	// 串行再跑一轮必须仍能正常执行（单飞锁已释放）。
	if f.svc.RunMaintenance(context.Background()).Skipped {
		t.Fatal("无并发时不应跳过")
	}
}
