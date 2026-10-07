// ops.go 提供数据库的运维观测与维护动作：容量统计、完整性校验、在线备份、
// 常规优化与 WAL 检查点。三条原则贯穿全部函数：
//  1. 只读观测走读连接池（db.R()）：WAL 下读不阻塞写，而写池连接有限，刷新状态页
//     不该让业务写排队；
//  2. 维护动作不包 InTx：VACUUM 不允许出现在事务中，其余维护语句各自幂等，失败
//     一步不应阻止后续步骤；
//  3. 若干维护 PRAGMA 在特定模式下是空操作（典型：非 INCREMENTAL 模式下的
//     incremental_vacuum），必须如实上报，否则运维看到"成功"却没有任何空间被回收。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxCountedRows 是逐表行数统计的封顶值。COUNT(*) 即使走索引也是 O(行数) 的遍历，
// 上千万行会让状态页超时，因此改用受限子查询 "SELECT COUNT(*) FROM (SELECT 1 FROM t
// LIMIT N+1)"：SQLite 的 LIMIT 提前终止，代价被硬性封顶；结果没到上限时即精确行数。
//
// 不用 dbstat 虚拟表：它需要编译期开启 SQLITE_ENABLE_DBSTAT_VTAB，
// modernc.org/sqlite 的默认构建并不保证提供。
const maxCountedRows = 2_000_000

// FileOps 描述数据库文件的组成与大小。
//
// 这些字段是"各自的快照"，彼此不保证严格自洽：page_count 来自数据库头，
// fileBytes 来自文件系统，而此刻 WAL 里可能还有尚未合并进主库的内容。
// 运维展示要的是"能拿到数、能看趋势"，强一致反而要求停写，得不偿失。
type FileOps struct {
	Path          string `json:"path"`
	PageSize      int64  `json:"pageSize"`
	PageCount     int64  `json:"pageCount"`
	FreelistCount int64  `json:"freelistCount"`
	InUseBytes    int64  `json:"inUseBytes"`
	FreeBytes     int64  `json:"freeBytes"`
	FileBytes     int64  `json:"fileBytes"`
	WALBytes      int64  `json:"walBytes"`
	// Tables 是各表的行数，按占用降序。
	Tables []TableStat `json:"tables"`
	// JournalMode / AutoVacuum / ForeignKeys / BusyTimeout 是运行期设置。
	// 放在这里是为了让管理端能一眼核对"库究竟跑在什么模式下"——例如
	// auto_vacuum=none 时，Optimize 里的页回收必然是空操作。
	JournalMode string `json:"journalMode"`
	AutoVacuum  string `json:"autoVacuum"`
	ForeignKeys bool   `json:"foreignKeys"`
	BusyTimeout int64  `json:"busyTimeoutMs"`
}

// TableStat 是单表的统计。
type TableStat struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	// PayloadBytes 恒为 0（dbstat 依赖额外编译选项，见 maxCountedRows）。不按
	// 行数×平均行长估算：那种数字看着像真的，却会在 BLOB 表上离谱到误导决策。
	PayloadBytes int64 `json:"payloadBytes"`
}

