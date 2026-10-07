package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/config"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/secretbox"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// stubBackend 是一个内存存储后端。
//
// 用它来验证 HTTP 层与业务层的接缝：路由、身份还原、错误映射、响应信封。
// 真实后端（123 云盘）需要外部凭据，不适合放进单测。
type stubBackend struct {
	objects map[string][]byte
	nextID  int
	// directLinks 按调用顺序记录每次直链空间开关的目标状态，供初始化/设置
	// 同步测试断言"勾选即指令"。
	directLinks []bool
}

func newStubBackend() *stubBackend {
	return &stubBackend{objects: map[string][]byte{}, nextID: 1000}
}

func (b *stubBackend) Kind() string { return "stub" }

func (b *stubBackend) Put(_ context.Context, req backend.PutRequest) (backend.PutResult, error) {
	b.nextID++
	ref := fmt.Sprintf("%d", b.nextID)
	data := make([]byte, req.SizeWire)
	if _, err := req.Source.ReadAt(data, 0); err != nil && err != io.EOF {
		return backend.PutResult{}, err
	}
	b.objects[ref] = data
	return backend.PutResult{ObjectRef: ref}, nil
}

func (b *stubBackend) Open(_ context.Context, ref string) (io.ReadSeekCloser, error) {
	data, ok := b.objects[ref]
	if !ok {
		return nil, backend.ErrNotFound
	}
	return &memReadSeekCloser{Reader: bytes.NewReader(data)}, nil
}

func (b *stubBackend) Delete(_ context.Context, ref string) error {
	delete(b.objects, ref)
	return nil
}

func (b *stubBackend) Stat(_ context.Context, ref string) (int64, error) {
	data, ok := b.objects[ref]
	if !ok {
		return 0, backend.ErrNotFound
	}
	return int64(len(data)), nil
}

func (b *stubBackend) Presign(_ context.Context, ref string, opt backend.PresignOptions) (string, error) {
	url := fmt.Sprintf("https://cdn.example.com/%s.xph", ref)
	if opt.TicketID != "" {
		url += "?" + service.HeaderPanTicket + "=" + opt.TicketID
	}
	return url, nil
}

func (b *stubBackend) PresignReady() bool    { return true }
func (b *stubBackend) SliceMD5Enabled() bool { return true }

func (b *stubBackend) SetDirectLink(_ context.Context, enabled bool) (string, error) {
	b.directLinks = append(b.directLinks, enabled)
	return "stub-直链目录", nil
}

type memReadSeekCloser struct{ *bytes.Reader }

func (m *memReadSeekCloser) Close() error { return nil }

// ---------------------------------------------------------------- 测试装置

type testEnv struct {
	server   *httptest.Server
	db       *store.DB
	backend  *service.Service
	stub     *stubBackend
	settings *settings.Store
	adminPwd string
}

