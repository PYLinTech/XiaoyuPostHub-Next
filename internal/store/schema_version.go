// 结构版本标记。
//
// 库结构只有一种形态：schema.sql 里的最终结构。启动时执行它并把
// PRAGMA user_version 戳成当前版本，就结束了——没有迁移、没有版本分支、
// 没有"这是旧库吗"的判断。
//
// schema.sql 全部是 CREATE ... IF NOT EXISTS，所以重复执行是幂等的：
// 全新库得到完整结构，同一个库文件反复打开不产生任何变化。
//
// 但幂等只覆盖**缺失的对象**，不覆盖**改过的定义**：CREATE TABLE IF NOT
// EXISTS 遇到同名表会原样跳过，因此把一张已有表的列改掉之后，旧库文件里
// 仍是老结构，而且不会报错——它会安静地在缺列上失败。所以改动已有表的
// 结构时，唯一正确的做法是**重建库文件**，本项目不为此提供任何迁移脚本。
//
// currentSchemaVersion 的用途是**标记**：让运维看一眼就知道这个库文件是
// 哪一版结构创建的。它不参与任何控制流，也不承担"兼容旧结构"的职责。
// 结构变了就直接改这个数字，程序假定库文件与它同源——开发期反复重建库
// 文件本来就是常态，为"从旧结构平滑升上来"保留一套判断逻辑，换来的是
// 零收益的复杂度，以及一个必须永远维护正确的兼容分支。

package store

import (
	"context"
	"fmt"
	"time"
)

// currentSchemaVersion 是当前代码期望的结构版本，与 schema.sql 同步演进。
//
// 恒为 1：库结构从零直建、从零演进，没有第二个版本，也就没有需要平滑升上来
// 的旧结构。发信链路（mail_senders、direction、reply_to 等）移除时随库文件
// 一起重建，不占用版本号。
const currentSchemaVersion = 1

const schemaVersionOpTimeout = 60 * time.Second

// initSchema 执行最终结构脚本并戳记结构版本。
func (db *DB) initSchema() error {
	ctx, cancel := context.WithTimeout(context.Background(), schemaVersionOpTimeout)
	defer cancel()

	if err := db.runSchemaScript(); err != nil {
		return err
	}
	if _, err := db.write.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
		return fmt.Errorf("写入结构版本失败: %w", err)
	}
	return nil
}

// runSchemaScript 读取并执行内嵌的最终结构脚本。
func (db *DB) runSchemaScript() error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("读取内嵌结构失败: %w", err)
	}
	return db.execScript(string(schema))
}
