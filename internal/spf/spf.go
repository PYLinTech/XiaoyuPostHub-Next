// Package spf 实现 RFC 7208 的最小可用子集，用于入站 SMTP 的发件策略：
//
//   - 机制：all、ip4、ip6、a、mx、include、redirect；
//   - 限定符：+（pass，缺省）、-（fail）、~（softfail）、?（neutral）；
//   - 防护：机制类 DNS 查询计数 ≤10、空回答（void）≤2、include 深度 ≤10；
//   - 不实现：ptr、exists、exp（一期策略不需要；ptr 也被 RFC 不建议使用）。
//
// DNS 解析走 Resolver 接口，默认用 net.DefaultResolver；测试用假解析器。
package spf

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
)

// Result 是 SPF 求值结果词。
type Result string

const (
	Pass      Result = "pass"
	Fail      Result = "fail"
	SoftFail  Result = "softfail"
	Neutral   Result = "neutral"
	None      Result = "none"      // 无 SPF 记录
	TempError Result = "temperror" // DNS 临时故障，建议 4xx
	PermError Result = "permerror" // 记录畸形 / 超出防护计数
)

// ErrNXDOMAIN 表示域名确定不存在（区别于临时 DNS 故障）。
var ErrNXDOMAIN = errors.New("spf: NXDOMAIN")

// Resolver 是求值需要的全部 DNS 能力。
type Resolver interface {
	// LookupTXT 返回域名的各条 TXT 记录（多字符串记录应已拼接）。
	LookupTXT(ctx context.Context, domain string) ([]string, error)
	// LookupIP 返回主机的 A + AAAA 地址。
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
	// LookupMXHosts 返回 MX 交换主机名（已去末尾点）。
	LookupMXHosts(ctx context.Context, domain string) ([]string, error)
}

// 防护阈值（RFC 7208 §4.6.4）。
const (
	maxDNSQueries   = 10
	maxVoidLookups  = 2
	maxIncludeDepth = 10
	// mx 机制最多解析的 MX 主机数；超出部分忽略。
	maxMXHosts = 10
)

// Checker 是 SPF 求值器。
type Checker struct {
	res Resolver
}

// NewChecker 构造求值器。r 为 nil 时使用 net.DefaultResolver。
func NewChecker(r Resolver) *Checker {
	if r == nil {
		r = netResolver{}
	}
	return &Checker{res: r}
}

// Check 对 (domain, 客户端 IP) 求值。domain 取信封发件人域，
// 空反向路径时取 EHLO 域。
func (c *Checker) Check(ctx context.Context, domain string, ip net.IP) (Result, error) {
	domain = normalizeDomain(domain)
	if domain == "" || ip == nil {
		return PermError, nil
	}
	cnt := &counters{}
	return c.eval(ctx, domain, ip, 0, cnt), nil
}

type counters struct {
	dns  int
	void int
}

// useDNS 登记一次机制类 DNS 查询；超计数返回 false。
func (c *counters) useDNS() bool {
	c.dns++
	return c.dns <= maxDNSQueries
}

// addVoid 登记一次空回答；超限返回 false。
func (c *counters) addVoid() bool {
	c.void++
	return c.void <= maxVoidLookups
}

