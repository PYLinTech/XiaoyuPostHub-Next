package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCallAllowsNilLimiter(t *testing.T) {
	b := &Pan123{
		apiBase:     "https://pan.example",
		token:       "token",
		tokenExpiry: time.Now().Add(time.Hour),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"message":"ok"}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	if err := b.call(context.Background(), apiUploadFinish, nil, http.MethodPost,
		jsonBody{"preuploadID": "p1"}, nil); err != nil {
		t.Fatalf("上传完成接口使用 nil limiter 不应失败: %v", err)
	}
}

func TestHTTPRangeReaderUsesOpenContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &httpRangeReader{
		ctx: ctx,
		url: "https://cdn.example/object",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
		size: -1,
	}
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Fatal("打开对象的上下文取消后，远程 Range 读取应立即失败")
	}
}

func TestHTTPRangeReaderRejectsOverflowingSeek(t *testing.T) {
	r := &httpRangeReader{pos: int64(^uint64(0) >> 1)}
	if _, err := r.Seek(1, io.SeekCurrent); err == nil {
		t.Fatal("Seek 溢出应被拒绝")
	}
}

// TestHTTPRangeReaderClampsWindowToObjectSize：对象小于读取窗口时，Range
// 末端必须截断到最后一个字节；某些上游 CDN 对越界区间回 416 而不是按 RFC
// 收敛，小对象（含 1 字节探针）会因此完全不可读。
func TestHTTPRangeReaderClampsWindowToObjectSize(t *testing.T) {
	var gotRange string
	var calls int
	r := &httpRangeReader{
		url:  "https://cdn.example/tiny",
		size: 1,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			gotRange = req.Header.Get("Range")
			h := make(http.Header)
			h.Set("Content-Range", "bytes 0-0/1")
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Body:       io.NopCloser(strings.NewReader("x")),
				Header:     h,
			}, nil
		})},
	}
	buf := make([]byte, 4)
	n, err := r.Read(buf)
	if err != nil || n != 1 || string(buf[:n]) != "x" {
		t.Fatalf("小对象首读应得到 1 字节内容，实得 n=%d err=%v", n, err)
	}
	if gotRange != "bytes=0-0" {
		t.Fatalf("Range 必须截断为 bytes=0-0，实得 %q", gotRange)
	}
	// 越过 EOF 的读取不得再发 HTTP 请求，直接 EOF。
	n, err = r.Read(buf)
	if n != 0 || err != io.EOF || calls != 1 {
		t.Fatalf("EOF 后应零请求返回 EOF，实得 n=%d err=%v calls=%d", n, err, calls)
	}
}

