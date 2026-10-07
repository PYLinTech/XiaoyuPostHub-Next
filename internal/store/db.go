// Package store 是 SQLite 持久层：结构定义与仓储方法。
//
// SQLite 的写入是全局串行的，本包围绕这一点而非绕过它：读写分离双池（读池置
// query_only=1）；写事务一律 BEGIN IMMEDIATE，因为默认延迟事务的锁升级失败
// 不可重试；高频的流量写入用缓冲合批；不需要原子性的长流程只在收尾处开短事务。
//
// 所有时间字段都是 INTEGER（Unix 秒），0 表示未设置，统一由 Now()/FromTime()/
// ToTime() 转换，避免各处自行格式化带来的时区与精度分歧。
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// 连接池规模。写池给多条连接而不是一条：一条连接时，任何"事务中再次开事务"
// 的错误都会变成永久挂起（等待永远不会归还的连接）；多条连接下它会退化成
// 一次 busy_timeout 后的明确报错——同样是错的，但能看见、能定位。
const (
	maxWriteConns = 4
	maxReadConns  = 8
	busyTimeoutMS = 10000
)

// DB 是数据库句柄。
type DB struct {
	read  *sql.DB
	write *sql.DB
	path  string

	traffic *TrafficBuffer
}

// Querier 同时覆盖 *sql.DB、*sql.Tx 与 *Tx，让仓储方法可以在事务内外复用。
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx 是一次写事务。
//
// 不用 database/sql 的 *sql.Tx：它固定以 BEGIN（延迟）开启，而我们必须在
// 事务一开始就取得写锁。因此这里在独占连接上手动发 BEGIN IMMEDIATE，
// 并把该连接包装成 Querier 交给业务代码。
type Tx struct {
	conn *sql.Conn
}

// ExecContext 实现 Querier。
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(ctx, query, args...)
}

// QueryContext 实现 Querier。
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(ctx, query, args...)
}

// QueryRowContext 实现 Querier。
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, query, args...)
}

// Open 打开（必要时创建）数据库并校验结构版本。
func Open(path string) (*DB, error) {
	base := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMS) + ")" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=cache_size(-65536)" + // 64 MiB 页缓存（负值表示 KiB）
		"&_pragma=mmap_size(268435456)" + // 256 MiB 内存映射读
		"&_pragma=wal_autocheckpoint(1000)" +
		"&_pragma=auto_vacuum(2)" // INCREMENTAL：可分段回收，避免文件只涨不缩

	write, err := sql.Open("sqlite", base)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	write.SetMaxOpenConns(maxWriteConns)
	write.SetMaxIdleConns(maxWriteConns)
	write.SetConnMaxLifetime(0)

	// 读池加 query_only：任何写语句在这里都会直接报错，
	// 而不是"偶尔写成功"造成难以复现的数据错乱。
	read, err := sql.Open("sqlite", base+"&_pragma=query_only(1)")
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("打开读连接池失败: %w", err)
	}
	read.SetMaxOpenConns(maxReadConns)
	read.SetMaxIdleConns(maxReadConns)
	read.SetConnMaxLifetime(0)

	db := &DB{read: read, write: write, path: path}
	if err := db.write.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库不可用: %w", err)
	}
	// 结构创建必须在流量缓冲启动之前完成，否则缓冲的写入会撞上不存在的表。
	if err := db.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	db.traffic = newTrafficBuffer(db, defaultTrafficFlushInterval, defaultTrafficBufferMax)
	db.traffic.Start()
	return db, nil
}