func newTestEnv(t *testing.T) *testEnv {
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

	cfg := &config.Bootstrap{
		Listen:       ":0",
		DBPath:       filepath.Join(dir, "test.db"),
		DataDir:      dir,
		TempDir:      filepath.Join(dir, "tmp"),
		MasterSecret: []byte("unit-test-master-secret"),
	}
	box, err := secretbox.New(cfg.MasterSecret)
	if err != nil {
		t.Fatalf("构造加密盒失败: %v", err)
	}
	settingsStore := settings.NewStore(db, box)
	// 测试走"零配置 + 少量必要覆盖"的路径——这既是最容易出问题的路径，
	// 也是新部署第一次启动时的真实状态。
	if err := settingsStore.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDeliveryTicketTTL: "1m",
		settings.KeySessionTTL:        "1h",
		// 测试需要覆盖不同来源的限流维度；httptest 客户端本身只有
		// 127.0.0.1，因此把本地回环标为可信代理后可注入两个测试地址。
		settings.KeyTrustedProxies: "127.0.0.0/8",
		settings.KeyCryptokeys:     "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		// 用户侧邮件接口要求"模式为文件与邮件 且 开启收件"才可达
		// （见 Runtime.MailAvailable）。夹具默认不写这两项，于是默认状态下
		// 整条 /api/mail/* 应当是 404——而绝大多数用例测的正是邮件功能本身，
		// 所以这里显式把收件打开。需要测"关闭"形态的用例自己再关掉。
		settings.KeyMailReceiveEnabled: "true",
	}, 0); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	authSvc := auth.NewService(db, settingsStore)
	stub := newStubBackend()
	svc, err := service.New(service.Deps{
		DB:       db,
		Auth:     authSvc,
		Backend:  stub,
		Settings: settingsStore,
		DataDir:  dir,
		TempDir:  filepath.Join(dir, "tmp"),
	})
	if err != nil {
		t.Fatalf("构造业务服务失败: %v", err)
	}

	const adminPwd = "admin-strong-password"
	hash, err := auth.HashPassword(adminPwd)
	if err != nil {
		t.Fatalf("哈希密码失败: %v", err)
	}
	if _, err := store.CreateUser(context.Background(), db.W(), store.User{
		Account:      "admin",
		PasswordHash: hash,
		GroupName:    perm.GroupAdmin,
		Status:       store.UserEnabled,
	}); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}

	srv := httptest.NewServer(NewServer(cfg, authSvc, svc, settingsStore))
	t.Cleanup(srv.Close)
	return &testEnv{
		server: srv, db: db, backend: svc, stub: stub,
		settings: settingsStore, adminPwd: adminPwd,
	}
}

// do 发起一次请求并返回状态码与响应体。
func (e *testEnv) do(t *testing.T, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	return e.doWithHeaders(t, method, path, body, nil, token)
}

// doWithHeaders 在 do 的基础上允许附加请求头。
//
// 初始化流程的访问控制依赖客户端地址，而地址要从转发头还原，
// 因此测试必须能构造带 X-Forwarded-For 的请求。
func (e *testEnv) doWithHeaders(t *testing.T, method, path string, body any, headers map[string]string, token string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var payload map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &payload)
	}
	return res.StatusCode, payload
}