func (c *Checker) eval(ctx context.Context, domain string, ip net.IP, depth int, cnt *counters) Result {
	if depth > maxIncludeDepth {
		return PermError
	}
	txts, err := c.res.LookupTXT(ctx, domain)
	if err != nil {
		if errors.Is(err, ErrNXDOMAIN) {
			return None
		}
		return TempError
	}
	records := spfRecords(txts)
	if len(records) == 0 {
		return None
	}
	if len(records) > 1 {
		return PermError
	}

	redirect := ""
	for _, tok := range strings.Fields(records[0]) {
		if tok == "v=spf1" {
			continue
		}
		qual, body := splitQualifier(tok)
		// 修饰符（name=value）：redirect 留到所有机制未命中后使用，exp 忽略。
		if name, value, ok := strings.Cut(body, "="); ok && !strings.Contains(name, ":") {
			if name == "redirect" {
				redirect = value
			}
			continue
		}
		name, arg, _ := strings.Cut(body, ":")
		switch strings.ToLower(name) {
		case "all":
			return qual
		case "ip4":
			match, bad := c.matchLiteralIP(arg, ip, false)
			if bad {
				return PermError
			}
			if match {
				return qual
			}
		case "ip6":
			match, bad := c.matchLiteralIP(arg, ip, true)
			if bad {
				return PermError
			}
			if match {
				return qual
			}
		case "a":
			r := c.checkA(ctx, arg, domain, ip, cnt)
			if r == Pass || r == TempError || r == PermError {
				if r == Pass {
					return qual
				}
				return r
			}
		case "mx":
			r := c.checkMX(ctx, arg, domain, ip, cnt)
			if r == Pass || r == TempError || r == PermError {
				if r == Pass {
					return qual
				}
				return r
			}
		case "include":
			if !cnt.useDNS() {
				return PermError
			}
			switch c.eval(ctx, normalizeDomain(arg), ip, depth+1, cnt) {
			case Pass:
				return qual
			case TempError:
				return TempError
			case PermError:
				return PermError
			}
			// fail / softfail / neutral / none 都不命中，继续。
		default:
			// ptr / exists / 未知机制：忽略。
		}
	}

	if redirect != "" {
		if !cnt.useDNS() {
			return PermError
		}
		r := c.eval(ctx, normalizeDomain(redirect), ip, depth+1, cnt)
		if r == None {
			// 重定向到没有 SPF 记录的域，按 RFC 属于 permerror。
			return PermError
		}
		return r
	}
	return Neutral
}

// matchLiteralIP 判定 ip4:/ip6: 字面量（可带前缀长度）。
// bad=true 表示机制书写畸形（应判 permerror）。
func (c *Checker) matchLiteralIP(arg string, client net.IP, wantV6 bool) (match, bad bool) {
	host, bits, hasBits := strings.Cut(arg, "/")
	ip := net.ParseIP(host)
	if ip == nil {
		return false, true
	}
	if !hasBits {
		if wantV6 {
			bits = "128"
		} else {
			bits = "32"
		}
	}
	prefixBits, convErr := strconv.Atoi(bits)
	if convErr != nil {
		return false, true
	}
	if wantV6 {
		v6 := ip.To16()
		if v6 == nil || strings.Contains(host, ".") || prefixBits < 0 || prefixBits > 128 {
			return false, true
		}
		clientV6 := client.To16()
		if clientV6 == nil || client.To4() != nil {
			return false, false
		}
		return cidrmatch(v6, clientV6, prefixBits), false
	}
	v4 := ip.To4()
	if v4 == nil || prefixBits < 0 || prefixBits > 32 {
		return false, true
	}
	clientV4 := client.To4()
	if clientV4 == nil {
		return false, false
	}
	return cidrmatch(v4, clientV4, prefixBits), false
}

// checkA 求值 a[:domain][/v4-bits][/v6-bits]。返回 Pass/Neutral/TempError/PermError。
func (c *Checker) checkA(ctx context.Context, arg, curDomain string, client net.IP, cnt *counters) Result {
	host, v4Bits, v6Bits, ok := parseDomainDual(arg, curDomain)
	if !ok {
		return PermError
	}
	if !cnt.useDNS() {
		return PermError
	}
	ips, err := c.res.LookupIP(ctx, host)
	if err != nil {
		if errors.Is(err, ErrNXDOMAIN) {
			if !cnt.addVoid() {
				return PermError
			}
			return Neutral
		}
		return TempError
	}
	if len(ips) == 0 {
		if !cnt.addVoid() {
			return PermError
		}
		return Neutral
	}
	if ipInRecords(ips, client, v4Bits, v6Bits) {
		return Pass
	}
	return Neutral
}

