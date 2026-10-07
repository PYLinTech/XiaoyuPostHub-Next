package xph

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
)

const testLog2 byte = 9 // 512 B 块，便于用小数据覆盖多块与残块

func newTestHeader(t *testing.T, plainSize int64) (Header, []byte) {
	t.Helper()
	hdr, err := NewHeaderRandom(plainSize, testLog2)
	if err != nil {
		t.Fatalf("生成文件头失败: %v", err)
	}
	dek, err := NewDEK()
	if err != nil {
		t.Fatalf("生成 DEK 失败: %v", err)
	}
	return hdr, dek
}

func deterministicPlain(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((i*31 + i/251) % 256)
	}
	return out
}

func TestCipherSizeFormula(t *testing.T) {
	bs := int64(1) << testLog2
	cases := []struct {
		plain    int64
		blocks   int64
		wantSize int64
	}{
		{0, 0, HeaderSize},
		{1, 1, HeaderSize + 1 + TagSize},
		{bs - 1, 1, HeaderSize + bs - 1 + TagSize},
		{bs, 1, HeaderSize + bs + TagSize},
		{bs + 1, 2, HeaderSize + bs + 1 + 2*TagSize},
		{3*bs + 7, 4, HeaderSize + 3*bs + 7 + 4*TagSize},
	}
	for _, tc := range cases {
		hdr, _ := newTestHeader(t, tc.plain)
		if got := hdr.BlockCount(); got != tc.blocks {
			t.Errorf("plain=%d 块数=%d，期望 %d", tc.plain, got, tc.blocks)
		}
		if got := hdr.CipherSize(); got != tc.wantSize {
			t.Errorf("plain=%d 密文长度=%d，期望 %d", tc.plain, got, tc.wantSize)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	bs := int64(1) << testLog2
	sizes := []int{0, 1, 100, int(bs) - 1, int(bs), int(bs) + 1, int(3*bs + 7), int(10 * bs)}
	for _, size := range sizes {
		plain := deterministicPlain(size)
		hdr, dek := newTestHeader(t, int64(size))

		cipher, err := EncryptAll(plain, dek, hdr)
		if err != nil {
			t.Fatalf("size=%d 加密失败: %v", size, err)
		}
		if int64(len(cipher)) != hdr.CipherSize() {
			t.Fatalf("size=%d 密文长度 %d，期望 %d", size, len(cipher), hdr.CipherSize())
		}
		got, err := DecryptAll(cipher, dek)
		if err != nil {
			t.Fatalf("size=%d 解密失败: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size=%d 往返不一致", size)
		}
	}
}

func TestHeaderIsSelfDescribing(t *testing.T) {
	plain := deterministicPlain(2000)
	hdr, dek := newTestHeader(t, int64(len(plain)))
	cipher, err := EncryptAll(plain, dek, hdr)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	parsed, err := ParseHeader(cipher)
	if err != nil {
		t.Fatalf("解析文件头失败: %v", err)
	}
	if parsed.PlainSize != int64(len(plain)) {
		t.Errorf("明文长度=%d，期望 %d", parsed.PlainSize, len(plain))
	}
	if parsed.BlockLog2 != testLog2 {
		t.Errorf("块大小指数=%d，期望 %d", parsed.BlockLog2, testLog2)
	}
	if parsed.FileSalt != hdr.FileSalt || parsed.NoncePrefix != hdr.NoncePrefix {
		t.Errorf("盐或 nonce 前缀未随文件头保留")
	}
	if err := parsed.VerifyCipherSize(int64(len(cipher))); err != nil {
		t.Errorf("密文长度自校验失败: %v", err)
	}
	if err := parsed.VerifyCipherSize(int64(len(cipher)) + TagSize); err == nil {
		t.Errorf("长度被篡改时应报错")
	}
}

// TestDecryptSeekMatchesFullDecrypt 覆盖首/中/尾/跨块等随机区间，
// 断言范围解密结果与整体解密后切片逐字节一致。
func TestDecryptSeekMatchesFullDecrypt(t *testing.T) {
	const size = 5000
	plain := deterministicPlain(size)
	hdr, dek := newTestHeader(t, size)
	cipher, err := EncryptAll(plain, dek, hdr)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	open := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		if offset < 0 || offset > int64(len(cipher)) {
			t.Fatalf("区间越界: offset=%d", offset)
		}
		end := int64(len(cipher))
		if length > 0 && offset+length < end {
			end = offset + length
		}
		return io.NopCloser(bytes.NewReader(cipher[offset:end])), nil
	}

	bs := int64(1) << testLog2
	starts := []int64{0, 1, bs - 1, bs, bs + 1, 2*bs + 3, int64(size) - 1}
	lengths := []int64{1, 10, bs, 2*bs + 5, 0, int64(size)}

	for _, s := range starts {
		for _, l := range lengths {
			rc, err := DecryptSeek(context.Background(), open, hdr, dek, s, l)
			if err != nil {
				t.Fatalf("seek(%d,%d) 失败: %v", s, l, err)
			}
			got, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatalf("seek(%d,%d) 读取失败: %v", s, l, err)
			}
			wantEnd := int64(size)
			if l > 0 && s+l < wantEnd {
				wantEnd = s + l
			}
			want := plain[s:wantEnd]
			if !bytes.Equal(got, want) {
				t.Fatalf("seek(%d,%d) 得到 %d 字节，期望 %d 字节，内容不一致", s, l, len(got), len(want))
			}
		}
	}
}

