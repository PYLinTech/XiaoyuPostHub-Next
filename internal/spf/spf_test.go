package spf

import (
	"context"
	"errors"
	"net"
	"testing"
)

type fakeResolver struct {
	txt map[string][]string
	ips map[string][]net.IP
	mxs map[string][]string
	// err 对所有查询返回临时错误。
	err error
}

func (f *fakeResolver) LookupTXT(_ context.Context, domain string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.txt[domain], nil
}

func (f *fakeResolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ips[host], nil
}

func (f *fakeResolver) LookupMXHosts(_ context.Context, domain string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.mxs[domain], nil
}

func ip4(s string) net.IP { return net.ParseIP(s) }

func TestSPFAllQualifiers(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"hard.example": {"v=spf1 -all"},
		"soft.example": {"v=spf1 ~all"},
		"neu.example":  {"v=spf1 ?all"},
		"pass.example": {"v=spf1 +all"},
		"bare.example": {"v=spf1 all"},
	}}
	c := NewChecker(r)
	cases := []struct {
		domain string
		want   Result
	}{
		{"hard.example", Fail}, {"soft.example", SoftFail},
		{"neu.example", Neutral}, {"pass.example", Pass}, {"bare.example", Pass},
	}
	for _, tc := range cases {
		got, _ := c.Check(context.Background(), tc.domain, ip4("1.2.3.4"))
		if got != tc.want {
			t.Fatalf("%s = %s，期望 %s", tc.domain, got, tc.want)
		}
	}
}