// Stats 收集数据库运行状态。
//
// includeTables 控制是否逐表统计行数：表很多或表很大时这一步是唯一可能慢的部分，
// 让调用方（管理端）自己决定要不要付这个代价。为 false 时 Tables 返回空切片，
// 前端可以据此显示"未采集"。
func (db *DB) Stats(ctx context.Context, includeTables bool) (FileOps, error) {
	out := FileOps{
		Path:   db.Path(),
		Tables: []TableStat{},
	}

	// 全部走读池：这些 PRAGMA 里有的是按连接生效的（foreign_keys、busy_timeout），
	// 但读池的每条连接都由同一个 DSN 打开，取值一致，因此可以安全地分散到读池上，
	// 换取"统计时完全不碰写连接"。
	q := db.R()

	var err error
	if out.PageSize, err = pragmaInt(ctx, q, "page_size"); err != nil {
		return FileOps{}, err
	}
	if out.PageCount, err = pragmaInt(ctx, q, "page_count"); err != nil {
		return FileOps{}, err
	}
	if out.FreelistCount, err = pragmaInt(ctx, q, "freelist_count"); err != nil {
		return FileOps{}, err
	}
	if out.JournalMode, err = pragmaText(ctx, q, "journal_mode"); err != nil {
		return FileOps{}, err
	}
	autoVacuumRaw, err := pragmaText(ctx, q, "auto_vacuum")
	if err != nil {
		return FileOps{}, err
	}
	out.AutoVacuum = normalizeAutoVacuum(autoVacuumRaw)
	fk, err := pragmaInt(ctx, q, "foreign_keys")
	if err != nil {
		return FileOps{}, err
	}
	out.ForeignKeys = fk != 0
	if out.BusyTimeout, err = pragmaInt(ctx, q, "busy_timeout"); err != nil {
		return FileOps{}, err
	}

	// 空闲页与在用页按页大小换算。freelist_count 是页数而不是字节数，
	// 直接用字节数展示会差好几个数量级。
	out.InUseBytes = (out.PageCount - out.FreelistCount) * out.PageSize
	out.FreeBytes = out.FreelistCount * out.PageSize

	// 文件大小取文件系统真值：PRAGMA 给不出 WAL 的体量，而 WAL 涨到几个 GB
	// 恰恰是最常见的运维事故。
	out.FileBytes = fileSizeOrZero(db.Path())
	out.WALBytes = fileSizeOrZero(db.Path() + "-wal")

	if !includeTables {
		return out, nil
	}

	names, err := listUserTables(ctx, q)
	if err != nil {
		return FileOps{}, err
	}
	for _, name := range names {
		stat := TableStat{Name: name}
		rows, err := countRowsCapped(ctx, q, name)
		if err != nil {
			// 单表统计失败（锁冲突、表结构损坏等）不该让整个状态页失败：
			// 其余表的数据仍然有用。标注 skipped 让管理端知道这一行不可信，
			// 而不是把 0 当成"表是空的"。
			stat.Name = name + " (skipped)"
			out.Tables = append(out.Tables, stat)
			continue
		}
		if rows > maxCountedRows {
			// 到上限说明表超过 maxCountedRows 行，具体多少没数——如实标注。
			stat.Name = name + " (skipped)"
			out.Tables = append(out.Tables, stat)
			continue
		}
		stat.Rows = rows
		out.Tables = append(out.Tables, stat)
	}

	// 按"占用"降序。PayloadBytes 目前恒为 0，所以实际按行数降序，再用表名兜底
	// 保证顺序稳定（否则前端每次刷新的行序都会跳）。
	sort.Slice(out.Tables, func(i, j int) bool {
		a, b := out.Tables[i], out.Tables[j]
		if a.PayloadBytes != b.PayloadBytes {
			return a.PayloadBytes > b.PayloadBytes
		}
		if a.Rows != b.Rows {
			return a.Rows > b.Rows
		}
		return a.Name < b.Name
	})

	return out, nil
}

// IntegrityCheck 执行完整性检查。results 为发现的问题列表（空表示完好）。
//
// quick=true 走 quick_check（跳过索引与内容的交叉验证，适合定时任务），false 走完整
// integrity_check（适合人工深度检查）。两者都返回多行结果：正常时只有一行 "ok"，
// 发现问题时每行一条描述，因此不能只读第一行就下结论。
func (db *DB) IntegrityCheck(ctx context.Context, quick bool) (results []string, err error) {
	pragma := "integrity_check"
	if quick {
		pragma = "quick_check"
	}
	// 读池：完整性检查是纯读操作，且在大库上可能跑很久，绝不能占着写连接。
	rows, err := db.R().QueryContext(ctx, "PRAGMA "+pragma)
	if err != nil {
		return nil, fmt.Errorf("执行 %s 失败: %w", pragma, err)
	}
	defer rows.Close()

	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("读取 %s 结果失败: %w", pragma, err)
		}
		// 只有 "ok" 表示该检查项通过；其余每一行都是一条问题描述。
		if !strings.EqualFold(strings.TrimSpace(line), "ok") {
			results = append(results, line)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 %s 结果失败: %w", pragma, err)
	}
	return results, nil
}

