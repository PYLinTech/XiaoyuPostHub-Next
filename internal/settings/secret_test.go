package settings

import (
	"strings"
	"testing"
)

// TestSealOpenSecretWithAAD 覆盖敏感值的静态加解密：库里必须是密文，
// 且密文与 AAD 绑定，不能挪到别的行上复用。
func TestSealOpenSecretWithAAD(t *testing.T) {
	st, _ := newTestStore(t)

	if got, err := st.SealSecret("", "aad"); err != nil || got != "" {
		t.Fatalf("空明文应透传空串: %q %v", got, err)
	}
	if got, err := st.OpenSecret("", "aad"); err != nil || got != "" {
		t.Fatalf("空密文应透传空串: %q %v", got, err)
	}

	sealed, err := st.SealSecret("pan123-secret", "pan123.client_secret")
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("密文应带 enc: 前缀: %q", sealed)
	}
	if sealed == "pan123-secret" {
		t.Fatal("密文不得等于明文")
	}
	plain, err := st.OpenSecret(sealed, "pan123.client_secret")
	if err != nil || plain != "pan123-secret" {
		t.Fatalf("OpenSecret 往返失败: %q %v", plain, err)
	}
	// AAD 绑定用途：换一个 AAD 打不开，防止密文被挪到别的配置行复用。
	if _, err := st.OpenSecret(sealed, "pan123.private_key"); err == nil {
		t.Fatal("AAD 不匹配时应解密失败")
	}
}
