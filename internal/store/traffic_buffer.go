package store

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// 流量写入缓冲的参数。刷写间隔取 2 秒：流量是"给人看的账"而不是"用来拦住请求
// 的额度"（后者走 quota_counters，是同步写入的），可以牺牲一点实时性换取写入
// 次数的量级下降。
const (
	defaultTrafficFlushInterval = 2 * time.Second
	defaultTrafficBufferMax     = 4096
)

// TrafficBuffer 把流量明细与日聚合合并成批量写入。
//
// 一次下载会产生 1 条明细 + 1 条日聚合；在单写者的 SQLite 上逐条落库意味着每次
// 下载都要排队抢两次写锁。合批后 2 秒内的这两类写入合并成一次事务，写锁竞争与
// 磁盘同步次数都下降一到两个数量级。
//
// 代价是崩溃时可能丢掉最多一个刷写周期的记录：流量记录用于审计与对账，而额度
// 判定不依赖它，丢几条不会让任何限制失效。
type TrafficBuffer struct {
	db       *DB
	interval time.Duration
	capacity int

	mu      sync.Mutex
	details []TrafficLog
	daily   map[string]*TrafficDaily

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
	started  atomic.Bool

	// 计数用于运维观测：积压说明刷写跟不上，丢弃说明已经出现过载。
	recorded atomic.Int64
	flushed  atomic.Int64
	dropped  atomic.Int64
	failed   atomic.Int64
}

func newTrafficBuffer(db *DB, interval time.Duration, capacity int) *TrafficBuffer {
	if interval <= 0 {
		interval = defaultTrafficFlushInterval
	}
	if capacity <= 0 {
		capacity = defaultTrafficBufferMax
	}
	return &TrafficBuffer{
		db:       db,
		interval: interval,
		capacity: capacity,
		daily:    map[string]*TrafficDaily{},
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// Start 启动后台刷写。
func (b *TrafficBuffer) Start() {
	if b == nil || !b.started.CompareAndSwap(false, true) {
		return
	}
	go b.loop()
}

// Stop 停止后台刷写并做最后一次落库。
func (b *TrafficBuffer) Stop() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() { close(b.stopCh) })
	if b.started.Load() {
		<-b.doneCh
	}
	// 无论后台循环是否跑过，都要把残留刷一次。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Flush(ctx); err != nil {
		log.Printf("store: 关闭时刷写流量数据失败: %v", err)
	}
}

func (b *TrafficBuffer) loop() {
	defer close(b.doneCh)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := b.Flush(ctx)
			cancel()
			if err != nil {
				log.Printf("store: 刷写流量数据失败: %v", err)
			}
		}
	}
}

// Record 记录一次传输。detail 是明细，daily 是同批次的日聚合增量。
//
// 日聚合按 (主体, 日期) 在内存里累加后再落库，因此同一个主体的一整天流量
// 在一次刷写里只会产生一次累加，而不是每个请求一次。
func (b *TrafficBuffer) Record(detail TrafficLog, daily TrafficDaily) {
	if b == nil {
		return
	}
	if detail.OccurredAt == 0 {
		detail.OccurredAt = Now()
	}

	b.mu.Lock()
	b.details = append(b.details, detail)
	key := daily.ActorKey + "\x00" + daily.Day
	if existing, ok := b.daily[key]; ok {
		existing.UpPlain += daily.UpPlain
		existing.UpWire += daily.UpWire
		existing.DownPlain += daily.DownPlain
		existing.DownWire += daily.DownWire
		// 组名取最新值：用户换组后，当天的归属应当反映最近的状态。
		if daily.GroupName != "" {
			existing.GroupName = daily.GroupName
		}
	} else {
		copied := daily
		b.daily[key] = &copied
	}
	overflow := len(b.details) > b.capacity
	b.recorded.Add(1)
	b.mu.Unlock()

	// 过载时同步刷一次，把内存占用压回去。
	// 在锁外做：刷写要开事务，持锁刷写会把所有 Record 一起卡住。
	if overflow {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := b.Flush(ctx)
		cancel()
		if err != nil {
			log.Printf("store: 缓冲过载时刷写失败: %v", err)
		}
	}
}

// Flush 把缓冲区内容写入数据库。
func (b *TrafficBuffer) Flush(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if len(b.details) == 0 && len(b.daily) == 0 {
		b.mu.Unlock()
		return nil
	}
	details := b.details
	daily := make([]TrafficDaily, 0, len(b.daily))
	for _, d := range b.daily {
		daily = append(daily, *d)
	}
	b.details = nil
	b.daily = map[string]*TrafficDaily{}
	b.mu.Unlock()

	err := b.db.InTx(ctx, func(tx Querier) error {
		for _, l := range details {
			if err := InsertTrafficLog(ctx, tx, l); err != nil {
				return err
			}
		}
		for _, d := range daily {
			if err := UpsertTrafficDaily(ctx, tx, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// 失败后不回填缓冲区：回填会让一个持续失败的原因把内存撑爆，
		// 而这里丢掉的只是可重建的统计口径，不影响任何额度判定。
		b.failed.Add(1)
		b.dropped.Add(int64(len(details)))
		return fmt.Errorf("刷写流量数据失败: %w", err)
	}
	b.flushed.Add(1)
	return nil
}

// TrafficStats 是缓冲区的运行状态，供管理端观测。
type TrafficStats struct {
	PendingDetails int   `json:"pendingDetails"`
	PendingActors  int   `json:"pendingActors"`
	Recorded       int64 `json:"recorded"`
	Flushes        int64 `json:"flushes"`
	Dropped        int64 `json:"dropped"`
	FailedFlushes  int64 `json:"failedFlushes"`
}

// Stats 返回缓冲区状态。
func (b *TrafficBuffer) Stats() TrafficStats {
	if b == nil {
		return TrafficStats{}
	}
	b.mu.Lock()
	pendingDetails, pendingActors := len(b.details), len(b.daily)
	b.mu.Unlock()
	return TrafficStats{
		PendingDetails: pendingDetails,
		PendingActors:  pendingActors,
		Recorded:       b.recorded.Load(),
		Flushes:        b.flushed.Load(),
		Dropped:        b.dropped.Load(),
		FailedFlushes:  b.failed.Load(),
	}
}
