package mailparse

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseString(t *testing.T, raw string) (*Message, func()) {
	t.Helper()
	dir := t.TempDir()
	m, cleanup, err := Parse(strings.NewReader(raw), dir)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return m, cleanup
}

func TestParseAlternative(t *testing.T) {
	raw := "From: Alice <alice@example.com>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: hello\r\n" +
		"Message-ID: <m1@example.com>\r\n" +
		"Date: Wed, 24 Sep 2025 10:00:00 +0000\r\n" +
		"Content-Type: multipart/alternative; boundary=\"ALT\"\r\n\r\n" +
		"preamble\r\n--ALT\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nplain body\r\n--ALT\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n<p>html body</p>\r\n--ALT--\r\n"
	m, cleanup := parseString(t, raw)
	defer cleanup()
	if string(m.Text) != "plain body" {
		t.Fatalf("text = %q", m.Text)
	}
	if string(m.HTML) != "<p>html body</p>" {
		t.Fatalf("html = %q", m.HTML)
	}
	if len(m.Parts) != 0 {
		t.Fatalf("不应有附件，得到 %d", len(m.Parts))
	}
	if m.FromAddress != "alice@example.com" || m.FromName != "Alice" {
		t.Fatalf("From 解析错误: %q %q", m.FromName, m.FromAddress)
	}
	if m.MessageID != "m1@example.com" || m.SentAt.IsZero() {
		t.Fatalf("Message-ID/Date 错误: %q %v", m.MessageID, m.SentAt)
	}
}

