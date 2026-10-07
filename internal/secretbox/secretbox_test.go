package secretbox

import "testing"

const testSecret = "unit-test-master-secret"
const testPlain = "hello-secret"

func mustBox(t *testing.T) *Box {
	t.Helper()
	box, err := New([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// TestAADRoundTrip：密文带字段绑定，同字段加解密闭环。
func TestAADRoundTrip(t *testing.T) {
	box := mustBox(t)
	sealed, err := box.Seal(testPlain, "pan123.client_secret")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := box.Open(sealed, "pan123.client_secret"); err != nil || got != testPlain {
		t.Fatalf("同字段解密应成功，实得 %q err=%v", got, err)
	}
}

// TestAADBindsField：字段绑定必须生效——把 A 字段的密文移植到 B 字段
// （数据库写权限攻击者的拿手好戏）必须在解密时被 AEAD 拒绝。
func TestAADBindsField(t *testing.T) {
	box := mustBox(t)
	sealed, err := box.Seal(testPlain, "pan123.client_secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Open(sealed, "pan123.private_key"); err == nil {
		t.Fatal("跨字段移植的密文必须解密失败")
	}
	if _, err := box.Open(sealed, ""); err == nil {
		t.Fatal("空 AAD 的解密必须失败")
	}
}

// TestRejectsForeignFormat：明文与任何非密文格式都必须被拒绝。
func TestRejectsForeignFormat(t *testing.T) {
	box := mustBox(t)
	for _, stored := range []string{testPlain, "encx:Zm9vYmFy", "not-a-ciphertext"} {
		if _, err := box.Open(stored, "pan123.client_secret"); err == nil {
			t.Fatalf("非密文格式 %q 必须解密失败", stored)
		}
	}
}

// TestSealFormat：新写入必须带密文格式前缀。
func TestSealFormat(t *testing.T) {
	box := mustBox(t)
	sealed, err := box.Seal(testPlain, "pan123.client_secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) < len(boxPrefix) || sealed[:len(boxPrefix)] != boxPrefix {
		t.Fatalf("新写入应以 %s 开头，实得 %q", boxPrefix, sealed[:min(10, len(sealed))])
	}
}
