// 逐代迁移：从库文件当前版本升到 currentSchemaVersion。
//
// 为什么要这套机制：schema.sql 全部是 CREATE ... IF NOT EXISTS，它只补
// **缺失的对象**，不修**改过的定义**。旧机制因此要求"改结构就重建库文件"，
// 但库一上线就再也重建不得——已有数据（邮件、文件、配额）都在里面。
// 迁移机制把这个约束解开：每代迁移只做增量，老库原地升上来。
//
// 两个文件各司其职，同一段 DDL 要在两处都写：
//
//	schema.sql    终版结构，给「全新库」直建（v=0 路径）
//	migrations/   逐代增量，给「老库」升级（v>0 路径）
//
// 两者不一致会导致「升级来的库」与「新建的库」结构不同，而这种差异不会
// 立刻报错，只会在某个特定查询上安静地返回错结果。
// TestSchemaMatchesMigrated 锁死这个不变式，不要因为"看起来重复"就删掉其中一份。

package store

import (
	"context"
	"embed"
	"fmt"
	"time"
)

// migrationsFS 内嵌逐代迁移脚本。目录必须带下划线或数字前缀才能被 embed 收录，
// 这里用 migrations/ 而不是 migration/，且路径以 ./ 开头明确表示匹配本目录。
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration 是一代结构升级。
type migration struct {
	// Version 是**升级后**的版本号，不是起始版本。文件名 0001_xxx.sql 升到 2，
	// 0002_xxx.sql 升到 3——与 schema.sql 的整体版本号对齐，不另起编号。
	Version int
	// Name 只进日志，出错时要能一眼看出是哪一代。
	Name string
	// File 是 migrations/ 下的文件名，loadMigrations 据此读出 SQL。
	File string
	// SQL 由 loadMigrations 从 File 读出后回填。
	SQL string
}

// migrations 是逐代升级的完整清单，按 Version 升序。
//
// ⚠️ 已发布的条目**永不改写**：改了老库升不上去（它已经按旧内容升过了）。
// 写错了就在末尾追加新版本补救，不要回头编辑旧条目。
//
// 初始只有 v1 → v2 一代，对应邮箱地址解绑申请表。
var migrations = []migration{
	{Version: 2, Name: "mail_unbind_requests", File: "0001_mail_unbind.sql"},
}

// loadMigrations 按注册表读取每个迁移的 SQL 并做完整性校验。
//
// 校验不是形式：注册表与文件目录一旦对不上（例如漏提交一个 .sql），
// 缺失只会在某个用户升级到那一代时才暴露，那时已经停服了。启动时炸掉
// 便宜得多。
func loadMigrations() ([]migration, error) {
	out := make([]migration, 0, len(migrations))
	seen := make(map[int]bool, len(migrations))
	prev := 0
	for _, m := range migrations {
		if m.Version <= prev {
			return nil, fmt.Errorf("迁移清单版本号未升序或有重复: %d", m.Version)
		}
		if seen[m.Version] {
			return nil, fmt.Errorf("迁移清单版本号重复: %d", m.Version)
		}
		if m.Version <= 1 {
			// v1 是基线，不存在"升到 v1"的迁移。
			return nil, fmt.Errorf("迁移版本号必须大于基线 1: %d", m.Version)
		}
		seen[m.Version] = true
		prev = m.Version

		sql, err := migrationsFS.ReadFile("migrations/" + m.File)
		if err != nil {
			return nil, fmt.Errorf("读取迁移文件 %s 失败: %w", m.File, err)
		}
		copied := m
		copied.SQL = string(sql)
		out = append(out, copied)
	}
	return out, nil
}

// runMigrations 把库从 from 升到 target，逐代执行。
//
// 每一代独立事务：SQLite 的 DDL 是事务性的，失败会连同结构改动一起回滚，
// 库停留在迁移前的状态——不是半建出来的新表加半删的旧列。这种"回滚到干净
// 的上一代"正是可回退性的全部含义。
//
// 版本号与迁移同事务提交。分开写会产生一个致命窗口：迁移成功但版本没写，
// 下次启动又跑一遍；对幂等语句无害，对非幂等的就会炸。放进同一个事务后
// 两者要么都成要么都败。
func (db *DB) runMigrations(ctx context.Context, from, target int) error {
	all, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.Version <= from || m.Version > target {
			continue
		}
		if err := db.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("迁移到 v%d（%s）失败: %w", m.Version, m.Name, err)
		}
	}
	return nil
}

const migrationOpTimeout = 120 * time.Second

// applyMigration 执行单代迁移，包在一个 BEGIN IMMEDIATE 事务里。
//
// 不复用 execScript：它按语句逐条执行且不开事务，中途失败会留下"建了一半"
// 的结构，而那种库文件既升不上去、也回不去。
func (db *DB) applyMigration(ctx context.Context, m migration) error {
	ctx, cancel := context.WithTimeout(ctx, migrationOpTimeout)
	defer cancel()

	conn, err := db.beginImmediate(ctx)
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer conn.Close()

	rollback := func(e error) error {
		// 回滚失败不该盖掉原始错误：原始错误才是"为什么迁移没成功"的答案，
		// 而回滚失败往往是它的次生现象。
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return e
	}

	for _, stmt := range splitStatements(m.SQL) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return rollback(fmt.Errorf("执行迁移语句失败: %w\n语句: %s", err, firstLine(stmt)))
		}
	}
	// 版本号必须与结构改动同事务落库，否则"改完没记账"会在下次启动重放。
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
		return rollback(fmt.Errorf("写入结构版本失败: %w", err))
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(fmt.Errorf("提交迁移事务失败: %w", err))
	}
	return nil
}
