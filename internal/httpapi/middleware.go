package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/apikey"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/clientip"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/config"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
)

// 请求上下文键。
type ctxKey int

const (
	ctxKeyPrincipal ctxKey = iota
	ctxKeyClientIP
	ctxKeyToken
)

// Server 聚合 HTTP 层依赖。
type Server struct {
	Boot     *config.Bootstrap
	Auth     *auth.Service
	Svc      *service.Service
	Settings *settings.Store

	proxies  proxyCache
	handlers http.Handler
}

// NewServer 构造 HTTP 层并注册路由。
func NewServer(boot *config.Bootstrap, authSvc *auth.Service, svc *service.Service, st *settings.Store) *Server {
	s := &Server{
		Boot:     boot,
		Auth:     authSvc,
		Svc:      svc,
		Settings: st,
	}
	s.handlers = s.routes()
	return s
}

// proxyCache 缓存解析后的可信代理网段。
//
// 按原始配置字符串做键：配置一改键就变，旧缓存自然失效，不需要失效通知。
// 解析失败时按"不信任任何代理"处理——宁可把来源都当成直连导致风控偏严，
// 也不能因一段写错的 CIDR 就信任客户端伪造的转发头。
type proxyCache struct {
	mu     sync.RWMutex
	raw    string
	parsed []netip.Prefix
	valid  bool
}

func (c *proxyCache) get(raw string) []netip.Prefix {
	c.mu.RLock()
	if c.valid && c.raw == raw {
		out := c.parsed
		c.mu.RUnlock()
		return out
	}
	c.mu.RUnlock()

	prefixes, err := clientip.Parse(raw)
	if err != nil {
		log.Printf("httpapi: 可信代理配置 %q 无法解析（%v），已按不信任任何代理处理", raw, err)
		prefixes = nil
	}
	c.mu.Lock()
	c.raw, c.parsed, c.valid = raw, prefixes, true
	c.mu.Unlock()
	return prefixes
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handlers.ServeHTTP(w, r)
}

// ---------------------------------------------------------------- 上下文取值

// principalOf 取出当前请求的身份。
func principalOf(ctx context.Context) auth.Principal {
	if p, ok := ctx.Value(ctxKeyPrincipal).(auth.Principal); ok {
		return p
	}
	// 身份是中间件建立的硬不变量。静默伪装成访客会把新增路由的
	// 鉴权遗漏变成可利用的匿名入口；直接让 recoverer 记录并返回 500，
	// 迫使路由修复。
	panic("httpapi: 请求身份未由中间件建立")
}

// tokenOf 取出请求携带的令牌。
func tokenOf(ctx context.Context) string {
	if t, ok := ctx.Value(ctxKeyToken).(string); ok {
		return t
	}
	return ""
}

// clientIPOf 取出客户端地址。
func clientIPOf(ctx context.Context) netip.Addr {
	if a, ok := ctx.Value(ctxKeyClientIP).(netip.Addr); ok {
		return a
	}
	return netip.Addr{}
}

// ---------------------------------------------------------------- 中间件

