package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// TestAPIKeyLifecycleAndGates 覆盖 API Key 的完整闸门：
// 站点开关、用户组权限、接口范围、删除即时失效，以及 SDK 只能由登录会话下载。
func TestAPIKeyLifecycleAndGates(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// 站点没开时管理面表现为功能不存在，而不是 403。
	if status, _ := env.do(t, http.MethodGet, "/api/api-keys", nil, env.login(t)); status != http.StatusNotFound {
		t.Fatalf("站点未启用 API 时管理面应 404，实际 %d", status)
	}
	if err := env.settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyAPIEnabled: "true",
	}, 0); err != nil {
		t.Fatalf("打开 API 开关失败: %v", err)
	}

	session := env.login(t)

	// SDK 下载只允许登录会话；下载内容必须是 ZIP。
	req, err := http.NewRequest(http.MethodGet, env.server.URL+"/api/api-keys/sdk", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.HasPrefix(raw, []byte("PK")) {
		t.Fatalf("SDK 下载应返回 ZIP，实际 %d（%d 字节）", res.StatusCode, len(raw))
	}

	status, payload := env.do(t, http.MethodPost, "/api/api-keys", map[string]any{
		"name": "自动化备份",
	}, session)
	if status != http.StatusOK {
		t.Fatalf("创建 API Key 应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	keyToken, _ := data["token"].(string)
	keyData, _ := data["key"].(map[string]any)
	keyID, _ := keyData["id"].(string)
	if keyToken == "" || keyID == "" {
		t.Fatalf("创建结果缺少令牌或标识: %v", payload)
	}

	// 数据面可用：列目录要求登录身份，能通即证明 API Key 被解析成了同一用户。
	if status, payload := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, keyToken); status != http.StatusOK {
		t.Fatalf("API Key 应能调用文件接口，实际 %d: %v", status, payload)
	}
	// 管理面与 Key 管理本身必须留在登录会话。
	if status, _ := env.do(t, http.MethodGet, "/api/admin/groups", nil, keyToken); status != http.StatusForbidden {
		t.Fatalf("API Key 调管理端应 403，实际 %d", status)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/api-keys", nil, keyToken); status != http.StatusForbidden {
		t.Fatalf("API Key 管理 API Key 应 403，实际 %d", status)
	}

	// 站点总开关关闭后，已签发的 Key 立即失效。
	if err := env.settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyAPIEnabled: "false",
	}, 0); err != nil {
		t.Fatal(err)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/auth/profile", nil, keyToken); status != http.StatusForbidden {
		t.Fatalf("关闭站点 API 后 Key 应 403，实际 %d", status)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/api-keys", nil, session); status != http.StatusNotFound {
		t.Fatalf("关闭站点 API 后管理面应 404，实际 %d", status)
	}
	if err := env.settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyAPIEnabled: "true",
	}, 0); err != nil {
		t.Fatal(err)
	}

	// 用户组收回权限位后同样立即失效；恢复后重新可用。
	adminGroup, err := store.GetGroup(ctx, env.db.R(), perm.GroupAdmin)
	if err != nil {
		t.Fatal(err)
	}
	original := adminGroup.Permissions
	if err := store.UpdateGroup(ctx, env.db.W(), perm.GroupAdmin, adminGroup.DisplayName,
		original&^int64(perm.API), adminGroup.Priority, adminGroup.ResourceSchedulingPriority); err != nil {
		t.Fatal(err)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, keyToken); status != http.StatusForbidden {
		t.Fatalf("用户组无 API 权限时 Key 应 403，实际 %d", status)
	}
	if err := store.UpdateGroup(ctx, env.db.W(), perm.GroupAdmin, adminGroup.DisplayName,
		original, adminGroup.Priority, adminGroup.ResourceSchedulingPriority); err != nil {
		t.Fatal(err)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, keyToken); status != http.StatusOK {
		t.Fatalf("恢复权限后 Key 应重新可用，实际 %d", status)
	}

	// 删除后下一次请求立即失效（不做身份缓存）。
	if status, payload := env.do(t, http.MethodDelete, "/api/api-keys/"+keyID, nil, session); status != http.StatusOK {
		t.Fatalf("删除 API Key 应成功，实际 %d: %v", status, payload)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, keyToken); status != http.StatusUnauthorized {
		t.Fatalf("删除后 Key 应 401，实际 %d", status)
	}
}
