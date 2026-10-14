package store

import (
	"context"
	"testing"
)

// rewindDirectLinkSchema 把全新库降回 v8 结构：删掉 v9 直链迁移带来的
// 表与新增列。
//
// 与 rewindTrafficBillingSchema 同一套做法——测试库是按**终版** schema.sql
// 建起来的，回退必须显式撤销每一代迁移新增的对象，否则后面重跑迁移会撞上
// "duplicate column name"。新增一代迁移时，这个清单要跟着加一行。
func rewindDirectLinkSchema(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `DROP TABLE direct_links`)
	for table, columns := range map[string][]string{
		"download_tickets": {"direct_link_id"},
		"traffic_logs":     {"direct_link_id"},
	} {
		for _, column := range columns {
			mustExec(t, db, "ALTER TABLE "+table+" DROP COLUMN "+column)
		}
	}
}

func TestDirectLinkMigrationAllowsIndependentContent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	rewindDirectLinkSchema(t, db)
	mustExec(t, db, `PRAGMA user_version = 8`)
	if err := db.initSchema(); err != nil {
		t.Fatal(err)
	}
	seedContent(t, db, "migration-content", FileNormal)
	owner := seedUser(t, db)
	seedLink(t, db, "retained-link", owner, "migration-content", DirectLink{
		SourcePath: "/original.bin", SourceName: "original.bin", MimeType: "application/octet-stream",
		SizePlain: 1024, ExpiresAt: 2000000000, MaxHits: 7, UpdatedAt: 42,
	})
	mustExec(t, db, `UPDATE direct_links SET hits = 3, disabled = 1 WHERE id = 'retained-link'`)
	before, err := GetDirectLink(ctx, db.R(), "retained-link")
	if err != nil {
		t.Fatal(err)
	}
	if err := ForceDeleteFileRow(ctx, db.W(), before.FileChecksum); err != nil {
		t.Fatalf("直链不应阻止删除内容池记录: %v", err)
	}
	links, err := ListDirectLinksByOwner(ctx, db.R(), owner, 100, 0)
	if err != nil || len(links) != 1 || links[0].TargetAvailable {
		t.Fatalf("内容缺失时应保留不可用的直链: %+v %v", links, err)
	}
	seedContent(t, db, before.FileChecksum, FileNormal)
	links, err = ListDirectLinksByOwner(ctx, db.R(), owner, 100, 0)
	if err != nil || len(links) != 1 || !links[0].TargetAvailable || !links[0].Disabled {
		t.Fatalf("重建内容后应保持旧直链停用: %+v %v", links, err)
	}
}
