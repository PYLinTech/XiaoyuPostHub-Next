package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

type browserPlanRequest struct {
	Path            string `json:"path"`
	ClientPublicKey string `json:"clientPublicKey"`
}

type browserSettleRequest struct {
	TicketID  string `json:"ticketId"`
	Completed bool   `json:"completed"`
}

// runDirectDeliveryServer 演示「后端请求得到地址后，前端直接去拿」：
// 后端只申请交付计划，浏览器拿计划里的 123 地址取密文并在本地解密；
// API Key 不出后端，站点与后端都不经过文件字节。
func runDirectDeliveryServer(addr string) {
	client, err := NewClient("", "")
	if err != nil {
		log.Fatal(err)
	}
	page := exampleWebFile("direct-delivery.html")
	module := exampleWebFile("browser-delivery.mjs")
	moduleBytes, err := os.ReadFile(module)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, page)
	})
	mux.HandleFunc("GET /browser-delivery.mjs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(moduleBytes)
	})
	mux.HandleFunc("POST /api/delivery/plan", func(w http.ResponseWriter, r *http.Request) {
		var req browserPlanRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			exampleError(w, http.StatusBadRequest, "请求体不是有效 JSON")
			return
		}
		req.Path = strings.TrimSpace(req.Path)
		req.ClientPublicKey = strings.TrimSpace(req.ClientPublicKey)
		if req.Path == "" {
			exampleError(w, http.StatusBadRequest, "path 不能为空")
			return
		}
		if req.ClientPublicKey == "" {
			exampleError(w, http.StatusBadRequest, "clientPublicKey 不能为空")
			return
		}
		// 代传终端用户真实地址：站点看到的是本后端 IP，而真正去 123 取字节
		// 的是浏览器用户；票据按访客网段绑定，123 回源鉴权才放行。不代传则
		// 真实部署（后端在服务器、用户在别处）直连必然 403。
		plan, err := client.PrepareDownload(r.Context(), req.Path, req.ClientPublicKey, false, visitorIP(r))
		if err != nil {
			exampleUpstreamError(w, err)
			return
		}
		if plan.Mode != "direct" || plan.ContentForm != "ciphertext" {
			// 不回执：密文票据一旦回执即按预扣全额结算，不回执则到期退款；
			// proxy_decrypt 由服务端结算，回执也会被拒绝。
			exampleError(w, http.StatusConflict,
				"本次交付被降级为站点中转，浏览器无法直接向 123 取密文；请确认站点开启「优先直链交付」且 123 直链空间可用")
			return
		}
		exampleJSON(w, http.StatusOK, plan)
	})
	mux.HandleFunc("POST /api/delivery/settle", func(w http.ResponseWriter, r *http.Request) {
		var req browserSettleRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			exampleError(w, http.StatusBadRequest, "请求体不是有效 JSON")
			return
		}
		if strings.TrimSpace(req.TicketID) == "" {
			exampleError(w, http.StatusBadRequest, "ticketId 不能为空")
			return
		}
		if err := client.Settle(r.Context(), req.TicketID, req.Completed); err != nil {
			exampleUpstreamError(w, err)
			return
		}
		exampleJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	log.Printf("方式 2 示例（后端请求得到地址后，前端直接去拿）已启动：http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// trustedProxies 是本后端自己信任的反向代理网段。站点侧「可信代理」只作用于
// 站点自身的入站请求，不会替本后端判断 X-Forwarded-For，因此这里独立配置。
// 默认空表示只认 TCP 对端地址；后端在反向代理后时，用逗号分隔的 IP 或 CIDR
// 声明代理网段，例如 TRUSTED_PROXIES=127.0.0.1/32,::1/128。
var trustedProxies = parseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))

func parseTrustedProxies(raw string) []netip.Prefix {
	var prefixes []netip.Prefix
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(item); err == nil {
			prefixes = append(prefixes, prefix)
			continue
		}
		if addr, ok := parseIP(item); ok {
			bits := 128
			if addr.Is4() {
				bits = 32
			}
			prefixes = append(prefixes, netip.PrefixFrom(addr, bits))
		}
	}
	return prefixes
}

func parseIP(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func isTrustedProxy(addr netip.Addr) bool {
	for _, prefix := range trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// visitorIP 取终端用户地址：只有 TCP 对端属于受信代理时才读取
// X-Forwarded-For，并从右往左跳过受信代理，取第一个非代理地址。转发链里出现
// 无法解析的条目时回退到 TCP 对端，而不是继续相信更左侧、可能由客户端伪造的值。
func visitorIP(r *http.Request) string {
	peer, ok := parseIP(r.RemoteAddr)
	if !ok {
		return ""
	}
	if !isTrustedProxy(peer) {
		return peer.String()
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	var leftmost netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseIP(hops[i])
		if !ok {
			return peer.String()
		}
		leftmost = addr
		if isTrustedProxy(addr) {
			continue
		}
		return addr.String()
	}
	if leftmost.IsValid() {
		return leftmost.String()
	}
	return peer.String()
}
