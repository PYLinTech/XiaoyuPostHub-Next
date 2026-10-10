package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

func TestShareNameVisibilityEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	uid := seedArchiveFixture(t, env)
	ctx := context.Background()
	if _, err := env.db.W().ExecContext(ctx, `UPDATE users SET display_name = '小鱼' WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	object := func(status int, payload map[string]any, key string) map[string]any {
		t.Helper()
		if status != http.StatusOK {
			t.Fatalf("HTTP %d: %v", status, payload)
		}
		return payload["data"].(map[string]any)[key].(map[string]any)
	}
	status, payload := env.do(t, http.MethodPost, "/api/share", map[string]any{"path": "/d/a.txt", "kind": "file"}, token)
	created := object(status, payload, "share")
	if created["showSharerName"] != true {
		t.Fatalf("新分享应默认展示名称: %v", created)
	}
	id := created["id"].(string)
	saved, err := store.GetShare(ctx, env.db.R(), id)
	if err != nil || !saved.ShowSharerName {
		t.Fatalf("选项未持久化: %+v %v", saved, err)
	}
	status, payload = env.do(t, http.MethodPost, "/api/share/"+id+"/pickup", map[string]any{"maxUses": 2}, token)
	code := object(status, payload, "pickup")["code"].(string)
	assertGuest := func(endpoint, name string) {
		t.Helper()
		status, payload := env.do(t, http.MethodPost, endpoint, map[string]any{}, "")
		guest := object(status, payload, "share")
		if name == "" {
			if _, exists := guest["sharerName"]; exists {
				t.Fatalf("关闭或空名称仍被返回: %v", guest)
			}
		} else if guest["sharerName"] != name {
			t.Fatalf("名称错误: %v", guest)
		}
		for _, key := range []string{"ownerId", "rootPath", "account", "email", "pwdHash", "pwdSalt"} {
			if _, exists := guest[key]; exists {
				t.Fatalf("访客泄露 %s", key)
			}
		}
		if guest["rootName"] != "a.txt" || guest["size"] != float64(12) {
			t.Fatalf("文件展示信息错误: %v", guest)
		}
	}
	for _, endpoint := range []string{"/api/s/" + id + "/resolve", "/api/p/" + code + "/resolve"} {
		assertGuest(endpoint, "小鱼")
	}
	status, payload = env.do(t, http.MethodPatch, "/api/share/"+id, map[string]any{"showSharerName": false}, token)
	if object(status, payload, "share")["showSharerName"] != false {
		t.Fatal("无法关闭名称")
	}
	for _, endpoint := range []string{"/api/s/" + id + "/resolve", "/api/p/" + code + "/resolve"} {
		assertGuest(endpoint, "")
	}
	status, payload = env.do(t, http.MethodPatch, "/api/share/"+id, map[string]any{"maxVisits": 50}, token)
	if object(status, payload, "share")["showSharerName"] != false {
		t.Fatal("无关更新重置名称选项")
	}
	status, payload = env.do(t, http.MethodPost, "/api/share", map[string]any{"path": "/d", "kind": "folder", "showSharerName": false}, token)
	if object(status, payload, "share")["showSharerName"] != false {
		t.Fatal("创建时关闭无效")
	}
	status, payload = env.do(t, http.MethodPatch, "/api/share/"+id, map[string]any{"showSharerName": true}, token)
	object(status, payload, "share")
	if _, err := env.db.W().ExecContext(ctx, `UPDATE users SET display_name = '' WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	assertGuest("/api/s/"+id+"/resolve", "") // 空名称不能回退到账号。
	status, payload = env.do(t, http.MethodPatch, "/api/share/"+id, map[string]any{"accessMode": "password", "password": "ABCD"}, token)
	object(status, payload, "share")
	status, payload = env.do(t, http.MethodPost, "/api/s/"+id+"/resolve", map[string]any{}, "")
	if status != http.StatusForbidden {
		t.Fatalf("提取码校验前应拒绝: %d %v", status, payload)
	}
}
