package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/config"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/secretbox"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// newBareEnv 构造一个**尚未初始化**的站点：库是空的，没有任何账号。
//
// 这正是新部署第一次启动时的状态，也是初始化流程唯一有意义的场景。
func newBareEnv(t *testing.T, extra map[settings.Key]string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.EnsureBuiltinGroups(context.Background(), db.W()); err != nil {
		t.Fatalf("写入预设组失败: %v", err)
	}

	boot := &config.Bootstrap{
		Listen:             ":0",
		DBPath:             filepath.Join(dir, "test.db"),
		DataDir:            dir,
		TempDir:            filepath.Join(dir, "tmp"),
		MasterSecret:       []byte("unit-test-master-secret"),
		MasterSecretSource: "generated",
	}
	box, err := secretbox.New(boot.MasterSecret)
	if err != nil {
		t.Fatalf("构造加密盒失败: %v", err)
	}
	settingsStore := settings.NewStore(db, box)
	if len(extra) > 0 {
		if err := settingsStore.SetMany(context.Background(), extra, 0); err != nil {
			t.Fatalf("写入测试配置失败: %v", err)
		}
	}

	authSvc := auth.NewService(db, settingsStore)
	stub := newStubBackend()
	svc, err := service.New(service.Deps{
		DB:                 db,
		Auth:               authSvc,
		Backend:            stub,
		Settings:           settingsStore,
		DataDir:            dir,
		TempDir:            filepath.Join(dir, "tmp"),
		MasterSecretSource: boot.MasterSecretSource,
	})
	if err != nil {
		t.Fatalf("构造业务服务失败: %v", err)
	}

	srv := httptest.NewServer(NewServer(boot, authSvc, svc, settingsStore))
	t.Cleanup(srv.Close)
	return &testEnv{server: srv, db: db, backend: svc, stub: stub, settings: settingsStore}
}

// nextRequest 构造一个"看起来来自远端"的请求：把 127.0.0.1 加进可信代理，
// 再用 X-Forwarded-For 声明一个公网来源。中间件会因此把客户端地址还原成
// 那个公网地址，从而触发令牌校验。
func (e *testEnv) doFromRemote(t *testing.T, method, path string, body any, remoteIP, token string) (int, map[string]any) {
	t.Helper()
	return e.doWithHeaders(t, method, path, body, map[string]string{
		"X-Forwarded-For": remoteIP,
	}, token)
}