// recoverer 捕获 panic：单个请求的异常不应让整个进程退出。
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("httpapi: 处理 %s %s 时发生 panic: %v\n%s",
					r.Method, r.URL.Path, rec, debug.Stack())
				writeErr(w, http.StatusInternalServerError, "服务器内部错误", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog 记录访问日志。只记路径不记查询串：票据与提取码都在查询串里，
// 落进日志等于把凭据写进磁盘。
//
// 静态资源不记：一次页面加载会产生好几个资源请求，把它们全写进日志会把
// 真正需要看的 API 调用淹掉，而静态资源的问题（404、缓存）在浏览器侧更容易看。
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !needsIdentity(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/")
}

// isDirectLinkPath 是直链数据面（/d/{token}）。
//
// 与 /api/* 分开判定：它返回的是文件字节而不是 JSON，既不该被 JSON 404
// 接管，也不该落进 SPA 的 index.html 兜底。但它**确实**需要请求身份——
// 访客流量按 IP 前缀分桶计费，票据归属也要比对身份。
func isDirectLinkPath(p string) bool {
	return strings.HasPrefix(p, "/d/")
}

// needsIdentity 是需要解析请求身份的路径集合：API 与直链数据面。
//
// 其余路径（静态资源与前端路由）刻意不解析身份，理由见 withIdentity 的注释。
func needsIdentity(p string) bool {
	return isAPIPath(p) || isDirectLinkPath(p)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	// wroteHeader 防止重复写头（业务代码在多处写头时会触发 net/http 的警告）。
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Write 在未显式写头时补一个 200，保证日志里的状态码与真实响应一致。
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// withIdentity 还原请求身份。
//
// 令牌只接受 Authorization 头。唯一例外是服务端解密流：媒体元素无法
// 自定义请求头，且该路由已用短期票据绑定请求，因此才允许把令牌放在查询串。
// 不在所有 API 上接受 query/cookie 兼容格式，避免凭据无谓进入日志与历史记录。
//
// 静态资源不解析身份：它们本来就是公开文件，而解析一旦失败（例如浏览器里
// 还留着上一个部署签发的过期令牌）会让整页资源 401——表现是"页面打不开"，
// 与真实原因（令牌过期）毫无关系，极难定位。
func (s *Server) withIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !needsIdentity(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		rt := s.Settings.Runtime(r.Context())
		ip := clientip.Extract(r, s.proxies.get(rt.Auth.TrustedProxiesRaw))
		token := auth.ParseBearer(r.Header.Get("Authorization"))
		if token == "" && r.URL.Path == "/api/fs/stream" {
			token = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		principal, err := s.Auth.Resolve(r.Context(), token, ip, r.UserAgent())
		if err != nil {
			// 身份解析失败属于服务端问题；直接按访客处理会让鉴权静默降级，
			// 因此明确报错。
			fail(w, err)
			return
		}
		if principal.APIKeyID != "" {
			// API Key 有四道并列的闸门：站点总开关、用户组权限、Key 能力范围、
			// 接口范围。四道都在这里统一拦截，而不是交给每个 handler 各判一次。
			if !rt.Site.APIEnabled {
				fail(w, fmt.Errorf("%w: 本站未开放 API 调用", service.ErrForbidden))
				return
			}
			if !principal.Can(perm.API) {
				fail(w, fmt.Errorf("%w: 当前用户组未开放 API 调用", service.ErrForbidden))
				return
			}
			if apiKeyScopeDenied(principal.APIKeyScopes, r.URL.Path) {
				fail(w, fmt.Errorf("%w: 该 API Key 未授予此操作权限", service.ErrForbidden))
				return
			}
			if apiKeyRequestDenied(r.URL.Path) {
				fail(w, fmt.Errorf("%w: 该接口仅限登录会话调用", service.ErrForbidden))
				return
			}
		}
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, principal)
		ctx = context.WithValue(ctx, ctxKeyClientIP, ip)
		ctx = context.WithValue(ctx, ctxKeyToken, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// apiKeyRequestDenied 列出 API Key 不能调用的接口。
//
// 凭据管理与身份管理必须留在登录会话里：一枚泄露的 Key 若能创建新 Key
// 或删除旧 Key，回收动作就会失效；若它能改密码，攻击者可以顺势接管账号。
// 管理端整体拒绝，是因为 API 只承诺用户侧能力。
//
// 访客分享/取件码入口与邮件、CDN 回源也必须拒绝：它们的授权凭据是分享口令
// 或上游票据，不属于「Key 的工作区」这套模型，放开等于绕过文件夹边界。
func apiKeyRequestDenied(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/admin/"),
		strings.HasPrefix(path, "/api/setup/"),
		strings.HasPrefix(path, "/api/api-keys"),
		strings.HasPrefix(path, "/api/mail/"),
		strings.HasPrefix(path, "/api/s/"),
		strings.HasPrefix(path, "/api/p/"),
		strings.HasPrefix(path, "/api/cdn/"):
		return true
	}
	switch path {
	case "/api/auth/login", "/api/auth/register", "/api/auth/logout", "/api/auth/password":
		return true
	}
	return false
}

// apiKeyScopeDenied 把用户侧接口映射到 Key 能力范围。
//
// 创建分享、改分享、删直链这类入口只要求登录，service 层不会再问一次
// perm.Share / perm.DirectLink（权限被回收后用户仍要能清理已有记录）。
// 对 API Key 来说，这会让"没勾分享"的 Key 仍然能删分享，因此能力范围必须
// 在路由层按接口族兜住，而不能只依赖业务层的权限位检查。
func apiKeyScopeDenied(scopes int64, path string) bool {
	required, ok := apiKeyScopeForPath(path)
	return ok && !apikey.HasAny(scopes, required)
}

func apiKeyScopeForPath(path string) (apikey.Scope, bool) {
	// 列目录、属性、用量是只读元数据，任何有效 Key 都可调用：上传、分享、
	// 直链等操作都要先能看到路径。这里只归类会改变状态或交付内容的接口。
	switch {
	case strings.HasPrefix(path, "/api/upload/"):
		return apikey.Upload, true
	case path == "/api/fs/mkdir", path == "/api/fs/rename",
		path == "/api/fs/move", path == "/api/fs/delete":
		return apikey.Manage, true
	case path == "/api/archive" || strings.HasPrefix(path, "/api/archive/"):
		return apikey.Manage, true
	case path == "/api/fs/download":
		return apikey.Download, true
	case path == "/api/fs/preview":
		return apikey.Preview, true
	case path == "/api/fs/stream", path == "/api/fs/settle":
		// 票据本身区分下载与预览，但路径无法区分；两者任一能力即可继续
		// 使用自己已有的票据，创建票据的入口已分别按 Download / Preview 拦截。
		return apikey.Download | apikey.Preview, true
	case path == "/api/share" || strings.HasPrefix(path, "/api/share/"),
		path == "/api/pickup" || strings.HasPrefix(path, "/api/pickup/"):
		return apikey.Share, true
	case path == "/api/direct-link" || strings.HasPrefix(path, "/api/direct-link/"):
		return apikey.DirectLink, true
	}
	return 0, false
}

// wrap 组合全局中间件。安全头、日志与 panic 捕获包在身份解析之外；CORS 位于
// 身份解析之前，以便 OPTIONS 预检不要求登录，同时仍由外层日志和安全头兜底。
func (s *Server) wrap(h http.Handler) http.Handler {
	return securityHeaders(recoverer(accessLog(s.cors(s.withIdentity(h)))))
}

const (
	corsAllowedMethods = "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowedHeaders = "Authorization, Content-Type, Range"
	corsExposedHeaders = "Accept-Ranges, Content-Disposition, Content-Length, Content-Range"
)

var corsMethodSet = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodOptions: {},
}

var corsHeaderSet = map[string]struct{}{
	"authorization": {}, "content-type": {}, "range": {},
}

// cors 动态读取运行期配置，并在身份中间件前结束预检请求。
// 即使业务路由要求登录，浏览器也必须先能无凭据地完成 OPTIONS 预检。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := corsPolicyForPath(s.Settings.Runtime(r.Context()).CORS, r.URL.Path)
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if !cfg.Enabled || origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		addVary(w.Header(), "Origin")

		allowed, wildcard := false, false
		for _, candidate := range cfg.Origins {
			if candidate == "*" {
				wildcard = true
				allowed = true
				break
			}
			if strings.EqualFold(candidate, origin) {
				allowed = true
			}
		}

		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if preflight {
			addVary(w.Header(), "Access-Control-Request-Method")
			addVary(w.Header(), "Access-Control-Request-Headers")
		}
		if !allowed {
			if preflight {
				http.Error(w, "跨域来源不在允许列表中", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if wildcard {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		if preflight {
			method := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
			if _, ok := corsMethodSet[method]; !ok || !corsHeadersAllowed(r.Header.Get("Access-Control-Request-Headers")) {
				http.Error(w, "跨域预检请求不被允许", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
			w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Set("Access-Control-Expose-Headers", corsExposedHeaders)
		next.ServeHTTP(w, r)
	})
}

// corsPolicyForPath 把每个请求唯一归入页面或接口：/api/* 与 /d/* 属于 SDK，
// 其余路径（包括分享页与静态资源）属于主站。这里不做“两类都命中”的兜底，
// 也不保留第三类——跨域配置只有主站与 SDK 两张表。
func corsPolicyForPath(cfg settings.CORSRuntime, rawPath string) settings.CORSPolicy {
	clean := path.Clean("/" + rawPath)
	if clean == "/api" || strings.HasPrefix(clean, "/api/") ||
		clean == "/d" || strings.HasPrefix(clean, "/d/") {
		return cfg.SDK
	}
	return cfg.Main
}

func corsHeadersAllowed(raw string) bool {
	for _, name := range strings.Split(raw, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if _, ok := corsHeaderSet[name]; !ok {
			return false
		}
	}
	return true
}

func addVary(header http.Header, value string) {
	for _, line := range header.Values("Vary") {
		for _, item := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}

// securityHeaders 统一写入与业务无关、但每个响应都应具备的浏览器防护头。
// 这些头放在最外层，保证路由 404、panic 恢复和静态资源也不会漏掉。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// API 响应已由 writeJSON 设置 no-store；这里覆盖所有未经过该
		// 帮助函数的 API 响应（例如 CDN 鉴权），避免票据被中间层缓存。
		if needsIdentity(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