func (e *testEnv) login(t *testing.T) string {
	t.Helper()
	status, payload := e.do(t, http.MethodPost, "/api/auth/login", map[string]any{
		"account":  "admin",
		"password": e.adminPwd,
	}, "")
	if status != http.StatusOK {
		t.Fatalf("登录应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	token, _ := data["token"].(string)
	if token == "" {
		t.Fatalf("登录未返回令牌: %v", payload)
	}
	return token
}

// ---------------------------------------------------------------- 用例

func TestHealthz(t *testing.T) {
	env := newTestEnv(t)
	status, payload := env.do(t, http.MethodGet, "/api/healthz", nil, "")
	if status != http.StatusOK {
		t.Fatalf("探活应返回 200，实际 %d", status)
	}
	data, _ := payload["data"].(map[string]any)
	if data["status"] != "ok" {
		t.Fatalf("探活状态异常: %v", payload)
	}
	if data["encryptionReady"] != true {
		t.Fatalf("配置了 KEK 时应报告加密就绪: %v", payload)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	env := newTestEnv(t)
	status, _ := env.do(t, http.MethodPost, "/api/auth/login", map[string]any{
		"account":  "admin",
		"password": "wrong-password",
	}, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("错误口令应返回 401，实际 %d", status)
	}
	// 账号不存在与口令错误必须返回同一状态，否则接口会变成账号枚举器。
	status2, _ := env.doWithHeaders(t, http.MethodPost, "/api/auth/login", map[string]any{
		"account":  "no-such-user",
		"password": "wrong-password",
	}, map[string]string{"X-Forwarded-For": "198.51.100.10"}, "")
	if status2 != status {
		t.Fatalf("账号不存在应返回与口令错误相同的状态，实得 %d 与 %d", status2, status)
	}
}

func TestUnauthenticatedIsRejected(t *testing.T) {
	env := newTestEnv(t)
	status, _ := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("未登录访问文件树应返回 401，实际 %d", status)
	}
	// 无效令牌同样不能悄悄降级成访客后放行。
	status2, _ := env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, "not-a-real-token")
	if status2 != http.StatusUnauthorized {
		t.Fatalf("无效令牌应返回 401，实际 %d", status2)
	}
}

func TestNodesLifecycle(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// 新建目录。
	status, payload := env.do(t, http.MethodPost, "/api/fs/mkdir", map[string]any{
		"parentPath": "/",
		"name":       "文档",
	}, token)
	if status != http.StatusOK {
		t.Fatalf("新建目录应成功，实际 %d: %v", status, payload)
	}

	// 同名再建必须冲突，而不是静默覆盖或生成第二份。
	status, _ = env.do(t, http.MethodPost, "/api/fs/mkdir", map[string]any{
		"parentPath": "/",
		"name":       "文档",
	}, token)
	if status != http.StatusConflict {
		t.Fatalf("重复建目录应返回 409，实际 %d", status)
	}

	// 列表里应看到它。
	status, payload = env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, token)
	if status != http.StatusOK {
		t.Fatalf("列目录应成功，实际 %d", status)
	}
	data, _ := payload["data"].(map[string]any)
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("根目录应有 1 个条目，实得 %d: %v", len(items), payload)
	}
	first, _ := items[0].(map[string]any)
	if first["name"] != "文档" || first["isFolder"] != true {
		t.Fatalf("目录项内容不正确: %v", first)
	}

	// 重命名。
	status, _ = env.do(t, http.MethodPost, "/api/fs/rename", map[string]any{
		"path":    "/文档",
		"newName": "资料",
	}, token)
	if status != http.StatusOK {
		t.Fatalf("重命名应成功，实际 %d", status)
	}
	status, _ = env.do(t, http.MethodGet, "/api/fs/stat?path=/资料", nil, token)
	if status != http.StatusOK {
		t.Fatalf("重命名后新路径应可访问，实际 %d", status)
	}

	// 把目录移到自己下面必须被拒绝（环检测）。
	status, _ = env.do(t, http.MethodPost, "/api/fs/move", map[string]any{
		"path":       "/资料",
		"destParent": "/资料",
	}, token)
	if status != http.StatusBadRequest {
		t.Fatalf("把目录移入自身应返回 400，实际 %d", status)
	}

	// 路径穿越在入口被拒。
	status, _ = env.do(t, http.MethodGet, "/api/fs/list?path=/../etc", nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("含穿越段的路径应返回 400，实际 %d", status)
	}

	// 删除。
	status, _ = env.do(t, http.MethodPost, "/api/fs/delete", map[string]any{"path": "/资料"}, token)
	if status != http.StatusOK {
		t.Fatalf("删除应成功，实际 %d", status)
	}
	status, _ = env.do(t, http.MethodGet, "/api/fs/list?path=/", nil, token)
	if status != http.StatusOK {
		t.Fatalf("删除后列目录应成功，实际 %d", status)
	}
}

