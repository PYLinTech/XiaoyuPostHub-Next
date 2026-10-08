// Package mailresource provides a tightly bounded fetcher for external resources
// referenced by a user's email. It is intentionally not a general-purpose proxy.
package mailresource

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	maxRedirects      = 3
	maxConcurrent     = 8
	maxRequestsMinute = 60
)

type Resource struct {
	Body        []byte
	ContentType string
	SourceURL   string
}

type userWindow struct {
	start time.Time
	count int
}

type batchKey struct {
	userID int64
	id     string
}

type batchUsage struct {
	updated    time.Time
	itemLimit  int64
	totalLimit int64
	consumed   int64
	reserved   int64
}

type reservation struct {
	gateway  *Gateway
	key      batchKey
	reserved int64
}

// Gateway limits outbound concurrency and per-user request volume. It never
// forwards cookies, authorization headers, or referrers to the resource host.
type Gateway struct {
	sem              chan struct{}
	mu               sync.Mutex
	perUser          map[int64]userWindow
	batches          map[batchKey]batchUsage
	lastUserCleanup  time.Time
	lastBatchCleanup time.Time
}

func NewGateway() *Gateway {
	return &Gateway{
		sem:     make(chan struct{}, maxConcurrent),
		perUser: make(map[int64]userWindow),
		batches: make(map[batchKey]batchUsage),
	}
}

func (g *Gateway) Fetch(ctx context.Context, userID int64, batchID, rawURL, kind string, maxItemBytes, maxTotalBytes int64) (Resource, error) {
	if err := g.allowUser(userID); err != nil {
		return Resource{}, err
	}
	if len(batchID) < 16 || len(batchID) > 64 || maxItemBytes <= 0 || maxTotalBytes <= 0 {
		return Resource{}, fmt.Errorf("mail resource: 代理批次或大小上限无效")
	}
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		return Resource{}, ctx.Err()
	}

	current, err := validateURL(rawURL)
	if err != nil {
		return Resource{}, err
	}
	itemLimit := min(maxItemBytes, maxTotalBytes)
	reservation, err := g.reserve(userID, batchID, itemLimit, maxTotalBytes)
	if err != nil {
		return Resource{}, err
	}
	consumed := int64(0)
	defer func() { reservation.finish(consumed) }()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           safeDialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       10 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for redirects := 0; ; redirects++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return Resource{}, fmt.Errorf("mail resource: 构造请求失败")
		}
		req.Header.Set("Accept", acceptForKind(kind))
		req.Header.Set("User-Agent", "XiaoyuPostHub-MailResource/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return Resource{}, fmt.Errorf("mail resource: 外部资源请求失败")
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if location == "" || redirects >= maxRedirects {
				return Resource{}, fmt.Errorf("mail resource: 外部资源重定向次数超限或目标无效")
			}
			redirect, err := current.Parse(location)
			if err != nil {
				return Resource{}, fmt.Errorf("mail resource: 外部资源重定向地址无效")
			}
			current, err = validateURL(redirect.String())
			if err != nil {
				return Resource{}, fmt.Errorf("mail resource: 外部资源重定向目标被拒绝")
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			return Resource{}, fmt.Errorf("mail resource: 外部资源返回 HTTP %d", resp.StatusCode)
		}
		if resp.ContentLength > reservation.reserved {
			_ = resp.Body.Close()
			return Resource{}, fmt.Errorf("mail resource: 外部资源超过本批次剩余上限")
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, reservation.reserved))
		consumed = int64(len(body))
		_ = resp.Body.Close()
		if err != nil {
			return Resource{}, fmt.Errorf("mail resource: 读取外部资源失败")
		}
		if int64(len(body)) == reservation.reserved && resp.ContentLength != reservation.reserved {
			return Resource{}, fmt.Errorf("mail resource: 外部资源超过本批次剩余上限")
		}
		if int64(len(body)) > maxItemBytes {
			return Resource{}, fmt.Errorf("mail resource: 外部资源超过单项上限")
		}
		contentType, err := safeContentType(resp.Header.Get("Content-Type"), current, kind, body)
		if err != nil {
			return Resource{}, err
		}
		return Resource{Body: body, ContentType: contentType, SourceURL: current.String()}, nil
	}
}

func (g *Gateway) reserve(userID int64, batchID string, itemLimit, totalLimit int64) (*reservation, error) {
	now := time.Now()
	key := batchKey{userID: userID, id: batchID}
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.lastBatchCleanup) >= time.Minute {
		for id, usage := range g.batches {
			if usage.reserved == 0 && now.Sub(usage.updated) >= 10*time.Minute {
				delete(g.batches, id)
			}
		}
		g.lastBatchCleanup = now
	}
	usage := g.batches[key]
	if usage.totalLimit == 0 || totalLimit < usage.totalLimit {
		usage.totalLimit = totalLimit
	}
	if usage.itemLimit == 0 || itemLimit < usage.itemLimit {
		usage.itemLimit = itemLimit
	}
	available := usage.totalLimit - usage.consumed - usage.reserved
	if available <= 0 {
		return nil, fmt.Errorf("mail resource: 本次确认的资源总量已达到上限")
	}
	amount := min(usage.itemLimit, available)
	usage.updated = now
	usage.reserved += amount
	g.batches[key] = usage
	return &reservation{gateway: g, key: key, reserved: amount}, nil
}

