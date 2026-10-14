package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// rawGet 发起一次请求并原样返回响应，供直链这类返回字节而非 JSON 的端点断言。
func rawGet(t *testing.T, e *testEnv, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 刻意不带任何 Authorization：直链的请求方是 wget / 播放器这类裸客户端。
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// enableDirectLink 打开直链总开关。
func enableDirectLink(t *testing.T, e *testEnv) {
	t.Helper()
	if err := e.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDirectLinkEnabled: "true",
	}, 0); err != nil {
		t.Fatalf("打开直链总开关失败: %v", err)
	}
}

// seedDirectLinkFile 走真实入库路径造一份文件节点，再经管理 API 创建直链——
// 连同 create 接口一起覆盖。
func seedDirectLinkFile(t *testing.T, e *testEnv, token, path string, payload []byte) store.DirectLink {
	t.Helper()
	ctx := context.Background()
	file, err := e.backend.IngestPlaintext(ctx, bytes.NewReader(payload), 1)
	if err != nil {
		t.Fatalf("入库明文失败: %v", err)
	}
	if err := store.InsertNode(ctx, e.db.W(), store.Node{
		UserID: 1, LogicalPath: path, NodeType: store.NodeFile,
		FileChecksum: file.Checksum, Name: vpath.Base(path),
		ParentPath: vpath.Parent(path), SizePlain: file.SizePlain,
		Mtime: store.Now(), CreatedAt: store.Now(),
	}); err != nil {
		t.Fatalf("创建用户节点失败: %v", err)
	}
	if _, err := store.AddFileRef(ctx, e.db.W(), file.Checksum); err != nil {
		t.Fatalf("建立内容引用失败: %v", err)
	}
	if token == "" {
		token = e.login(t)
	}
	status, body := e.do(t, http.MethodPost, "/api/direct-link", map[string]any{"path": path}, token)
	if status != http.StatusOK {
		t.Fatalf("创建直链应成功，实得 %d %v", status, body)
	}
	raw, err := json.Marshal(body["data"].(map[string]any)["link"])
	if err != nil {
		t.Fatalf("序列化直链响应失败: %v", err)
	}
	var link store.DirectLink
	if err := json.Unmarshal(raw, &link); err != nil {
		t.Fatalf("解析直链响应失败: %v", err)
	}
	if link.ID == "" {
		t.Fatalf("创建直链未返回标识: %v", body)
	}
	return link
}

// TestDirectLinkDeliversPlaintextToAnonymousClient 是本功能的核心断言：
// 匿名 GET /d/{token} 拿到的是明文字节，不是页面。
func TestDirectLinkDeliversPlaintextToAnonymousClient(t *testing.T) {
	e := newTestEnv(t)
	token := e.login(t)
	_ = token
	enableDirectLink(t, e)
	payload := bytes.Repeat([]byte("direct-link-plaintext "), 50_000) // > 1 MiB
	link := seedDirectLinkFile(t, e, token, "/文件.bin", payload)

	res := rawGet(t, e, http.MethodGet, "/d/"+link.ID, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("直链应返回 200，实得 %d", res.StatusCode)
	}
	// 绝不能被 SPA 兜底吞成 index.html。
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/") {
		t.Fatalf("直链应返回文件类型而不是页面，实得 Content-Type=%q", ct)
	}
	if got := res.Header.Get(service.HeaderContentForm); got != "plaintext" {
		t.Fatalf("直链必须交付明文，实得 contentForm=%q", got)
	}
	if got := res.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("直链必须给出 Content-Disposition 供 wget 决定落盘文件名，实得 %q", got)
	}
	if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("直链应支持区间请求，实得 Accept-Ranges=%q", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("直链内容绝不能被缓存，实得 Cache-Control=%q", got)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("直链返回的字节与原文不一致：期望 %d，实得 %d", len(payload), len(body))
	}
}

// TestDirectLinkSupportsRangeAndHead 区间与 HEAD 语义。
func TestDirectLinkSupportsRangeAndHead(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	link := seedDirectLinkFile(t, e, "", "/range.bin", []byte("0123456789ABCDEFGHIJ"))

	res := rawGet(t, e, http.MethodGet, "/d/"+link.ID, map[string]string{"Range": "bytes=10-14"})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("区间请求应返回 206，实得 %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 10-14/20" {
		t.Fatalf("Content-Range 应按明文口径，实得 %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "ABCDE" {
		t.Fatalf("区间内容应为 ABCDE，实得 %q", body)
	}

	head := rawGet(t, e, http.MethodHead, "/d/"+link.ID, nil)
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 应返回 200，实得 %d", head.StatusCode)
	}
	if got := head.Header.Get("Content-Length"); got != "20" {
		t.Fatalf("HEAD 的 Content-Length 应为明文长度 20，实得 %q", got)
	}

	// 越界区间：416，且不落进 SPA 兜底。
	bad := rawGet(t, e, http.MethodGet, "/d/"+link.ID, map[string]string{"Range": "bytes=999-"})
	if bad.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界区间应返回 416，实得 %d", bad.StatusCode)
	}
}