func TestDecryptSeekEmptyFile(t *testing.T) {
	hdr, dek := newTestHeader(t, 0)
	cipher, err := EncryptAll(nil, dek, hdr)
	if err != nil {
		t.Fatalf("加密空文件失败: %v", err)
	}
	if int64(len(cipher)) != HeaderSize {
		t.Fatalf("空文件密文长度=%d，期望 %d", len(cipher), HeaderSize)
	}
	open := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(cipher)), nil
	}
	rc, err := DecryptSeek(context.Background(), open, hdr, dek, 0, 0)
	if err != nil {
		t.Fatalf("空文件 seek 失败: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("空文件读取失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空文件应读出 0 字节，实得 %d", len(got))
	}
}

func TestWrongKeyFails(t *testing.T) {
	plain := deterministicPlain(1200)
	hdr, dek := newTestHeader(t, int64(len(plain)))
	cipher, err := EncryptAll(plain, dek, hdr)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	other, err := NewDEK()
	if err != nil {
		t.Fatalf("生成 DEK 失败: %v", err)
	}
	if _, err := DecryptAll(cipher, other); err == nil {
		t.Fatal("使用错误密钥应解密失败")
	}
}

// TestTamperedBodyFails 篡改任何一个密文字节都必须导致认证失败，
// 而不是产生部分正确的明文。
func TestTamperedBodyFails(t *testing.T) {
	plain := deterministicPlain(1200)
	hdr, dek := newTestHeader(t, int64(len(plain)))
	cipher, err := EncryptAll(plain, dek, hdr)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	for _, pos := range []int{HeaderSize, HeaderSize + 5, len(cipher) - 1, HeaderSize + 600} {
		bad := append([]byte(nil), cipher...)
		bad[pos] ^= 0x40
		if _, err := DecryptAll(bad, dek); err == nil {
			t.Fatalf("篡改偏移 %d 后仍解密成功", pos)
		}
	}
}

// TestTamperedHeaderFails 文件头参与 AAD，因此改动头部字段会让解密认证失败，
// 而不是解析出错后产生难以归因的行为。
func TestTamperedHeaderFails(t *testing.T) {
	plain := deterministicPlain(1200)
	hdr, dek := newTestHeader(t, int64(len(plain)))
	cipher, err := EncryptAll(plain, dek, hdr)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	for _, pos := range []int{28, 36, 12} {
		bad := append([]byte(nil), cipher...)
		bad[pos] ^= 0x01
		h2, parseErr := ParseHeader(bad)
		if parseErr != nil {
			// 解析阶段就拒绝也算正确处置。
			continue
		}
		if _, err := DecryptAll(bad, dek); err == nil {
			t.Fatalf("篡改头部偏移 %d 后仍解密成功（plainSize=%d）", pos, h2.PlainSize)
		}
	}
}

func TestEncryptorRejectsExtraData(t *testing.T) {
	hdr, dek := newTestHeader(t, 100)
	enc, err := NewEncryptor(bytes.NewReader(deterministicPlain(101)), dek, hdr)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}
	if _, err := io.ReadAll(enc); err == nil {
		t.Fatal("来源多出数据时应报错")
	}
}

