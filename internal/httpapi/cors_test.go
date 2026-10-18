package httpapi

import (
	"context"
	"net/http"
	"testing"

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

func TestCORSScopesAreIndependent(t *testing.T) {
	env := newTestEnv(t)
	mainOrigin := "https://main.example.com"
	shareOrigin := "https://share.example.com"
	directOrigin := "https://links.example.com"

	res := corsDo(t, env, http.MethodGet, "/api/healthz", map[string]string{"Origin": mainOrigin})
	if res.StatusCode != http.StatusOK || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("默认应关闭所有跨域策略: status=%d headers=%v", res.StatusCode, res.Header)
	}

	if err := env.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCORSMainEnabled: "true",
		settings.KeyCORSMainOrigins: mainOrigin,
	}, 1); err != nil {
		t.Fatalf("设置主站跨域失败: %v", err)
	}
	res = corsDo(t, env, http.MethodGet, "/api/healthz", map[string]string{"Origin": mainOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != mainOrigin {
		t.Fatalf("主站来源未获准: %v", res.Header)
	}
	res = corsDo(t, env, http.MethodOptions, "/api/admin/settings", map[string]string{
		"Origin": mainOrigin, "Access-Control-Request-Method": "GET",
		"Access-Control-Request-Headers": "authorization,content-type",
	})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != mainOrigin {
		t.Fatalf("主站预检应在身份验证前通过: status=%d headers=%v", res.StatusCode, res.Header)
	}
	if res.Header.Get("Access-Control-Allow-Headers") != corsAllowedHeaders || res.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("预检应允许指定请求头但不允许跨域 Cookie: %v", res.Header)
	}
	res = corsDo(t, env, http.MethodGet, "/api/s/share-1/resolve", map[string]string{"Origin": mainOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("主站策略不应覆盖分享路由: %v", res.Header)
	}

	if err := env.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCORSShareEnabled: "true",
		settings.KeyCORSShareOrigins: shareOrigin,
	}, 1); err != nil {
		t.Fatalf("设置分享跨域失败: %v", err)
	}
	res = corsDo(t, env, http.MethodOptions, "/api/s/share-1/resolve", map[string]string{
		"Origin": shareOrigin, "Access-Control-Request-Method": "POST",
		"Access-Control-Request-Headers": "content-type",
	})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != shareOrigin {
		t.Fatalf("分享预检应通过独立策略: status=%d headers=%v", res.StatusCode, res.Header)
	}
	res = corsDo(t, env, http.MethodGet, "/api/healthz", map[string]string{"Origin": shareOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("分享来源不应自动获准访问主站: %v", res.Header)
	}

	if err := env.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCORSDirectEnabled: "true",
		settings.KeyCORSDirectOrigins: directOrigin,
	}, 1); err != nil {
		t.Fatalf("设置直链跨域失败: %v", err)
	}
	res = corsDo(t, env, http.MethodOptions, "/api/direct-link", map[string]string{
		"Origin": directOrigin, "Access-Control-Request-Method": "POST",
		"Access-Control-Request-Headers": "authorization,content-type",
	})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != directOrigin {
		t.Fatalf("直链预检应通过独立策略: status=%d headers=%v", res.StatusCode, res.Header)
	}
	res = corsDo(t, env, http.MethodGet, "/d/token", map[string]string{"Origin": mainOrigin})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("主站策略不应覆盖直链交付路由: %v", res.Header)
	}
}

func TestCORSWildcardAppliesOnlyToItsRouteGroup(t *testing.T) {
	env := newTestEnv(t)
	if err := env.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyCORSDirectEnabled: "true",
		settings.KeyCORSDirectOrigins: "*",
	}, 1); err != nil {
		t.Fatalf("启用直链通配跨域失败: %v", err)
	}
	res := corsDo(t, env, http.MethodGet, "/d/token", map[string]string{"Origin": "https://any.example.com"})
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("直链通配符应允许任意来源: %v", res.Header)
	}
	res = corsDo(t, env, http.MethodGet, "/api/healthz", map[string]string{"Origin": "https://any.example.com"})
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("直链通配符不应影响主站: %v", res.Header)
	}
}
