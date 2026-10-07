package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 口令哈希参数。64 MiB 内存、3 轮迭代是当前较均衡的取值：
// 单次校验约几十毫秒，足以让离线爆破代价高昂，但登录延迟对用户无感。
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 3
	argonThreads   = 4
	argonKeyLen    = 32
	argonSaltLen   = 16
	// 解析数据库中的哈希时也限制参数上界。否则篡改数据库哈希即可让登录
	// 请求分配过量内存并长期占用 CPU。
	maxArgonMemoryKiB = 256 * 1024
	maxArgonTime      = 10
	maxArgonThreads   = 16
	maxArgonSaltLen   = 64
	maxArgonKeyLen    = 64

	// MinPasswordChars / MaxPasswordChars 约束密码的 ASCII 字符数。
	// 密码只接受 0x21-0x7e 的可打印 ASCII 字符：大小写英文、数字和符号，
	// 不包含空格、控制字符或其它 Unicode 字符。真正落库的永远是下面的慢哈希
	// 编码串，明文密码不进入存储层。
	MinPasswordChars = 8
	MaxPasswordChars = 32
)

var (
	// ErrInvalidCredentials 是统一的凭据错误：不区分"账号不存在"与"密码错误"，
	// 避免把账号存在性暴露给攻击者。
	ErrInvalidCredentials = errors.New("auth: 账号或密码错误")
	// ErrAccountDisabled 表示账号被禁用。
	ErrAccountDisabled = errors.New("auth: 账号已被禁用")
	// ErrPasswordTooShort 表示密码过短。
	ErrPasswordTooShort = errors.New("auth: 密码至少 8 个字符")
	// ErrPasswordTooLong 表示密码过长（上限同时避免超长输入拖垮哈希）。
	ErrPasswordTooLong = errors.New("auth: 密码最多 32 个字符")
	// ErrPasswordInvalidChars 表示密码包含不支持的字符。
	ErrPasswordInvalidChars = errors.New("auth: 密码只能使用大小写英文字母、数字和符号，不能包含空格")
	// ErrBadHashFormat 表示库中的哈希串格式无法识别。
	ErrBadHashFormat = errors.New("auth: 密码哈希格式无法识别")
)

// ValidatePassword 校验密码是否符合 8-32 个可打印 ASCII 字符的要求。
// 允许大小写英文字母、数字和 ASCII 符号，不允许空格或其它 Unicode 字符。
func ValidatePassword(pw string) error {
	if len(pw) < MinPasswordChars {
		return ErrPasswordTooShort
	}
	if len(pw) > MaxPasswordChars {
		return ErrPasswordTooLong
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] < 0x21 || pw[i] > 0x7e {
			return ErrPasswordInvalidChars
		}
	}
	return nil
}

// HashPassword 生成 PHC 格式的 argon2id 编码串：
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
//
// 盐随哈希一起存放，同一口令每次结果都不同，无法靠比对哈希值判断两个账号是否同口令。
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: 生成盐失败: %w", err)
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword 校验口令。比较使用恒定时间函数，避免通过响应时间推断哈希。
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, want, err := decodeArgonHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash 判断库中的哈希是否使用了当前参数。登录成功后若为真，
// 应当用当前参数重新哈希并落库。
func NeedsRehash(encoded string) bool {
	params, _, _, err := decodeArgonHash(encoded)
	if err != nil {
		return true
	}
	return params.memory != argonMemoryKiB || params.time != argonTime || params.threads != argonThreads
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeArgonHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(strings.TrimSpace(encoded), "$")
	// 形如 ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrBadHashFormat
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return argonParams{}, nil, nil, ErrBadHashFormat
	}
	var p argonParams
	for _, kv := range strings.Split(parts[3], ",") {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return argonParams{}, nil, nil, ErrBadHashFormat
		}
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return argonParams{}, nil, nil, ErrBadHashFormat
		}
		switch key {
		case "m":
			p.memory = uint32(n)
		case "t":
			p.time = uint32(n)
		case "p":
			if n > maxArgonThreads {
				return argonParams{}, nil, nil, ErrBadHashFormat
			}
			p.threads = uint8(n)
		default:
			return argonParams{}, nil, nil, ErrBadHashFormat
		}
	}
	if p.memory == 0 || p.memory > maxArgonMemoryKiB ||
		p.time == 0 || p.time > maxArgonTime ||
		p.threads == 0 || p.threads > maxArgonThreads {
		return argonParams{}, nil, nil, ErrBadHashFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 || len(salt) > maxArgonSaltLen {
		return argonParams{}, nil, nil, ErrBadHashFormat
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) == 0 || len(hash) > maxArgonKeyLen {
		return argonParams{}, nil, nil, ErrBadHashFormat
	}
	return p, salt, hash, nil
}

// dummyHash 是一个固定的合法哈希，用于账号不存在时消耗与真实校验相当的
// 时间。没有它，攻击者可以通过响应时间区分"账号不存在"与"密码错误"，
// 从而枚举出全部有效账号。
var dummyHash = func() string {
	sum := argon2.IDKey([]byte("dummy-password-for-timing"), []byte("dummy-salt-16byt"), argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString([]byte("dummy-salt-16byt")),
		base64.RawStdEncoding.EncodeToString(sum))
}()

// VerifyDummy 在账号不存在时执行一次等价开销的校验，抹平时间差。
func VerifyDummy(password string) {
	_, _ = VerifyPassword(dummyHash, password)
}

// HashSessionToken 计算会话令牌的存储形式。
//
// 令牌本身是 256 位随机值，不需要慢哈希，也不该用随机盐（因为要按值查找）。
// 用 SHA-256 即可：库被读走也拿不到可用的令牌。
func HashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashLookupSecret 计算需要按值查找的短凭据的存储形式（邀请码）。
//
// 这类凭据必须能"由明文反查"，因此不能用随机盐的慢哈希；改用带服务端
// pepper 的 HMAC，使得仅有数据库也无法离线枚举出短码。
func HashLookupSecret(pepper, secret string) string {
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(strings.ToUpper(strings.TrimSpace(secret))))
	return hex.EncodeToString(mac.Sum(nil))
}

// NewSessionToken 生成会话令牌。
func NewSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: 生成会话令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ParseBearer 从 Authorization 头解析 Bearer 会话令牌。
//
// 认证头只接受明确的 Bearer 方案。裸令牌不是 HTTP 认证协议的一部分，
// 继续接受它会让代理配置错误时把任意头内容当成凭据。
func ParseBearer(header string) string {
	header = strings.TrimSpace(header)
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(scheme), "Bearer") {
		return ""
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return ""
	}
	return token
}
