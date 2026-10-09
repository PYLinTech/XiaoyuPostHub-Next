// 结构版本调度：决定这个库文件该被建、被升级，还是该拒绝启动。
//
// 结构演进现在走两条路径：
//
//	v = 0        全新库。跑 schema.sql 直建终版结构。
//	0 < v < cur   老库。逐代执行 migrations/ 里的增量。
//	v = cur       已是最新，什么都不做。
//	v > cur       库比程序新。拒绝启动。
//
// 第四种情况是这套机制里最要紧的一条。旧库可以直接换新程序升上来，
// 但新库不能配旧程序跑——那会让旧代码在缺列上安静地失败，而"安静"
// 意味着它会继续处理业务直到某条数据损坏。真要在旧库文件上回退代码，
// 必须先把 PRAGMA user_version 拨回它认识的版本。
package store

import (
	"context"
	"fmt"
	"time"
)

// currentSchemaVersion 是当前代码期望的结构版本，与 schema.sql 和
// migrations/ 同步演进。
//
// 1 是基线：首批建库直起的结构；v2 邮箱解绑申请；v3 上传平台（收尾队列、流式暂存、
// 密文分卷与用户组资源调度优先级）；v4 并行上传校验摘要。
const currentSchemaVersion = 4

const schemaVersionOpTimeout = 120 * time.Second

// initSchema 读当前版本，按上面的四种情况分派。
func (db *DB) initSchema() error {
	ctx, cancel := context.WithTimeout(context.Background(), schemaVersionOpTimeout)
	defer cancel()

	from, err := db.readUserVersion(ctx)
	if err != nil {
		return err
	}

	switch {
	case from == 0:
		// 全新库：基表已是终版结构，直接盖章。不跑迁移——那只会重复建表。
		if err := db.runSchemaScript(); err != nil {
			return err
		}
		return db.writeUserVersion(ctx, currentSchemaVersion)

	case from < currentSchemaVersion:
		// 老库：逐代升级。任一代失败即返回错误，服务拒绝启动——
		// 结构不对却照常跑业务，比拒绝启动糟糕得多。
		if err := db.runMigrations(ctx, from, currentSchemaVersion); err != nil {
			return err
		}
		return db.writeUserVersion(ctx, currentSchemaVersion)

	case from == currentSchemaVersion:
		return nil

	default:
		return fmt.Errorf(
			"数据库结构版本 v%d 高于本程序支持的 v%d，请升级程序；"+
				"若确需用旧版本运行，请先确认旧代码认识的结构版本",
			from, currentSchemaVersion)
	}
}

// readUserVersion 读 PRAGMA user_version。空库返回 0（SQLite 的默认值）。
func (db *DB) readUserVersion(ctx context.Context) (int, error) {
	var v int
	if err := db.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("读取结构版本失败: %w", err)
	}
	return v, nil
}

// writeUserVersion 单独写版本号。逐代迁移里版本号由 applyMigration 在同一
// 事务内写入，这里只服务于"全新库建完"与"迁移全部跑完"两个收尾场景。
func (db *DB) writeUserVersion(ctx context.Context, v int) error {
	if _, err := db.write.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return fmt.Errorf("写入结构版本失败: %w", err)
	}
	return nil
}

// runSchemaScript 读取并执行内嵌的终版结构脚本。
func (db *DB) runSchemaScript() error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("读取内嵌结构失败: %w", err)
	}
	return db.execScript(string(schema))
}