// TestDirectLinkNotSwallowedBySPAFallback 未注册或已删除的直链必须返回 404，
// 而不是 SPA 的 index.html——否则贴进 wget 会下载到一个网页。
func TestDirectLinkNotSwallowedBySPAFallback(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	res := rawGet(t, e, http.MethodGet, "/d/does-not-exist", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的直链应返回 404，实得 %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("不存在的直链不得落进 SPA 兜底返回 HTML，实得 %q", ct)
	}
}

// TestDirectLinkHiddenWhenMasterSwitchOff 总开关关闭时整条能力表现为不存在。
func TestDirectLinkHiddenWhenMasterSwitchOff(t *testing.T) {
	e := newTestEnv(t)
	// 建链本身也受总开关管，因此顺序必须是「先开 → 建 → 再关」。
	enableDirectLink(t, e)
	link := seedDirectLinkFile(t, e, "", "/switch.bin", []byte("switch"))
	if rawGet(t, e, http.MethodGet, "/d/"+link.ID, nil).StatusCode != http.StatusOK {
		t.Fatal("开关打开时直链应可用")
	}
	if err := e.settings.SetMany(context.Background(), map[settings.Key]string{
		settings.KeyDirectLinkEnabled: "false",
	}, 0); err != nil {
		t.Fatalf("关闭总开关失败: %v", err)
	}
	// 与系统模式关闭邮件同一套语义：表现为"这个站点没有直链功能"。
	res := rawGet(t, e, http.MethodGet, "/d/"+link.ID, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("总开关关闭时应按不存在处理，实得 %d", res.StatusCode)
	}
	// 关闭时管理面也不可用，数据库中的记录保留。
	token := e.login(t)
	if status, _ := e.do(t, http.MethodGet, "/api/direct-link", nil, token); status != http.StatusNotFound {
		t.Fatalf("总开关关闭时列表应被关闭，实得 %d", status)
	}
}

// TestDirectLinkManagementRequiresLogin 管理接口必须要求登录。
func TestDirectLinkManagementRequiresLogin(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/direct-link"},
		{http.MethodGet, "/api/direct-link"},
		{http.MethodDelete, "/api/direct-link/x"},
	} {
		status, _ := e.do(t, tc.method, tc.path, nil, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("%s %s 匿名访问应被拒绝，实得 %d", tc.method, tc.path, status)
		}
	}
}

func TestDirectLinkSettlesAuthenticatedRequests(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	token := e.login(t)
	payload := []byte("0123456789ABCDEFGHIJ")
	link := seedDirectLinkFile(t, e, token, "/authenticated.bin", payload)
	for _, rangeHeader := range []string{"", "bytes=10-14"} {
		t.Run(rangeHeader, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/d/"+link.ID, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Range", rangeHeader)
			res := httptest.NewRecorder()
			e.server.Config.Handler.ServeHTTP(res, req)
			if res.Code != http.StatusOK && res.Code != http.StatusPartialContent {
				t.Fatalf("download failed: %d %s", res.Code, res.Body.String())
			}
			ticket, err := store.GetTicket(context.Background(), e.db.R(), res.Header().Get(service.HeaderTicket))
			if err != nil {
				t.Fatal(err)
			}
			if ticket.SettledBytes <= 0 {
				t.Fatalf("authenticated download was not settled: %+v", ticket)
			}
		})
	}
}

func TestDirectLinkManagementAfterPermissionRevoked(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	token := e.login(t)
	link := seedDirectLinkFile(t, e, token, "/manage.bin", []byte("manage"))
	ctx := context.Background()
	if _, err := e.db.W().ExecContext(ctx, `UPDATE user_groups SET permissions = permissions & ~? WHERE name = ?`, int64(perm.DirectLink), perm.GroupAdmin); err != nil {
		t.Fatal(err)
	}
	e.backend.Auth.InvalidateGroupCache(perm.GroupAdmin)
	for _, req := range []struct {
		method string
		path   string
		body   any
		status int
	}{
		{http.MethodPost, "/api/direct-link", map[string]any{"path": "/manage.bin"}, http.StatusForbidden},
		{http.MethodGet, "/api/direct-link", nil, http.StatusOK},
		{http.MethodPatch, "/api/direct-link/" + link.ID, map[string]any{"disabled": true}, http.StatusOK},
		{http.MethodDelete, "/api/direct-link/" + link.ID, nil, http.StatusOK},
	} {
		status, body := e.do(t, req.method, req.path, req.body, token)
		if status != req.status {
			t.Fatalf("%s %s: expected %d, got %d %v", req.method, req.path, req.status, status, body)
		}
	}
}

