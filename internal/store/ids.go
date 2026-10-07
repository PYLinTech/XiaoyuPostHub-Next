package store

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// tokenEncoding 用无填充的 base32：大小写不敏感的场景下不会因为大小写转换出错，
// 也不含容易被误读的符号。
var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateToken 生成 n 字节随机数据的 base32 字符串。
func GenerateToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成随机令牌失败: %w", err)
	}
	return tokenEncoding.EncodeToString(raw), nil
}

// NewID 生成带前缀的资源标识（公告、上传会话、分享等）。
func NewID(prefix string) (string, error) {
	token, err := GenerateToken(12)
	if err != nil {
		return "", err
	}
	if prefix == "" {
		return strings.ToLower(token), nil
	}
	return prefix + "_" + strings.ToLower(token), nil
}

// pickupAlphabet 取件码字符集：6 位字母数字组合，大小写不区分（存储与查询
// 统一归一化为大写），并剔除全部易混字符：
//   - 数字 0/1/9：分别与字母 O、I/L、q/g 形近；
//   - 字母 O/o、I/i、L/l：与 0、1 形近；
//   - 字母 Q/q、G/g：与 9 及彼此形近。
//
// 即数字保留 2-8（7 个），大写字母保留 A-Z 去掉 G/I/L/O/Q（21 个），
// 共 28 个规范字符；用户输入小写时按大写等价处理。
const pickupAlphabet = "2345678ABCDEFHJKMNPRSTUVWXYZ"

// PickupCodeLength 是取件码的固定位数：28^6 ≈ 4.82 亿组合，配合尝试次数
// 限制，在线猜测不可行，同时保留口头转述与手抄的便利。
const PickupCodeLength = 6

// PickupCodePoolSize 是全局取件码码空间的总容量（28^6 ≈ 4.82 亿）。
//
// 取件码是全站共用的随机短码池而不是每分享独立：有效期窗口内存活的码达到
// 该数量即视为码池用尽，生成必须明确失败并提示用户，而不是陷入无界的
// 主键碰撞重试。len 作用于字符串常量，是编译期常量表达式。
const PickupCodePoolSize int64 = int64(len(pickupAlphabet)) *
	int64(len(pickupAlphabet)) * int64(len(pickupAlphabet)) *
	int64(len(pickupAlphabet)) * int64(len(pickupAlphabet)) *
	int64(len(pickupAlphabet))

// randomCode 从取件码字符集里取 length 个字符。使用拒绝采样消除字节取模偏差，
// 让每个字符的概率完全一致。
func randomCode(length int, what string) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("生成%s失败：长度必须大于 0", what)
	}
	out := make([]byte, length)
	limit := 256 - (256 % len(pickupAlphabet))
	buf := make([]byte, length)
	for written := 0; written < length; {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("生成%s失败: %w", what, err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out[written] = pickupAlphabet[int(b)%len(pickupAlphabet)]
			written++
			if written == length {
				break
			}
		}
	}
	return string(out), nil
}

// NewInviteCode 生成注册邀请码。
//
// 比取件码长得多：邀请码一旦泄露就意味着一批不受控的账号被创建，而它的使用
// 频次远低于取件码，长一些不影响体验。
func NewInviteCode() (string, error) {
	return randomCode(12, "邀请码")
}

// NewPickupCode 生成短取件码。
//
// 短码是凭据，因此必须配合尝试次数限制使用；业务层使用 6 位码——28^6 约
// 4.82 亿组合，叠加退避后在线猜测不可行，同时保留口头转述的便利。
func NewPickupCode(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("取件码长度必须大于 0")
	}
	return randomCode(length, "取件码")
}
