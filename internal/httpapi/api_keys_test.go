package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
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
	if status, payload := env.do(t, http.MethodPost, "/api/fs/mkdir", map[string]any{
		"parentPath": "/",
		"name":       "api-workspace",
	}, session); status != http.StatusOK {
		t.Fatalf("创建工作区目录失败: %d %v", status, payload)
	}

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
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="XiaoyuPostHub-Next_SDK_v2.zip"` {
		t.Fatalf("SDK 下载文件名不符: %q", got)
	}

	// 版本查询公开：已分发脚本不需要凭据也能判断是否需要更新。
	status, payload := env.do(t, http.MethodGet, "/api/sdk/version", nil, "")
	if status != http.StatusOK {
		t.Fatalf("SDK 版本查询应公开可用，实际 %d: %v", status, payload)
	}
	versionData, _ := payload["data"].(map[string]any)
	if versionData["version"] != "v2" {
		t.Fatalf("SDK 版本应为 v2，实际 %v", versionData["version"])
	}

	status, payload = env.do(t, http.MethodPost, "/api/api-keys", map[string]any{
		"name":          "自动化备份",
		"workspacePath": "/api-workspace",
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
	if got := keyData["workspacePath"]; got != "/api-workspace" {
		t.Fatalf("创建结果应回显工作区，实际 %v", got)
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

// TestAPIKeyWorkspaceIsolation 从 HTTP 面验证工作区边界：调用方看到的 "/"
// 是工作区目录，响应不泄露账号内绝对路径，越界与动工作区根都被拒绝，
// 目录改名后 Key 跟随，工作区被删后 Key 一起失效。
func TestAPIKeyWorkspaceIsolation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	if err := env.settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyAPIEnabled: "true",
	}, 0); err != nil {
		t.Fatalf("打开 API 开关失败: %v", err)
	}
	session := env.login(t)

	mkdir := func(parent, name string) {
		t.Helper()
		if status, payload := env.do(t, http.MethodPost, "/api/fs/mkdir", map[string]any{
			"parentPath": parent,
			"name":       name,
		}, session); status != http.StatusOK {
			t.Fatalf("创建目录 %s/%s 失败: %d %v", parent, name, status, payload)
		}
	}
	mkdir("/", "ws-a")
	mkdir("/ws-a", "child")
	mkdir("/", "outside")
	mkdir("/", "ws-b")

	// 工作区必填且目录必须存在；根目录与普通目录一样可选。
	if status, _ := env.do(t, http.MethodPost, "/api/api-keys", map[string]any{
		"name": "缺工作区",
	}, session); status != http.StatusBadRequest {
		t.Fatalf("未选工作区应 400，实际 %d", status)
	}
	if status, _ := env.do(t, http.MethodPost, "/api/api-keys", map[string]any{
		"name": "不存在", "workspacePath": "/missing",
	}, session); status != http.StatusNotFound {
		t.Fatalf("不存在的工作区应 404，实际 %d", status)
	}

	status, payload := env.do(t, http.MethodPost, "/api/api-keys", map[string]any{
		"name": "根工作区", "workspacePath": "/",
	}, session)
	if status != http.StatusOK {
		t.Fatalf("根目录工作区应可创建，实际 %d: %v", status, payload)
	}
	rootData, _ := payload["data"].(map[string]any)
	rootKey, _ := rootData["key"].(map[string]any)
	if rootKey["workspacePath"] != "/" {
		t.Fatalf("根目录工作区应回显 /，实际 %v", rootKey["workspacePath"])
	}
	rootToken, _ := rootData["token"].(string)
	status, payload = env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, rootToken)
	if status != http.StatusOK {
		t.Fatalf("根工作区 Key 列根目录失败: %d %v", status, payload)
	}
	rootList, _ := payload["data"].(map[string]any)
	rootItems, _ := rootList["items"].([]any)
	if len(rootItems) != 3 {
		t.Fatalf("根工作区 Key 应看到全部 3 个目录，实际 %v", rootItems)
	}

	status, payload = env.do(t, http.MethodPost, "/api/api-keys", map[string]any{
		"name": "工作区 A", "workspacePath": "/ws-a",
	}, session)
	if status != http.StatusOK {
		t.Fatalf("创建 Key 失败: %d %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	keyToken, _ := data["token"].(string)

	// 列工作区根：返回相对路径，且不出现账号内的绝对路径。
	status, payload = env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, keyToken)
	if status != http.StatusOK {
		t.Fatalf("列工作区根失败: %d %v", status, payload)
	}
	listData, _ := payload["data"].(map[string]any)
	items, _ := listData["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("工作区根应只有 1 个子目录，实际 %v", items)
	}
	first, _ := items[0].(map[string]any)
	if first["path"] != "/child" || first["name"] != "child" {
		t.Fatalf("子目录应映射成 /child，实际 %v", first)
	}
	if bytes.Contains(mustJSON(t, payload), []byte("/ws-a")) {
		t.Fatalf("响应泄露了工作区的绝对路径: %v", payload)
	}

	// 根目录映射为工作区目录本身，但调用方视角仍是 "/"。
	status, payload = env.do(t, http.MethodGet, "/api/fs/stat?path=/", nil, keyToken)
	if status != http.StatusOK {
		t.Fatalf("读取工作区根失败: %d %v", status, payload)
	}
	statData, _ := payload["data"].(map[string]any)
	node, _ := statData["node"].(map[string]any)
	if node["path"] != "/" || node["parentPath"] != "" {
		t.Fatalf("工作区根应映射成 /，实际 %v", node)
	}

	// 4xx 的 detail 也会回给客户端，错误文案同样不能泄露工作区绝对路径。
	status, payload = env.do(t, http.MethodPost, "/api/fs/mkdir", map[string]any{
		"parentPath": "/", "name": "child",
	}, keyToken)
	if status != http.StatusConflict {
		t.Fatalf("重建同名目录应 409，实际 %d: %v", status, payload)
	}
	if bytes.Contains(mustJSON(t, payload), []byte("/ws-a")) {
		t.Fatalf("错误详情泄露了工作区的绝对路径: %v", payload)
	}

	// 越界路径一律按不存在处理；带 .. 的路径在规范化阶段被拒。
	if status, _ := env.do(t, http.MethodGet, "/api/fs/stat?path=/outside", nil, keyToken); status != http.StatusNotFound {
		t.Fatalf("工作区外路径应 404，实际 %d", status)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/fs/stat?path=/../outside", nil, keyToken); status != http.StatusBadRequest {
		t.Fatalf("含 .. 的路径应 400，实际 %d", status)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/fs/stat?path=/ws-b", nil, keyToken); status != http.StatusNotFound {
		t.Fatalf("绝对外部路径应映射进工作区并按 404 处理，实际 %d", status)
	}

	// 工作区根不能被重命名、移动或删除。
	if status, _ := env.do(t, http.MethodPost, "/api/fs/rename", map[string]any{
		"path": "/", "newName": "evil",
	}, keyToken); status != http.StatusBadRequest {
		t.Fatalf("重命名工作区根应 400，实际 %d", status)
	}
	if status, _ := env.do(t, http.MethodPost, "/api/fs/move", map[string]any{
		"path": "/", "destParent": "/child",
	}, keyToken); status != http.StatusBadRequest {
		t.Fatalf("移动工作区根应 400，实际 %d", status)
	}
	if status, _ := env.do(t, http.MethodPost, "/api/fs/delete", map[string]any{
		"path": "/",
	}, keyToken); status != http.StatusBadRequest {
		t.Fatalf("删除工作区根应 400，实际 %d", status)
	}

	// 工作区内的写操作落到正确位置，并返回相对路径。
	if status, payload := env.do(t, http.MethodPost, "/api/fs/mkdir", map[string]any{
		"parentPath": "/", "name": "uploaded",
	}, keyToken); status != http.StatusOK {
		t.Fatalf("在工作区内建目录失败: %d %v", status, payload)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/fs/stat?path=/ws-a/uploaded", nil, session); status != http.StatusOK {
		t.Fatalf("Key 建的目录应落在工作区绝对路径下，实际 %d", status)
	}

	// 账号内目录改名后，Key 的工作区跟随；工作区被删后 Key 一并失效。
	if status, payload := env.do(t, http.MethodPost, "/api/fs/rename", map[string]any{
		"path": "/ws-a", "newName": "ws-renamed",
	}, session); status != http.StatusOK {
		t.Fatalf("重命名工作区目录失败: %d %v", status, payload)
	}
	if status, payload := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, keyToken); status != http.StatusOK {
		t.Fatalf("目录改名后 Key 应继续可用，实际 %d %v", status, payload)
	}
	if status, payload := env.do(t, http.MethodPost, "/api/fs/delete", map[string]any{
		"path": "/ws-renamed",
	}, session); status != http.StatusOK {
		t.Fatalf("删除工作区目录失败: %d %v", status, payload)
	}
	if status, _ := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, keyToken); status != http.StatusUnauthorized {
		t.Fatalf("工作区被删后 Key 应失效（401），实际 %d", status)
	}
	if status, payload := env.do(t, http.MethodGet, "/api/api-keys", nil, session); status != http.StatusOK {
		t.Fatalf("读取 Key 列表失败: %d %v", status, payload)
	} else if listData, _ := payload["data"].(map[string]any); listData != nil {
		remaining, _ := listData["items"].([]any)
		if len(remaining) != 1 {
			t.Fatalf("工作区被删后只应保留根目录 Key: %v", remaining)
		}
		if entry, _ := remaining[0].(map[string]any); entry["name"] != "根工作区" {
			t.Fatalf("残留的应是根目录 Key，实际 %v", remaining)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化响应失败: %v", err)
	}
	return raw
}
