package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/relayauth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 回源鉴权回调从 123 收到的参数名。部署侧在 123 面板「远程鉴权」的「自定义参数」
// 里选 URL 参数类型，参数名在这里定死，避免两端各写各的。
//
//	鉴权服务器地址：https://<域名>/api/cdn/auth   （不带任何参数）
//	自定义参数：需要配置URL参数：①选择参数 → remote_addr → $remote_addr   ②选择参数 → request_uri → $request_uri
const (
	// paramClaimedIP 是 123 观测到的访客地址。
	paramClaimedIP = "remote_addr"
	// paramRequestURI 是访客向 123 请求该文件时的 path+query，票据在里面。
	paramRequestURI = "request_uri"
)

type deliveryRequest struct {
	Path string `json:"path"`
	// ClientPublicKey 是前端现场生成的临时公钥（base64 SPKI DER）。
	// 提供时内容密钥以 RSA-OAEP 信封下发，私钥不出浏览器内存。
	ClientPublicKey string `json:"clientPublicKey"`
}

// handleDownload 准备一次下载。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.prepareDelivery(w, r, store.PurposeDownload)
}

// handlePreview 准备一次预览。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	s.prepareDelivery(w, r, store.PurposePreview)
}

// prepareDelivery 是下载与预览共用的准备路径，只在 purpose 上分岔。
// 合一条路径才不会出现"预览能看、下载被拒"这类不一致。
func (s *Server) prepareDelivery(w http.ResponseWriter, r *http.Request, purpose store.Purpose) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req deliveryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	plan, err := s.Svc.PrepareDelivery(r.Context(), p, service.DeliveryTarget{
		OwnerUserID: p.UserID(),
		Path:        req.Path,
	}, service.DeliveryRequest{
		Purpose:         purpose,
		ClientPublicKey: req.ClientPublicKey,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, plan)
}

type settleRequest struct {
	TicketID  string `json:"ticketId"`
	Completed bool   `json:"completed"`
}