func TestRegisterFollowsConfiguredMode(t *testing.T) {
	env := newTestEnv(t)
	// 默认注册模式是"仅邀请码"，缺邀请码必须被拒。
	status, _ := env.do(t, http.MethodPost, "/api/auth/register", map[string]any{
		"account":  "newbie",
		"password": "a-long-enough-password",
	}, "")
	if status != http.StatusForbidden {
		t.Fatalf("仅邀请码模式缺码应返回 403，实际 %d", status)
	}
	// 账号名过短应被参数校验拦下。
	status, _ = env.do(t, http.MethodPost, "/api/auth/register", map[string]any{
		"account":  "a",
		"password": "a-long-enough-password",
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("账号名过短应返回 400，实际 %d", status)
	}
}

func TestAdminEndpointsRequirePermission(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// 管理员应当能拿到概览。
	status, payload := env.do(t, http.MethodGet, "/api/admin/overview", nil, token)
	if status != http.StatusOK {
		t.Fatalf("管理员读取概览应成功，实际 %d: %v", status, payload)
	}

	// 普通用户组不带头部权限：建一个普通账号验证越权被拒。
	hash, err := auth.HashPassword("normal-user-password")
	if err != nil {
		t.Fatalf("哈希密码失败: %v", err)
	}
	if _, err := store.CreateUser(context.Background(), env.db.W(), store.User{
		Account:      "normal",
		PasswordHash: hash,
		GroupName:    perm.GroupNormal,
		Status:       store.UserEnabled,
	}); err != nil {
		t.Fatalf("创建普通用户失败: %v", err)
	}
	status, payload = env.do(t, http.MethodPost, "/api/auth/login", map[string]any{
		"account":  "normal",
		"password": "normal-user-password",
	}, "")
	if status != http.StatusOK {
		t.Fatalf("普通用户登录应成功，实际 %d", status)
	}
	data, _ := payload["data"].(map[string]any)
	userToken, _ := data["token"].(string)

	status, _ = env.do(t, http.MethodGet, "/api/admin/overview", nil, userToken)
	if status != http.StatusForbidden {
		t.Fatalf("普通用户读取管理概览应返回 403，实际 %d", status)
	}
	status, _ = env.do(t, http.MethodGet, "/api/admin/users", nil, userToken)
	if status != http.StatusForbidden {
		t.Fatalf("普通用户读取账号列表应返回 403，实际 %d", status)
	}
}

func TestUnknownJSONFieldIsRejected(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	// 字段名写错时必须明确报错，而不是静默按默认值处理，
	// 否则会变成"设置没生效"这类难查的问题。
	status, _ := env.do(t, http.MethodPost, "/api/fs/mkdir", map[string]any{
		"parentPat": "/",
		"name":      "x",
	}, token)
	if status != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400，实际 %d", status)
	}
}