// TestSetupStateOnFreshInstance 全新实例必须报告"未初始化"且要求令牌。
func TestSetupStateOnFreshInstance(t *testing.T) {
	env := newBareEnv(t, nil)

	status, payload := env.do(t, http.MethodGet, "/api/setup/state", nil, "")
	if status != http.StatusOK {
		t.Fatalf("读取初始化状态应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["initialized"] != false {
		t.Fatalf("全新实例应报告未初始化: %v", data)
	}
	if data["encryptionReady"] != false {
		t.Fatalf("未初始化时不应报告加密就绪: %v", data)
	}
	// 未初始化时才暴露落盘路径，便于运维确认。
	if data["dbPath"] == "" || data["dataDir"] == "" {
		t.Fatalf("未初始化时应返回落盘路径: %v", data)
	}
	if data["minPasswordLen"] == nil {
		t.Fatalf("应返回口令长度要求供引导页校验: %v", data)
	}
}

// TestSetupCreatesAdminAndKey 初始化应创建管理员并生成主密钥。
func TestSetupCreatesAdminAndKey(t *testing.T) {
	env := newBareEnv(t, nil)

	status, payload := env.doSetupInit(t, map[string]any{
		"account":  "root",
		"password": "a-strong-initial-password",
		"siteName": "我的文件站",
	})
	if status != http.StatusOK {
		t.Fatalf("初始化应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["adminAccount"] != "root" {
		t.Fatalf("应返回管理员账号: %v", data)
	}
	if data["siteName"] != "我的文件站" {
		t.Fatalf("站点名应已应用: %v", data)
	}
	if data["encryptionGenerated"] != true {
		t.Fatalf("未提供密钥时应由服务端生成: %v", data)
	}
	// 生成的密钥必须回传一次：它是 master.key 丢失后唯一的救命凭据。
	// 现在是单把主密钥，格式为 URL 安全 Base64 的 32 字节（43 字符，无填充）。
	key, _ := data["encryptionKey"].(string)
	raw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		t.Fatalf("应回传 URL 安全 Base64 的 32 字节主密钥，实得 %q", key)
	}

	// 状态应翻转为已初始化，且不再暴露路径与令牌要求。
	status, payload = env.do(t, http.MethodGet, "/api/setup/state", nil, "")
	if status != http.StatusOK {
		t.Fatalf("读取状态失败: %d", status)
	}
	data, _ = payload["data"].(map[string]any)
	if data["initialized"] != true {
		t.Fatalf("初始化后应报告已初始化: %v", data)
	}
	if data["encryptionReady"] != true {
		t.Fatalf("初始化后加密应就绪: %v", data)
	}
	if v, ok := data["dbPath"]; ok && v != "" {
		t.Fatalf("初始化后不应再暴露落盘路径: %v", data)
	}

	// 新管理员必须能登录，且具备管理权限。
	token := func() string {
		status, payload := env.do(t, http.MethodPost, "/api/auth/login", map[string]any{
			"account":  "root",
			"password": "a-strong-initial-password",
		}, "")
		if status != http.StatusOK {
			t.Fatalf("初始化出的管理员应能登录，实际 %d: %v", status, payload)
		}
		d, _ := payload["data"].(map[string]any)
		return d["token"].(string)
	}()
	status, _ = env.do(t, http.MethodGet, "/api/admin/settings", nil, token)
	if status != http.StatusOK {
		t.Fatalf("初始化出的管理员应具备管理权限，实际 %d", status)
	}
}

// TestSetupIsOneShot 初始化只能成功一次。
//
// 这是最关键的一条断言：如果初始化入口在站点投入使用后仍然可用，
// 任何人都能再创建（或顶替）一个管理员。
func TestSetupIsOneShot(t *testing.T) {
	env := newBareEnv(t, nil)
	first := map[string]any{"account": "root", "password": "a-strong-initial-password"}

	if status, payload := env.doSetupInit(t, first); status != http.StatusOK {
		t.Fatalf("首次初始化应成功，实际 %d: %v", status, payload)
	}
	status, payload := env.do(t, http.MethodPost, "/api/setup/init", map[string]any{
		"account":  "attacker",
		"password": "another-strong-password",
	}, "")
	if status != http.StatusConflict {
		t.Fatalf("重复初始化应返回 409，实际 %d: %v", status, payload)
	}
	// 确认没有创建出第二个账号。
	var count int64
	if err := env.db.R().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("统计账号失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("应只有 1 个账号，实得 %d", count)
	}
}

// TestSetupRejectsWeakInput 初始化也要走与注册相同的参数校验。
func TestSetupRejectsWeakInput(t *testing.T) {
	cases := []struct {
		name string
		req  map[string]any
	}{
		{"账号过短", map[string]any{"account": "a", "password": "a-strong-initial-password"}},
		{"口令过短", map[string]any{"account": "root", "password": "short"}},
		{"口令含中文", map[string]any{"account": "root", "password": "a-strong-pass中"}},
		{"账号含空白", map[string]any{"account": "ro ot", "password": "a-strong-initial-password"}},
		{"账号含控制字符", map[string]any{"account": "ro\tot", "password": "a-strong-initial-password"}},
	}
	for _, tc := range cases {
		env := newBareEnv(t, nil)
		status, payload := env.doSetupInit(t, tc.req)
		if status != http.StatusBadRequest {
			t.Errorf("%s: 应返回 400，实际 %d: %v", tc.name, status, payload)
		}
	}
}

// TestSetupAcceptsExplicitKey 提供主密钥时不应再生成，也不应回传。
func TestSetupAcceptsExplicitKey(t *testing.T) {
	env := newBareEnv(t, nil)

	// 主密钥是 URL 安全 Base64 编码的 32 字节（43 字符，无填充）；
	// 只在初始化写入，之后不可更换。
	const raw = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	status, payload := env.doSetupInit(t, map[string]any{
		"account":       "root",
		"password":      "a-strong-initial-password",
		"encryptionKey": raw,
	})
	if status != http.StatusOK {
		t.Fatalf("初始化应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["encryptionGenerated"] != false {
		t.Fatalf("提供了密钥时不应报告生成: %v", data)
	}
	if data["encryptionKey"] != nil && data["encryptionKey"] != "" {
		t.Fatalf("提供的密钥不应被回传: %v", data)
	}
	if data["keyId"] != "primary" {
		t.Fatalf("应返回主密钥版本号: %v", data)
	}

	// 非法的密钥必须被拒绝，且不能留下半个管理员。
	env2 := newBareEnv(t, nil)
	status, _ = env2.doSetupInit(t, map[string]any{
		"account":       "root",
		"password":      "a-strong-initial-password",
		"encryptionKey": "不是base64",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("非法主密钥应返回 400，实际 %d", status)
	}
	var count int64
	if err := env2.db.R().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("统计账号失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("校验失败时不应创建账号，实得 %d 个", count)
	}
}

// TestSetupRemoteRequiresToken 任何来源（含本机）都必须提供一次性令牌。
//
// 这是初始化入口唯一的访问控制。没有它，一个刚部署好、尚未初始化的公网实例
// 会被第一个访问者接管；回环来源也不再豁免，避免同机的其他进程抢先初始化。
func TestSetupRemoteRequiresToken(t *testing.T) {
	env := newBareEnv(t, map[settings.Key]string{
		// 信任回环作为代理，这样 X-Forwarded-For 才会被采信，
		// 我们借此模拟"请求来自公网"。
		settings.KeyTrustedProxies: "127.0.0.1/32",
	})
	const remote = "203.0.113.9"

	// 先取到令牌（模拟运维在日志里看到的值）。
	status, payload := env.doFromRemote(t, http.MethodGet, "/api/setup/state", nil, remote, "")
	if status != http.StatusOK {
		t.Fatalf("读取状态失败: %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["initialized"] != false {
		t.Fatalf("此时应仍未初始化: %v", data)
	}

	body := map[string]any{"account": "root", "password": "a-strong-initial-password"}

	// 本机无令牌 → 同样 403。
	status, _ = env.do(t, http.MethodPost, "/api/setup/init", body, "")
	if status != http.StatusForbidden {
		t.Fatalf("本机无令牌应返回 403，实际 %d", status)
	}
	// 远端无令牌 → 403。
	status, _ = env.doFromRemote(t, http.MethodPost, "/api/setup/init", body, remote, "")
	if status != http.StatusForbidden {
		t.Fatalf("远端无令牌应返回 403，实际 %d", status)
	}
	// 错令牌 → 403。
	status, _ = env.doFromRemote(t, http.MethodPost, "/api/setup/init", map[string]any{
		"account": "root", "password": "a-strong-initial-password", "token": "wrong-token",
	}, remote, "")
	if status != http.StatusForbidden {
		t.Fatalf("错误令牌应返回 403，实际 %d", status)
	}

	// 取真实令牌（服务端持有一份，测试里直接读出来模拟日志）。
	token := env.setupToken(t)
	status, payload = env.doFromRemote(t, http.MethodPost, "/api/setup/init", map[string]any{
		"account": "root", "password": "a-strong-initial-password", "token": token,
	}, remote, "")
	if status != http.StatusOK {
		t.Fatalf("带正确令牌应成功，实际 %d: %v", status, payload)
	}

	// 令牌用后即废。
	env2 := newBareEnv(t, nil)
	if got := env2.setupToken(t); got == "" {
		t.Fatal("未初始化时应能取到令牌")
	}
}

// TestSetupWithStorageConfiguresBackend 初始化时可以顺手把存储凭据填好。
func TestSetupWithStorageConfiguresBackend(t *testing.T) {
	env := newBareEnv(t, nil)

	status, payload := env.doSetupInit(t, map[string]any{
		"account":  "root",
		"password": "a-strong-initial-password",
		"storage": map[string]any{
			"clientId":     "test-client",
			"clientSecret": "test-secret",
			"rootDirId":    "0",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("初始化应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["storageConfigured"] != true {
		t.Fatalf("填写了凭据时存储应就绪: %v", data)
	}
	if warn, _ := data["storageWarning"].(string); warn != "" {
		t.Fatalf("不应有存储告警: %s", warn)
	}

	// 凭据必须落库，且敏感项被加密。
	var stored string
	if err := env.db.R().QueryRowContext(context.Background(),
		`SELECT value FROM system_config WHERE key = 'pan123.client_secret'`).Scan(&stored); err != nil {
		t.Fatalf("读取落库值失败: %v", err)
	}
	if !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("存储凭据应加密落库，实得 %q", stored)
	}
}

// TestSetupDirectLinkSwitchAppliedOnSubmit：初始化向导里勾选/取消勾选直链空间
// 只是表单项，必须在点按提交后才对后端执行一次启用/禁用；勾选还要把配置落库。
func TestSetupDirectLinkSwitchAppliedOnSubmit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		wantToggle bool
	}{
		{"勾选启用", true, true},
		{"取消勾选则禁用", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newBareEnv(t, nil)

			status, payload := env.doSetupInit(t, map[string]any{
				"account":  "root",
				"password": "a-strong-initial-password",
				"storage": map[string]any{
					"clientId":     "test-client",
					"clientSecret": "test-secret",
					"rootDirId":    "80360960",
					"directLink":   tc.enabled,
				},
			})
			if status != http.StatusOK {
				t.Fatalf("初始化应成功，实际 %d: %v", status, payload)
			}
			data, _ := payload["data"].(map[string]any)
			if warn, _ := data["storageWarning"].(string); warn != "" {
				t.Fatalf("直链开关执行不应告警: %s", warn)
			}
			// 提交时必须跑一次驱动层探针：四阶段全绿且探针对象已清理。
			probe, ok := data["storageProbe"].(map[string]any)
			if !ok {
				t.Fatalf("初始化结果应包含 storageProbe，实得 %v", data)
			}
			for _, stage := range []string{"uploaded", "downloaded", "verified", "deleted"} {
				if v, _ := probe[stage].(bool); !v {
					t.Fatalf("探针阶段 %s 应为 true，实得 %v", stage, probe[stage])
				}
			}
			if len(env.stub.objects) != 0 {
				t.Fatalf("探针清理后后端不应残留对象，实得 %d 个", len(env.stub.objects))
			}
			if got := env.stub.directLinks; len(got) != 1 || got[0] != tc.wantToggle {
				t.Fatalf("提交后应执行一次 %v，实得 %v", tc.wantToggle, got)
			}

			var n int
			if err := env.db.R().QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM system_config WHERE key = 'pan123.direct_link' AND value = 'true'`,
			).Scan(&n); err != nil {
				t.Fatalf("查询直链配置失败: %v", err)
			}
			if (n == 1) != tc.enabled {
				t.Fatalf("勾选时配置应落库为 true，取消勾选时不应留覆盖项，实得行数 %d", n)
			}
		})
	}
}

// TestSetupEnabledRoutesAfterInit 初始化完成后引导入口自动失效。
func TestSetupEnabledRoutesAfterInit(t *testing.T) {
	env := newBareEnv(t, nil)
	if status, _ := env.doSetupInit(t, map[string]any{
		"account":  "root",
		"password": "a-strong-initial-password",
	}); status != http.StatusOK {
		t.Fatalf("初始化失败")
	}
	// 状态接口仍然可用（前端需要它来决定不显示引导页），但 init 已关闭。
	status, payload := env.do(t, http.MethodGet, "/api/setup/state", nil, "")
	if status != http.StatusOK {
		t.Fatalf("状态接口应保持可用: %d", status)
	}
	data, _ := payload["data"].(map[string]any)
	if data["initialized"] != true {
		t.Fatalf("应报告已初始化: %v", data)
	}
	status, _ = env.do(t, http.MethodPost, "/api/setup/init", map[string]any{
		"account":  "another",
		"password": "a-strong-initial-password",
	}, "")
	if status != http.StatusConflict {
		t.Fatalf("初始化入口应已关闭，实际 %d", status)
	}
}

// setupToken 从服务端读出当前的一次性初始化令牌。
//
// 测试里直接读内存值等价于"运维在启动日志里看到了它"，避免为了可测性
// 把令牌暴露成接口。
func (e *testEnv) setupToken(t *testing.T) string {
	t.Helper()
	return e.backend.SetupToken(context.Background())
}

// doSetupInit 发起一个带有效令牌的初始化请求，覆盖"运维照着日志填令牌"的
// 正常路径；令牌相关的异常路径由 TestSetupRemoteRequiresToken 单独覆盖。
func (e *testEnv) doSetupInit(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	body["token"] = e.setupToken(t)
	return e.do(t, http.MethodPost, "/api/setup/init", body, "")
}

// TestSetupPersistsSystemMode 初始化选定的系统模式必须真正落库并在状态接口可见。
//
// 三种取值逐个走一遍：向导把值提交上来 → 写进配置 → setup/state 回读一致。
// 只断言"没报错"是不够的——那证明不了值被保存过，只证明接口没崩。
func TestSetupPersistsSystemMode(t *testing.T) {
	for _, mode := range []string{
		settings.SystemModeFilesOnly, settings.SystemModeBoth,
	} {
		t.Run(mode, func(t *testing.T) {
			env := newBareEnv(t, nil)
			status, payload := env.doSetupInit(t, map[string]any{
				"account":    "root",
				"password":   "a-strong-initial-password",
				"systemMode": mode,
			})
			if status != http.StatusOK {
				t.Fatalf("初始化应成功，实际 %d: %v", status, payload)
			}

			status, payload = env.do(t, http.MethodGet, "/api/setup/state", nil, "")
			if status != http.StatusOK {
				t.Fatalf("读取状态失败: %d", status)
			}
			data, _ := payload["data"].(map[string]any)
			if data["systemMode"] != mode {
				t.Fatalf("系统模式未生效：期望 %q，实际 %v", mode, data["systemMode"])
			}

			// 前端靠这个字段决定导航，因此它必须在未登录时也能拿到。
			if rt := env.settings.Runtime(context.Background()); rt.Site.SystemMode != mode {
				t.Fatalf("运行期快照与写入值不符: %q", rt.Site.SystemMode)
			}
		})
	}
}

// TestSetupDefaultsSystemModeWhenOmitted 旧客户端不传该字段时保持默认。
func TestSetupDefaultsSystemModeWhenOmitted(t *testing.T) {
	env := newBareEnv(t, nil)
	if status, payload := env.doSetupInit(t, map[string]any{
		"account":  "root",
		"password": "a-strong-initial-password",
	}); status != http.StatusOK {
		t.Fatalf("初始化应成功，实际 %d: %v", status, payload)
	}
	status, payload := env.do(t, http.MethodGet, "/api/setup/state", nil, "")
	if status != http.StatusOK {
		t.Fatalf("读取状态失败: %d", status)
	}
	data, _ := payload["data"].(map[string]any)
	if data["systemMode"] != settings.SystemModeBoth {
		t.Fatalf("缺省应为 both，实际 %v", data["systemMode"])
	}
}

// TestSetupRejectsUnknownSystemMode 非法模式在初始化就被拒。
func TestSetupRejectsUnknownSystemMode(t *testing.T) {
	env := newBareEnv(t, nil)
	status, payload := env.doSetupInit(t, map[string]any{
		"account":    "root",
		"password":   "a-strong-initial-password",
		"systemMode": "everything",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("非法系统模式应返回 400，实际 %d: %v", status, payload)
	}
	// 失败的初始化不能留下痕迹：站点必须仍是未初始化状态。
	status, payload = env.do(t, http.MethodGet, "/api/setup/state", nil, "")
	if status != http.StatusOK {
		t.Fatalf("读取状态失败: %d", status)
	}
	data, _ := payload["data"].(map[string]any)
	if data["initialized"] == true {
		t.Fatalf("初始化失败后不应留下已初始化的站点: %v", data)
	}
}

// TestSetupValidateStep 引导页分步校验：每一步点"下一步"都会打到这里，
// 规则与最终提交完全一致，但只做无副作用的检查。
func TestSetupValidateStep(t *testing.T) {
	env := newBareEnv(t, nil)
	token := env.backend.SetupToken(context.Background())
	if token == "" {
		t.Fatal("未初始化实例应发出一次性令牌")
	}

	post := func(step string, body map[string]any) (int, map[string]any) {
		body["step"] = step
		return env.do(t, http.MethodPost, "/api/setup/validate", body, "")
	}

	// 令牌：正确则通过，错误则当场拒绝（不必等到最后一步提交才发现）。
	if status, payload := post("token", map[string]any{"token": token}); status != http.StatusOK {
		t.Fatalf("正确令牌应通过，实际 %d: %v", status, payload)
	}
	if status, _ := post("token", map[string]any{"token": "wrong-token"}); status == http.StatusOK {
		t.Fatal("错误令牌不应通过")
	}

	// 管理员账号：长度与口令字符集按后端规则判定。
	if status, _ := post("admin", map[string]any{
		"account": "root", "password": "Passw0rd!23",
	}); status != http.StatusOK {
		t.Fatalf("合规账号口令应通过，实际 %d", status)
	}
	if status, _ := post("admin", map[string]any{
		"account": "root", "password": "short",
	}); status == http.StatusOK {
		t.Fatal("过短口令不应通过")
	}
	if status, _ := post("admin", map[string]any{
		"account": "a b", "password": "Passw0rd!23",
	}); status == http.StatusOK {
		t.Fatal("含空格的账号不应通过")
	}

	// 主密钥：留空表示由服务端生成（通过）；非法 base64 应被拒。
	if status, _ := post("encryption", map[string]any{"encryptionKey": ""}); status != http.StatusOK {
		t.Fatal("留空密钥（服务端生成）应通过")
	}
	good := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if status, _ := post("encryption", map[string]any{"encryptionKey": good}); status != http.StatusOK {
		t.Fatal("合法主密钥应通过")
	}
	if status, _ := post("encryption", map[string]any{"encryptionKey": "not-a-key"}); status == http.StatusOK {
		t.Fatal("非法主密钥不应通过")
	}

	// 存储凭据：完整性 + 直链与根目录互斥。
	if status, _ := post("storage", map[string]any{"storage": map[string]any{
		"clientId": "id", "clientSecret": "secret", "rootDirId": "123",
	}}); status != http.StatusOK {
		t.Fatal("完整凭据应通过")
	}
	if status, _ := post("storage", map[string]any{"storage": map[string]any{
		"clientId": "id", "clientSecret": "", "rootDirId": "123",
	}}); status == http.StatusOK {
		t.Fatal("缺少 Client Secret 不应通过")
	}
	if status, _ := post("storage", map[string]any{"storage": map[string]any{
		"clientId": "id", "clientSecret": "s", "rootDirId": "0", "directLink": true,
	}}); status == http.StatusOK {
		t.Fatal("根目录启用直链空间不应通过")
	}

	// 未知步骤必须被拒，不能被当成"全部通过"。
	if status, _ := post("nope", map[string]any{}); status == http.StatusOK {
		t.Fatal("未知校验步骤不应通过")
	}
}

// TestSetupValidateAfterInitClosed 初始化完成后分步校验入口立即失效。
func TestSetupValidateAfterInitClosed(t *testing.T) {
	env := newBareEnv(t, nil)
	ctx := context.Background()
	token := env.backend.SetupToken(ctx)
	if _, err := env.backend.CompleteSetup(ctx, service.SetupRequest{
		Account: "root", Password: "Passw0rd!23", Token: token,
	}); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	status, _ := env.do(t, http.MethodPost, "/api/setup/validate",
		map[string]any{"step": "token", "token": token}, "")
	if status != http.StatusConflict {
		t.Fatalf("初始化后校验入口应返回 409，实际 %d", status)
	}
}