// checkMX 求值 mx[:domain][/dual-cidr]：解析 MX 主机集合再逐个比对 A/AAAA。
func (c *Checker) checkMX(ctx context.Context, arg, curDomain string, client net.IP, cnt *counters) Result {
	target, v4Bits, v6Bits, ok := parseDomainDual(arg, curDomain)
	if !ok {
		return PermError
	}
	if !cnt.useDNS() {
		return PermError
	}
	hosts, err := c.res.LookupMXHosts(ctx, target)
	if err != nil {
		if errors.Is(err, ErrNXDOMAIN) {
			if !cnt.addVoid() {
				return PermError
			}
			return Neutral
		}
		return TempError
	}
	if len(hosts) == 0 {
		if !cnt.addVoid() {
			return PermError
		}
		return Neutral
	}
	if len(hosts) > maxMXHosts {
		hosts = hosts[:maxMXHosts]
	}
	for _, h := range hosts {
		h = normalizeDomain(h)
		if h == "" {
			continue
		}
		if !cnt.useDNS() {
			return PermError
		}
		ips, ipErr := c.res.LookupIP(ctx, h)
		if ipErr != nil {
			if errors.Is(ipErr, ErrNXDOMAIN) {
				if !cnt.addVoid() {
					return PermError
				}
				continue
			}
			return TempError
		}
		if len(ips) == 0 {
			if !cnt.addVoid() {
				return PermError
			}
			continue
		}
		if ipInRecords(ips, client, v4Bits, v6Bits) {
			return Pass
		}
	}
	return Neutral
}

// parseDomainDual 解析 "domain/v4bits/v6bits" 形态的机制参数。
// arg 为空时使用当前 SPF 域；位长缺省 32/128。
func parseDomainDual(arg, curDomain string) (host string, v4Bits, v6Bits int, ok bool) {
	v4Bits, v6Bits = 32, 128
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return curDomain, v4Bits, v6Bits, true
	}
	if strings.HasPrefix(arg, "/") {
		arg = curDomain + arg
	}
	parts := strings.Split(arg, "/")
	if len(parts) > 3 || parts[0] == "" {
		return "", 0, 0, false
	}
	host = normalizeDomain(parts[0])
	if len(parts) >= 2 {
		n, err := strconv.Atoi(parts[1])
		if err != nil || n < 0 || n > 32 {
			return "", 0, 0, false
		}
		v4Bits = n
	}
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 0 || n > 128 {
			return "", 0, 0, false
		}
		v6Bits = n
	}
	return host, v4Bits, v6Bits, true
}

func ipInRecords(ips []net.IP, client net.IP, v4Bits, v6Bits int) bool {
	clientV4 := client.To4()
	for _, rec := range ips {
		if recV4 := rec.To4(); recV4 != nil && clientV4 != nil {
			if cidrmatch(recV4, clientV4, v4Bits) {
				return true
			}
			continue
		}
		recV6 := rec.To16()
		clientV6 := client.To16()
		if recV6 != nil && clientV6 != nil && clientV4 == nil {
			if cidrmatch(recV6, clientV6, v6Bits) {
				return true
			}
		}
	}
	return false
}

// cidrmatch 用前 n 位比较两个等长 IP 字节。
func cidrmatch(network, addr net.IP, bits int) bool {
	if len(network) != len(addr) {
		return false
	}
	full := bits / 8
	for i := 0; i < full; i++ {
		if network[i] != addr[i] {
			return false
		}
	}
	if rem := bits % 8; rem != 0 && full < len(network) {
		mask := byte(0xff << (8 - rem))
		if network[full]&mask != addr[full]&mask {
			return false
		}
	}
	return true
}

// splitQualifier 取下机制限定符；缺省为 +（Pass）。
func splitQualifier(tok string) (qual Result, body string) {
	if len(tok) == 0 {
		return Neutral, tok
	}
	body = tok
	switch tok[0] {
	case '+':
		qual, body = Pass, tok[1:]
	case '-':
		qual, body = Fail, tok[1:]
	case '~':
		qual, body = SoftFail, tok[1:]
	case '?':
		qual, body = Neutral, tok[1:]
	default:
		qual = Pass
	}
	return qual, body
}

// spfRecords 从 TXT 集合中挑出去重后的 SPF 记录（首字段为 v=spf1）。
func spfRecords(txts []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 1)
	for _, t := range txts {
		fields := strings.Fields(t)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "v=spf1") {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func normalizeDomain(d string) string {
	d = strings.TrimSpace(strings.ToLower(d))
	return strings.TrimSuffix(d, ".")
}