// Close 停止后台写入并关闭两个连接池。
//
// 先停流量缓冲再关连接池：反过来的话，最后一次落库会在已关闭的连接上失败，
// 丢掉最后几秒的流量记录。
func (db *DB) Close() error {
	if db.traffic != nil {
		db.traffic.Stop()
	}
	var first error
	if db.read != nil {
		if err := db.read.Close(); err != nil {
			first = err
		}
	}
	if db.write != nil {
		if err := db.write.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Path 返回数据库文件路径。
func (db *DB) Path() string { return db.path }

// R 返回读连接池（连接已置 query_only，只允许读）。
func (db *DB) R() *sql.DB { return db.read }

// W 返回写连接池。仅在**不需要事务**的写入上直接使用；
// 需要原子性时一律用 InTx。
func (db *DB) W() *sql.DB { return db.write }

// Traffic 返回流量写入缓冲。
func (db *DB) Traffic() *TrafficBuffer { return db.traffic }

// InTx 在写事务中执行 fn。
//
// 以 BEGIN IMMEDIATE 开启："先读后写"在延迟事务下会尝试升级锁，而升级失败时
// SQLite 要求整个事务重来（读快照已作废），对已经产生副作用的 fn 不可接受。
//
// **fn 必须是纯数据库操作，且不得再调用 InTx**：嵌套会阻塞在写锁上直到
// busy_timeout，然后以错误结束。
func (db *DB) InTx(ctx context.Context, fn func(tx Querier) error) error {
	conn, err := db.beginImmediate(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tx := &Tx{conn: conn}
	if err := fn(tx); err != nil {
		// 回滚用独立的上下文：调用方的 ctx 可能已经取消，
		// 而"取消时忘记回滚"会让写锁一直被占住。
		if _, rbErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rbErr != nil {
			log.Printf("store: 回滚失败: %v", rbErr)
		}
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		if _, rbErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rbErr != nil {
			log.Printf("store: 提交失败后回滚亦失败: %v", rbErr)
		}
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// beginImmediate 取得一个独占连接并以 BEGIN IMMEDIATE 开启写事务。
//
// 只重试 BEGIN 本身，不重试 fn：BEGIN 失败时事务尚未开始、没有任何副作用，
// 重试是安全的；而 BEGIN 成功之后我们已经持有写锁，不会再遇到锁冲突。
func (db *DB) beginImmediate(ctx context.Context) (*sql.Conn, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := db.write.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("获取写连接失败: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			conn.Close()
			if !IsBusy(err) {
				return nil, fmt.Errorf("开启写事务失败: %w", err)
			}
			lastErr = err
			// 退避带抖动：多个写入者若按同一节奏重试，会在同一时刻再次相撞。
			delay := time.Duration(20*(1<<uint(attempt))) * time.Millisecond
			delay += time.Duration(rand.Int63n(int64(delay/2 + 1)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		return conn, nil
	}
	return nil, fmt.Errorf("写事务在多次退避后仍无法开始（数据库持续繁忙）: %w", lastErr)
}

// IsBusy 判断错误是否属于"数据库忙/被锁"。
//
// 单列一个函数而不是各处比对字符串：驱动换实现或换措辞时只改这一处。
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "busy_timeout")
}

// ---------------------------------------------------------------- 结构初始化

// execScript 在单个连接上按顺序执行多条语句。不走事务：DDL 在 SQLite 中会隐式提交，
// 包在事务里反而让"部分成功"难以判断；而整份结构是幂等的，失败后重跑即可。
func (db *DB) execScript(script string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := db.write.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	for _, stmt := range splitStatements(script) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行结构语句失败: %w\n语句: %s", err, firstLine(stmt))
		}
	}
	return nil
}

// splitStatements 按分号切分脚本，同时丢弃纯注释行。
// 本项目的结构脚本不含触发器与字符串内分号，因此按分号切分是安全的。
func splitStatements(script string) []string {
	var out []string
	var buf strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSuffix(strings.TrimSpace(buf.String()), ";")
			if strings.TrimSpace(stmt) != "" {
				out = append(out, stmt)
			}
			buf.Reset()
		}
	}
	if tail := strings.TrimSpace(buf.String()); tail != "" {
		out = append(out, tail)
	}
	return out
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// ---------------------------------------------------------------- 时间工具

// Now 返回当前 Unix 秒。全库统一走这个函数，便于测试替换。
func Now() int64 { return time.Now().Unix() }

// FromTime 把 time.Time 转成库内表示（Unix 秒；零值得到 0）。
func FromTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// ToTime 把库内表示转成 time.Time；0 返回零值。
func ToTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// DayKey 返回流量聚合用的日期键（UTC）。
func DayKey(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}
