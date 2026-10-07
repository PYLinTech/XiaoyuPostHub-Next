// Package httpapi 是 HTTP 接入层：解析请求、还原身份、调用业务层、映射响应。
//
// 本层不做业务判断，只做三件事：还原入参、把业务错误映射成 HTTP 状态码、
// 统一响应信封（避免每个接口各自发明格式）。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 响应信封。
//
// 成功：HTTP 200 + {"data": ...}
// 失败：HTTP 4xx/5xx + {"error": {"message": "...", "detail": "..."}}
//
// message 面向最终用户（中文、中性、不含实现细节）；detail 只用于排障，
// 可能包含技术原因，前端不应直接展示。
type apiError struct {
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

type apiResponse struct {
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

// maxJSONBody 限制请求体大小。管理接口的请求体都是小对象，限制得紧一些，
// 避免把内存交给客户端控制。
const maxJSONBody = 1 << 20

func writeJSON(w http.ResponseWriter, status int, payload apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 鉴权与配额相关的响应一律不缓存：任何中间层缓存身份相关的响应，
	// 都可能让另一个用户拿到别人的数据。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		log.Printf("httpapi: 写出响应失败: %v", err)
	}
}

func writeData(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, apiResponse{Data: data})
}

func writeErr(w http.ResponseWriter, status int, message, detail string) {
	writeJSON(w, status, apiResponse{Error: &apiError{Message: message, Detail: detail}})
}

// decodeJSON 解析请求体。拒绝未知字段：客户端传错字段名时应当明确报错，
// 而不是静默按默认值处理（那会变成"设置没生效"这类难查的问题）。
func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不正确", err.Error())
		return false
	}
	// 一个请求只能包含一个 JSON 值。只 Decode 一次会把“合法对象后拼接
	// 垃圾数据”的请求当成成功，既不利于客户端排错，也可能让签名/审计
	// 边界出现歧义。
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("请求体只能包含一个 JSON 值")
		}
		writeErr(w, http.StatusBadRequest, "请求格式不正确", err.Error())
		return false
	}
	return true
}

