package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
)

// 用户侧与管���侧的邮件接口清单。
//
// 分开列是有意的：两者的门禁判定与返回码都不同（见 mailguard.go），
// 放进一个循环断言会把它们混成同一种行为。
var userMailRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/mail/messages"},
	{http.MethodGet, "/api/mail/addresses"},
	{http.MethodPost, "/api/mail/addresses"},
	{http.MethodGet, "/api/mail/domains"},
	{http.MethodPost, "/api/mail/parts/1/delivery"},
}

var adminMailRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/admin/mail/messages"},
}

// TestUserMailRoutesVanishWithoutMail 邮件不可用时，用户侧接口应当**不存在**。
//
// 断言对象是守卫本身而不是某一条具体接口：新增邮件接口时忘了包 userMailOnly，
// 用例会因为"某个端点没被拒绝"而失败。逐条抄接口清单恰恰漏的就是刚新增那条。
func TestUserMailRoutesVanishWithoutMail(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// 两种"不可用"都要覆盖：模式关掉，以及模式开着但收件没开。
	// 后者正是新增的那条——只判模式会漏。
	for _, c := range []struct {
		name  string
		apply func(t *testing.T)
	}{
		{"模式为仅文件", func(t *testing.T) { setMode(t, env, settings.SystemModeFilesOnly) }},
		{"模式开着但收件关闭", func(t *testing.T) { setReceive(t, env, false) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.apply(t)
			for _, r := range userMailRoutes {
				status, payload := env.do(t, r.method, r.path, nil, token)
				if status != http.StatusNotFound {
					t.Fatalf("%s %s: 邮件不可用时用户侧应 404，实际 %d (%v)", r.method, r.path, status, payload)
				}
				// 措辞必须与"路由不存在"完全一致：调用方不该能靠它区分
				// "被禁用"与"没这个功能"，那等于把内部模式泄露出去。
				errObj, _ := payload["error"].(map[string]any)
				if errObj == nil || errObj["message"] != "接口不存在" {
					t.Fatalf("%s %s: 404 应与路由不存在同措辞，实际 %v", r.method, r.path, payload)
				}
			}
		})
	}

	// 两项都满足时才可达。
	setMode(t, env, settings.SystemModeBoth)
	setReceive(t, env, true)
	if status, _ := env.do(t, http.MethodGet, "/api/mail/messages", nil, token); status != http.StatusOK {
		t.Fatalf("模式与收件都开启时用户侧应 200，实际 %d", status)
	}
}

// TestAdminMailRoutesStayReachableWhenMailOff 管理端不受收件开关影响。
//
// 管理员恰恰要在收件关闭、甚至模式是「仅文件」时进去看历史邮件排查问题；
// 这里关掉的是"用户看得见吗"，不是"管理员能不能查"。
func TestAdminMailRoutesStayReachableWhenMailOff(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	setReceive(t, env, false)

	for _, r := range adminMailRoutes {
		status, _ := env.do(t, r.method, r.path, nil, token)
		if status != http.StatusOK {
			t.Fatalf("%s %s: 收件关闭时管理端应仍可用，实际 %d", r.method, r.path, status)
		}
	}

	// 模式关掉时管理端是 403（可恢复的拒绝），不是 404：
	// 这一页确实存在，管理员还需要进去把模式切回来。
	setMode(t, env, settings.SystemModeFilesOnly)
	for _, r := range adminMailRoutes {
		status, _ := env.do(t, r.method, r.path, nil, token)
		if status != http.StatusForbidden {
			t.Fatalf("%s %s: files_only 下管理端应 403，实际 %d", r.method, r.path, status)
		}
	}
}

// TestSystemModeDisablesMailRoutes 覆盖「仅文件」模式下邮件接口全部被拒绝。
func TestSystemModeDisablesMailRoutes(t *testing.T) {
	env := newTestEnv(t)
	setMode(t, env, settings.SystemModeFilesOnly)
	token := env.login(t)

	all := append(append([]struct {
		method string
		path   string
	}{}, userMailRoutes...), adminMailRoutes...)
	for _, r := range all {
		status, payload := env.do(t, r.method, r.path, nil, token)
		want := http.StatusNotFound
		if r.path[:len("/api/admin")] == "/api/admin" {
			want = http.StatusForbidden
		}
		if status != want {
			t.Fatalf("%s %s: files_only 下应返回 %d，实际 %d (%v)", r.method, r.path, want, status, payload)
		}
		errObj, _ := payload["error"].(map[string]any)
		if errObj == nil || errObj["message"] == "" {
			t.Fatalf("%s %s: 应返回结构化错误体，实际 %v", r.method, r.path, payload)
		}
	}

	// 文件侧不受牵连。
	if status, _ := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, token); status != http.StatusOK {
		t.Fatalf("files_only 不应影响文件侧，实际 %d", status)
	}
	// 管理员仍能通过设置页把模式切回去——否则 files_only 变成单向门。
	if status, _ := env.do(t, http.MethodGet, "/api/admin/settings", nil, token); status != http.StatusOK {
		t.Fatalf("files_only 下设置页必须仍可访问，否则无法切回，实际 %d", status)
	}
}

