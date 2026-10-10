package store

import "testing"

func TestShareNameMigrationEnablesExistingSharesByDefault(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `INSERT INTO user_groups(name,display_name,is_builtin,permissions,priority,created_at) VALUES('normal','普通用户',1,32831,100,1)`)
	mustExec(t, db, `INSERT INTO users(id,account,password_hash,group_name,created_at,updated_at) VALUES(1,'legacy','x','normal',1,1)`)
	mustExec(t, db, `ALTER TABLE shares DROP COLUMN show_sharer_name`)
	mustExec(t, db, `INSERT INTO shares(id,owner_id,root_path,kind,access_mode,created_at) VALUES('legacy',1,'/','folder','public',1)`)
	mustExec(t, db, `PRAGMA user_version = 4`)
	if err := db.initSchema(); err != nil {
		t.Fatal(err)
	}
	share, err := GetShare(testContext(), db.R(), "legacy")
	if err != nil || !share.ShowSharerName {
		t.Fatalf("旧分享应默认展示分享者名称: %+v %v", share, err)
	}
	if err := db.initSchema(); err != nil {
		t.Fatalf("重启重复迁移: %v", err)
	}
}
