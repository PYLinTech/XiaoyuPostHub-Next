package httpapi

import "net/http"

// 邮件接口的系统模式守卫。
//
// 这里有两道门，判定不同、返回也不同，是有意区分的：
//
//	mailOnly     管理端（/api/admin/mail/*）。只看站点模式，关掉时返回 403。
//	userMailOnly 用户侧（/api/mail/*）。模式与收件开关都要开，否则返回 404。
//
// 为什么用户侧要 404 而不是 403：邮件关闭之后，用户侧就是"这个站点没有邮件
// 功能"——页面不存在、接口不存在，与一个从未部署过邮件的站点没有区别。返回
// 403 反而在对外承认"这里本来有东西，只是不给看"，既让用户困惑，也把内部
// 模式直接泄露给了不该知道的人。
//
// 管理端相反：管理员必须在收件关闭、甚至模式是「仅文件」时仍然进得去，
// 否则出问题时连查看历史邮件的入口都没有。所以那边保持 403（可恢复的拒绝）。
//
// 守卫写在路由注册处（见 router.go 里的包法）而不是一条按前缀匹配的中间件，
// 是为了让这件事在路由表上看得见：新增邮件接口时忘了包，会直接漏出一处可用
// 入口，而不是悄悄存在。

// mailOnly 给管理端邮件接口套上模式守卫。
func (s *Server) mailOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Settings.Runtime(r.Context()).Site.MailEnabled() {
			writeErr(w, http.StatusForbidden,
				"当前站点未启用邮件功能", "system_mode = files_only")
			return
		}
		h(w, r)
	}
}

// userMailOnly 给用户侧邮件接口套上守卫：模式与收件开关都要开。
//
// 判定用 404 而不是 403，理由见文件头的说明。这里同样刻意不回显是哪个条件
// 没满足——把"哪个开关没开"告诉未认证的调用方没有意义。
func (s *Server) userMailOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Settings.Runtime(r.Context()).MailAvailable() {
			// 刻意与真正的"路由不存在"用同一句话：调用方（包括中间的错误
			// 提示与前端路由表）不该能靠措辞区分这两者。
			writeErr(w, http.StatusNotFound, "接口不存在", r.URL.Path)
			return
		}
		h(w, r)
	}
}
