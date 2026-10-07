package backend

// ErrUploadBandwidth 快速失败测试：带宽不足是部署侧问题、重试无法解决，
// 重试循环必须在第一次识别后立即停止，不能让用户白等 3 分钟。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadSliceFastFailsOnBandwidth(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "Read from request Body failed: i/o timeout", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newClassifyFixture(t, srv.URL)
	src := bytes.NewReader(bytes.Repeat([]byte{0}, 4096))
	start := time.Now()
	err := p.uploadSliceWithRetry(context.Background(), srv.URL, "preupload-1", 1, src, 0, 4096)
	if !IsUploadBandwidthError(err) {
		t.Fatalf("应返回带宽错误: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("带宽错误应立即终止、只请求 1 次，实得 %d 次（耗时 %s）", got, time.Since(start))
	}
}