func TestEncryptorRejectsShortData(t *testing.T) {
	hdr, dek := newTestHeader(t, 100)
	enc, err := NewEncryptor(bytes.NewReader(deterministicPlain(99)), dek, hdr)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}
	if _, err := io.ReadAll(enc); err == nil {
		t.Fatal("来源数据不足时应报错")
	}
}

func TestChunkRangeAlignment(t *testing.T) {
	hdr, _ := newTestHeader(t, 5000)
	bs := hdr.BlockSize()

	first, last, start, end, skip, err := hdr.ChunkRange(bs+10, 5)
	if err != nil {
		t.Fatalf("计算区间失败: %v", err)
	}
	if first != 1 || last != 1 || skip != 10 {
		t.Fatalf("单块区间解算错误: first=%d last=%d skip=%d", first, last, skip)
	}
	if start != hdr.BlockCipherOffset(1) || end != start+hdr.BlockCipherLen(1) {
		t.Fatalf("密文区间未按块对齐: [%d,%d)", start, end)
	}

	first, last, _, _, skip, err = hdr.ChunkRange(bs-1, 3)
	if err != nil {
		t.Fatalf("计算区间失败: %v", err)
	}
	if first != 0 || last != 1 || skip != bs-1 {
		t.Fatalf("跨块区间解算错误: first=%d last=%d skip=%d", first, last, skip)
	}
}

func TestChunkRangeRejectsOverflowingGeometry(t *testing.T) {
	hdr := NewHeader(int64(^uint64(0)>>1), testLog2)
	if _, _, _, _, _, err := hdr.ChunkRange(0, 1); err == nil {
		t.Fatal("溢出的 XPH 几何应被拒绝")
	}
	if got := hdr.CipherSize(); got >= 0 {
		t.Fatalf("溢出的密文长度应返回失败哨兵，实得 %d", got)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	kek := make([]byte, KeySize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("生成 KEK 失败: %v", err)
	}
	dek, err := NewDEK()
	if err != nil {
		t.Fatalf("生成 DEK 失败: %v", err)
	}
	env, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("包裹 DEK 失败: %v", err)
	}
	if len(env) != EnvelopeSize {
		t.Fatalf("信封长度=%d，期望 %d", len(env), EnvelopeSize)
	}
	got, err := UnwrapDEK(kek, env)
	if err != nil {
		t.Fatalf("解开信封失败: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("解出的 DEK 与原始不一致")
	}

	otherKek := append([]byte(nil), kek...)
	otherKek[0] ^= 0xFF
	if _, err := UnwrapDEK(otherKek, env); err == nil {
		t.Fatal("使用错误 KEK 应解封失败")
	}
	broken := append([]byte(nil), env...)
	broken[len(broken)-1] ^= 0x01
	if _, err := UnwrapDEK(kek, broken); err == nil {
		t.Fatal("信封被篡改应解封失败")
	}
}

func TestObjectNamePrefixIsDeterministicAndOpaque(t *testing.T) {
	kek := make([]byte, KeySize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("生成 KEK 失败: %v", err)
	}
	nameKey, err := DeriveObjectNameKey(kek)
	if err != nil {
		t.Fatalf("派生命名密钥失败: %v", err)
	}
	checksum := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	a, err := ObjectNamePrefix(nameKey, checksum)
	if err != nil {
		t.Fatalf("生成对象名前缀失败: %v", err)
	}
	b, _ := ObjectNamePrefix(nameKey, checksum)
	if a != b {
		t.Fatalf("命名应确定性，得到 %q 与 %q", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("前缀长度=%d，期望 16", len(a))
	}
	if bytes.Contains([]byte(a), []byte(checksum[:8])) {
		t.Fatal("对象名前缀不得包含明文校验码片段")
	}
	c, _ := ObjectNamePrefix(nameKey, checksum[:len(checksum)-1]+"9")
	if c == a {
		t.Fatal("不同校验码应得到不同前缀")
	}
}

func BenchmarkEncryptor1MiB(b *testing.B) {
	plain := deterministicPlain(1 << 20)
	hdr, err := NewHeaderRandom(int64(len(plain)), DefaultBlockLog2)
	if err != nil {
		b.Fatal(err)
	}
	dek, err := NewDEK()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(plain)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc, err := NewEncryptor(bytes.NewReader(plain), dek, hdr)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, enc); err != nil {
			b.Fatal(err)
		}
	}
}
