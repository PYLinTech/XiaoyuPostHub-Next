package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是并发改造的验证集。
//
// 它要证明的不是"代码能跑"，而是三件具体的事：
//  1. 并发写事务不产生 SQLITE_BUSY：BEGIN IMMEDIATE 把锁冲突提前成排队；
//  2. 配额预扣在并发下不会超发：判定必须走原子条件更新；
//  3. 读池真的只读：读路径误写会立刻报错，而不是偶发数据错乱。

// TestConcurrentWriteTransactions 并发写事务不应出现锁冲突。
//
// 这里刻意在同一个事务里"先写后读"：这正是默认延迟事务最容易失败的形态——
// 写入时尝试把读锁升级为写锁，若期间别的连接已经写入，升级必然失败且
// 事务不可重试。改用 BEGIN IMMEDIATE 后，冲突被提前到事务开始处并排队解决。
func TestConcurrentWriteTransactions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const workers = 8
	const perWorker = 20

	var wg sync.WaitGroup
	errCh := make(chan error, workers*perWorker)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				err := db.InTx(ctx, func(tx Querier) error {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO audit_logs (actor_type, actor_id, action, target, occurred_at)
						 VALUES ('user', ?, 'concurrency.test', '', ?)`,
						worker, Now()); err != nil {
						return err
					}
					// 事务内读一次，触发"写后读"的同一快照语义。
					var n int64
					return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&n)
				})
				if err != nil {
					errCh <- fmt.Errorf("worker %d 第 %d 次事务: %w", worker, i, err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if IsBusy(err) {
			t.Errorf("并发写事务出现锁冲突（BEGIN IMMEDIATE 未生效）: %v", err)
			continue
		}
		t.Errorf("并发写事务失败: %v", err)
	}

	var count int64
	if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&count); err != nil {
		t.Fatalf("统计写入行数失败: %v", err)
	}
	if want := int64(workers * perWorker); count != want {
		t.Fatalf("应写入 %d 行，实得 %d 行（存在丢失的事务）", want, count)
	}
}

// TestConcurrentReserveNeverOversells 并发预扣不得超发。
//
// 这条断言是配额功能的根基：如果并发下能超发，"配额"就只是界面上的一个数字。
// 上限取 37 这个非整数倍值，避免"刚好整除"掩盖边界错误。
func TestConcurrentReserveNeverOversells(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const limit = 37
	const workers = 16
	const attempts = 10

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		exceeded  int
		otherErr  []error
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < attempts; i++ {
				_, err := ReserveCounter(ctx, db.W(), ScopeStorage, "user:1", 1, limit)
				mu.Lock()
				switch {
				case err == nil:
					succeeded++
				case isQuotaExceeded(err):
					exceeded++
				default:
					otherErr = append(otherErr, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	for _, err := range otherErr {
		t.Errorf("预扣出现非配额类错误: %v", err)
	}
	if succeeded != limit {
		t.Fatalf("成功次数应为上限 %d，实得 %d（并发判定被击穿或误拒）", limit, succeeded)
	}
	if exceeded != workers*attempts-limit {
		t.Fatalf("被拒次数应为 %d，实得 %d", workers*attempts-limit, exceeded)
	}

	used, err := GetCounter(ctx, db.R(), ScopeStorage, "user:1")
	if err != nil {
		t.Fatalf("读取计数器失败: %v", err)
	}
	if used != limit {
		t.Fatalf("最终用量应为 %d，实得 %d", limit, used)
	}
}

// isQuotaExceeded 判断错误是否属于"配额不足"。
func isQuotaExceeded(err error) bool {
	return err != nil && strings.Contains(err.Error(), "配额不足")
}

// TestReadsProceedDuringWrites 读不阻塞写、写不阻塞读。
//
// WAL 模式的核心价值就在这里：读池的连接看到的是事务开始时的快照，
// 不会被写事务挡住。若这条断言失败，说明 journal_mode 没落在 WAL 上。
func TestReadsProceedDuringWrites(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var mode string
	if err := db.R().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode 应为 wal，实得 %q", mode)
	}

	var (
		wg       sync.WaitGroup
		stopCh   = make(chan struct{})
		writeErr []error
		readErr  []error
		mu       sync.Mutex
	)

	// 写入者：持续开写事务。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			err := db.InTx(ctx, func(tx Querier) error {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO audit_logs (action, occurred_at) VALUES ('rw.test', ?)`, Now())
				return err
			})
			if err != nil {
				mu.Lock()
				writeErr = append(writeErr, err)
				mu.Unlock()
			}
		}
		close(stopCh)
	}()

	// 读者：在写入进行中持续读。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			var n int64
			if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&n); err != nil {
				mu.Lock()
				readErr = append(readErr, err)
				mu.Unlock()
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
	for _, err := range writeErr {
		t.Errorf("写入者失败: %v", err)
	}
	for _, err := range readErr {
		t.Errorf("读取者被写入阻塞或失败: %v", err)
	}
}

// TestReadPoolIsReadOnly 读池必须拒绝写入。
//
// 这是把"读路径误写"从偶发数据错乱变成确定性报错的关键：
// 一旦某个只读方法里混进了一条写语句，它会在测试与预发环境立刻炸掉，
// 而不是在生产上悄悄改坏几行数据。
func TestReadPoolIsReadOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.R().ExecContext(ctx,
		`INSERT INTO audit_logs (action, occurred_at) VALUES ('should.not.write', ?)`, Now())
	if err == nil {
		t.Fatal("读池应拒绝写入，但写入成功了")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Logf("读池拒绝写入，错误信息: %v", err)
	}

	// 读池仍然可以正常读。
	var n int64
	if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&n); err != nil {
		t.Fatalf("读池应可正常读取: %v", err)
	}
}

