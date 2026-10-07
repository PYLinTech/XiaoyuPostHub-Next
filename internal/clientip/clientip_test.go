package clientip

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func req(remote string, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote + ":12345"
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func mustParse(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	prefixes, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return prefixes
}

// TestNoSpoofWhenBehindTrustedProxy 覆盖本包的核心保证：不在可信网段内的
// 直连客户端，无论怎么伪造转发头都拿不到自选 IP。
func TestNoSpoofWhenBehindTrustedProxy(t *testing.T) {
	trusted := mustParse(t, "127.0.0.1/32,::1/128")

	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"无头时用对端", "10.0.0.5", "", "10.0.0.5"},
		{"伪造单条目被无视", "10.0.0.5", "9.9.9.9", "10.0.0.5"},
		{"伪造多条目被无视", "10.0.0.5", "9.9.9.9, 8.8.8.8", "10.0.0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(req(tc.remote, tc.xff), trusted)
			if got.String() != tc.want {
				t.Fatalf("客户端 IP = %s，期望 %s", got, tc.want)
			}
		})
	}
}

// TestMalformedEntryDoesNotHandOutSpoofedIP 是本次修复的核心回归用例。
//
// 追加语义下，受信代理会把观测到的真实对端追加到链尾。若解析方向是
// 左→右并在遇到畸形条目时停止，攻击者只要塞一个无法解析的条目，就能让
// 代理追加的真实地址连同其右侧全部被丢弃，剩下的唯一条目就是他自己写的
// 伪造 IP。该 IP 会被用于登录限流、访客配额与下载票据的 IP 绑定。
func TestMalformedEntryDoesNotHandOutSpoofedIP(t *testing.T) {
	trusted := mustParse(t, "10.0.0.0/8")

	// 攻击者发送 "9.9.9.9, unknown"，受信代理把观测到的真实对端 203.0.113.7
	// 追加到链尾 → 完整链 "9.9.9.9, unknown, 203.0.113.7"。
	//
	// 旧的左→右解析在 "unknown" 处 break，把右侧由代理追加的 203.0.113.7
	// 一起丢掉，只剩攻击者自己写的 9.9.9.9 —— 直接把伪造 IP 交了出去。
	got := Extract(req("10.0.0.1", "9.9.9.9, unknown, 203.0.113.7"), trusted)
	if got.String() == "9.9.9.9" {
		t.Fatal("畸形条目右侧的可信条目被丢弃，结果变成了攻击者伪造的 IP")
	}
	if got.String() != "203.0.113.7" {
		t.Fatalf("应取代理观测到的真实对端 203.0.113.7，实际 %s", got)
	}

	// 畸形条目在伪造值右侧时同样安全：右→左扫描遇到畸形即停，
	// 伪造值连同其左侧全部丢弃。
	got = Extract(req("10.0.0.1", "203.0.113.7, unknown, 9.9.9.9"), trusted)
	if got.String() != "9.9.9.9" {
		t.Fatalf("畸形条目左侧必须丢弃，期望 9.9.9.9，实际 %s", got)
	}
}

// TestRightmostUntrustedStillWins 确认正常链路语义没被改坏。
func TestRightmostUntrustedStillWins(t *testing.T) {
	trusted := mustParse(t, "10.0.0.0/8")
	// 两个可信代理依次追加：client → 10.0.0.1 → 10.0.0.2
	// 本服务对端是 10.0.0.2，最右端不可信条目是 9.9.9.9。
	if got := Extract(req("10.0.0.2", "9.9.9.9, 10.0.0.1"), trusted).String(); got != "9.9.9.9" {
		t.Fatalf("应取最右端不可信条目 9.9.9.9，实际 %s", got)
	}
	// 全链可信：取最左端。
	if got := Extract(req("10.0.0.2", "10.0.0.5, 10.0.0.1"), trusted).String(); got != "10.0.0.5" {
		t.Fatalf("全链可信时应取最左端 10.0.0.5，实际 %s", got)
	}
}
