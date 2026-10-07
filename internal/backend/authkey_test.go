package backend

import "testing"

// TestAuthKeyOfSignsEscapedPath：auth_key 的待签路径必须是 URL 的原始转义
// 形态——CDN 按字面路径验签，用解码后的路径参签会在含转义字符的路径上失败。
func TestAuthKeyOfSignsEscapedPath(t *testing.T) {
	const privateKey = "0123456789abcdef"
	const uid = uint64(1817754570)
	const ts = int64(1790170000)
	const randPart = "abcdef0123456789"

	got := authKeyOf("/2026/09/23/a%20b.xph", privateKey, uid, ts, randPart)
	want := "1790170000-abcdef0123456789-1817754570-a9fcd3612fa757d1433a44ed76feaa88"
	if got != want {
		t.Fatalf("auth_key 不匹配：got=%s want=%s", got, want)
	}
}