func TestAnnouncementsVisibleToGuest(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	// 建一条全体公告。
	status, payload := env.do(t, http.MethodPost, "/api/admin/announcements", map[string]any{
		"kind":     "ticker",
		"audience": "all",
		"title":    "维护通知",
		"body":     "今晚维护",
		"enabled":  true,
	}, token)
	if status != http.StatusOK {
		t.Fatalf("创建滚动公告应成功，实际 %d: %v", status, payload)
	}
	// 访客（无令牌）也应能看到全体公告。
	status, payload = env.do(t, http.MethodGet, "/api/announcements", nil, "")
	if status != http.StatusOK {
		t.Fatalf("访客读取公告应成功，实际 %d: %v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	ticker, _ := data["ticker"].(map[string]any)
	if ticker == nil || !strings.Contains(fmt.Sprint(ticker["title"]), "维护") {
		t.Fatalf("访客应看到滚动公告: %v", payload)
	}
}

func TestStreamRequiresValidTicket(t *testing.T) {
	env := newTestEnv(t)
	// 票据是 URL 上的唯一凭据，伪造值必须被拒。
	status, _ := env.do(t, http.MethodGet, "/api/fs/stream?ticket=tk_fake", nil, "")
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Fatalf("伪造票据应被拒，实际 %d", status)
	}
}

func TestCDNAuthRejectsUnknownTicket(t *testing.T) {
	env := newTestEnv(t)
	status, _ := env.do(t, http.MethodGet, "/api/cdn/auth?xph=tk_fake", nil, "")
	if status != http.StatusForbidden {
		t.Fatalf("未知票据的鉴权应返回 403，实际 %d", status)
	}
}

// TestCDNAuthAcceptsRealCallbackShape 按 123 实际发来的回调形态做一次端到端。
//
// 这是 123 当前真实拼出的样子（注意 request_uri 里的 & 没有被转义，xph 因此被
// 解析到了顶层）：remote_addr 给访客地址，request_uri 带整条原始 URI。
func TestCDNAuthAcceptsRealCallbackShape(t *testing.T) {
	env := newTestEnv(t)
	const callback = "/api/cdn/auth?remote_addr=118.254.219.198" +
		"&request_uri=/XiaoyuPostHub-Next/xph-probe-1.xph?auth_key=1700000000-a-1817754570-b" +
		"&xph=tk_cz2zccwvnt76w4qcluxq"
	status, _ := env.do(t, http.MethodGet, callback, nil, "")
	if status != http.StatusForbidden {
		t.Fatalf("未知票据应返回 403，实际 %d", status)
	}
}

// TestClaimedTicketIDReadsTopLevelAndRequestURI 两条取票路径都要通。
//
// 123 目前没转义 request_uri 里的 &，票据因此被解析到顶层；哪天它改成转义，
// 票据就会整个缩回 request_uri 内部。两条都得读得到，否则 123 一次转义行为
// 调整就会让全部直链 403。
func TestClaimedTicketIDReadsTopLevelAndRequestURI(t *testing.T) {
	// 现状：xph 被拆到顶层。
	leaked := httptest.NewRequest(http.MethodGet,
		"/api/cdn/auth?remote_addr=1.2.3.4&request_uri=/a/f.xph?auth_key=k&xph=tk_top", nil)
	if got := claimedTicketID(leaked); got != "tk_top" {
		t.Fatalf("应从顶层读到票据，却读到 %q", got)
	}

	// 假如 123 改成转义 &，票据只剩在 request_uri 内部。
	encoded := httptest.NewRequest(http.MethodGet,
		"/api/cdn/auth?remote_addr=1.2.3.4&request_uri=%2Fa%2Ff.xph%3Fauth_key%3Dk%26xph%3Dtk_inner", nil)
	if got := claimedTicketID(encoded); got != "tk_inner" {
		t.Fatalf("应从 request_uri 里解出票据，却读到 %q", got)
	}

	// 两条都没有时返回空，让 AuthorizeCDN 报"缺少票据"。
	if got := claimedTicketID(httptest.NewRequest(http.MethodGet, "/api/cdn/auth?remote_addr=1.2.3.4", nil)); got != "" {
		t.Fatalf("无票据时应返回空，却读到 %q", got)
	}
	// 顶层优先：两者都在时以顶层为准（它才是被拆出来的那一个）。
	both := httptest.NewRequest(http.MethodGet,
		"/api/cdn/auth?xph=tk_top&request_uri=%2Fa%2Ff.xph%3Fxph%3Dtk_inner", nil)
	if got := claimedTicketID(both); got != "tk_top" {
		t.Fatalf("应优先取顶层票据，却读到 %q", got)
	}
}

// TestCDNAuthRejectIsLogged：回源鉴权的拒绝必须落服务端日志，且不得把票据值
// 抄进日志。
//
// 响应体刻意不回细节（端点公开可达，细节会变成探测工具），因此这行日志是运维
// 唯一的诊断入口：上游没带访客地址时，浏览器与管理端都只看到同一个裸 403。
// 少了日志，"上游没回传票据参数""票据已失效""上游没带访客地址"三种截然不同的
// 配置问题在现象上完全无法区分。
func TestCDNAuthRejectIsLogged(t *testing.T) {
	env := newTestEnv(t)

	var buf bytes.Buffer
	prevWriter, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevWriter); log.SetFlags(prevFlags) })

	const secret = "tk_supersecret_value"
	status, _ := env.do(t, http.MethodGet,
		"/api/cdn/auth?"+service.HeaderPanTicket+"="+secret+"&"+paramClaimedIP+"=1.2.3.4", nil, "")
	if status != http.StatusForbidden {
		t.Fatalf("未知票据应返回 403，实际 %d", status)
	}

	out := buf.String()
	if !strings.Contains(out, "回源鉴权拒绝") {
		t.Fatalf("拒绝必须落日志，实际输出：%q", out)
	}
	if !strings.Contains(out, "票据不存在") {
		t.Fatalf("日志应带出拒绝原因，实际输出：%q", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("日志不得回显卡据值，实际输出：%q", out)
	}
	if !strings.Contains(out, "访客地址=1.2.3.4") {
		t.Fatalf("日志应带出上游声明的访客地址，实际输出：%q", out)
	}
	// 哨兵前缀（"service: 无权访问:"）在日志里纯属噪音，应当已被剥掉。
	if strings.Contains(out, "service:") {
		t.Fatalf("日志不应带出哨兵前缀，实际输出：%q", out)
	}
}

