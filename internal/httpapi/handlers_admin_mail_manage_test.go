package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// loginNormalUser 在测试库造一个普通用户并登录，返回其 token。
func loginNormalUser(t *testing.T, env *testEnv, account string) string {
	t.Helper()
	hash, err := auth.HashPassword("normal-user-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(context.Background(), env.db.W(), store.User{
		Account: account, PasswordHash: hash,
		GroupName: perm.GroupNormal, Status: store.UserEnabled,
	}); err != nil {
		t.Fatal(err)
	}
	_, payload := env.do(t, http.MethodPost, "/api/auth/login", map[string]any{
		"account": account, "password": "normal-user-password",
	}, "")
	token, _ := payload["data"].(map[string]any)["token"].(string)
	if token == "" {
		t.Fatalf("普通用户登录失败: %v", payload)
	}
	return token
}

// TestAdminGroupCarriesMailDomain 收件域名随用户组一起提交与返回。
//
// 域名曾经有过一套独立的 /api/admin/mail/domains 端点，那等于给同一个
// 字段留了两个写入口。这里一并锁住它的下线。
func TestAdminGroupCarriesMailDomain(t *testing.T) {
	env := newTestEnv(t)
	adminToken := env.login(t)
	normalToken := loginNormalUser(t, env, "normal")

	// 旧端点已下线：域名的唯一写入口是用户组。
	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/admin/mail/domains"},
		{http.MethodPost, "/api/admin/mail/domains"},
		{http.MethodPatch, "/api/admin/mail/domains/d5.example"},
		{http.MethodDelete, "/api/admin/mail/domains/d5.example"},
	} {
		if status, _ := env.do(t, c.method, c.path, map[string]any{"domain": "d5.example"}, adminToken); status != http.StatusNotFound {
			t.Fatalf("%s %s 应 404（端点已下线），得 %d", c.method, c.path, status)
		}
	}

	// 建组 + 绑域名一次完成。
	status, payload := env.do(t, http.MethodPost, "/api/admin/groups", map[string]any{
		"name": "ops", "displayName": "运维组",
		"receiveDomains": []map[string]any{{"domain": "ops.example.com", "receiveEnabled": true}},
	}, adminToken)
	if status != http.StatusOK {
		t.Fatalf("建组应 200，得 %d: %v", status, payload)
	}

	// 列表里要能看到这个域名，地址数也要带出来。
	status, payload = env.do(t, http.MethodGet, "/api/admin/groups", nil, adminToken)
	if status != http.StatusOK {
		t.Fatalf("列组应 200，得 %d: %v", status, payload)
	}
	var found map[string]any
	for _, it := range payload["data"].(map[string]any)["items"].([]any) {
		if it.(map[string]any)["group"].(map[string]any)["name"] == "ops" {
			for _, d := range it.(map[string]any)["mailDomains"].([]any) {
				if d.(map[string]any)["domain"] == "ops.example.com" {
					found = d.(map[string]any)
				}
			}
		}
	}
	if found == nil {
		t.Fatalf("ops 组应带出 mailDomains: %v", payload)
	}
	if found["receiveEnabled"] != true {
		t.Fatalf("mailDomains 内容不对: %v", found)
	}
	// JSON 里必须是数字 0 而不是 null：前端要拿它判断"域名能不能移除"。
	if _, ok := found["addressCount"].(float64); !ok {
		t.Fatalf("addressCount 应为数字，得 %v", found["addressCount"])
	}

	// 没托管域名的组给出空数组而不是 null，前端要直接对它取 .length。
	for _, it := range payload["data"].(map[string]any)["items"].([]any) {
		grp := it.(map[string]any)
		if grp["group"].(map[string]any)["name"] == "admin" {
			if md, ok := grp["mailDomains"].([]any); !ok || len(md) != 0 {
				t.Fatalf("admin 组没域名，mailDomains 应为空数组，得 %v", grp["mailDomains"])
			}
		}
	}

	// 普通用户无权建组。
	if status, _ = env.do(t, http.MethodPost, "/api/admin/groups", map[string]any{
		"name": "x", "displayName": "x", "receiveDomains": []map[string]any{{"domain": "x.example.com", "receiveEnabled": true}},
	}, normalToken); status != http.StatusForbidden {
		t.Fatalf("普通用户建组应 403，得 %d", status)
	}

	// 同一个域名可以被第二个组共用：这是多对多模型下的合法操作。
	if status, _ = env.do(t, http.MethodPost, "/api/admin/groups", map[string]any{
		"name": "ops2", "displayName": "运维二组", "receiveDomains": []map[string]any{{"domain": "ops.example.com", "receiveEnabled": true}},
	}, adminToken); status != http.StatusOK {
		t.Fatalf("共用域名应被允许，得 %d", status)
	}
	if _, err := store.GetGroupMailDomain(context.Background(), env.db.R(), "ops", "ops.example.com"); err != nil {
		t.Fatalf("ops 的原有绑定不应被改动: %v", err)
	}
	if _, err := store.GetGroupMailDomain(context.Background(), env.db.R(), "ops2", "ops.example.com"); err != nil {
		t.Fatalf("ops2 应绑上同一域名: %v", err)
	}
	// 清理 ops2：先移除它托管的域名，否则删组会被绑定挡住。
	if status, _ = env.do(t, http.MethodPost, "/api/admin/groups", map[string]any{
		"name": "ops2", "displayName": "运维二组", "receiveDomains": []map[string]any{},
	}, adminToken); status != http.StatusOK {
		t.Fatalf("移除 ops2 的域名应 200，得 %d", status)
	}
	if status, _ = env.do(t, http.MethodDelete, "/api/admin/groups/ops2", nil, adminToken); status != http.StatusOK {
		t.Fatalf("清理 ops2 失败，得 %d", status)
	}

	// 绑着域名的组删不掉。
	if status, _ = env.do(t, http.MethodDelete, "/api/admin/groups/ops", nil, adminToken); status != http.StatusConflict {
		t.Fatalf("绑域名的组删除应 409，得 %d", status)
	}

	// 移除域名后可以删。
	if status, _ = env.do(t, http.MethodPost, "/api/admin/groups", map[string]any{
		"name": "ops", "displayName": "运维组", "receiveDomains": []map[string]any{},
	}, adminToken); status != http.StatusOK {
		t.Fatalf("移除域名应 200")
	}
	if _, err := store.GetMailDomain(context.Background(), env.db.R(), "ops.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("移除后域名应消失，得 %v", err)
	}
	if status, _ = env.do(t, http.MethodDelete, "/api/admin/groups/ops", nil, adminToken); status != http.StatusOK {
		t.Fatalf("解绑后组应可删，得 %d", status)
	}
}

