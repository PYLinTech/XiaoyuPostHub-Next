package httpapi

import (
	"context"
	"log"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/clientip"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/config"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailresource"
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

	proxies       proxyCache
	mailResources *mailresource.Gateway
	handlers      http.Handler
}

// NewServer 构造 HTTP 层并注册路由。
func NewServer(boot *config.Bootstrap, authSvc *auth.Service, svc *service.Service, st *settings.Store) *Server {
	s := &Server{
		Boot:          boot,
		Auth:          authSvc,
		Svc:           svc,
		Settings:      st,
		mailResources: mailresource.NewGateway(),
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
		if !isAPIPath(r.URL.Path) {
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
		if !isAPIPath(r.URL.Path) {
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
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, principal)
		ctx = context.WithValue(ctx, ctxKeyClientIP, ip)
		ctx = context.WithValue(ctx, ctxKeyToken, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// wrap 组合全局中间件。顺序固定：日志与 panic 捕获必须在身份解析之外，
// 否则身份解析自身的 panic 会把请求打成裸 500。
func (s *Server) wrap(h http.Handler) http.Handler {
	return securityHeaders(recoverer(accessLog(s.withIdentity(h))))
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
		if isAPIPath(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
