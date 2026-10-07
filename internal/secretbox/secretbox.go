// Package secretbox 负责"落库的敏感配置值"的静态加密。
//
// 配置从环境变量搬进数据库后，123 的 ClientSecret、URL 鉴权私钥、KEK 集合都会
// 出现在 SQLite 文件里。原样存储等于"备份一份数据库"就是"泄露全部密钥"，比放
// 环境变量里更糟——数据库文件会被复制、被同步到对象存储、被开发机拉走。
//
// 因此敏感值统一加密后再入库，密钥来自部署侧的 master secret（见 config 包的
// 解析顺序）。这个 master secret 是唯一必须留在环境变量/本机文件里的秘密，
// 也让"把数据库交给别人"不再等于交出一切。
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// boxPrefix 是密文格式标识。密文把配置键名作为 AEAD 的关联数据写入
// 认证——持有数据库写权限的攻击者无法把 A 字段的密文移植到 B 字段冒充
// 合法值。
const boxPrefix = "enc:"

// 域分离：同一把 master secret 可能将来还要派生别的密钥。
const (
	deriveSalt = "xph/secretbox-salt"
	deriveInfo = "xph/secretbox"
)

// ErrCorrupted 表示密文损坏或密钥不匹配。
var ErrCorrupted = errors.New("secretbox: 密文损坏或 master secret 不匹配")

// Box 是加解密器。
type Box struct {
	aead cipher.AEAD
}

// New 由 master secret 构造。secret 长度不限，内部做 HKDF 派生。
func New(secret []byte) (*Box, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("secretbox: master secret 不得为空")
	}
	key := hkdfSHA256(secret, []byte(deriveSalt), []byte(deriveInfo), 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: 构造 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: 构造 GCM 失败: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal 加密一个值，aad 通常是配置键名，用于字段绑定。
// 空串保持空串——"未设置"与"设置成空"必须可区分。
func (b *Box) Seal(plain, aad string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: 生成 nonce 失败: %w", err)
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return boxPrefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open 解密。密文必须以写入时的同一 aad 通过认证；明文或非密文格式
// 直接视为损坏。
func (b *Box) Open(stored, aad string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, boxPrefix) {
		return "", ErrCorrupted
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, boxPrefix))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if len(raw) < b.aead.NonceSize() {
		return "", ErrCorrupted
	}
	nonce := raw[:b.aead.NonceSize()]
	plain, err := b.aead.Open(nil, nonce, raw[b.aead.NonceSize():], []byte(aad))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	return string(plain), nil
}

// Mask 生成用于展示的掩码。只保留首尾各 2 个字符：
// 管理员需要能分辨"是不是换过"，但不需要看到完整内容。
func Mask(plain string) string {
	if plain == "" {
		return ""
	}
	runes := []rune(plain)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:2]) + strings.Repeat("*", 6) + string(runes[len(runes)-2:])
}

// hkdfSHA256 实现 RFC 5869 的 Extract + Expand（单块输出）。
//
// 不引入外部依赖：这里只需要固定 32 字节输出，实现量很小，
// 而多一个依赖就多一份供应链面。
func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(secret)
	prk := mac.Sum(nil)

	out := make([]byte, 0, length+sha256.Size)
	var block []byte
	for i := byte(1); len(out) < length; i++ {
		mac = hmac.New(sha256.New, prk)
		mac.Write(block)
		mac.Write(info)
		mac.Write([]byte{i})
		block = mac.Sum(nil)
		out = append(out, block...)
	}
	return out[:length]
}
