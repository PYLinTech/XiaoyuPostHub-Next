package store

import (
	"context"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/apikey"
)

// TestAPIKeyScopesMigration 验证 v13 → v14：老 Key 补齐全部能力位，
// 原有字段与数据保持不变。
func TestAPIKeyScopesMigration(t *testing.T) {
	db := openTestDB(t)
	owner := seedUser(t, db)
	mustExec(t, db, `INSERT INTO api_keys
		(id, user_id, name, key_hash, prefix, workspace_path, expires_at, last_used_at, created_at)
		VALUES ('legacy-key', ?, '旧 Key', 'legacy-hash', 'xph_legacy', '/docs', 2000000000, 123, 456)`, owner)
	mustExec(t, db, `ALTER TABLE api_keys DROP COLUMN scopes`)
	mustExec(t, db, `PRAGMA user_version = 13`)

	if err := db.initSchema(); err != nil {
		t.Fatalf("v13 → v14 迁移失败: %v", err)
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Fatalf("迁移后版本应为 %d，得 %d", currentSchemaVersion, got)
	}

	key, err := GetAPIKeyByHash(context.Background(), db.R(), "legacy-hash")
	if err != nil {
		t.Fatalf("迁移后读取旧 Key 失败: %v", err)
	}
	if key.Scopes != int64(apikey.All) {
		t.Fatalf("旧 Key 应补齐为全部能力 %d，实际 %d", apikey.All, key.Scopes)
	}
	if key.ID != "legacy-key" || key.UserID != owner || key.Name != "旧 Key" ||
		key.Prefix != "xph_legacy" || key.WorkspacePath != "/docs" ||
		key.ExpiresAt != 2000000000 || key.LastUsedAt != 123 || key.CreatedAt != 456 {
		t.Fatalf("迁移不应改动旧 Key 的其它字段: %+v", key)
	}
}
