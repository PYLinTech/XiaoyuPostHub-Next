package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/apikey"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
)

func corsDo(t *testing.T, env *testEnv, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, env.server.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// TestCORSPoliciesAreBinary 钉住“页面 / 接口”二分：分享页归主站，
// API 与直链交付归 SDK，两个策略不能互相覆盖。
func TestCORSPoliciesAreBinary(t *testing.T) {
	env := newTestEnv(t)
	mainOrigin := "https://main.example.com"
	sdkOrigin := "https://sdk.example.com"

	res := corsDo(t, env, http.MethodGet, "/", map[string]string{"Origin": mainOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("默认应关闭页面跨域: %v", res.Header)
	}
	res = corsDo(t, env, http.MethodGet, "/api/healthz", map[string]string{"Origin": sdkOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("默认应关闭接口跨域: %v", res.Header)
	}

	if err := env.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCORSMainEnabled: "true",
		settings.KeyCORSMainOrigins: mainOrigin,
	}, 1); err != nil {
		t.Fatalf("设置主站跨域失败: %v", err)
	}
	for _, path := range []string{"/", "/s/share-1"} {
		res = corsDo(t, env, http.MethodGet, path, map[string]string{"Origin": mainOrigin})
		if res.Header.Get("Access-Control-Allow-Origin") != mainOrigin {
			t.Fatalf("页面路径 %s 应使用主站策略: %v", path, res.Header)
		}
	}
	for _, path := range []string{"/api/healthz", "/api/s/share-1/resolve", "/d/token"} {
		res = corsDo(t, env, http.MethodGet, path, map[string]string{"Origin": mainOrigin})
		if res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("接口路径 %s 不应使用主站策略: %v", path, res.Header)
		}
	}

	if err := env.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCORSSDKEnabled: "true",
		settings.KeyCORSSDKOrigins: sdkOrigin,
	}, 1); err != nil {
		t.Fatalf("设置 SDK 跨域失败: %v", err)
	}
	for _, path := range []string{"/api/healthz", "/api/s/share-1/resolve", "/d/token"} {
		res = corsDo(t, env, http.MethodGet, path, map[string]string{"Origin": sdkOrigin})
		if res.Header.Get("Access-Control-Allow-Origin") != sdkOrigin {
			t.Fatalf("接口路径 %s 应使用 SDK 策略: %v", path, res.Header)
		}
	}
	res = corsDo(t, env, http.MethodGet, "/s/share-1", map[string]string{"Origin": sdkOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("页面路径不应使用 SDK 策略: %v", res.Header)
	}

	// 预检也按同一二分规则选择策略，并且在身份解析前结束。
	res = corsDo(t, env, http.MethodOptions, "/api/admin/settings", map[string]string{
		"Origin": sdkOrigin, "Access-Control-Request-Method": "GET",
		"Access-Control-Request-Headers": "authorization,content-type",
	})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != sdkOrigin {
		t.Fatalf("SDK 预检应在鉴权前通过: status=%d headers=%v", res.StatusCode, res.Header)
	}
	if res.Header.Get("Access-Control-Allow-Headers") != corsAllowedHeaders ||
		res.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("预检应允许指定请求头但不允许跨域 Cookie: %v", res.Header)
	}
	res = corsDo(t, env, http.MethodOptions, "/s/share-1", map[string]string{
		"Origin": mainOrigin, "Access-Control-Request-Method": "GET",
	})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != mainOrigin {
		t.Fatalf("页面预检应使用主站策略: status=%d headers=%v", res.StatusCode, res.Header)
	}
}

// TestCORSPolicyClassificationIsExhaustive 覆盖页面与接口的边界路径，
// 确保每个路径只会落到一个策略里。
func TestCORSPolicyClassificationIsExhaustive(t *testing.T) {
	cfg := settings.CORSRuntime{
		Main: settings.CORSPolicy{Enabled: true, Origins: []string{"https://main.example.com"}},
		SDK:  settings.CORSPolicy{Enabled: true, Origins: []string{"https://sdk.example.com"}},
	}
	cases := []struct {
		path string
		want string
	}{
		{"/", "https://main.example.com"},
		{"/s/share-1", "https://main.example.com"},
		{"/assets/app.js", "https://main.example.com"},
		{"/login", "https://main.example.com"},
		{"/api-keys", "https://main.example.com"},
		{"/api", "https://sdk.example.com"},
		{"/api/healthz", "https://sdk.example.com"},
		{"/api/s/share-1/resolve", "https://sdk.example.com"},
		{"/api/p/code/download", "https://sdk.example.com"},
		{"/api/direct-link", "https://sdk.example.com"},
		{"/d", "https://sdk.example.com"},
		{"/d/token", "https://sdk.example.com"},
		// 带点段的路径按清理后的目标归类，与 ServeMux 的最终路由一致。
		{"/api/../s/share-1", "https://main.example.com"},
		{"/s/../api/healthz", "https://sdk.example.com"},
	}
	for _, tc := range cases {
		got := corsPolicyForPath(cfg, tc.path)
		if len(got.Origins) != 1 || got.Origins[0] != tc.want {
			t.Errorf("路径 %s 归类错误: got=%+v want=%s", tc.path, got, tc.want)
		}
	}
}