// TestSystemModeRoundTrip 仅文件 → 恢复 both，确认守卫热生效，且禁用不删除数据。
func TestSystemModeRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	setMode(t, env, settings.SystemModeFilesOnly)
	if status, payload := env.do(t, http.MethodGet, "/api/mail/messages", nil, token); status != http.StatusNotFound {
		t.Fatalf("切到 files_only 用户侧应 404，实际 %d: %v", status, payload)
	}
	// 管理端是 403：这一页确实存在，管理员要靠它把模式切回来。
	if status, _ := env.do(t, http.MethodGet, "/api/admin/mail/messages", nil, token); status != http.StatusForbidden {
		t.Fatalf("files_only 下管理端应 403，实际 %d", status)
	}

	// 切回 both 且收件已开：守卫立刻放开，不需要重启进程。
	setMode(t, env, settings.SystemModeBoth)
	if status, payload := env.do(t, http.MethodGet, "/api/mail/messages", nil, token); status != http.StatusOK {
		t.Fatalf("切回 both 后不应再被守卫拒绝，实际 %d: %v", status, payload)
	}
}

// TestSystemModeMailEnabledByDefault 守卫不能误伤默认模式。
func TestSystemModeMailEnabledByDefault(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	status, payload := env.do(t, http.MethodGet, "/api/mail/messages", nil, token)
	// 默认 both 下应穿过守卫，落到真正的业务处理（域名未配置时是业务错误，
	// 而不是守卫的 403）。
	if status == http.StatusForbidden {
		t.Fatalf("默认 both 模式不应拒绝邮件接口: %v", payload)
	}
}

// TestSystemModeDirtyValueFallsBack 脏值不能凭空关掉邮件。
func TestSystemModeDirtyValueFallsBack(t *testing.T) {
	for _, raw := range []string{
		"garbage",
		// 早期版本把"仅邮件"作为合法取值写入过库。那些库升级到当前版本后
		// 带着一个已不存在的枚举值——必须收敛到"两侧齐全"，而不是掉进
		// default 分支之外、或让邮件凭空消失。
		"mail_only",
	} {
		t.Run(raw, func(t *testing.T) {
			env := newTestEnv(t)
			// 枚举校验挡住写入路径，因此这里直接改库模拟导入/手工改写留下的脏值。
			forceRawMode(t, env, raw)

			token := env.login(t)
			status, payload := env.do(t, http.MethodGet, "/api/mail/messages", nil, token)
			if status == http.StatusForbidden {
				t.Fatalf("未知取值 %q 应回落到两侧齐全而非关闭邮件: %v", raw, payload)
			}
			// 运行期快照也必须是收敛后的值，不能把原始脏串传下去。
			if got := env.settings.Runtime(context.Background()).Site.SystemMode; got != settings.SystemModeBoth {
				t.Fatalf("运行期未收敛：期望 %q，实际 %q", settings.SystemModeBoth, got)
			}
		})
	}
}

// TestSystemModeSettingValidation 枚举校验：非法值拒写，三种合法值都放行。
func TestSystemModeSettingValidation(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	status, _ := env.do(t, http.MethodPut, "/api/admin/settings", map[string]any{
		"values": map[string]string{string(settings.KeySystemMode): "files_only_extra"},
	}, token)
	if status != http.StatusBadRequest {
		t.Fatalf("非法系统模式应被拒绝，实际 %d", status)
	}
	for _, mode := range []string{
		settings.SystemModeBoth, settings.SystemModeFilesOnly,
	} {
		status, payload := env.do(t, http.MethodPut, "/api/admin/settings", map[string]any{
			"values": map[string]string{string(settings.KeySystemMode): mode},
		}, token)
		if status != http.StatusOK {
			t.Fatalf("系统模式 %s 应可写入，实际 %d: %v", mode, status, payload)
		}
	}
}

// ---------------------------------------------------------------- 辅助

func setMode(t *testing.T, e *testEnv, mode string) {
	t.Helper()
	// 走真实的配置写入路径而不是直连改库：settings 有 5s 快照缓存，只有
	// Store 的写入才会主动失效它。测试若绕过 Store，读到的仍是旧快照，
	// 测的就不是生产会经历的行为。
	if err := e.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeySystemMode: mode,
	}, 0); err != nil {
		t.Fatalf("写入系统模式 %s 失败: %v", mode, err)
	}
}

// forceRawMode 直连改库写入一个描述符不可能接受的脏值（模拟导入文件、
// 手工改库或旧版本遗留）。随后调用一次无关写入把快照缓存打掉——
// settings 对未知值没有写入路径校验，脏值只能从这里进来。
func forceRawMode(t *testing.T, e *testEnv, raw string) {
	t.Helper()
	_, err := e.db.W().ExecContext(context.Background(), `
		INSERT INTO system_config (key, value, value_type, updated_at, updated_by)
		VALUES (?, ?, 'string', 0, 0)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		string(settings.KeySystemMode), raw)
	if err != nil {
		t.Fatalf("直写脏值失败: %v", err)
	}
	// 打掉快照：写一个与目标无关的合法配置项。
	if err := e.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyGuestPreview: "true",
	}, 0); err != nil {
		t.Fatalf("刷新快照失败: %v", err)
	}
}

// setReceive 开关「启用收件」。
//
// 和 setMode 一样走真实的配置写入路径：settings 有 5s 快照缓存，只有 Store
// 的写入才会失效它，直连改库测到的不是生产会经历的行为。
func setReceive(t *testing.T, e *testEnv, on bool) {
	t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	if err := e.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyMailReceiveEnabled: v,
	}, 0); err != nil {
		t.Fatalf("写入启用收件 %s 失败: %v", v, err)
	}
}
