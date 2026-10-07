package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailin"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// seedHTTPMail 在 httpapi 测试库里给管理员落一封纯文本信，返回内部邮件 ID。
func seedHTTPMail(t *testing.T, env *testEnv, rfcID string) string {
	t.Helper()
	ctx := context.Background()
	var uid int64
	if err := env.db.R().QueryRowContext(ctx,
		`SELECT id FROM users WHERE account = 'admin'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureMailDomain(ctx, env.db.W(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindGroupMailDomain(ctx, env.db.W(), perm.GroupAdmin, "example.com", true); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMailAddress(ctx, env.db.W(), store.MailAddress{
		Address: "admin@example.com", LocalPart: "admin", Domain: "example.com",
		UserID: uid, Status: store.MailAddressActive,
	}); err != nil {
		t.Fatal(err)
	}
	raw := "From: Sender <sender@ext.example>\r\n" +
		"To: admin@example.com\r\n" +
		"Subject: api test\r\n" +
		"Message-ID: <" + rfcID + ">\r\n" +
		"Date: Thu, 25 Sep 2025 12:00:00 +0000\r\n" +
		"Content-Type: text/plain\r\n\r\nhello http mail\r\n"
	mlEnv := mailin.Envelope{
		RemoteIP: "9.9.9.9", Recipients: []string{"admin@example.com"}, SPFResult: "pass",
	}
	if err := env.backend.Receive(ctx, mlEnv, strings.NewReader(raw)); err != nil {
		t.Fatalf("落信失败: %v", err)
	}
	m, err := store.GetMailMessageByRFCID(ctx, env.db.R(), rfcID)
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func TestMailAPIContract(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// 未登录 → 401。
	if status, _ := env.do(t, http.MethodGet, "/api/mail/messages", nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，得到 %d", status)
	}

	id := seedHTTPMail(t, env, "http-1@ext.example")

	// 列表。
	status, payload := env.do(t, http.MethodGet, "/api/mail/messages?view=inbox", nil, token)
	if status != http.StatusOK {
		t.Fatalf("列表应 200，得到 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["total"].(float64) != 1 {
		t.Fatalf("total 应 1: %v", data)
	}
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items 应 1 条: %v", data)
	}

	// 详情（自动已读）。
	status, payload = env.do(t, http.MethodGet, "/api/mail/messages/"+id, nil, token)
	if status != http.StatusOK {
		t.Fatalf("详情应 200，得到 %d: %v", status, payload)
	}
	detail, _ := payload["data"].(map[string]any)
	msg, _ := detail["message"].(map[string]any)
	if msg["subject"] != "api test" {
		t.Fatalf("主题错误: %v", msg)
	}
	parts, _ := detail["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("纯文本信应有 1 个正文部件: %v", parts)
	}
	bodyPart, _ := parts[0].(map[string]any)
	bodyPartID := int64(bodyPart["id"].(float64))

	// 部件交付出票（正文强制 preview）。
	status, payload = env.do(t, http.MethodPost,
		"/api/mail/parts/"+itoa(bodyPartID)+"/delivery", map[string]any{}, token)
	if status != http.StatusOK {
		t.Fatalf("部件交付应 200，得到 %d: %v", status, payload)
	}
	plan, _ := payload["data"].(map[string]any)
	if plan["purpose"] != "preview" || plan["ticketId"] == "" {
		t.Fatalf("交付计划错误: %v", plan)
	}

	// 星标 → starred 视图可见。
	if status, _ = env.do(t, http.MethodPost, "/api/mail/messages/"+id+"/star",
		map[string]any{"starred": true}, token); status != http.StatusOK {
		t.Fatalf("星标应 200，得到 %d", status)
	}
	if status, payload = env.do(t, http.MethodGet, "/api/mail/messages?view=starred", nil, token); status != http.StatusOK ||
		payload["data"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("星标视图应有 1 封: %d %v", status, payload)
	}

	// 回收 → trash 可见、inbox 不可见 → 恢复。
	if status, _ = env.do(t, http.MethodPost, "/api/mail/messages/"+id+"/archive", nil, token); status != http.StatusOK {
		t.Fatalf("回收应 200，得到 %d", status)
	}
	if status, payload = env.do(t, http.MethodGet, "/api/mail/messages?view=inbox", nil, token); status != http.StatusOK ||
		payload["data"].(map[string]any)["total"].(float64) != 0 {
		t.Fatalf("收件箱应为空: %d %v", status, payload)
	}
	if status, payload = env.do(t, http.MethodGet, "/api/mail/messages?view=trash", nil, token); status != http.StatusOK ||
		payload["data"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("归档应有 1 封: %d %v", status, payload)
	}
	if status, _ = env.do(t, http.MethodPost, "/api/mail/messages/"+id+"/restore", nil, token); status != http.StatusOK {
		t.Fatalf("恢复应 200，得到 %d", status)
	}

	// 地址列表含 admin@example.com。
	status, payload = env.do(t, http.MethodGet, "/api/mail/addresses", nil, token)
	if status != http.StatusOK {
		t.Fatalf("地址列表应 200，得到 %d: %v", status, payload)
	}
	addrItems, _ := payload["data"].(map[string]any)["items"].([]any)
	if len(addrItems) != 1 || addrItems[0].(map[string]any)["address"] != "admin@example.com" {
		t.Fatalf("地址列表错误: %v", addrItems)
	}
}

func TestMailAPIUnknownView(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	if status, _ := env.do(t, http.MethodGet, "/api/mail/messages?view=bogus", nil, token); status != http.StatusBadRequest {
		t.Fatalf("未知视图应 400，得到 %d", status)
	}
}

// itoa 是测试内的小工具，避免在断言里反复 fmt.Sprintf。
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