func (r *reservation) finish(consumed int64) {
	if r == nil || r.gateway == nil {
		return
	}
	if consumed < 0 {
		consumed = 0
	}
	if consumed > r.reserved {
		consumed = r.reserved
	}
	r.gateway.mu.Lock()
	usage := r.gateway.batches[r.key]
	usage.reserved -= r.reserved
	if usage.reserved < 0 {
		usage.reserved = 0
	}
	usage.consumed += consumed
	usage.updated = time.Now()
	r.gateway.batches[r.key] = usage
	r.gateway.mu.Unlock()
}

func (g *Gateway) allowUser(userID int64) error {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.lastUserCleanup) >= time.Minute {
		for id, window := range g.perUser {
			if now.Sub(window.start) >= time.Minute {
				delete(g.perUser, id)
			}
		}
		g.lastUserCleanup = now
	}
	window := g.perUser[userID]
	if now.Sub(window.start) >= time.Minute || window.start.IsZero() {
		window = userWindow{start: now}
	}
	if window.count >= maxRequestsMinute {
		return fmt.Errorf("mail resource: 每用户每分钟最多代理 %d 个资源", maxRequestsMinute)
	}
	window.count++
	g.perUser[userID] = window
	return nil
}

func validateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" {
		return nil, fmt.Errorf("mail resource: 只允许有效的 HTTP/HTTPS 外部资源")
	}
	// Fragment 不会发到服务器，去掉它以兼容 SVG 片段、媒体定位等外链形式。
	u.Fragment = ""
	u.RawFragment = ""
	port := u.Port()
	if port != "" && !((u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443")) {
		return nil, fmt.Errorf("mail resource: 不允许访问非标准 HTTP 端口")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || strings.Contains(host, "%") {
		return nil, fmt.Errorf("mail resource: 外部主机名无效")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return nil, fmt.Errorf("mail resource: 不允许访问非公网地址")
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return u, nil
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !publicIP(ip) {
			return nil, fmt.Errorf("mail resource: 不允许访问非公网地址")
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	answers, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(answers) == 0 {
		return nil, fmt.Errorf("mail resource: 外部主机名解析失败")
	}
	for _, ip := range answers {
		if !publicIP(ip) {
			return nil, fmt.Errorf("mail resource: 外部主机解析到非公网地址")
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, ip := range answers {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("mail resource: 无法连接外部主机: %v", lastErr)
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	blocked := []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("3fff::/20"),
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func acceptForKind(kind string) string {
	switch kind {
	case "style":
		return "text/css, text/plain;q=0.8, */*;q=0.1"
	case "font":
		return "font/woff2, font/woff, application/font-woff, application/octet-stream;q=0.5"
	default:
		return "image/avif, image/webp, image/png, image/jpeg, image/gif, */*;q=0.1"
	}
}

func safeContentType(header string, u *url.URL, kind string, body []byte) (string, error) {
	mediaType, _, _ := mime.ParseMediaType(header)
	mediaType = strings.ToLower(mediaType)
	if mediaType == "" || mediaType == "application/octet-stream" || mediaType == "binary/octet-stream" {
		mediaType = sniffContentType(u, body)
	}
	switch kind {
	case "style":
		if mediaType != "text/css" && !(mediaType == "text/plain" && path.Ext(u.Path) == ".css") {
			return "", fmt.Errorf("mail resource: 外部资源不是 CSS 样式表")
		}
		if looksLikeHTML(body) {
			return "", fmt.Errorf("mail resource: 外部样式表返回了 HTML 页面")
		}
		return "text/css; charset=utf-8", nil
	case "font":
		if !isFontType(mediaType) {
			return "", fmt.Errorf("mail resource: 外部资源不是受支持的字体")
		}
		return mediaType, nil
	default:
		if !isRasterImageType(mediaType) {
			return "", fmt.Errorf("mail resource: 外部资源不是受支持的图片")
		}
		return mediaType, nil
	}
}

func sniffContentType(u *url.URL, body []byte) string {
	if len(body) >= 4 {
		switch string(body[:4]) {
		case "wOF2":
			return "font/woff2"
		case "wOFF":
			return "font/woff"
		case "OTTO":
			return "font/otf"
		case "true", "typ1":
			return "font/ttf"
		}
		if string(body[:4]) == "\x00\x01\x00\x00" {
			return "font/ttf"
		}
	}
	if path.Ext(strings.ToLower(u.Path)) == ".css" {
		return "text/css"
	}
	return http.DetectContentType(body)
}

func looksLikeHTML(body []byte) bool {
	text := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 512)])))
	return strings.HasPrefix(text, "<!doctype html") || strings.HasPrefix(text, "<html") || strings.HasPrefix(text, "<head")
}

func isRasterImageType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp":
		return true
	default:
		return false
	}
}

func isFontType(mediaType string) bool {
	switch mediaType {
	case "font/woff", "font/woff2", "font/ttf", "font/otf", "application/font-woff",
		"application/font-woff2", "application/vnd.ms-fontobject", "application/x-font-ttf",
		"application/x-font-opentype":
		return true
	default:
		return false
	}
}
