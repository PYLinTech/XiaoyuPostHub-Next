package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 错误响应的"message + detail"分工，是前端唯一的排障信息源。
//
// 早期 detail 直接用 err.Error()，形状是 "service: 目标状态冲突: 具体原因"，
// 而前端 describeError 只显示 message —— 于是"域名下还有 2 个邮箱地址"
// 被压成一句"目标已存在"。这两组用例把拆开后的形状钉住。

func TestDetailTextStripsKnownSentinel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "冲突",
			err: fmt.Errorf("%w: 域名 example.com 下仍有 2 个邮箱地址，无法解除绑定；请先处理这些地址",
				service.ErrConflict),
			want: "域名 example.com 下仍有 2 个邮箱地址，无法解除绑定；请先处理这些地址",
		},
		{
			name: "参数",
			err:  fmt.Errorf("%w: 收件域名格式不正确", service.ErrBadRequest),
			want: "收件域名格式不正确",
		},
		{
			name: "未包装的普通错误原样返回",
			err:  fmt.Errorf("底层数据库炸了"),
			want: "底层数据库炸了",
		},
		{
			// 具体原因本身带冒号时不能被切坏：只在前缀确实匹配哨兵时才剥。
			name: "原因里带冒号",
			err: fmt.Errorf("%w: 用户组 ops: 还绑着收件域名 x",
				service.ErrConflict),
			want: "用户组 ops: 还绑着收件域名 x",
		},
		{
			// 前缀只是恰好长得像，但不能误伤未包装的错误。
			name: "未包装但以哨兵字样开头",
			err:  fmt.Errorf("service: 目标状态冲突: 手工构造的字符串"),
			want: "手工构造的字符串",
		},
		{
			// 错误串就是哨兵本身、没有包任何原因：detail 必须为空，
			// 否则用户会看到"请先登录：auth: 需要登录"这种废话拼接。
			name: "裸哨兵",
			err:  auth.ErrNotAuthenticated,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detailText(c.err); got != c.want {
				t.Fatalf("detailText=%q，期望 %q", got, c.want)
			}
		})
	}
}

// TestErrorStatusConflictIsNotAboutDuplication 冲突标题必须对所有冲突都成立。
func TestErrorStatusConflictIsNotAboutDuplication(t *testing.T) {
	// 域名被地址占住：这不是"目标已存在"，而是"当前状态不允许"。
	err := fmt.Errorf("%w: 域名 example.com 下仍有 2 个邮箱地址", service.ErrConflict)
	status, message, detail := errorStatus(err)
	if status != http.StatusConflict {
		t.Fatalf("应为 409，得 %d", status)
	}
	if message != "当前状态不允许该操作" {
		t.Fatalf("冲突标题应中性表述，得 %q", message)
	}
	if detail != "域名 example.com 下仍有 2 个邮箱地址" {
		t.Fatalf("detail 应是具体原因，得 %q", detail)
	}
	// 前端会把两者拼起来给人看，拼完必须是一句完整可读的话。
	if message == "目标已存在" {
		t.Fatal("标题仍会把管理员引向「重复」这个错误方向")
	}
}

// TestFailResponseCarriesDetailToClient 端到端确认 detail 真到了响应体里。
func TestFailResponseCarriesDetailToClient(t *testing.T) {
	rec := httptest.NewRecorder()
	fail(rec, fmt.Errorf("%w: 用户组 ops 还绑着收件域名 a.example", service.ErrConflict))

	if rec.Code != http.StatusConflict {
		t.Fatalf("应 409，得 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"当前状态不允许该操作", "用户组 ops 还绑着收件域名 a.example"} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应体缺少 %q：%s", want, body)
		}
	}
	// 哨兵属于内部噪音，不能漏给用户。
	if strings.Contains(body, store.ErrConflict.Error()) {
		t.Fatalf("响应体仍带着哨兵前缀：%s", body)
	}
}

