package httpapi

// 带宽不足错误的 HTTP 映射测试：上传链路里的 backend.ErrUploadBandwidth
// 必须给出专属文案，而不是泛化的"服务暂时不可用"。

import (
	"errors"
	"fmt"
	"testing"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
)

func TestErrorStatusUploadBandwidth(t *testing.T) {
	err := fmt.Errorf("%w: 写入存储后端失败: %w", service.ErrUnavailable,
		fmt.Errorf("backend: 上传分片失败: %w", backend.ErrUploadBandwidth))
	status, message, _ := errorStatus(err)
	if status != 503 {
		t.Fatalf("应为 503，实得 %d", status)
	}
	if message != "服务器上传带宽过小，请联系管理员处理" {
		t.Fatalf("文案不符: %q", message)
	}
	if !errors.Is(err, service.ErrUnavailable) {
		t.Fatal("错误链应保留 ErrUnavailable")
	}
}