func TestParseMixedAttachments(t *testing.T) {
	want1 := "hello attachment content"
	enc1 := base64.StdEncoding.EncodeToString([]byte(want1))
	want2 := "café qp" // = c a f é( C3 A9 ) space q p
	raw := "From: a@x.com\r\nSubject: mix\r\n" +
		"Content-Type: multipart/mixed; boundary=\"MIX\"\r\n\r\n" +
		"--MIX\r\nContent-Type: text/plain\r\n\r\nbody text\r\n--MIX\r\n" +
		"Content-Type: application/octet-stream; name=\"a.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\n" +
		enc1 + "\r\n--MIX\r\n" +
		"Content-Type: text/plain; name=\"b.txt\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Disposition: attachment\r\n\r\n" +
		"caf=C3=A9 qp\r\n--MIX--\r\n"
	m, cleanup := parseString(t, raw)
	defer cleanup()
	if string(m.Text) != "body text" {
		t.Fatalf("text = %q", m.Text)
	}
	if len(m.Parts) != 2 {
		t.Fatalf("附件数 = %d", len(m.Parts))
	}
	data1, err := os.ReadFile(m.Parts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data1) != want1 || m.Parts[0].Size != int64(len(want1)) {
		t.Fatalf("附件1 内容/大小错误: %q %d", data1, m.Parts[0].Size)
	}
	if m.Parts[0].FileName != "a.bin" || m.Parts[0].Kind != PartAttachment {
		t.Fatalf("附件1 元数据错误: %+v", m.Parts[0])
	}
	data2, err := os.ReadFile(m.Parts[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != want2 {
		t.Fatalf("附件2 qp 解码 = %q, 期望 %q", data2, want2)
	}
	for _, p := range m.Parts {
		if _, statErr := os.Stat(p.Path); statErr != nil {
			t.Fatalf("cleanup 前文件应存在: %v", statErr)
		}
	}
	cleanup()
	for _, p := range m.Parts {
		if _, statErr := os.Stat(p.Path); !os.IsNotExist(statErr) {
			t.Fatalf("cleanup 后临时文件应删除: %s", p.Path)
		}
	}
}

func TestParseRelatedInline(t *testing.T) {
	img := base64.StdEncoding.EncodeToString([]byte("PNGFAKE"))
	raw := "From: a@x.com\r\nSubject: rel\r\n" +
		"Content-Type: multipart/related; boundary=\"REL\"; type=\"text/html\"\r\n\r\n" +
		"--REL\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:logo@x\">\r\n--REL\r\n" +
		"Content-Type: image/png\r\nContent-Transfer-Encoding: base64\r\n" +
		"Content-ID: <logo@x>\r\n\r\n" + img + "\r\n--REL--\r\n"
	m, cleanup := parseString(t, raw)
	defer cleanup()
	if string(m.HTML) == "" {
		t.Fatal("HTML 正文缺失")
	}
	if len(m.Parts) != 1 || m.Parts[0].Kind != PartInline || m.Parts[0].ContentID != "logo@x" {
		t.Fatalf("内嵌图解析错误: %+v", m.Parts)
	}
}

func TestParseSingleBodies(t *testing.T) {
	for _, ct := range []string{"text/plain", "text/html"} {
		raw := "From: a@x.com\r\nContent-Type: " + ct + "\r\n\r\nsolo body"
		m, cleanup := parseString(t, raw)
		if ct == "text/plain" && string(m.Text) != "solo body" {
			t.Fatalf("纯文本段错误: %q", m.Text)
		}
		if ct == "text/html" && string(m.HTML) != "solo body" {
			t.Fatalf("纯 HTML 段错误: %q", m.HTML)
		}
		if len(m.Parts) != 0 {
			t.Fatalf("单段不应产生附件")
		}
		cleanup()
	}
}

func TestParseEncodedSubject(t *testing.T) {
	fromEnc := base64.StdEncoding.EncodeToString([]byte("测试"))
	subjEnc := base64.StdEncoding.EncodeToString([]byte("测试的信"))
	raw := "From: =?UTF-8?B?" + fromEnc + "?= <test@x.com>\r\n" +
		"Subject: =?UTF-8?B?" + subjEnc + "?=\r\n" +
		"Content-Type: text/plain\r\n\r\nhi"
	m, cleanup := parseString(t, raw)
	defer cleanup()
	if m.Subject != "测试的信" {
		t.Fatalf("主题解码 = %q", m.Subject)
	}
	if m.FromName != "测试" {
		t.Fatalf("发件人名解码 = %q", m.FromName)
	}
}

func TestParseMessageRFC822Attachment(t *testing.T) {
	nested := "From: nested@y.com\r\nSubject: nested\r\nContent-Type: text/plain\r\n\r\ninner"
	raw := "From: a@x.com\r\n" +
		"Content-Type: multipart/mixed; boundary=\"M\"\r\n\r\n--M\r\n" +
		"Content-Type: text/plain\r\n\r\nouter\r\n--M\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"Content-Disposition: attachment; filename=\"forward.eml\"\r\n\r\n" +
		nested + "\r\n--M--\r\n"
	m, cleanup := parseString(t, raw)
	defer cleanup()
	if len(m.Parts) != 1 {
		t.Fatalf("应得到 1 个转发附件，得到 %d", len(m.Parts))
	}
	if filepath.Base(m.Parts[0].FileName) != "forward.eml" {
		t.Fatalf("附件名 = %q", m.Parts[0].FileName)
	}
	data, err := os.ReadFile(m.Parts[0].Path)
	if err != nil || !strings.Contains(string(data), "inner") {
		t.Fatalf("转发附件内容错误: %v %q", err, data)
	}
}

func TestParseDeepNestingRejected(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("From: a@x.com\r\n")
	for i := 0; i < 34; i++ {
		sb.WriteString("Content-Type: multipart/mixed; boundary=\"BN")
		sb.WriteString(itoa(i))
		sb.WriteString("\"\r\n\r\npreamble\r\n--BN")
		sb.WriteString(itoa(i))
		sb.WriteString("\r\n")
	}
	sb.WriteString("Content-Type: text/plain\r\n\r\ndeep\r\n")
	for i := 33; i >= 0; i-- {
		sb.WriteString("\r\n--BN")
		sb.WriteString(itoa(i))
		sb.WriteString("--\r\n")
	}
	dir := t.TempDir()
	if _, _, err := Parse(strings.NewReader(sb.String()), dir); err == nil {
		t.Fatal("超过 32 层嵌套应报错")
	}
}

func TestParseThreadingHeaders(t *testing.T) {
	raw := "From: a@x.com\r\n" +
		"Message-ID: <cur@x>\r\nIn-Reply-To: <parent@x>\r\n" +
		"References: <root@x> <parent@x>\r\n" +
		"Content-Type: text/plain\r\n\r\nx"
	m, cleanup := parseString(t, raw)
	defer cleanup()
	if m.InReplyTo != "parent@x" {
		t.Fatalf("InReplyTo = %q", m.InReplyTo)
	}
	if m.ThreadRoot != "root@x" {
		t.Fatalf("ThreadRoot 应取 References 首项，得到 %q", m.ThreadRoot)
	}

	rawNoRef := strings.Replace(raw, "References: <root@x> <parent@x>\r\n", "", 1)
	m2, cleanup2 := parseString(t, rawNoRef)
	defer cleanup2()
	if m2.ThreadRoot != "parent@x" {
		t.Fatalf("无 References 时 ThreadRoot 应回退 In-Reply-To，得到 %q", m2.ThreadRoot)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