// errorStatus 把业务错误映射成 HTTP 状态与用户可见文案。
//
// 映射规则刻意保守：不把"资源存在但你没权限"与"资源不存在"区分成不同状态，
// 否则接口会变成枚举器。
func errorStatus(err error) (int, string, string) {
	// 引导页的校验失败自带一句人话，优先于下面那条泛化的 ErrBadRequest：
	// 到这一步还只回"请求参数不正确"，等于没有告诉用户是哪一项填错了。
	var setupInvalid *service.SetupInvalidError
	if errors.As(err, &setupInvalid) {
		return http.StatusBadRequest, setupInvalid.Message(), detailText(err)
	}

	switch {
	case err == nil:
		return http.StatusOK, "", ""

	case errors.Is(err, auth.ErrPasswordInvalidChars):
		return http.StatusBadRequest, "密码只能使用大小写英文字母、数字和符号，不能包含空格", detailText(err)

	case errors.Is(err, auth.ErrPasswordTooShort):
		return http.StatusBadRequest, "密码至少 8 个字符", detailText(err)

	case errors.Is(err, auth.ErrPasswordTooLong):
		return http.StatusBadRequest, "密码最多 32 个字符", detailText(err)

	// 416 必须排在 ErrBadRequest 之前：ErrRangeNotSatisfiable 本身就是
	// fmt.Errorf 包着 ErrBadRequest 造出来的，errors.Is 两者都成立，
	// 顺序颠倒会让区间错误被降级成 400。
	//
	// 归到这一层裁决而不是让每个 handler 自己去判：映射规则散在 handler 里，
	// detail 就会绕过 detailText，把完整的哨兵链直接送给前端。
	case errors.Is(err, service.ErrRangeNotSatisfiable):
		return http.StatusRequestedRangeNotSatisfiable, "请求区间不正确", detailText(err)

	case errors.Is(err, service.ErrBadRequest):
		return http.StatusBadRequest, "请求参数不正确", detailText(err)

	// 配置校验失败是请求本身的问题，不是服务端故障：传了一个描述符不接受的
	// 值（或干脆是不存在的配置项）。归到 500 会让管理界面的配置保存永远只
	// 显示"服务器内部错误"，用户既不知道是哪个键写错，也无从修正。
	case errors.Is(err, settings.ErrInvalidValue), errors.Is(err, settings.ErrUnknownKey):
		return http.StatusBadRequest, "配置取值不合法", detailText(err)

	case errors.Is(err, service.ErrAlreadyInitialized):
		return http.StatusConflict, "站点已完成初始化", detailText(err)

	case errors.Is(err, service.ErrSetupTokenRequired):
		return http.StatusForbidden, "需要一个有效的初始化令牌（见服务端启动日志）", detailText(err)

	case errors.Is(err, service.ErrForbidden), errors.Is(err, auth.ErrForbidden):
		return http.StatusForbidden, "没有权限执行该操作", detailText(err)

	case errors.Is(err, auth.ErrNotAuthenticated):
		return http.StatusUnauthorized, "请先登录", detailText(err)

	case errors.Is(err, auth.ErrSessionInvalid):
		return http.StatusUnauthorized, "登录已过期，请重新登录", detailText(err)

	case errors.Is(err, auth.ErrInvalidCredentials):
		return http.StatusUnauthorized, "账号或密码错误", detailText(err)

	case errors.Is(err, auth.ErrAccountDisabled):
		return http.StatusForbidden, "账号已被禁用，请联系管理员", detailText(err)

	case errors.Is(err, service.ErrNotFound), errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "目标不存在", detailText(err)

	case errors.Is(err, service.ErrConflict), errors.Is(err, store.ErrConflict):
		// 标题必须对所有冲突都成立：这里的冲突大半不是"重复"，
		// 而是"当前状态不允许"（域名下还有地址、组仍有成员、分享仍被引用）。
		// 写成"目标已存在"会把管理员引到完全错误的方向上。
		return http.StatusConflict, "当前状态不允许该操作", detailText(err)

	case errors.Is(err, service.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "文件超出上限", detailText(err)

	case errors.Is(err, service.ErrQuotaExceeded), errors.Is(err, store.ErrQuotaExceeded):
		return http.StatusInsufficientStorage, "配额不足", detailText(err)

	case errors.Is(err, service.ErrNotSupported):
		return http.StatusNotImplemented, "当前配置不支持该操作", detailText(err)

	case errors.Is(err, service.ErrBusy):
		return http.StatusConflict, "该文件正在上传中，请稍后重试", detailText(err)

	// 取件码码池耗尽是明确的容量约束：重试没有意义，必须把"为什么生成不了、
	// 什么时候能恢复"讲清楚，因此用户文案不复用泛化的冲突提示。
	case errors.Is(err, service.ErrPickupPoolExhausted):
		return http.StatusConflict,
			"取件码已用尽：当前有效期窗口内生成的取件码数量达到码空间上限，请等待部分取件码过期，或联系管理员调整取件码有效期后再试",
			detailText(err)

	// 上传网关 60s 读超时：分片大小由 123 服务端固定 16MB，撞上它说明
	// 服务器上行带宽不足以在时限内传完一个分片。这不是"稍后重试"能解决
	// 的事，必须告诉用户去找管理员。
	case errors.Is(err, backend.ErrUploadBandwidth):
		return http.StatusServiceUnavailable, "服务器上传带宽过小，请联系管理员处理", detailText(err)

	case errors.Is(err, service.ErrUnavailable):
		return http.StatusServiceUnavailable, "服务暂时不可用，请稍后重试", detailText(err)

	case errors.Is(err, auth.ErrThrottleUnavailable):
		return http.StatusServiceUnavailable, "服务暂时不可用，请稍后重试", detailText(err)

	// 存储未就绪给一句不同的提示：泛化的"稍后重试"会让用户一遍遍重试一件
	// 永远不会成功的事，而能解决它的是管理员。
	case errors.Is(err, service.ErrStorageNotReady):
		return http.StatusServiceUnavailable, "存储尚未就绪，请联系管理员", detailText(err)

	case auth.IsThrottled(err):
		// detail 留空：ThrottledError 的文本里带着维度（account/ip/share）和
		// 精确到秒的剩余时间。把它透给未认证的调用方，等于让对方能逐个账号
		// 读出"这个账号累积了多少次失败"，以及 IP 桶上次是什么时候被清空的。
		// 需要告知客户端的等待时长由 Retry-After 响应头单独承担。
		return http.StatusTooManyRequests, "操作过于频繁，请稍后再试", ""
	}
	return http.StatusInternalServerError, "服务器内部错误", detailText(err)
}

// errorSentinels 是 detail 里要剥掉的哨兵前缀。
//
// 业务错误一律是 fmt.Errorf("%w: %s", ErrXxx, "具体原因")，于是 err.Error()
// 的形状是 "service: 目标状态冲突: 域名下仍有 2 个邮箱地址"。前端现在会把
// message 与 detail 一起显示给人，哨兵属于内部噪音，必须去掉——
// "当前状态不允许该操作：service: 目标状态冲突：域名下还有 2 个地址"
// 既难看又读不懂。
var errorSentinels = []string{
	service.ErrBadRequest.Error(),
	// ErrForbidden 此前漏在这里，代价是全站每一处 403 都把
	// "service: 无权访问: <原因>" 整串发给了客户端——而下面这条 case 本来
	// 就是要把它收敛成"没有权限执行该操作"的。补上后 detail 只剩原因本身。
	service.ErrForbidden.Error(),
	service.ErrConflict.Error(),
	service.ErrNotFound.Error(),
	service.ErrTooLarge.Error(),
	service.ErrQuotaExceeded.Error(),
	service.ErrNotSupported.Error(),
	service.ErrBusy.Error(),
	service.ErrPickupPoolExhausted.Error(),
	service.ErrUnavailable.Error(),
	service.ErrStorageNotReady.Error(),
	service.ErrAlreadyInitialized.Error(),
	service.ErrSetupTokenRequired.Error(),
	store.ErrNotFound.Error(),
	store.ErrConflict.Error(),
	store.ErrQuotaExceeded.Error(),
	auth.ErrForbidden.Error(),
	auth.ErrNotAuthenticated.Error(),
	auth.ErrSessionInvalid.Error(),
	auth.ErrInvalidCredentials.Error(),
	auth.ErrAccountDisabled.Error(),
	auth.ErrThrottleUnavailable.Error(),
	backend.ErrUploadBandwidth.Error(),
}

// detailText 取错误里的"具体原因"部分：剥掉已知的哨兵前缀。
//
// 只在前缀**确实匹配**某个哨兵时才剥——具体原因本身也可能带冒号
// （例如 "用户组 ops: 还绑着域名"），按第一个冒号切会把它切坏。
// 匹配不上就原样返回：宁可多显示一点，也不要显示一半。
//
// 错误串**恰好就是**哨兵本身时（没有包任何原因）返回空串：此时 message
// 已经说完了所有信息，再附一遍只会变成"请先登录：auth: 需要登录"。
func detailText(err error) string {
	msg := err.Error()
	for _, sentinel := range errorSentinels {
		if msg == sentinel {
			return ""
		}
		if rest, ok := strings.CutPrefix(msg, sentinel+": "); ok {
			return rest
		}
	}
	return msg
}

// fail 按错误类型写出响应。
func fail(w http.ResponseWriter, err error) {
	status, message, detail := errorStatus(err)
	// 5xx 一律不送 detail。
	//
	// 4xx 的 detail 是业务代码刻意写给人看的具体原因（"域名下仍有 2 个地址"），
	// 那是这一层存在的意义；5xx 的 detail 则是未经滤的内部细节——SQL 原文、
	// 存储桶名、文件路径、驱动报错都可能在里面。
	//
	// 此前 5xx 也带着 detail，只是前端从不显示，所以没出事。改成"detail 也要
	// 给人看"之后，它立刻变成一个信息泄漏面：数据库一锁，用户就能看到
	// "扫描用户组失败: sql: database is locked"。具体内容只进服务端日志。
	if status >= http.StatusInternalServerError {
		log.Printf("httpapi: 服务端错误（%s）: %v", message, err)
		detail = ""
	}
	writeErr(w, status, message, detail)
}

// itemsOf 在 JSON 编码边界上把 nil 切片转成空数组。
//
// Go 的 json 编码把 nil 切片写成 null，而"空列表"在接口契约里必须是 []：
// 前端拿到 null 之后调 .map 会直接抛错，表现为**整页空白**——而这个错误既不会
// 在编译期暴露，也不会被只断言业务逻辑的单测发现（实测就是靠浏览器里的一次
// 整页空白才抓到）。把 nil 关在编码边界内，比要求每个调用方都记得判空可靠。
func itemsOf[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