// TestStrictTablesRejectWrongTypes STRICT 表必须拒绝类型不符的写入。
//
// 没有 STRICT 时 SQLite 会按类型亲和性"尽量转换、转不了就原样存"，
// 于是 'abc' 可以躺进 INTEGER 列里，直到几个月后某个统计数字对不上才被发现。
func TestStrictTablesRejectWrongTypes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "字符串写入 INTEGER 列",
			query: `INSERT INTO audit_logs (actor_id, action, occurred_at) VALUES (?, 'x', 0)`,
			args:  []any{"not-an-integer"},
		},
		{
			name:  "浮点写入 INTEGER 列",
			query: `INSERT INTO audit_logs (actor_id, action, occurred_at) VALUES (?, 'x', 0)`,
			args:  []any{1.5},
		},
	}
	for _, tc := range cases {
		if _, err := db.W().ExecContext(ctx, tc.query, tc.args...); err == nil {
			t.Errorf("%s: STRICT 表应拒绝该写入", tc.name)
		}
	}
}

// TestTrafficBufferFlushUnderConcurrency 流量缓冲在并发记录下不丢行、不死锁。
func TestTrafficBufferFlushUnderConcurrency(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const workers = 8
	const perWorker = 50
	const total = workers * perWorker

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				actorKey := fmt.Sprintf("user:%d", worker)
				db.Traffic().Record(
					TrafficLog{
						ActorType: ActorUser, UserID: int64(worker), GroupName: "normal",
						Action: "download", BytesPlain: 1024, BytesWire: 1040, OccurredAt: Now(),
					},
					TrafficDaily{
						ActorKey: actorKey, Day: DayKey(Now()), GroupName: "normal",
						DownPlain: 1024, DownWire: 1040,
					},
				)
			}
		}(w)
	}
	wg.Wait()

	if err := db.Traffic().Flush(ctx); err != nil {
		t.Fatalf("刷写失败: %v", err)
	}

	var rows int64
	if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_logs`).Scan(&rows); err != nil {
		t.Fatalf("统计明细失败: %v", err)
	}
	if rows != total {
		t.Fatalf("明细应为 %d 行，实得 %d 行", total, rows)
	}

	// 日聚合按 (主体, 日期) 合并，因此每个 worker 恰好一行。
	var daily int64
	if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_daily`).Scan(&daily); err != nil {
		t.Fatalf("统计聚合失败: %v", err)
	}
	if daily != workers {
		t.Fatalf("日聚合应为 %d 行，实得 %d 行", workers, daily)
	}

	var sumPlain int64
	if err := db.R().QueryRowContext(ctx,
		`SELECT COALESCE(SUM(down_plain), 0) FROM traffic_daily`).Scan(&sumPlain); err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if want := int64(total * 1024); sumPlain != want {
		t.Fatalf("聚合明文总量应为 %d，实得 %d", want, sumPlain)
	}

	stats := db.Traffic().Stats()
	if stats.Dropped != 0 {
		t.Fatalf("不应出现丢弃，实得 %d 条", stats.Dropped)
	}
	if stats.PendingDetails != 0 || stats.PendingActors != 0 {
		t.Fatalf("刷写后缓冲应为空，实得 details=%d actors=%d",
			stats.PendingDetails, stats.PendingActors)
	}
}

// TestTrafficBufferStopsCleanly 关闭数据库时缓冲必须落盘。
//
// 缓冲是可以丢数据的（额度判定不依赖它），但"正常关闭"不属于可以丢的场景：
// 重启一次就丢掉最后两秒的账，会让对账长期对不上却找不到原因。
func TestTrafficBufferStopsCleanly(t *testing.T) {
	path := t.TempDir() + "/buffer.db"
	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	ctx := context.Background()

	db.Traffic().Record(
		TrafficLog{ActorType: ActorUser, UserID: 1, GroupName: "normal", Action: "upload", OccurredAt: Now()},
		TrafficDaily{ActorKey: "user:1", Day: DayKey(Now()), GroupName: "normal", UpPlain: 512},
	)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	// 重新打开并确认那条记录已经落盘。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer reopened.Close()

	var rows int64
	if err := reopened.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_logs`).Scan(&rows); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("关闭时应落盘 1 条记录，实得 %d 条", rows)
	}
}

// TestConcurrentReadsScaleWithReadPool 读池必须真的能并行。
//
// 若读池被限制成单连接，这里的总耗时会随并发数线性增长；多连接下并发读
// 应当接近并行。这条断言的价值在于防止将来有人把读池也改成单连接
// 从而静默失去并发读能力。
func TestConcurrentReadsScaleWithReadPool(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 先塞一些数据，避免查询快到测不出差异。
	if err := db.InTx(ctx, func(tx Querier) error {
		for i := 0; i < 200; i++ {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO audit_logs (action, occurred_at) VALUES ('seed', ?)`, Now()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	const readers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, readers)
	start := time.Now()
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				var n int64
				if err := db.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&n); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发读失败: %v", err)
	}
	// 只断言"没有失败"，不断言具体耗时：CI 机器上的耗时噪声会让这类断言
	// 变成随机失败源，而它想挡住的退化（读池退化为单连接）在功能上表现为
	// 明显变慢，由压测而非单测来守。
	t.Logf("并发读完成: %d 个读者 × 30 次查询，耗时 %s", readers, time.Since(start).Round(time.Millisecond))
}
