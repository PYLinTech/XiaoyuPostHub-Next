package httpapi

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/webui"
)

// 前端页面的托管。
//
// 产物有两个来源，按优先级取：
//
//	XPH_STATIC_DIR 指定的目录 —— 运行时换前端、联调时用 go run 跑未内嵌的源码
//	二进制内嵌的产物          —— build.sh 打进二进制的正式构建（正常路径）
//	都没有                      —— 503 + 一句给人看的提示，API 照常可用
//
// 前端与后端必须同源（票据走 URL、内容密钥走同源响应体、Service Worker 只能
// 注册在同源下），所以生产环境由本进程直接吐出页面，而不是让前端单独部署在
// 另一个域名上。

// staticHandler 返回托管前端的处理器。
func (s *Server) staticHandler() http.Handler {
	if root := strings.TrimSpace(s.Boot.StaticDir); root != "" {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return staticFromDisk(root)
		}
		// 显式指定了却不指向目录：这是配置写错了，不该静默退回内嵌产物——
		// 那会让"我明明换了前端"变成一句无法解释的现象。
		return staticUnavailable("XPH_STATIC_DIR 指向的目录不存在或不是目录：" + root)
	}
	if webui.Available() {
		return staticFromFS(webui.FS())
	}
	return staticUnavailable("")
}

// staticFromDisk 托管磁盘上的构建产物。
func staticFromDisk(root string) http.Handler {
	files := http.FileServer(http.Dir(root))
	return serveStatic(func(clean string) bool {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/"))))
		return err == nil && !info.IsDir()
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 入口文件绝不能缓存：它引用的是带哈希的资源名，必须每次获取最新入口。
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	}, files)
}

// staticFromFS 托管内嵌产物。
//
// 内嵌的是内存里的文件树，http.FileServer 也能用，但它每次请求要 Stat 一次；
// 这里改成直接 fs.Stat，同一份逻辑两种来源只差"怎么判断存在、怎么把字节取出来"。
func staticFromFS(assets fs.FS) http.Handler {
	return serveStatic(func(clean string) bool {
		info, err := fs.Stat(assets, strings.TrimPrefix(clean, "/"))
		return err == nil && !info.IsDir()
	}, func(w http.ResponseWriter, r *http.Request) {
		page, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, "内嵌前端缺少 index.html", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	}, http.FileServer(http.FS(assets)))
}

// serveStatic 是两种来源共用的外壳：决定"这个路径是文件、是 SPA 路由、还是真丢了"，
// 再套上缓存策略。
func serveStatic(
	isFile func(clean string) bool,
	serveIndex func(w http.ResponseWriter, r *http.Request),
	files http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 未匹配的 /api 路径必须仍然回 JSON：回落到 index.html 会让调用方
		// 收到一份 HTML，然后在前端解析 JSON 时报一个与真实原因无关的错。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "接口不存在", r.URL.Path)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的请求方法", "")
			return
		}

		clean := path.Clean("/" + r.URL.Path)
		if isFile(clean) {
			applyStaticCacheHeaders(w, clean)
			files.ServeHTTP(w, r)
			return
		}

		// 只有"看起来不是文件"的路径才回退到单页应用入口。
		// 带扩展名的请求是真丢了资源（例如某个 js 被删了），回退成 HTML 只会
		// 在浏览器里变成一个 MIME 类型错误，把真正的原因埋掉。
		if path.Ext(clean) != "" {
			http.NotFound(w, r)
			return
		}

		serveIndex(w, r)
	})
}

// applyStaticCacheHeaders 给静态资源设置缓存策略。
//
// 只有 Vite 产出的带内容哈希的资源才能长期缓存；Service Worker 恰好相反，
// 它必须每次回源校验，否则修好的解密逻辑在用户机器上不会生效。
func applyStaticCacheHeaders(w http.ResponseWriter, cleanPath string) {
	switch {
	case strings.HasPrefix(cleanPath, "/assets/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case strings.HasSuffix(cleanPath, "/xph-sw.js"):
		w.Header().Set("Cache-Control", "no-cache")
		// 允许 SW 作用到根路径，否则它只能拦截 /assets 下的请求，
		// 而解密流用的是 /__xph/ 前缀，会拦截不到。
		w.Header().Set("Service-Worker-Allowed", "/")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
}

// staticUnavailable 回一段**给人看**的纯文本，而不是 JSON：它的读者是运维。
func staticUnavailable(reason string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "接口不存在", r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		msg := "这个二进制没有内嵌前端页面，API 正常可用。\n\n" +
			"· 正式构建：在项目根目录执行 ./build.sh（会一并构建前端并内嵌）\n" +
			"· 联调前端：在 frontend/ 执行 npm run dev，页面由 Vite 提供（5173）\n" +
			"· 临时换一套：用 XPH_STATIC_DIR 指向已有的构建产物目录\n"
		if reason != "" {
			msg += "\n原因：" + reason + "\n"
		}
		_, _ = w.Write([]byte(msg))
	})
}
