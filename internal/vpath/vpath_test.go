package vpath

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	ok := []struct {
		in   string
		want string
	}{
		{"/", "/"},
		{"/a", "/a"},
		{"/a/b/c", "/a/b/c"},
		{"/中文目录/文件.txt", "/中文目录/文件.txt"},
		{"/a b/c d", "/a b/c d"},
	}
	for _, tc := range ok {
		got, err := Normalize(tc.in)
		if err != nil {
			t.Errorf("Normalize(%q) 失败: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Normalize(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"", "a", "a/b", "/a/", "/a//b", "/a/./b", "/a/../b", "/..", "/.",
		"/a/\x00b", "/a/\tb", "/a/\nb", "/a/\u00a0b", "/a/\u3000b",
		"/a/trailing ",
		"/a/" + strings.Repeat("x", MaxNameBytes+1),
		"/" + strings.Repeat("a/", MaxSegments+1) + "a",
		"/" + strings.Repeat("x", MaxPathBytes),
	}
	for _, in := range bad {
		if _, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) 应报错", in)
		}
	}
}

// TestTraversalIsRejected 路径穿越必须在入口被拒绝，而不是靠下游拼接时侥幸。
func TestTraversalIsRejected(t *testing.T) {
	for _, in := range []string{
		"/a/../../etc/passwd",
		"/../etc/passwd",
		"/a/b/../../..",
	} {
		if _, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) 应拒绝路径穿越", in)
		}
	}
	// 系统里不存在"先解码再拼路径"的步骤，因此字面量 %2e%2e 只是普通文件名。
	// 这里固化这个语义：它必须被当作名字接受，而不是被当成穿越——否则会出现
	// "合法文件名被拒" 与 "穿越被放行" 两种相反的错误理解。
	if _, err := Normalize("/a/%2e%2e/b"); err != nil {
		t.Errorf("字面量 %%2e%%2e 是合法文件名，不应被当作穿越拒绝: %v", err)
	}
}

func TestJoinAndParts(t *testing.T) {
	cases := []struct {
		parent string
		name   string
		want   string
	}{
		{"/", "a", "/a"},
		{"/a", "b", "/a/b"},
		{"/a/b", "c.txt", "/a/b/c.txt"},
	}
	for _, tc := range cases {
		got, err := Join(tc.parent, tc.name)
		if err != nil {
			t.Errorf("Join(%q,%q) 失败: %v", tc.parent, tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Join(%q,%q) = %q，期望 %q", tc.parent, tc.name, got, tc.want)
		}
		if Parent(got) != tc.parent {
			t.Errorf("Parent(%q) = %q，期望 %q", got, Parent(got), tc.parent)
		}
		if Base(got) != tc.name {
			t.Errorf("Base(%q) = %q，期望 %q", got, Base(got), tc.name)
		}
	}
	if Parent(Root) != "" {
		t.Errorf("根目录的父路径应为空串")
	}
	if Base(Root) != "" {
		t.Errorf("根目录的末段名应为空串")
	}
}

// TestIsAncestorBoundary 前缀判定必须带分隔符：/a/b 不能匹配到 /a/bc。
// 这是分享与目录权限最容易出错的地方。
func TestIsAncestorBoundary(t *testing.T) {
	cases := []struct {
		ancestor string
		p        string
		want     bool
	}{
		{"/", "/a", true},
		{"/", "/", true},
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a", "/a/b/c", true},
		{"/a", "/ab", false},
		{"/a", "/a-b", false},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a", false},
		{"", "/a", false},
	}
	for _, tc := range cases {
		if got := IsAncestor(tc.ancestor, tc.p); got != tc.want {
			t.Errorf("IsAncestor(%q,%q) = %v，期望 %v", tc.ancestor, tc.p, got, tc.want)
		}
	}
}

func TestIsWithinExcludesSelf(t *testing.T) {
	if IsWithin("/a", "/a") {
		t.Error("IsWithin 应排除自身")
	}
	if !IsWithin("/a", "/a/b") {
		t.Error("IsWithin 应包含后代")
	}
	if IsWithin("/a", "/ab") {
		t.Error("IsWithin 不得把同前缀的兄弟路径算作后代")
	}
}

func TestValidateName(t *testing.T) {
	good := []string{"a.txt", "中文名", "a b", "a-b_c.d", "🎬影片"}
	for _, n := range good {
		if _, err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) 应通过: %v", n, err)
		}
	}
	bad := []string{"", ".", "..", "a/b", "a\nb", "a\x00b", "a\tb", " 前后有空格 ", "尾部空格 ", "__xph"}
	for _, n := range bad {
		if _, err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) 应拒绝", n)
		}
	}
}

func TestErrorsAreDistinguishable(t *testing.T) {
	if _, err := Normalize(""); !errors.Is(err, ErrEmpty) {
		t.Errorf("空路径应返回 ErrEmpty，实得 %v", err)
	}
	if _, err := Normalize("a/b"); !errors.Is(err, ErrNotAbsolute) {
		t.Errorf("相对路径应返回 ErrNotAbsolute，实得 %v", err)
	}
	if _, err := Normalize("/a/../b"); !errors.Is(err, ErrBadSegment) {
		t.Errorf("穿越段应返回 ErrBadSegment，实得 %v", err)
	}
}