func TestDirectLinkPartialPatchPreservesFields(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	token := e.login(t)
	link := seedDirectLinkFile(t, e, token, "/patch.bin", []byte("patch"))
	for _, body := range []map[string]any{{"expiresAt": 2000000000, "maxHits": 5, "disabled": true}, {"maxHits": 8}} {
		status, res := e.do(t, http.MethodPatch, "/api/direct-link/"+link.ID, body, token)
		if status != http.StatusOK {
			t.Fatalf("PATCH: %d %v", status, res)
		}
	}
	got, err := store.GetDirectLink(context.Background(), e.db.R(), link.ID)
	if err != nil || got.ExpiresAt != 2000000000 || got.MaxHits != 8 || !got.Disabled {
		t.Fatalf("partial PATCH reset fields: %+v %v", got, err)
	}
	status, res := e.do(t, http.MethodPatch, "/api/direct-link/"+link.ID, map[string]any{"expiresAt": 0, "maxHits": 0, "disabled": false}, token)
	if status != http.StatusOK {
		t.Fatalf("explicit zero: %d %v", status, res)
	}
	got, err = store.GetDirectLink(context.Background(), e.db.R(), link.ID)
	if err != nil || got.ExpiresAt != 0 || got.MaxHits != 0 || got.Disabled {
		t.Fatalf("explicit values ignored: %+v %v", got, err)
	}
}

func TestDirectLinkSwitchClosesAllEndpoints(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	token := e.login(t)
	link := seedDirectLinkFile(t, e, token, "/switch-all.bin", []byte("switch"))
	if err := e.settings.SetMany(context.Background(), map[settings.Key]string{settings.KeyDirectLinkEnabled: "false"}, 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/direct-link"}, {http.MethodGet, "/api/direct-link"},
		{http.MethodGet, "/api/direct-link/nodes?checksums=" + link.FileChecksum},
		{http.MethodPatch, "/api/direct-link/" + link.ID}, {http.MethodDelete, "/api/direct-link/" + link.ID},
		{http.MethodGet, "/d/" + link.ID}, {http.MethodHead, "/d/" + link.ID},
	} {
		status, body := e.do(t, tc.method, tc.path, nil, token)
		if status != http.StatusNotFound {
			t.Fatalf("disabled %s %s: %d %v", tc.method, tc.path, status, body)
		}
	}
	state, err := e.backend.SetupState(context.Background())
	if err != nil || state.DirectLinkEnabled {
		t.Fatalf("public switch: %+v %v", state, err)
	}
	retained, err := store.GetDirectLink(context.Background(), e.db.R(), link.ID)
	if err != nil || retained.ID != link.ID {
		t.Fatalf("disabled system removed record: %v", err)
	}
	enableDirectLink(t, e)
	if status, _ := e.do(t, http.MethodGet, "/api/direct-link", nil, token); status != http.StatusOK {
		t.Fatalf("re-enabled list: %d", status)
	}
}

type cancelDirectLinkWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelDirectLinkWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n > 1024 {
		n = 1024
	}
	written, _ := w.ResponseRecorder.Write(p[:n])
	w.cancel()
	return written, io.ErrClosedPipe
}
func TestDirectLinkCancelledRequestSettlesSentBytes(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	link := seedDirectLinkFile(t, e, "", "/cancel.bin", bytes.Repeat([]byte("x"), 100000))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/d/"+link.ID, nil).WithContext(ctx)
	res := &cancelDirectLinkWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	e.server.Config.Handler.ServeHTTP(res, req)
	ticket, err := store.GetTicket(context.Background(), e.db.R(), res.Header().Get(service.HeaderTicket))
	if err != nil || ticket.SettledBytes <= 0 || ticket.SettledBytes >= ticket.ReservedBytes {
		t.Fatalf("cancelled response was not partially settled: %+v %v", ticket, err)
	}
}

func TestDirectLinkTargetReturnsCurrentOwnedPath(t *testing.T) {
	e := newTestEnv(t)
	enableDirectLink(t, e)
	token := e.login(t)
	link := seedDirectLinkFile(t, e, token, "/old.bin", []byte("target"))
	status, body := e.do(t, http.MethodPost, "/api/fs/rename", map[string]any{"path": "/old.bin", "newName": "renamed.bin"}, token)
	if status != http.StatusOK {
		t.Fatalf("rename: %d %v", status, body)
	}
	status, body = e.do(t, http.MethodGet, "/api/direct-link/"+link.ID+"/target", nil, token)
	if status != http.StatusOK || body["data"].(map[string]any)["path"] != "/renamed.bin" {
		t.Fatalf("target: %d %v", status, body)
	}
	if err := e.settings.SetMany(context.Background(), map[settings.Key]string{settings.KeyDirectLinkEnabled: "false"}, 0); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.do(t, http.MethodGet, "/api/direct-link/"+link.ID+"/target", nil, token); status != http.StatusNotFound {
		t.Fatalf("target still available with switch off: %d", status)
	}
}