// TestAdminSaveGroupAcceptsFrontendPayloadShape 用**前端真实提交的形状**建组。
//
// 这条用例是有来由的：receiveDomains 一度在后端声明成字符串数组，而前端提交
// 的是带 receiveEnabled 的对象数组，形状不匹配的表象是解码 400 —— 而
// "新建组并绑域名"这条主路径当时没有任何测试用前端的写法跑过，于是带域名的
// 用户组一律存不进去。Go 的类型与 TypeScript 的类型分处两个世界，中间没有任何
// 编译期联系，只能靠这条用例把接缝钉住。
func TestAdminSaveGroupAcceptsFrontendPayloadShape(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	status, payload := env.do(t, http.MethodPost, "/api/admin/groups", map[string]any{
		"name": "vip", "displayName": "会员组",
		"receiveDomains": []map[string]any{
			{"domain": "vip.example.com", "receiveEnabled": true},
			{"domain": "paused.example.com", "receiveEnabled": false},
		},
	}, token)
	if status != http.StatusOK {
		t.Fatalf("前端提交的形状必须被接受，得 %d: %v", status, payload)
	}

	// 开关必须一路落库，不能只在解码那层被接受。
	binding, err := store.GetGroupMailDomain(context.Background(), env.db.R(),
		"vip", "paused.example.com")
	if err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if binding.ReceiveEnabled {
		t.Fatal("提交 receiveEnabled=false，库里却记成了开启")
	}
}

