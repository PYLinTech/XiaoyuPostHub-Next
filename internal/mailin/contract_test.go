package mailin

import (
	"errors"
	"fmt"
	"testing"
)

func TestStatusError(t *testing.T) {
	se := Reject(550, "no such user")
	if se.Code != 550 || se.Error() != "no such user" {
		t.Fatalf("StatusError 构造错误: %+v", se)
	}

	wrapped := fmt.Errorf("落信失败: %w", se)
	if got := AsStatusError(wrapped); got != se {
		t.Fatalf("单链包装应取回状态错误，得到 %v", got)
	}

	joined := errors.Join(se, errors.New("回退配额失败"))
	if got := AsStatusError(joined); got == nil || got.Code != 550 {
		t.Fatalf("errors.Join 链应取回状态错误，得到 %v", got)
	}

	if AsStatusError(errors.New("普通错误")) != nil {
		t.Fatal("普通错误不应取回状态错误")
	}
	if AsStatusError(nil) != nil {
		t.Fatal("nil 不应取回状态错误")
	}
}
