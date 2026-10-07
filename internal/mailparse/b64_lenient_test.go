package mailparse

import (
	"bytes"
	"encoding/base64"
	"io"
	"testing"
)

func TestLenientBase64MatchesStandard(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("hello world"),
		[]byte("a"),
		[]byte("ab"),
		[]byte("abc"),
		[]byte("中文内容测试"),
		bytes.Repeat([]byte("x"), 5000),
		{},
	} {
		enc := base64.StdEncoding.EncodeToString(raw)
		got, err := io.ReadAll(newLenientBase64Reader(bytes.NewReader([]byte(enc))))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("mismatch: got %d bytes, want %d", len(got), len(raw))
		}
	}
}

func TestLenientBase64SkipsCorruption(t *testing.T) {
	raw := []byte("hello world, this is a test payload")
	enc := base64.StdEncoding.EncodeToString(raw)
	// 注入非法字符与换行，不应中止解码。
	dirty := []byte("\n" + enc[:10] + "!!!???" + enc[10:40] + "\r\n  " + enc[40:] + "\n")
	got, err := io.ReadAll(newLenientBase64Reader(bytes.NewReader(dirty)))
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("跳过非法字符后应还原原文: got %q", got)
	}
	// 对照：标准解码器在这种输入上会失败，正是本改动要避免的丢信来源。
	if _, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(dirty))); err == nil {
		t.Fatal("对照用例失效：标准解码器本应在此输入上报错")
	}
}

func TestLenientBase64TruncatedTail(t *testing.T) {
	raw := []byte("truncated payload here")
	enc := base64.StdEncoding.EncodeToString(raw)
	// 裁掉末尾 2 个字符：应还原出可用前缀，而不是整段报错。
	cut := enc[:len(enc)-2]
	got, err := io.ReadAll(newLenientBase64Reader(bytes.NewReader([]byte(cut))))
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if len(got) < len(raw)-2 {
		t.Fatalf("被截断时应还原可用前缀: got %d bytes, want >= %d", len(got), len(raw)-2)
	}
	if !bytes.Equal(got, raw[:len(got)]) {
		t.Fatalf("前缀内容不匹配: got %q", got)
	}
}