func TestAdminGlobalMailLifecycle(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t) // admin 同时是这封信的属主
	seedHTTPMail(t, env, "m5-admin@ext.example")

	// 从用户视角拿到 mailbox 行 id，并确认初始未读。
	status, payload := env.do(t, http.MethodGet, "/api/mail/messages?view=inbox", nil, token)
	if status != http.StatusOK {
		t.Fatalf("用户列表应 200，得 %d: %v", status, payload)
	}
	first := payload["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
	boxID := int64(first["id"].(float64))
	boxPath := "/api/admin/mail/mailboxes/" + strconv.FormatInt(boxID, 10)

	// 普通用户无权访问全局视图。
	normalToken := loginNormalUser(t, env, "normal2")
	if status, _ = env.do(t, http.MethodGet, "/api/admin/mail/messages", nil, normalToken); status != http.StatusForbidden {
		t.Fatalf("普通用户全局列表应 403，得 %d", status)
	}

	// 全局列表：默认 normal 命中，方向+关键词过滤生效。
	status, payload = env.do(t, http.MethodGet,
		"/api/admin/mail/messages?direction=in&q=ext.example", nil, token)
	if status != http.StatusOK {
		t.Fatalf("全局列表应 200，得 %d: %v", status, payload)
	}
	data := payload["data"].(map[string]any)
	if data["total"].(float64) != 1 {
		t.Fatalf("应命中 1 行，得 %v", data["total"])
	}
	row := data["items"].([]any)[0].(map[string]any)
	if row["ownerAccount"] != "admin" {
		t.Fatalf("属主应是 admin，得 %v", row["ownerAccount"])
	}

	// 按属主账号过滤命中 / 无命中。
	for _, c := range []struct {
		path string
		want float64
	}{
		{"/api/admin/mail/messages?owner=admin", 1},
		{"/api/admin/mail/messages?owner=ghost-account", 0},
	} {
		status, payload = env.do(t, http.MethodGet, c.path, nil, token)
		if status != http.StatusOK {
			t.Fatalf("%s 应 200，得 %d: %v", c.path, status, payload)
		}
		if got := payload["data"].(map[string]any)["total"].(float64); got != c.want {
			t.Fatalf("%s 应 %v 行，得 %v", c.path, c.want, got)
		}
	}
	// 非法 userId → 400。
	if status, _ = env.do(t, http.MethodGet, "/api/admin/mail/messages?userId=-1", nil, token); status != http.StatusBadRequest {
		t.Fatalf("负 userId 应 400，得 %d", status)
	}

	// 全局详情不标已读。
	if status, payload = env.do(t, http.MethodGet, boxPath, nil, token); status != http.StatusOK {
		t.Fatalf("全局详情应 200，得 %d: %v", status, payload)
	}
	if n := countUnreadMailbox(env, boxID); n != 1 {
		t.Fatalf("管理员查看后用户未读不应变化，得 %d", n)
	}

	// 处置生命周期。
	for _, c := range []struct {
		method, suffix string
		want           int
	}{
		{http.MethodPost, "/archive", http.StatusOK},
		{http.MethodPost, "/restore", http.StatusOK},
		{http.MethodPost, "/purge", http.StatusBadRequest}, // normal 态不能直接销毁
		{http.MethodPost, "/archive", http.StatusOK},
		{http.MethodPost, "/purge", http.StatusOK},
	} {
		if status, payload = env.do(t, c.method, boxPath+c.suffix, nil, token); status != c.want {
			t.Fatalf("%s%s 应 %d，得 %d: %v", c.method, c.suffix, c.want, status, payload)
		}
	}
	// 已销毁：详情仍可读元数据（销毁留痕），状态为 released。
	if status, payload = env.do(t, http.MethodGet, boxPath, nil, token); status != http.StatusOK {
		t.Fatalf("released 详情应 200（留痕），得 %d: %v", status, payload)
	}
	if got := payload["data"].(map[string]any)["box"].(map[string]any)["status"]; got != "released" {
		t.Fatalf("归属状态应 released，得 %v", got)
	}
	// 已销毁再处置 → 404；非法 box id → 400。
	if status, _ = env.do(t, http.MethodPost, boxPath+"/purge", nil, token); status != http.StatusNotFound {
		t.Fatalf("released 再销毁应 404，得 %d", status)
	}
	if status, _ = env.do(t, http.MethodPost, "/api/admin/mail/mailboxes/0/archive", nil, token); status != http.StatusBadRequest {
		t.Fatalf("box id=0 应 400，得 %d", status)
	}
}

func countUnreadMailbox(env *testEnv, boxID int64) int {
	var n int
	if err := env.db.R().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM mailboxes WHERE id = ? AND is_read = 0`, boxID).Scan(&n); err != nil {
		return -1
	}
	return n
}