// TestAccessTokenSingleflight 钉住令牌刷新的合并语义：大量并发调用在令牌缺失
// 时只能触发一次对上游的刷新请求，且全部拿到同一个令牌。
func TestAccessTokenSingleflight(t *testing.T) {
	var requests atomic.Int64
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			close(entered)
		}
		// 阻塞到测试放行：若 singleflight 失效，多余的刷新会在这里排队，
		// 放行后 requests 计数将大于 1。
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"data":{"accessToken":"tok","expiredAt":%q}}`,
			time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()

	b, err := NewPan123(Pan123Config{
		ClientID: "cid", ClientSecret: "secret", APIBase: srv.URL,
		DirectLinkTTL: time.Minute, UploadThreads: 1, QPS: 0,
		PollInterval: time.Second, PollAttempts: 1,
	})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	tokens := make(chan string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			tok, err := b.accessToken(context.Background(), false)
			if err != nil {
				errs <- err
				return
			}
			tokens <- tok
		}()
	}

	// 等发起者进入 HTTP，再留一个窗口让其余调用抵达等待点。
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("刷新请求未在预期时间内到达上游")
	}
	time.Sleep(100 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		close(gate)
		t.Fatalf("并发刷新未被合并：刷新窗口内已发出 %d 次令牌请求", got)
	}
	close(gate)
	wg.Wait()
	close(errs)
	close(tokens)

	for err := range errs {
		if err != nil {
			t.Fatalf("并发获取令牌不应失败: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("期望只刷新一次，实际 %d 次", got)
	}
	for tok := range tokens {
		if tok != "tok" {
			t.Fatalf("等待者应拿到刷新出的同一令牌，实得 %q", tok)
		}
	}
}

// TestSetDirectLinkSwitchesEndpoint：勾选/取消勾选必须分别打到
// /direct-link/enable 与 /direct-link/disable，请求体带根目录 fileID。
func TestSetDirectLinkSwitchesEndpoint(t *testing.T) {
	var switches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiAccessToken:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"code":0,"data":{"accessToken":"tok","expiredAt":%q}}`,
				time.Now().Add(time.Hour).Format(time.RFC3339))
		case apiDirectLinkEnable, apiDirectLinkDisable:
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost {
				t.Errorf("开关接口必须用 POST，实得 %s", r.Method)
			}
			if !strings.Contains(string(body), `"fileID":80360960`) {
				t.Errorf("请求体必须携带根目录 fileID，实得 %s", body)
			}
			switches = append(switches, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"data":{"filename":"小宇中转目录"}}`)
		default:
			t.Errorf("未预期的上游请求: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, err := NewPan123(Pan123Config{
		ClientID: "cid", ClientSecret: "secret", APIBase: srv.URL, RootDirID: "80360960",
		DirectLinkTTL: time.Minute, UploadThreads: 1, QPS: 0,
		PollInterval: time.Second, PollAttempts: 1,
	})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	if name, err := b.SetDirectLink(context.Background(), true); err != nil || name != "小宇中转目录" {
		t.Fatalf("启用直链空间结果不正确: name=%q err=%v", name, err)
	}
	if name, err := b.SetDirectLink(context.Background(), false); err != nil || name != "小宇中转目录" {
		t.Fatalf("关闭直链空间结果不正确: name=%q err=%v", name, err)
	}
	if len(switches) != 2 || switches[0] != apiDirectLinkEnable || switches[1] != apiDirectLinkDisable {
		t.Fatalf("开关请求路径序列不正确: %v", switches)
	}
}

// TestSetDirectLinkRejectsNetdiskRoot：网盘根目录（编号 0）不能作为开关目标，
// 必须在发请求前直接拒绝。
func TestSetDirectLinkRejectsNetdiskRoot(t *testing.T) {
	b, err := NewPan123(Pan123Config{
		ClientID: "cid", ClientSecret: "secret", APIBase: "http://127.0.0.1:0",
		DirectLinkTTL: time.Minute, UploadThreads: 1, QPS: 0,
		PollInterval: time.Second, PollAttempts: 1,
	})
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	if _, err := b.SetDirectLink(context.Background(), true); err == nil {
		t.Fatal("根目录编号为 0 时启用直链空间必须被拒绝")
	}
	if _, err := b.SetDirectLink(context.Background(), false); err == nil {
		t.Fatal("根目录编号为 0 时关闭直链空间也必须被拒绝")
	}
}

func TestParseContentRange(t *testing.T) {
	start, end, total, ok := parseContentRange("bytes 1048576-2097151/4194304")
	if !ok || start != 1048576 || end != 2097151 || total != 4194304 {
		t.Fatalf("解析 Content-Range 结果不正确: %d-%d/%d ok=%v", start, end, total, ok)
	}
	if _, _, _, ok := parseContentRange("bytes 0-10/10"); ok {
		t.Fatal("总长度不得小于等于结束偏移")
	}
	if _, _, _, ok := parseContentRange("bytes 10-9/20"); ok {
		t.Fatal("反向范围应被拒绝")
	}
}

func TestHTTPRangeReaderReusesIgnoredRangeResponse(t *testing.T) {
	payload := make([]byte, 2*rangeReadChunk+13)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	calls := 0
	r := &httpRangeReader{
		url: "https://cdn.example/object", size: -1,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, ContentLength: -1,
				Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
		})},
	}
	defer r.Close()
	first := make([]byte, 32)
	if _, err := io.ReadFull(r, first); err != nil {
		t.Fatal(err)
	}
	// 当前窗口内回退应复用缓冲，后续窗口仍从保留响应的正确位置读取。
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, payload) || calls != 1 {
		t.Fatalf("sequential fallback must use one response: len=%d calls=%d err=%v", len(got), calls, err)
	}
	if _, err := r.Read(make([]byte, 1)); err != io.EOF || calls != 1 {
		t.Fatalf("unknown-length EOF must not reopen: calls=%d err=%v", calls, err)
	}
	if _, err := r.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(r)
	if err != nil || !bytes.Equal(got, payload[100:]) || calls != 2 {
		t.Fatalf("seek must reopen from requested offset: len=%d calls=%d err=%v", len(got), calls, err)
	}
}
