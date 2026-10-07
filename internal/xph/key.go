package xph

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
)

// info 常量用于从 KEK 派生用途互不重叠的密钥，避免同一把主密钥在不同场景下
// 派生出可以互相替代的密钥。
const (
	infoName    = "xph/object-name"
	nameSalt    = "xph/object-name-salt"
	namePrefixB = 10 // HMAC 截取字节数 → base32 后 16 字符
)

// NewDEK 生成一个每文件独立的内容密钥。
//
// 内容密钥随机而非由文件盐派生：这样密钥实体只有一个来源（数据库中的信封），
// 不需要在文件里再放一份派生参数，也便于将来按 keyId 轮换 KEK。
func NewDEK() ([]byte, error) { return randomBytes(KeySize) }

// NewHeaderRandom 生成带随机盐与前缀的文件头。
func NewHeaderRandom(plainSize int64, blockLog2 byte) (Header, error) {
	if blockLog2 < MinBlockLog2 || blockLog2 > MaxBlockLog2 {
		return Header{}, ErrBadBlockLog2
	}
	if plainSize < 0 || NewHeader(plainSize, blockLog2).CipherSize() < 0 {
		return Header{}, ErrBadPlainSize
	}
	salt, err := randomBytes(SaltSize)
	if err != nil {
		return Header{}, err
	}
	raw, err := randomBytes(4)
	if err != nil {
		return Header{}, err
	}
	h := NewHeader(plainSize, blockLog2)
	copy(h.FileSalt[:], salt)
	h.NoncePrefix = binary.BigEndian.Uint32(raw)
	return h, nil
}

// WrapDEK 用 KEK 包裹内容密钥，返回 60 字节信封（nonce ‖ 密文 ‖ tag）。
//
// 数据库里只放信封、不放明文 DEK：SQLite 文件泄露不应等价于全盘明文泄露。
func WrapDEK(kek, dek []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, ErrBadKeySize
	}
	if len(dek) != KeySize {
		return nil, ErrBadKeySize
	}
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	nonce, err := randomBytes(aead.NonceSize())
	if err != nil {
		return nil, err
	}
	envelope := make([]byte, 0, EnvelopeSize)
	envelope = append(envelope, nonce...)
	return aead.Seal(envelope, nonce, dek, nil), nil
}

// UnwrapDEK 解开信封取回内容密钥。
func UnwrapDEK(kek, envelope []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, ErrBadKeySize
	}
	if len(envelope) < EnvelopeSize {
		return nil, ErrBadEnvelope
	}
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	nonceLen := aead.NonceSize()
	// 保持布局可扩展：信封始终是 nonce ‖ 密文+tag。
	nonce := envelope[:nonceLen]
	dek, err := aead.Open(nil, nonce, envelope[nonceLen:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadEnvelope, err)
	}
	if len(dek) != KeySize {
		return nil, ErrBadEnvelope
	}
	return dek, nil
}

// DeriveObjectNameKey 从 KEK 派生对象命名密钥。
//
// 命名密钥只用于把校验码映射成不可逆的对象名前缀：既不向第三方暴露明文内容
// 指纹，又保留「由内容决定、可复现、天然防重名」的性质。
func DeriveObjectNameKey(kek []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, ErrBadKeySize
	}
	mac := hmac.New(sha256.New, kek)
	mac.Write([]byte(nameSalt))
	prk := mac.Sum(nil)

	mac = hmac.New(sha256.New, prk)
	mac.Write([]byte(infoName))
	mac.Write([]byte{1})
	return mac.Sum(nil), nil
}

// ObjectNamePrefix 返回对象名前缀：对校验码做 HMAC 后取 base32 编码的前 16 字符。
//
// 不使用裸校验码：对象名会落在第三方存储上，明文 SHA-256 前缀等于把内容指纹
// 交给对方。HMAC 版本同样确定性、同样防重名，但不泄露指纹。
func ObjectNamePrefix(nameKey []byte, checksum string) (string, error) {
	if len(nameKey) != KeySize {
		return "", ErrBadKeySize
	}
	if checksum == "" {
		return "", fmt.Errorf("xph: 校验码为空")
	}
	mac := hmac.New(sha256.New, nameKey)
	mac.Write([]byte(checksum))
	sum := mac.Sum(nil)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:namePrefixB])
	return encoded, nil
}

// ChunkEncrypt 加密单块，返回 密文‖tag。aad 必须与解密侧完全一致。
func ChunkEncrypt(dek, aad, nonce, plaintext []byte) ([]byte, error) {
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("xph: nonce 长度应为 %d", aead.NonceSize())
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

// ChunkDecrypt 解密单块，认证失败返回错误。
func ChunkDecrypt(dek, aad, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("xph: nonce 长度应为 %d", aead.NonceSize())
	}
	return aead.Open(nil, nonce, ciphertext, aad)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, ErrBadKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("xph: 构造 AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("xph: 构造 GCM: %w", err)
	}
	return aead, nil
}

func randomBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("xph: 读取随机数: %w", err)
	}
	return buf, nil
}

// RandomToken 生成 n 字节密码学随机数据的 base32 表示，用于对象名随机段与各类
// 不可枚举的标识符。
func RandomToken(n int) (string, error) {
	raw, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}
