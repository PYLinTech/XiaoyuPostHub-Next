package backend

// 上传网关超时分类测试：123 上传网关对 60s 内未传完请求体的连接返回
// 空体/带 "i/o timeout" 的 HTTP 500——这是"服务器上传带宽过小"的判定
// 信号，必须被识别成专用错误而不是泛化的上传失败。

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newClassifyFixture 构造一个指向测试服务器的 Pan123：token 预置为可用。
func newClassifyFixture(t *testing.T, url string) *Pan123 {
	t.Helper()
	p, err := NewPan123(Pan123Config{
		ClientID: "id", ClientSecret: "secret", APIBase: url,
		DirectLinkTTL: time.Minute, UploadThreads: 1, QPS: 0,
		PollInterval: time.Second, PollAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.token = "test-token"
	p.tokenExpiry = time.Now().Add(time.Hour)
	return p
}

// TestMultipartBodyLengthMatchesReal：dry-run 长度必须与真实请求体
// 逐字节等长——HTTP/2 会在长度不符时直接拒绝请求。
func TestMultipartBodyLengthMatchesReal(t *testing.T) {
	const fileLen = 4096
	want, _, boundary := multipartBodyLength(fileLen, "preupload-1", 1, "md5md5md5md5md5md5md5md5")

	var real bytes.Buffer
	mw := multipart.NewWriter(&real)
	if err := mw.SetBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("preuploadID", "preupload-1")
	_ = mw.WriteField("sliceNo", "1")
	_ = mw.WriteField("sliceMD5", "md5md5md5md5md5md5md5md5")
	fw, _ := mw.CreateFormFile("slice", sliceFormName(1))
	_, _ = fw.Write(bytes.Repeat([]byte{0}, fileLen))
	_ = mw.Close()

	if int64(real.Len()) != want {
		t.Fatalf("长度不一致：声明 %d，实际 %d", want, real.Len())
	}
}

// TestSliceUploadDetectsGatewayBodyTimeout：网关 60s 读超时的典型签名
// （HTTP 500 + "Read from request Body failed ... i/o timeout"）必须
// 识别为 ErrUploadBandwidth。
func TestSliceUploadDetectsGatewayBodyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 网关收完请求体才返回错误时，错误体才能可靠到达客户端。
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "Read from request Body failed: read tcp 10.0.0.1:1->10.0.0.2:2: i/o timeout", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newClassifyFixture(t, srv.URL)
	src := bytes.NewReader(bytes.Repeat([]byte{0}, 4096))
	err := p.uploadSlice(context.Background(), srv.URL, "preupload-1", 1, src, 0, 4096)
	if !strings.Contains(err.Error(), "分片 1") {
		t.Fatalf("错误应带分片序号: %v", err)
	}
	if !IsUploadBandwidthError(err) {
		t.Fatalf("网关读超时应识别为带宽不足: %v", err)
	}
}

// TestSliceUploadPlain500NotBandwidth：空响应体且请求耗时很短的 500
// 不应误判为带宽问题。
func TestSliceUploadPlain500NotBandwidth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newClassifyFixture(t, srv.URL)
	src := bytes.NewReader(bytes.Repeat([]byte{0}, 4096))
	err := p.uploadSlice(context.Background(), srv.URL, "preupload-1", 1, src, 0, 4096)
	if err == nil || IsUploadBandwidthError(err) {
		t.Fatalf("瞬时 500 不应判为带宽不足: %v", err)
	}
}

// TestSliceUploadLongRequest500IsBandwidth：即使响应体为空，只要请求
// 耗时达到网关读超时阈值，就应判定为带宽不足。
func TestSliceUploadLongRequest500IsBandwidth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	original := sliceBodyTimeoutFloor
	sliceBodyTimeoutFloor = 0
	t.Cleanup(func() { sliceBodyTimeoutFloor = original })

	p := newClassifyFixture(t, srv.URL)
	src := bytes.NewReader(bytes.Repeat([]byte{0}, 4096))
	err := p.uploadSlice(context.Background(), srv.URL, "preupload-1", 1, src, 0, 4096)
	if !IsUploadBandwidthError(err) {
		t.Fatalf("长耗时 500 应判为带宽不足: %v", err)
	}
}