// TestCDNAuthRejectDropsNonIPVisitorParam 非 IP 的访客地址一个字符都不进日志。
//
// 查询参数可以合法地用 %0A 编码换行，解码后原样进入日志。日志只写能解析成 IP
// 的值，因此注入内容连一个字都进不去——地址这个类型本身装不下换行。
func TestCDNAuthRejectDropsNonIPVisitorParam(t *testing.T) {
	env := newTestEnv(t)

	var buf bytes.Buffer
	prevWriter, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevWriter); log.SetFlags(prevFlags) })

	const injected = "1.2.3.4%0Ahttpapi:%20%E5%9B%9E%E6%BA%90%E9%89%B4%E6%9D%83%E6%94%BE%E8%A1%8C%0D"
	status, _ := env.do(t, http.MethodGet,
		"/api/cdn/auth?xph=tk_fake&"+paramClaimedIP+"="+injected, nil, "")
	if status != http.StatusForbidden {
		t.Fatalf("未知票据应返回 403，实际 %d", status)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("注入伪造出了额外日志行，实得 %d 行：%q", len(lines), buf.String())
	}
	if strings.Contains(buf.String(), "鉴权放行") {
		t.Fatalf("注入内容进入了日志：%q", buf.String())
	}
	if !strings.Contains(lines[0], "访客地址=未透传") {
		t.Fatalf("无法解析的来源应记为未透传：%q", lines[0])
	}
}

// TestClaimedIPTakesFirstParamValue 钉住"取第一个同名参数"这条安全约定。
//
// 拿到直链的人可以自己往 CDN 链接追加 &remote_addr=6.6.6.6，那些参数会随
// $request_uri 原样带过来。Query().Get 只取第一个，所以部署侧必须把
// remote_addr 写在 request_uri 之前；一旦写反，攻击者追加的值就会排在前面被
// 采信，来源校验形同虚设。
func TestClaimedIPTakesFirstParamValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet,
		"/api/cdn/auth?remote_addr=1.2.3.4&xph=tk_x&remote_addr=6.6.6.6", nil)
	if got := claimedClientIP(r); got != "1.2.3.4" {
		t.Fatalf("应取第一个 remote_addr（部署侧注入的那个），得 %q", got)
	}
}

// TestClaimedIPEmptyWhenAbsent 什么都没给时返回空，交给 checkTicketIP 拒绝，
// 而不是回退到某个猜测值。
func TestClaimedIPEmptyWhenAbsent(t *testing.T) {
	empty := httptest.NewRequest(http.MethodGet, "/api/cdn/auth?xph=tk_x", nil)
	if got := claimedClientIP(empty); got != "" {
		t.Fatalf("无 remote_addr 参数时应返回空，却取到 %q", got)
	}
	// 空白值等同于没给。
	blank := httptest.NewRequest(http.MethodGet, "/api/cdn/auth?remote_addr=%20%20", nil)
	if got := claimedClientIP(blank); got != "" {
		t.Fatalf("空白 remote_addr 应视为未给，却取到 %q", got)
	}
}