func TestSPFIPLiterals(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"v4.example":  {"v=spf1 ip4:192.0.2.0/24 -all"},
		"v6.example":  {"v=spf1 ip6:2001:db8::/32 -all"},
		"bad.example": {"v=spf1 ip4:not-an-ip -all"},
	}}
	c := NewChecker(r)
	if got, _ := c.Check(context.Background(), "v4.example", ip4("192.0.2.55")); got != Pass {
		t.Fatalf("v4 网段内应 pass，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "v4.example", ip4("192.0.3.1")); got != Fail {
		t.Fatalf("v4 网段外应 fail，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "v6.example", net.ParseIP("2001:db8::1234")); got != Pass {
		t.Fatalf("v6 网段内应 pass，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "v6.example", net.ParseIP("2001:db9::1")); got != Fail {
		t.Fatalf("v6 网段外应 fail，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "bad.example", ip4("1.1.1.1")); got != PermError {
		t.Fatalf("畸形 ip4 应 permerror，得到 %s", got)
	}
}

func TestSPFAMechanism(t *testing.T) {
	r := &fakeResolver{
		txt: map[string][]string{"a.example": {"v=spf1 a:mail.example.com/24 -all"}},
		ips: map[string][]net.IP{"mail.example.com": {ip4("10.20.30.40")}},
	}
	c := NewChecker(r)
	if got, _ := c.Check(context.Background(), "a.example", ip4("10.20.30.99")); got != Pass {
		t.Fatalf("a 机制 /24 内应 pass，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "a.example", ip4("10.20.31.1")); got != Fail {
		t.Fatalf("a 机制 /24 外应 fail，得到 %s", got)
	}
}

func TestSPFMXMechanism(t *testing.T) {
	r := &fakeResolver{
		txt: map[string][]string{"mx.example": {"v=spf1 mx -all"}},
		mxs: map[string][]string{"mx.example": {"mx1.mail.example", "mx2.mail.example"}},
		ips: map[string][]net.IP{
			"mx1.mail.example": {ip4("172.16.0.1")},
			"mx2.mail.example": {ip4("172.16.0.2")},
		},
	}
	c := NewChecker(r)
	if got, _ := c.Check(context.Background(), "mx.example", ip4("172.16.0.2")); got != Pass {
		t.Fatalf("mx 主机 IP 应 pass，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "mx.example", ip4("8.8.8.8")); got != Fail {
		t.Fatalf("非 MX 主机应 fail，得到 %s", got)
	}
}

func TestSPFInclude(t *testing.T) {
	r := &fakeResolver{
		txt: map[string][]string{
			"top.example":  {"v=spf1 include:good.third -all"},
			"good.third":   {"v=spf1 ip4:203.0.113.0/24 -all"},
			"soft.third":   {"v=spf1 ~all"},
			"top2.example": {"v=spf1 include:soft.third -all"},
		},
	}
	c := NewChecker(r)
	if got, _ := c.Check(context.Background(), "top.example", ip4("203.0.113.7")); got != Pass {
		t.Fatalf("include 内 pass 应穿透，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "top.example", ip4("203.0.114.7")); got != Fail {
		t.Fatalf("include 内外都不匹配时取外层 -all，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "top2.example", ip4("1.1.1.1")); got != Fail {
		t.Fatalf("include softfail 不应命中，应继续到外层 -all，得到 %s", got)
	}
}

func TestSPFIncludeDepth(t *testing.T) {
	txt := map[string][]string{}
	for i := 0; i < 12; i++ {
		domain := "lvl" + itoa(i) + ".example"
		if i == 11 {
			txt[domain] = []string{"v=spf1 -all"}
			continue
		}
		txt[domain] = []string{"v=spf1 include:lvl" + itoa(i+1) + ".example -all"}
	}
	c := NewChecker(&fakeResolver{txt: txt})
	got, _ := c.Check(context.Background(), "lvl0.example", ip4("1.1.1.1"))
	if got != PermError {
		t.Fatalf("include 深度超 10 应 permerror，得到 %s", got)
	}
}

func TestSPFRedirect(t *testing.T) {
	r := &fakeResolver{
		txt: map[string][]string{
			"use.example":     {"v=spf1 redirect=target.example"},
			"target.example":  {"v=spf1 ip4:198.51.100.0/24 -all"},
			"withall.example": {"v=spf1 -all redirect=target.example"},
			"badred.example":  {"v=spf1 redirect=void.example"},
		},
	}
	c := NewChecker(r)
	if got, _ := c.Check(context.Background(), "use.example", ip4("198.51.100.5")); got != Pass {
		t.Fatalf("redirect 目标 pass 应生效，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "use.example", ip4("1.1.1.1")); got != Fail {
		t.Fatalf("redirect 目标 fail 应生效，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "withall.example", ip4("198.51.100.5")); got != Fail {
		t.Fatalf("存在 all 时 redirect 不应生效，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "badred.example", ip4("1.1.1.1")); got != PermError {
		t.Fatalf("redirect 到无记录域应 permerror，得到 %s", got)
	}
}

func TestSPFNoneAndDuplicateAndTemp(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"dup.example": {"v=spf1 -all", "v=spf1 ~all"},
	}}
	c := NewChecker(r)
	if got, _ := c.Check(context.Background(), "void.example", ip4("1.1.1.1")); got != None {
		t.Fatalf("无 TXT 应 none，得到 %s", got)
	}
	if got, _ := c.Check(context.Background(), "dup.example", ip4("1.1.1.1")); got != PermError {
		t.Fatalf("多条 SPF 记录应 permerror，得到 %s", got)
	}
	c2 := NewChecker(&fakeResolver{err: errors.New("dns timeout")})
	if got, _ := c2.Check(context.Background(), "x.example", ip4("1.1.1.1")); got != TempError {
		t.Fatalf("DNS 临时错误应 temperror，得到 %s", got)
	}
}

func TestSPFQueryAndVoidLimits(t *testing.T) {
	// 11 个 a 机制：第 11 次查询触发 permerror。
	txt := map[string][]string{}
	ips := map[string][]net.IP{}
	record := "v=spf1 "
	for i := 0; i < 11; i++ {
		host := "h" + itoa(i) + ".example"
		record += "a:" + host + " "
		ips[host] = []net.IP{ip4("10.0." + itoa(i) + ".1")}
	}
	record += "-all"
	txt["many.example"] = []string{record}
	c := NewChecker(&fakeResolver{txt: txt, ips: ips})
	if got, _ := c.Check(context.Background(), "many.example", ip4("9.9.9.9")); got != PermError {
		t.Fatalf("DNS 查询超 10 次应 permerror，得到 %s", got)
	}

	// 3 个 a 机制全部空回答 → void 超 2 → permerror。
	txt2 := map[string][]string{
		"void3.example": {"v=spf1 a:v0.example a:v1.example a:v2.example -all"},
	}
	c2 := NewChecker(&fakeResolver{txt: txt2})
	if got, _ := c2.Check(context.Background(), "void3.example", ip4("9.9.9.9")); got != PermError {
		t.Fatalf("void 查询超 2 次应 permerror，得到 %s", got)
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
