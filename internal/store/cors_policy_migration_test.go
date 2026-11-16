package store

import "testing"

// TestCORSPolicyMigrationDropsLegacySettings 验证 v14 → v15：主站与 SDK 配置保留，
// 旧的分享与直链跨域配置从库中删除。
func TestCORSPolicyMigrationDropsLegacySettings(t *testing.T) {
	db := openTestDB(t)
	for _, item := range []struct {
		key   string
		value string
	}{
		{"cors.main_enabled", "true"},
		{"cors.main_origins", "https://main.example.com"},
		{"cors.sdk_enabled", "true"},
		{"cors.sdk_origins", "https://sdk.example.com"},
		{"cors.share_enabled", "true"},
		{"cors.share_origins", "https://share.example.com"},
		{"cors.direct_link_enabled", "true"},
		{"cors.direct_link_origins", "https://links.example.com"},
	} {
		mustExec(t, db, `INSERT INTO system_config
			(key, value, value_type, updated_at, updated_by) VALUES (?, ?, 'string', 1, 0)`,
			item.key, item.value)
	}
	mustExec(t, db, `PRAGMA user_version = 14`)

	if err := db.initSchema(); err != nil {
		t.Fatalf("v14 → v15 迁移失败: %v", err)
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Fatalf("迁移后版本应为 %d，得 %d", currentSchemaVersion, got)
	}

	var legacy int
	mustQuery(t, db, `SELECT COUNT(*) FROM system_config WHERE key IN (
		'cors.share_enabled', 'cors.share_origins',
		'cors.direct_link_enabled', 'cors.direct_link_origins')`, &legacy)
	if legacy != 0 {
		t.Fatalf("旧分享与直链配置应被删除，实剩 %d 项", legacy)
	}
	for key, want := range map[string]string{
		"cors.main_enabled": "true",
		"cors.main_origins": "https://main.example.com",
		"cors.sdk_enabled":  "true",
		"cors.sdk_origins":  "https://sdk.example.com",
	} {
		var got string
		mustQuery(t, db, `SELECT value FROM system_config WHERE key = ?`, &got, key)
		if got != want {
			t.Fatalf("配置 %s 应保留 %q，实得 %q", key, want, got)
		}
	}
}