// Backup 用 VACUUM INTO 生成一致性快照（在线备份，不阻塞读写）。dest 为目标文件绝对路径。
//
// 选 VACUUM INTO 而不是复制文件：cp 会复制到写了一半的页，得到看似完整实则损坏的库；
// VACUUM INTO 在单个读事务里重写整库到新文件，顺带完成碎片整理，也不需要维持长生命
// 周期连接。
//
// 目标文件必须不存在，这是 VACUUM INTO 自身的语义；这里先 os.Stat 预判，是为了给出
// 可读的错误而不是把 SQLite 的原始报错透给用户。
func (db *DB) Backup(ctx context.Context, dest string) (bytesWritten int64, err error) {
	if strings.TrimSpace(dest) == "" {
		return 0, fmt.Errorf("备份目标路径不得为空")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		return 0, fmt.Errorf("备份目标已存在，拒绝覆盖: %s", dest)
	} else if !os.IsNotExist(statErr) {
		return 0, fmt.Errorf("检查备份目标失败: %w", statErr)
	}
	// 目录不存在时 VACUUM INTO 只会报错，不会替我们建目录。
	// 0o700：备份是整库明文副本，含密码哈希与会话数据，只允许属主可读。
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return 0, fmt.Errorf("创建备份目录失败: %w", err)
	}

	// VACUUM 不能在事务里执行，所以直接用写连接 ExecContext，不包 InTx。
	// 路径通过占位符传入，绝不拼进 SQL 文本。
	if _, err := db.W().ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return 0, fmt.Errorf("执行 VACUUM INTO 备份失败: %w", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		return 0, fmt.Errorf("备份已生成但读取其大小失败: %w", err)
	}
	return info.Size(), nil
}

// Optimize 执行常规优化：PRAGMA optimize、增量回收空闲页、WAL 检查点，返回每一步
// 的简要结果。任何一步失败都不中断后续步骤——维护动作彼此独立，往往是"能修一点是
// 一点"，而因 optimize 失败就跳过 checkpoint 只会让 WAL 继续膨胀。
func (db *DB) Optimize(ctx context.Context, vacuumPages int) (map[string]string, error) {
	out := make(map[string]string, 3)

	// 1) PRAGMA optimize：让 SQLite 依据当前统计信息决定要不要跑 ANALYZE。
	// 使用 SQLite 当前稳定语法，避免把实现细节暴露给管理操作。
	if err := execPragma(ctx, db.W(), "PRAGMA optimize"); err != nil {
		out["optimize"] = maintenanceErrText(err)
	} else {
		out["optimize"] = "ok"
	}

	// 2) 增量回收空闲页。必须先看 auto_vacuum：只有 INCREMENTAL 模式下
	// 这条 PRAGMA 才会真正搬移页；其他模式下它是**静默的空操作**，
	// 报"ok"会让运维以为空间已回收。
	autoVacuum, err := pragmaText(ctx, db.W(), "auto_vacuum")
	switch mode := normalizeAutoVacuum(autoVacuum); {
	case err != nil:
		out["incremental_vacuum"] = maintenanceErrText(err)
	case mode != "incremental":
		out["incremental_vacuum"] = fmt.Sprintf("skipped: auto_vacuum=%s，该模式下不回收空闲页", mode)
	case vacuumPages <= 0:
		out["incremental_vacuum"] = "skipped: 未指定回收页数（vacuumPages <= 0）"
	default:
		// vacuumPages 是调用方给的整数，但 %d 只会产出数字字面量，
		// 不存在注入面；PRAGMA 的参数位置也不支持占位符。
		stmt := fmt.Sprintf("PRAGMA incremental_vacuum(%d)", vacuumPages)
		if err := execPragma(ctx, db.W(), stmt); err != nil {
			out["incremental_vacuum"] = maintenanceErrText(err)
		} else {
			out["incremental_vacuum"] = fmt.Sprintf("ok: 最多回收 %d 页", vacuumPages)
		}
	}

	// 3) WAL 检查点并截断。放在最后：前面的 ANALYZE 会产生新的 WAL 帧，
	// 先把它们合并回主库，才能把 -wal 文件真正清空。
	if err := execPragma(ctx, db.W(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		out["wal_checkpoint"] = maintenanceErrText(err)
	} else {
		out["wal_checkpoint"] = "ok"
	}

	return out, nil
}

// CheckpointWAL 执行 WAL 检查点；truncate 为真时把 WAL 文件截断回零。
//
// 该 PRAGMA 固定返回一行三列 (busy, log, checkpointed)：busy 非 0 表示有读者/写者
// 挡住了检查点，本次只做了一部分、WAL 不会缩小。这种情况必须视为错误：返回成功却
// 没有任何帧被搬运，会让定时任务一直报健康而磁盘一直涨。
func (db *DB) CheckpointWAL(ctx context.Context, truncate bool) error {
	mode := "PASSIVE"
	if truncate {
		mode = "TRUNCATE"
	}
	var busy, logFrames, checkpointed int64
	err := db.W().QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").
		Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		return fmt.Errorf("WAL 检查点失败: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("WAL 检查点未完成：存在并发读者/写者，log=%d 帧，已写回 %d 帧",
			logFrames, checkpointed)
	}
	return nil
}

