// Package vpath 负责用户逻辑路径的规范化与比较。
//
// 全部路径操作必须走这里，不允许在别处拼接字符串：路径既是主键又是权限判定
// 的依据，任何一处规则不一致都会造成越权或数据错位。
//
// 规则：
//   - 一律以 "/" 开头；根目录就是 "/"，不带末尾斜杠
//   - 段之间单个 "/"，拒绝空段（即禁止 "//"）
//   - 拒绝控制字符与 "."、".." 段
//   - 统一 Unicode NFC 归一化，避免视觉相同但字节不同的重名
//   - 大小写敏感
package vpath

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Root 是根目录。
const Root = "/"

// 路径长度限制。双重限制（全路径 + 单段）是必需的：只限总量会让深层目录出现
// 超长段，只限单段则会让总长度失控。
const (
	MaxPathBytes   = 1024
	MaxNameBytes   = 255
	MaxSegments    = 64
	separator      = "/"
	controlRuneMax = 0x1F
)

var (
	ErrEmpty        = errors.New("vpath: 路径为空")
	ErrNotAbsolute  = errors.New("vpath: 路径必须以 / 开头")
	ErrBadSegment   = errors.New("vpath: 路径包含非法段")
	ErrTooLong      = errors.New("vpath: 路径或名称超长")
	ErrTooDeep      = errors.New("vpath: 目录层级过深")
	ErrBadName      = errors.New("vpath: 名称非法")
	ErrReservedName = errors.New("vpath: 名称为系统保留")
)

// Normalize 校验并规范化一个逻辑路径。
func Normalize(p string) (string, error) {
	if p == "" {
		return "", ErrEmpty
	}
	if !utf8.ValidString(p) {
		return "", ErrBadSegment
	}
	if !strings.HasPrefix(p, separator) {
		return "", ErrNotAbsolute
	}
	p = norm.NFC.String(p)
	if len(p) > MaxPathBytes {
		return "", ErrTooLong
	}
	if p == Root {
		return Root, nil
	}
	if strings.HasSuffix(p, separator) {
		return "", ErrBadSegment
	}
	segs := strings.Split(p[1:], separator)
	if len(segs) > MaxSegments {
		return "", ErrTooDeep
	}
	for _, seg := range segs {
		if err := validateSegment(seg); err != nil {
			return "", err
		}
	}
	return p, nil
}

// ValidateName 校验单个名称（不含分隔符）。
func ValidateName(name string) (string, error) {
	if name == "" {
		return "", ErrEmpty
	}
	if !utf8.ValidString(name) {
		return "", ErrBadName
	}
	name = norm.NFC.String(name)
	if err := validateSegment(name); err != nil {
		return "", err
	}
	return name, nil
}

func validateSegment(seg string) error {
	if seg == "" {
		return ErrBadSegment
	}
	if seg == "." || seg == ".." {
		return ErrBadSegment
	}
	if len(seg) > MaxNameBytes {
		return ErrTooLong
	}
	if strings.Contains(seg, separator) {
		return ErrBadSegment
	}
	for _, r := range seg {
		if r == utf8.RuneError {
			return ErrBadSegment
		}
		if r <= controlRuneMax || r == 0x7F {
			return ErrBadSegment
		}
		if unicode.IsSpace(r) && r != ' ' {
			// 制表/换行/不换行空格等一律拒绝：它们在不同终端与文件系统上
			// 显示一致但字节不同，是重名与钓鱼的常见来源。
			return ErrBadSegment
		}
	}
	if strings.TrimSpace(seg) == "" || seg != strings.TrimRight(seg, " ") {
		return ErrBadName
	}
	if strings.EqualFold(seg, "__xph") {
		return ErrReservedName
	}
	return nil
}

// Join 把名称拼到父路径下。
func Join(parent, name string) (string, error) {
	p, err := Normalize(parent)
	if err != nil {
		return "", err
	}
	n, err := ValidateName(name)
	if err != nil {
		return "", err
	}
	if p == Root {
		return Root + n, nil
	}
	full := p + separator + n
	if len(full) > MaxPathBytes {
		return "", ErrTooLong
	}
	return full, nil
}

// Parent 返回父路径；根目录的父路径是空串。
func Parent(p string) string {
	if p == Root || p == "" {
		return ""
	}
	idx := strings.LastIndex(p, separator)
	if idx <= 0 {
		return Root
	}
	return p[:idx]
}

// Base 返回末段名；根目录返回空串。
func Base(p string) string {
	if p == Root || p == "" {
		return ""
	}
	if idx := strings.LastIndex(p, separator); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// IsAncestor 判断 ancestor 是否是 p 的祖先（含自身）。
//
// 必须按"前缀 + 分隔符"判定，不能裸用 HasPrefix：否则 "/a/b" 会匹配到
// 用户不该访问的 "/a/bc"。
func IsAncestor(ancestor, p string) bool {
	if ancestor == Root {
		return true
	}
	if ancestor == "" {
		return false
	}
	if p == ancestor {
		return true
	}
	return strings.HasPrefix(p, ancestor+separator)
}

// IsWithin 判断 p 是否在 root 子树内且不等于 root。
func IsWithin(root, p string) bool {
	return root != p && IsAncestor(root, p)
}
