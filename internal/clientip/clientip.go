// Package clientip 负责从请求中还原真实客户端地址。
//
// 这是安全相关的关键环节：访客配额、风控退避、下载票据的 IP 绑定全部建立在
// 它之上。不加可信代理白名单就直接采信 X-Forwarded-For，等于让任何客户端
// 自由伪造自己的身份从而绕过全部按 IP 的限制。
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const headerForwardedFor = "X-Forwarded-For"

// Extract 返回发起请求的客户端地址。
//
// trusted 是可信代理网段：只有当 TCP 对端落在其中时才采信请求头的转发链。
// 取值从链的最右端起，跳过所有落在可信网段内的地址，第一个不可信地址即真实
// 来源，因此客户端自己伪造的左侧条目不会被采信。
//
// 无法解析时返回零值 Addr，调用方应按"未知来源"处理（例如不限流但要记日志）。
func Extract(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := peerAddr(r)
	if !peer.IsValid() {
		return netip.Addr{}
	}
	if !isTrusted(peer, trusted) {
		return peer.Unmap()
	}

	chain := forwardedChain(r)
	// 从右往左找第一个不可信地址。
	for i := len(chain) - 1; i >= 0; i-- {
		if !isTrusted(chain[i], trusted) {
			return chain[i].Unmap()
		}
	}
	// 整条链都在可信网段内：取最左端作为原始来源。
	if len(chain) > 0 {
		return chain[0].Unmap()
	}
	return peer.Unmap()
}

func peerAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

// forwardedChain 解析转发链，按"从右往左、遇到第一个无法解析的条目即停"的
// 规则截断。
//
// 方向必须是右→左：追加语义下（每跳代理都往末尾追加自己的观测），最右端
// 是最靠近本服务的基础设施写的，最左侧才是客户端可控的部分。左→右解析
// 并在遇到畸形条目时 break，会把最右侧那些由可信代理追加的条目一起丢掉，
// 只剩客户端自己塞的前缀——等于把伪造 IP 交给了调用方。
func forwardedChain(r *http.Request) []netip.Addr {
	raw := r.Header.Get(headerForwardedFor)
	if strings.TrimSpace(raw) == "" {
		// 部分代理只设置 X-Real-IP。
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			if addr, err := netip.ParseAddr(real); err == nil {
				return []netip.Addr{addr}
			}
		}
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]netip.Addr, 0, len(parts))
	// 从最右端往左扫，第一个解析不了的条目及其左侧全部丢弃。
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		// 条目可能带端口（"1.2.3.4:5678"）。
		if host, _, err := net.SplitHostPort(part); err == nil {
			part = host
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			break
		}
		out = append(out, addr)
	}
	// 反转回左→右，保持"左端最不可信"的既有语义。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

// Prefix 返回用于绑定与计量的地址前缀。
//
// 绑定精确地址会让移动网络切换（4G↔Wi-Fi）直接失效，也会让同一 NAT 下的
// 多台设备各自"重置换算额度"。按前缀聚合是两者之间的合理折中。
func Prefix(addr netip.Addr, v4Bits, v6Bits int) string {
	if !addr.IsValid() {
		return "unknown"
	}
	addr = addr.Unmap()
	bits := v4Bits
	if addr.Is6() {
		bits = v6Bits
	}
	if bits <= 0 {
		return "unknown"
	}
	if bits > addr.BitLen() {
		bits = addr.BitLen()
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return addr.String()
	}
	return p.String()
}

// Parse 解析配置里的地址或网段，便于构造可信代理列表。
func Parse(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}