// ---------------------------------------------------------------- 内部工具

// pragmaInt 读取返回单个整数的 PRAGMA。
//
// name 只接受包内硬编码的常量：PRAGMA 的名称位置无法用占位符，只能拼接，
// 因此绝不能让外部输入流到这里。
func pragmaInt(ctx context.Context, q *sql.DB, name string) (int64, error) {
	var v int64
	if err := q.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		return 0, fmt.Errorf("读取 PRAGMA %s 失败: %w", name, err)
	}
	return v, nil
}

// pragmaText 读取返回单个文本值的 PRAGMA。用 string 统一承接：SQLite 各版本对同一
// PRAGMA 的返回类型并不一致（auto_vacuum 早期为 0/1/2，较新为 none/full/incremental），
// 而 database/sql 会把整数转成字符串，一个出口即可覆盖两种形态。
func pragmaText(ctx context.Context, q *sql.DB, name string) (string, error) {
	var v string
	if err := q.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		return "", fmt.Errorf("读取 PRAGMA %s 失败: %w", name, err)
	}
	return v, nil
}

// execPragma 执行一条不需要取值的 PRAGMA。
//
// 用 Query 而不是 Exec：部分 PRAGMA（如 wal_checkpoint）会返回结果行，走 Exec 在不同
// 驱动上行为不一致。返回行必须读空，否则连接不会归还连接池，在写池上会很快耗尽。
func execPragma(ctx context.Context, q *sql.DB, stmt string) error {
	rows, err := q.QueryContext(ctx, stmt)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// 显式丢弃：本函数的调用方只关心 PRAGMA 是否成功执行。
	}
	return rows.Err()
}

// normalizeAutoVacuum 把 auto_vacuum 的两种表示（数字/文本）统一成文本标签。
func normalizeAutoVacuum(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "none":
		return "none"
	case "1", "full":
		return "full"
	case "2", "incremental":
		return "incremental"
	default:
		return strings.TrimSpace(raw)
	}
}

// listUserTables 列出普通表名。
//
// 过滤 sqlite_% 是为了排除 SQLite 内部表（sqlite_sequence、sqlite_stat1 等）：
// 它们既不属于业务，也可能因为文件损坏而读不出来，不该出现在运维报表里。
func listUserTables(ctx context.Context, q *sql.DB) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("列出数据表失败: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("扫描表名失败: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出数据表失败: %w", err)
	}
	return names, nil
}

// countRowsCapped 统计单表行数，代价被 maxCountedRows 封顶。
//
// LIMIT 放在子查询里：外层聚合需要多少行就拉多少行，到上限即停止，
// 因此上千万行的大表最多也只遍历 maxCountedRows+1 行。
func countRowsCapped(ctx context.Context, q *sql.DB, table string) (int64, error) {
	stmt := fmt.Sprintf(`SELECT COUNT(*) FROM (SELECT 1 FROM %s LIMIT %d)`,
		quoteIdent(table), maxCountedRows+1)
	var n int64
	if err := q.QueryRowContext(ctx, stmt).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计表 %s 行数失败: %w", table, err)
	}
	return n, nil
}

// quoteIdent 按 SQL 规则给标识符加双引号。
//
// 表名来自 sqlite_master（库自身目录）而不是外部输入，但库结构可能被人工改过，
// 双引号转义是零成本的兜底，避免畸形表名让统计语句语法出错。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// fileSizeOrZero 返回文件字节数；文件不存在时返回 0。
//
// WAL 与 SHM 文件在检查点后会被删除，这是正常状态而非错误；把"文件还没生成"
// 当成失败会让状态页在刚建库时直接报错。
func fileSizeOrZero(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// maintenanceErrText 把维护语句的失败转成可展示的文本。
//
// 锁冲突单独归类为 skipped：维护动作（optimize/checkpoint/vacuum）撞上业务写入
// 是常态而不是故障，运维看到"失败"会去排查根本不存在的 bug。真正的错误才标 failed。
func maintenanceErrText(err error) string {
	if err == nil {
		return "ok"
	}
	if isLockedErr(err) {
		return "skipped: 数据库繁忙，与业务写入冲突，可稍后重试（" + err.Error() + "）"
	}
	return "failed: " + err.Error()
}

// isLockedErr 判断是否为 SQLite 的锁/超时类错误。
// 集中一处判断，避免每个调用点各自写一遍字符串匹配。
func isLockedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy")
}