// handleSettle 是直链加密 / 中转加密票据的客户端结算入口。
//
// 这两种票据的字节在服务端视野外交付，客户端上报的用量不可信，因此服务层
// 一律按预扣全额结算。completed 只决定是否记录为客户端确认完成的下载，不能
// 折减配额，也不能证明文件已实际保存；中转解密票据由服务端实测结算。
func (s *Server) handleSettle(w http.ResponseWriter, r *http.Request) {
	// 交付计划也会签发给分享访客；结算必须允许匿名请求，但由服务层
	// 校验票据的用户/访客归属，不能仅凭 ticketId 修改别人的额度。
	actor := principalOf(r.Context())
	var req settleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Svc.SettleDeliveryFromClient(r.Context(), actor, req.TicketID, req.Completed); err != nil {
		if !errors.Is(err, service.ErrForbidden) && !errors.Is(err, service.ErrBadRequest) {
			log.Printf("client delivery settlement failed: %v", err)
		}
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

// handleStream 输出服务端中转流，支持 Range。
//
// 中转解密时输出明文，中转加密时输出密文。两者共用同一套票据与来源校验——
// 票据泄露的后果同样严重。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	ticketID := strings.TrimSpace(r.URL.Query().Get("ticket"))
	if ticketID == "" {
		writeErr(w, http.StatusBadRequest, "缺少票据", "")
		return
	}
	p := principalOf(r.Context())

	// Range 解析在 service 层完成：后缀式区间需要票据口径下的对象总长度。
	rangeHeader := strings.TrimSpace(r.Header.Get("Range"))
	var stream service.ServerStream
	var err error
	headOnly := r.Method == http.MethodHead
	if headOnly {
		stream, err = s.Svc.HeadServerStreamForActor(r.Context(), ticketID, p, rangeHeader)
	} else {
		stream, err = s.Svc.OpenServerStreamForActor(r.Context(), ticketID, p, rangeHeader)
	}
	if err != nil {
		// 区间不可满足的 416 映射在 errorStatus 里统一裁决，这里不再特判——
		// 特判会让这条路径绕过 detailText，把完整哨兵链直接送给前端。
		fail(w, err)
		return
	}
	s.writeServerStream(w, r, p, stream)
}

// writeServerStream 统一中转与直链的响应头、区间输出和服务端结算。
func (s *Server) writeServerStream(w http.ResponseWriter, r *http.Request, actor auth.Principal, stream service.ServerStream) {
	reader, ticket, file := stream.Reader, stream.Ticket, stream.File
	if reader != nil {
		defer reader.Close()
	}

	w.Header().Set(service.HeaderContentForm, stream.ContentForm)
	w.Header().Set(service.HeaderContentSHA256, file.Checksum)
	w.Header().Set(service.HeaderTicket, ticket.ID)
	w.Header().Set("Accept-Ranges", "bytes")
	// 类型必须具体：全局响应头带 nosniff，octet-stream 会被浏览器拒绝渲染为
	// 图片/媒体，中转流的预览因此整条不可用。
	w.Header().Set("Content-Type", stream.MimeType)
	// 内容绝不能被任何中间层缓存。
	w.Header().Set("Cache-Control", "no-store")

	// 中转解密按明文长度响应，中转加密按密文长度响应。
	size := stream.CipherSize
	if stream.ContentForm == service.ContentFormPlaintext {
		size = stream.PlainSize
	}
	contentLength := size - stream.Offset
	if stream.Limit > 0 && stream.Limit < contentLength {
		contentLength = stream.Limit
	}
	end := stream.Offset + contentLength - 1
	if !stream.RangeRequested {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	} else {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", stream.Offset, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method == http.MethodHead {
		return
	}

	written, copyErr := io.Copy(w, reader)
	if copyErr != nil {
		log.Printf("httpapi: 票据 %s 输出流中断（已写 %d 字节）: %v", ticket.ID, written, copyErr)
	}
	if shouldSettleStream(stream) {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if err := s.Svc.SettleDelivery(settleCtx, actor, ticket.ID, written); err != nil {
			// 响应已经开始写出，结算错误只能记录。
			log.Printf("httpapi: 票据 %s 结算失败: %v", ticket.ID, err)
		}
	}
}

// shouldSettleStream 判断本次响应是否要按实发字节结算。
//
// 只有明文（proxy_decrypt）流需要服务端结算：密文流的实际用量由客户端自报，
// 服务端实测只会重复记账。整文件才结算是分享与普通下载的既有行为；直链的
// 请求方是 wget / 播放器这类没有自结算通道的裸 HTTP 客户端，Range 是常态，
// 通过 DirectLinkID 识别这些单次请求票据，按每次请求结算。
func shouldSettleStream(stream service.ServerStream) bool {
	if stream.ContentForm != service.ContentFormPlaintext {
		return false
	}
	return stream.Ticket.DirectLinkID != "" || !stream.RangeRequested
}

// handleCDNAuth 是 123 的回源鉴权端点。
//
// 交付链路的分工要先说清楚：**文件字节始终由 123 出**，访客要拿字节就必须让 123
// 受理他的请求。123 受理每一个请求时，会回源到本端点问一次"这条能不能放"，
// 我们答 200 或 403，它据此决定放行还是拒绝。
//
// 因此**能决定结果的那次调用只发生在 123 与我们之间**。攻击者绕过 123 直连本
// 端点，即使拿到 200 也换不到任何字节——123 处理他的请求时会独立地问我们一次。
// 这条推论是本端点所有信任判断的立足点。
//
// 于是本端点不依赖登录态（123 手里没有用户的 Cookie），凭据全在 URL 上，且只有
// 两样是 123 告诉我们、而非访客能凭空造的：
//
//   - 票据：我们在签发直链时自己写上去的，123 原样带在 $request_uri 里；
//   - 访客地址：123 观测到的，即 $remote_addr。
//
// 服务端中转改用同一 xph 参数承载独立签名凭据：绑定对象路径与有效期，
// HEAD 和多个 Range 可复用，不消费用户票据。只验证签名，不按服务器 IP 放行。
//
// 状态码是双方唯一都稳定的契约：200 放行，403 拒绝。
func (s *Server) handleCDNAuth(w http.ResponseWriter, r *http.Request) {
	ticketID := claimedTicketID(r)
	claimedIP := claimedClientIP(r)
	// 鉴权结果与具体访客地址相关，任何中间层缓存都可能把放行结果带给别人。
	w.Header().Set("Cache-Control", "no-store")
	var authErr error
	if strings.HasPrefix(ticketID, relayauth.Prefix) {
		authErr = s.Svc.AuthorizeRelayCDN(r.Context(), ticketID, claimedRequestPath(r))
	} else {
		_, authErr = s.Svc.AuthorizeCDN(r.Context(), ticketID, claimedIP)
	}
	if err := authErr; err != nil {
		// 响应体不回细节：端点公开可达，错误信息会变成探测工具。
		// 但**必须**落服务端日志——123 那边只看得到 403，运维手里没有任何线索。
		// 拒绝原因恰好分属三种互不相同的配置问题：request_uri 没配上导致票据
		// 没回传、票据已失效、remote_addr 没带上导致来源校验必然失败。三者在
		// 管理端都表现为同一句"取密文失败：HTTP 403"，只有这行日志能分开。
		//
		// 票据 ID 不入日志：端点公开可探测，回显它等于把在途凭据抄进日志文件。
		// 原因走 detailText 剥掉哨兵前缀——直接打 %v 会得到
		// "service: 无权访问: 票据不存在" 这种双前缀串，前缀在这里纯属噪音。
		// AuthorizeCDN 的每条返回路径都带原因，detailText 不会给出空串。
		//
		// 访客地址只写能解析成 IP 的那一种：地址这个类型装不下换行，非 IP 的
		// 值（remote_addr 没配上时来自请求方）连一个字都进不了日志。
		visitors := "未透传"
		if addr, perr := netip.ParseAddr(claimedIP); perr == nil {
			visitors = addr.Unmap().String()
		}
		log.Printf("httpapi: 回源鉴权拒绝：%s（票据参数=%s 访客地址=%s 对端=%s）",
			detailText(err), ticketPresent(ticketID), visitors, r.RemoteAddr)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ticketPresent 只报告票据参数是否到位，不回显其值。
func ticketPresent(id string) string {
	if id == "" {
		return "未回传"
	}
	return "已回传"
}

// claimedTicketID 取回源回调里带的票据。
//
// 票据是我们在签发直链时自己写上去的（见 backend/pan123.go 的 query.Set("xph", …)），
// 123 把它原样带在 $request_uri 里——那是访客向 123 请求该文件时的 path+query。
//
// 它落在哪儿取决于 123 是否转义 request_uri 内部的 &：转义了，票据整个缩在
// request_uri 里；没转义，里面的 & 会把 xph 顶到顶层。两种都读，顶层优先。
func claimedTicketID(r *http.Request) string {
	q := r.URL.Query()
	if id := strings.TrimSpace(q.Get(service.HeaderPanTicket)); id != "" {
		return id
	}
	// $request_uri 形如 "路径?查询串"，取第一个 ? 之后的部分再按查询串解。
	raw := q.Get(paramRequestURI)
	if idx := strings.IndexByte(raw, '?'); idx >= 0 {
		raw = raw[idx+1:]
	}
	inner, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(inner.Get(service.HeaderPanTicket))
}

// claimedRequestPath 保留原始转义路径。兼容 123 未编码 request_uri、
// 或把整个 request_uri 编码为一个参数的两种回调；缺失路径一律拒绝。
func claimedRequestPath(r *http.Request) string {
	for _, field := range strings.Split(r.URL.RawQuery, "&") {
		name, raw, _ := strings.Cut(field, "=")
		if name != paramRequestURI {
			continue
		}
		if !strings.HasPrefix(raw, "/") {
			var err error
			raw, err = url.QueryUnescape(raw)
			if err != nil {
				return ""
			}
		}
		if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
			return ""
		}
		uri, err := url.ParseRequestURI(raw)
		if err != nil || uri.IsAbs() || uri.Host != "" {
			return ""
		}
		return uri.EscapedPath()
	}
	return ""
}

// claimedClientIP 取 123 观测到的访客地址，即 $remote_addr。
//
// 可以直接采信，理由见 handleCDNAuth：伪造它换不到任何字节，123 问我们时用的
// 永远是它自己算出来的地址。
//
// 取**第一个**同名参数是有讲究的：访客能往直链上追加参数，而那些参数会随
// $request_uri 原样带过来。部署侧把 remote_addr 写在 request_uri 之前，注入的
// 地址就排在前面；写反了，访客追加的 &remote_addr= 会被采信，校验等于作废。
//
// 不回退到 TCP 对端地址：回源请求的对端永远是 123 自己，拿它做一致性校验只会
// 得到"永远不匹配"。
func claimedClientIP(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get(paramClaimedIP))
}