// TestFailHidesDetailOnServerErrors 5xx 不得携带 detail。
//
// 4xx 的 detail 是业务刻意写给人看的；5xx 的 detail 是未滤过的内部细节。
// 前端开始显示 detail 之后，这条就从"多余的字段"变成了信息泄漏面。
func TestFailHidesDetailOnServerErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"未分类", fmt.Errorf("扫描用户组失败: sql: database is locked"), http.StatusInternalServerError},
		{"依赖不可用", fmt.Errorf("%w: 123 云端返回 503 bucket=private-xph", service.ErrUnavailable), http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			fail(rec, c.err)
			if rec.Code != c.want {
				t.Fatalf("状态码应 %d，得 %d", c.want, rec.Code)
			}
			body := rec.Body.String()
			for _, leak := range []string{"sql:", "bucket=", "database is locked"} {
				if strings.Contains(body, leak) {
					t.Fatalf("5xx 响应泄漏了内部细节 %q：%s", leak, body)
				}
			}
			if !strings.Contains(body, "服务器内部错误") && !strings.Contains(body, "服务暂时不可用") {
				t.Fatalf("5xx 仍应给出泛化提示：%s", body)
			}
		})
	}
}

// TestFailKeepsDetailOnClientErrors 反向锁定：4xx 必须仍然送达 detail。
func TestFailKeepsDetailOnClientErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	fail(rec, fmt.Errorf("%w: 用户组 ops 还绑着收件域名 a.example", service.ErrConflict))
	body := rec.Body.String()
	if !strings.Contains(body, "用户组 ops 还绑着收件域名 a.example") {
		t.Fatalf("4xx 必须保留 detail：%s", body)
	}
}

// TestEveryServiceSentinelIsStripped 锁住哨兵表的完整性。
//
// errorSentinels 是手工维护的列表。新加哨兵时忘了登记，后果既不报错也不崩，
// 只是用户看到"service: 无权访问: 具体原因"这种双前缀串——正是最容易被放过
// 的一类缺陷（已真实漏过 ErrForbidden 与 ErrPickupPoolExhausted 两项）。
// 这条用例把 service 包的哨兵逐个过一遍，漏登记当场失败。
func TestEveryServiceSentinelIsStripped(t *testing.T) {
	sentinels := map[string]error{
		"ErrBadRequest":          service.ErrBadRequest,
		"ErrForbidden":           service.ErrForbidden,
		"ErrNotFound":            service.ErrNotFound,
		"ErrConflict":            service.ErrConflict,
		"ErrTooLarge":            service.ErrTooLarge,
		"ErrQuotaExceeded":       service.ErrQuotaExceeded,
		"ErrNotSupported":        service.ErrNotSupported,
		"ErrUnavailable":         service.ErrUnavailable,
		"ErrStorageNotReady":     service.ErrStorageNotReady,
		"ErrBusy":                service.ErrBusy,
		"ErrPickupPoolExhausted": service.ErrPickupPoolExhausted,
		"ErrAlreadyInitialized":  service.ErrAlreadyInitialized,
		"ErrSetupTokenRequired":  service.ErrSetupTokenRequired,
	}
	for name, sentinel := range sentinels {
		t.Run(name, func(t *testing.T) {
			if got := detailText(fmt.Errorf("%w: 具体原因", sentinel)); got != "具体原因" {
				t.Fatalf("%s 未登记进 errorSentinels，detailText=%q", name, got)
			}
		})
	}

	// 派生哨兵不单列是**正确的**：它由父哨兵包出来，剥父哨兵时顺带剥净。
	// 单独钉一句，免得有人看到它不在表里就又加一条重复项。
	t.Run("派生的 ErrRangeNotSatisfiable", func(t *testing.T) {
		got := detailText(fmt.Errorf("%w: 具体原因", service.ErrRangeNotSatisfiable))
		if strings.Contains(got, "service:") {
			t.Fatalf("哨兵链未被剥净：%q", got)
		}
	})
}

// TestRangeNotSatisfiableGoesThroughErrorStatus 416 必须走统一映射。
//
// 它原先在 handler 里特判，detail 直接用 err.Error()——而这个错误是
// fmt.Errorf 包出来的，err.Error() 会带上完整哨兵链，与其它 4xx 不一致。
func TestRangeNotSatisfiableGoesThroughErrorStatus(t *testing.T) {
	err := fmt.Errorf("%w: %v", service.ErrRangeNotSatisfiable, "起止颠倒")
	status, message, detail := errorStatus(err)
	if status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("应为 416，得 %d", status)
	}
	if message != "请求区间不正确" {
		t.Fatalf("标题应稳定，得 %q", message)
	}
	if strings.HasPrefix(detail, "service:") {
		t.Fatalf("detail 仍带哨兵链：%q", detail)
	}
	if !strings.Contains(detail, "起止颠倒") {
		t.Fatalf("detail 应保留具体原因，得 %q", detail)
	}
}