func TestCORSWildcardStaysWithinPolicy(t *testing.T) {
	env := newTestEnv(t)
	mainOrigin := "https://main.example.com"
	if err := env.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCORSMainEnabled: "true",
		settings.KeyCORSMainOrigins: mainOrigin,
		settings.KeyCORSSDKEnabled:  "true",
		settings.KeyCORSSDKOrigins:  "*",
	}, 1); err != nil {
		t.Fatalf("设置跨域失败: %v", err)
	}

	res := corsDo(t, env, http.MethodGet, "/api/healthz", map[string]string{
		"Origin": "https://any.example.com",
	})
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("SDK 通配符应允许任意接口来源: %v", res.Header)
	}
	res = corsDo(t, env, http.MethodGet, "/", map[string]string{
		"Origin": "https://any.example.com",
	})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("SDK 通配符不应影响页面: %v", res.Header)
	}
	res = corsDo(t, env, http.MethodGet, "/", map[string]string{"Origin": mainOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != mainOrigin {
		t.Fatalf("主站策略不应被 SDK 通配符替代: %v", res.Header)
	}
}

// TestCORSAllowsAPIKeyCalls 锁定 API Key 的跨域链路：预检必须早于身份解析，
// 实际请求与能力不足的 403 都必须带 ACAO，浏览器才能读到结果或错误。
func TestCORSAllowsAPIKeyCalls(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	origin := "https://app.example.com"
	if err := env.settings.SetMany(ctx, map[settings.Key]string{
		settings.KeyAPIEnabled:     "true",
		settings.KeyCORSSDKEnabled: "true",
		settings.KeyCORSSDKOrigins: origin,
	}, 0); err != nil {
		t.Fatalf("启用 SDK 跨域与 API 失败: %v", err)
	}
	session := env.login(t)

	createKey := func(name string, scopes int64) string {
		t.Helper()
		status, payload := env.do(t, http.MethodPost, "/api/api-keys", map[string]any{
			"name": name, "workspacePath": "/", "scopes": scopes,
		}, session)
		if status != http.StatusOK {
			t.Fatalf("创建 API Key 失败: %d %v", status, payload)
		}
		data, _ := payload["data"].(map[string]any)
		token, _ := data["token"].(string)
		if token == "" {
			t.Fatalf("创建 API Key 未返回令牌: %v", payload)
		}
		return token
	}

	fullKey := createKey("跨域全功能", int64(apikey.All))
	downloadKey := createKey("跨域仅下载", int64(apikey.Download))

	// 上传接口需要 Authorization；预检没有真实凭据也必须先通过。
	res := corsDo(t, env, http.MethodOptions, "/api/upload/tasks", map[string]string{
		"Origin": origin, "Access-Control-Request-Method": "GET",
		"Access-Control-Request-Headers": "authorization",
	})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("API Key 预检应在鉴权前通过: status=%d headers=%v", res.StatusCode, res.Header)
	}
	if !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("预检未放行 Authorization: %v", res.Header)
	}

	res = corsDo(t, env, http.MethodGet, "/api/fs/list?path=/", map[string]string{
		"Origin": origin, "Authorization": "Bearer " + fullKey,
	})
	if res.StatusCode != http.StatusOK || res.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("API Key 实际请求应带 ACAO: status=%d headers=%v", res.StatusCode, res.Header)
	}
	if !strings.Contains(res.Header.Get("Access-Control-Expose-Headers"), "Content-Disposition") {
		t.Fatalf("实际响应未暴露下载头: %v", res.Header)
	}

	// 能力范围拦截发生在身份层；浏览器仍应能读到 403 与错误正文。
	res = corsDo(t, env, http.MethodGet, "/api/upload/tasks", map[string]string{
		"Origin": origin, "Authorization": "Bearer " + downloadKey,
	})
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("能力不足的响应应保留 ACAO: status=%d headers=%v", res.StatusCode, res.Header)
	}
}
