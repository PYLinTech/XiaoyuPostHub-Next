// Package smtpd 是最小可用的入站 SMTP 协议服务器：EHLO/ESMTP SIZE、
// 可选 STARTTLS、每 IP 连接与速率限制，信封各阶段回调 mailin.Receiver。
// 它只负责"把字节变成信封 + 原始邮件"，不做任何存储与策略判断。
package smtpd

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailin"
)

const (
	defaultHostname        = "xiaoyuposthub"
	defaultMaxMessageBytes = int64(25 << 20)
	defaultMaxConnsPerIP   = 10
	defaultMsgsPerMinute   = 60
	defaultReadTimeout     = 5 * time.Minute
	// defaultConnTimeout 是单条连接的总时长上限。只读超时会逐次重置，
	// 对端只要持续慢速发字节就永远不触发——协程、socket 与 DATA 临时文件
	// 因此可以被无限期占住。总时长从 accept 起算，独立于每次读。
	defaultConnTimeout = 10 * time.Minute
	// defaultTempDir 留空时退回 os.TempDir。
	defaultTempDir = ""

	maxCommandLine = 4096
	maxDataLine    = 65536 // DATA 正文单行上限，超出即置 oversized 并丢弃到点行
	maxRecipients  = 100
	// maxCommandsPerConn 限制单条连接可发出的命令总数。MAIL FROM 会触发一次
	// SPF 求值（最多 10 次 DNS 查询），而 RSET 可以无限次重置事务——没有这个
	// 上限，30 字节的客户端流量就能换来对任意域的 DNS 放大。
	maxCommandsPerConn = 500
)

// Config 描述一台入站 SMTP 服务器的运行参数。
type Config struct {
	Receiver          mailin.Receiver
	Hostname          string        // EHLO 宣告名，默认 xiaoyuposthub
	MaxMessageBytes   int64         // <=0 用 25MiB
	MaxConnsPerIP     int           // <=0 用 10
	MessagesPerMinute int           // <=0 用 60
	ReadTimeout       time.Duration // <=0 用 5min（每次读的空闲超时）
	ConnTimeout       time.Duration // <=0 用 10min（单连接总时长上限）
	TempDir           string        // DATA 临时目录，空串用 os.TempDir
	TLSConfig         *tls.Config   // 非 nil 才宣告 STARTTLS
}

func (c Config) withDefaults() Config {
	if c.Hostname == "" {
		c.Hostname = defaultHostname
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = defaultMaxMessageBytes
	}
	if c.MaxConnsPerIP <= 0 {
		c.MaxConnsPerIP = defaultMaxConnsPerIP
	}
	if c.MessagesPerMinute <= 0 {
		c.MessagesPerMinute = defaultMsgsPerMinute
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = defaultReadTimeout
	}
	if c.ConnTimeout <= 0 {
		c.ConnTimeout = defaultConnTimeout
	}
	if c.TempDir == "" {
		c.TempDir = defaultTempDir
	}
	return c
}

// Server 接受 SMTP 入站连接。零值不可用，必须经 New 构造。
type Server struct {
	cfg Config
	lim *limiter
	now func() time.Time

	wg      sync.WaitGroup
	mu      sync.Mutex
	ln      net.Listener
	closed  bool
	closeCh chan struct{}
}

// New 构造服务器（尚未监听）。
func New(cfg Config) *Server {
	cfg = cfg.withDefaults()
	return &Server{
		cfg:     cfg,
		lim:     newLimiter(cfg.MaxConnsPerIP, cfg.MessagesPerMinute, time.Now),
		now:     time.Now,
		closeCh: make(chan struct{}),
	}
}

// Serve 在给定 listener 上接受连接直到 Shutdown 或 listener 失效。
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				s.wg.Wait()
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

// Shutdown 关闭 listener 并等待在途连接处理结束（或 ctx 超时）。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	s.mu.Unlock()
	close(s.closeCh)
	if ln != nil {
		_ = ln.Close()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// connState 是一条 SMTP 连接的会话状态。
type connState struct {
	remoteIP     string
	helo         string
	tls          bool
	mailSet      bool
	mailFrom     string
	declaredSize int64
	rcpts        []string
	seen         map[string]struct{}
	spfResult    string
	// commands 是本连接已处理过的命令数，用于封顶 SPF/DNS 放大。
	commands int
}

// readLine 读一行（含结尾换行）并做两件事：
//
//  1. 在**分配之前**封顶。bufio.Reader.ReadBytes 会为一条没有换行符的长行
//     持续追加分片并最终按总长再分配一次，因此"先读完再判断长度"等于
//     没有限制——对端不发换行就能让内存按已发送字节数增长。
//  2. 超限时丢弃该行余下字节直到换行，保持状态机与协议同步（否则残留字节
//     会被当成下一条命令解析）。
//
// 超长时返回 truncated=true，调用方据此回 500/置 oversized。
func readLine(r *bufio.Reader, max int) (line []byte, truncated bool, err error) {
	buf := make([]byte, 0, 256)
	for {
		frag, rerr := r.ReadSlice('\n')
		if len(frag) > 0 && !truncated {
			if room := max - len(buf); room > 0 {
				if len(frag) > room {
					buf = append(buf, frag[:room]...)
					truncated = true
				} else {
					buf = append(buf, frag...)
				}
			} else {
				truncated = true
			}
		}
		if rerr == nil {
			return buf, truncated, nil
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue // 分片已丢弃或已记账，继续读到换行
		}
		return buf, truncated, rerr
	}
}

func (st *connState) resetTransaction() {
	st.mailSet = false
	st.mailFrom = ""
	st.declaredSize = 0
	st.rcpts = nil
	st.seen = map[string]struct{}{}
	st.spfResult = ""
}

func (st *connState) resetSession() {
	st.helo = ""
	st.tls = false
	st.resetTransaction()
}

func (s *Server) handle(conn net.Conn) {
	defer s.wg.Done()
	remoteIP := ""
	if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
		remoteIP = host
	}

	if !s.lim.acquire(remoteIP) {
		fmt.Fprintf(conn, "421 %s too many connections from your address\r\n", s.cfg.Hostname)
		conn.Close()
		return
	}
	defer func() {
		s.lim.release(remoteIP)
		conn.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Fprintf(conn, "220 %s ESMTP XiaoyuPostHub-Next\r\n", s.cfg.Hostname)

	reader := bufio.NewReader(conn)
	st := &connState{remoteIP: remoteIP, seen: map[string]struct{}{}}

	// 连接总时长上限。readDeadline 只在"每次读"上重置，慢速持续发送的
	// 对端永远不触发；总时长从 accept 起算，独立于每一次读。
	connDeadline := time.Now().Add(s.cfg.ConnTimeout)
	// 关闭时中断在途连接：只关 listener 的话，已建立的连接会一直跑到
	// 自己的超时，设置热生效因此无法真正踢掉它们。
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-s.closeCh:
			_ = conn.Close()
		case <-watchDone:
		}
	}()

	for {
		if time.Now().After(connDeadline) {
			return
		}
		deadline := time.Now().Add(s.cfg.ReadTimeout)
		if deadline.After(connDeadline) {
			deadline = connDeadline
		}
		_ = conn.SetReadDeadline(deadline)
		line, truncated, err := readLine(reader, maxCommandLine)
		if err != nil {
			return // IO 错误 / 超时 / 对端关闭：静默结束
		}
		if truncated {
			fmt.Fprint(conn, "500 command line too long\r\n")
			continue
		}
		// 命令总数封顶：MAIL FROM 每次都会触发 SPF 求值（最多 10 次 DNS 查询），
		// 而 RSET 可无限次重置事务。没有这个上限，几十字节的客户端流量就能
		// 换来对任意域的持续 DNS 放大。
		st.commands++
		if st.commands > maxCommandsPerConn {
			fmt.Fprint(conn, "421 4.5.2 Too many commands\r\n")
			return
		}
		keepGoing, upConn, upReader := s.serveCommand(ctx, conn, reader, line, st)
		if upConn != nil {
			conn = upConn
		}
		if upReader != nil {
			reader = upReader
		}
		if !keepGoing {
			return
		}
	}
}

// serveCommand 处理一条命令。返回 keepGoing=false 表示应关闭连接；
// STARTTLS 成功时返回升级后的 conn/reader。
func (s *Server) serveCommand(ctx context.Context, conn net.Conn, reader *bufio.Reader,
	raw []byte, st *connState) (bool, net.Conn, *bufio.Reader) {
	line := strings.TrimRight(string(raw), "\r\n")
	if line == "" {
		fmt.Fprint(conn, "500 empty command\r\n")
		return true, nil, nil
	}
	verb, arg, _ := strings.Cut(line, " ")
	verb = strings.ToUpper(verb)
	arg = strings.TrimSpace(arg)

	switch verb {
	case "EHLO", "HELO":
		s.cmdGreet(conn, verb, arg, st)
	case "STARTTLS":
		if up, upr, ok := s.cmdStartTLS(conn, st); ok {
			if up == nil {
				return false, nil, nil // 握手失败：关连接
			}
			return true, up, upr
		}
	case "MAIL":
		s.cmdMail(ctx, conn, arg, st)
	case "RCPT":
		s.cmdRcpt(ctx, conn, arg, st)
	case "DATA":
		s.cmdData(ctx, conn, reader, st)
	case "RSET":
		st.resetTransaction()
		fmt.Fprint(conn, "250 2.0.0 Ok\r\n")
	case "NOOP":
		fmt.Fprint(conn, "250 2.0.0 Ok\r\n")
	case "VRFY":
		fmt.Fprint(conn, "252 Cannot VRFY\r\n")
	case "QUIT":
		fmt.Fprint(conn, "221 2.0.0 Bye\r\n")
		return false, nil, nil
	default:
		fmt.Fprint(conn, "502 5.5.1 Command not implemented\r\n")
	}
	return true, nil, nil
}

func (s *Server) cmdGreet(conn net.Conn, verb, arg string, st *connState) {
	if arg == "" {
		fmt.Fprint(conn, "501 5.5.2 Missing domain/address argument\r\n")
		return
	}
	// 重新 EHLO/HELO 开始一个新事务（RFC 5321）。
	st.helo = arg
	st.mailSet = false
	st.rcpts = nil
	st.seen = map[string]struct{}{}
	st.mailFrom = ""
	st.declaredSize = 0
	st.spfResult = ""

	if verb == "EHLO" {
		fmt.Fprintf(conn, "250-%s Hello %s\r\n", s.cfg.Hostname, arg)
		fmt.Fprintf(conn, "250-SIZE %d\r\n", s.cfg.MaxMessageBytes)
		fmt.Fprint(conn, "250-8BITMIME\r\n")
		if s.cfg.TLSConfig != nil && !st.tls {
			fmt.Fprint(conn, "250-STARTTLS\r\n")
		}
		fmt.Fprint(conn, "250 HELP\r\n")
	} else {
		fmt.Fprintf(conn, "250 %s Hello %s\r\n", s.cfg.Hostname, arg)
	}
}

// cmdStartTLS 返回 (tlsConn, reader, 是否进入了 TLS 流程)。
// ok=true 且 tlsConn=nil 表示握手失败应关连接。
func (s *Server) cmdStartTLS(conn net.Conn, st *connState) (net.Conn, *bufio.Reader, bool) {
	if s.cfg.TLSConfig == nil {
		fmt.Fprint(conn, "502 5.5.1 STARTTLS not offered\r\n")
		return nil, nil, false
	}
	if st.helo == "" {
		fmt.Fprint(conn, "503 5.5.1 Send EHLO first\r\n")
		return nil, nil, false
	}
	if st.tls {
		fmt.Fprint(conn, "503 5.5.1 TLS already active\r\n")
		return nil, nil, false
	}
	fmt.Fprint(conn, "220 2.0.0 Ready to start TLS\r\n")
	tlsConn := tls.Server(conn, s.cfg.TLSConfig.Clone())
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		return nil, nil, true
	}
	_ = tlsConn.SetDeadline(time.Time{})
	// 升级后信封清空，客户端必须重新 EHLO。
	st.resetSession()
	st.tls = true
	return net.Conn(tlsConn), bufio.NewReader(tlsConn), true
}

// parsePath 解析 `FROM:<addr> PARAM=...` / `TO:<addr>`，返回地址与剩余参数。
func parsePath(arg, keyword string) (addr string, params map[string]string, ok bool) {
	up := strings.ToUpper(arg)
	if !strings.HasPrefix(up, keyword+":") {
		return "", nil, false
	}
	rest := strings.TrimSpace(arg[len(keyword)+1:])
	start := strings.IndexByte(rest, '<')
	end := strings.IndexByte(rest, '>')
	if start < 0 || end < start {
		return "", nil, false
	}
	addr = strings.TrimSpace(rest[:start])
	if addr != "" {
		return "", nil, false
	}
	addr = rest[start+1 : end]
	params = map[string]string{}
	for _, p := range strings.Fields(rest[end+1:]) {
		k, v, has := strings.Cut(p, "=")
		if has {
			params[strings.ToUpper(k)] = v
		}
	}
	return addr, params, true
}

func (s *Server) cmdMail(ctx context.Context, conn net.Conn, arg string, st *connState) {
	if st.helo == "" {
		fmt.Fprint(conn, "503 5.5.1 Send EHLO first\r\n")
		return
	}
	if st.mailSet {
		fmt.Fprint(conn, "503 5.5.1 Nested MAIL command\r\n")
		return
	}
	from, params, ok := parsePath(arg, "FROM")
	if !ok {
		fmt.Fprint(conn, "501 5.5.4 Syntax: MAIL FROM:<address>\r\n")
		return
	}
	var size int64
	if v := params["SIZE"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fmt.Fprint(conn, "501 5.5.4 Invalid SIZE parameter\r\n")
			return
		}
		size = n
		if size > s.cfg.MaxMessageBytes {
			fmt.Fprintf(conn, "552 5.3.4 Message exceeds fixed size limit %d\r\n", s.cfg.MaxMessageBytes)
			return
		}
	}
	result, err := s.cfg.Receiver.CheckSender(ctx, st.remoteIP, st.helo, from)
	if err != nil {
		replyReceiverError(conn, err)
		return
	}
	st.mailSet = true
	st.mailFrom = from
	st.declaredSize = size
	st.spfResult = result
	fmt.Fprint(conn, "250 2.1.0 Sender ok\r\n")
}

func (s *Server) cmdRcpt(ctx context.Context, conn net.Conn, arg string, st *connState) {
	if !st.mailSet {
		fmt.Fprint(conn, "503 5.5.1 Send MAIL first\r\n")
		return
	}
	if len(st.rcpts) >= maxRecipients {
		fmt.Fprint(conn, "452 4.5.3 Too many recipients\r\n")
		return
	}
	addr, _, ok := parsePath(arg, "TO")
	if !ok {
		fmt.Fprint(conn, "501 5.5.4 Syntax: RCPT TO:<address>\r\n")
		return
	}
	key := strings.ToLower(strings.TrimSpace(addr))
	if _, dup := st.seen[key]; dup {
		// 重复收件人幂等回应，不重复记账。
		fmt.Fprint(conn, "250 2.1.5 Recipient ok (duplicate)\r\n")
		return
	}
	if err := s.cfg.Receiver.VerifyRecipient(ctx, addr, st.declaredSize); err != nil {
		replyReceiverError(conn, err)
		return
	}
	st.seen[key] = struct{}{}
	st.rcpts = append(st.rcpts, addr)
	fmt.Fprint(conn, "250 2.1.5 Recipient ok\r\n")
}

func (s *Server) cmdData(ctx context.Context, conn net.Conn, reader *bufio.Reader, st *connState) {
	if !st.mailSet {
		fmt.Fprint(conn, "503 5.5.1 Send MAIL first\r\n")
		return
	}
	if len(st.rcpts) == 0 {
		fmt.Fprint(conn, "503 5.5.1 No valid recipients\r\n")
		return
	}
	if !s.lim.allowMessage(st.remoteIP) {
		fmt.Fprint(conn, "451 4.7.1 Rate limit exceeded, try again later\r\n")
		return
	}
	fmt.Fprint(conn, "354 Start mail input; end with <CRLF>.<CRLF>\r\n")

	f, err := os.CreateTemp(s.cfg.TempDir, "smtpd-*")
	if err != nil {
		fmt.Fprint(conn, "451 4.3.0 Local spool unavailable\r\n")
		st.resetTransaction()
		return
	}
	tempName := f.Name()
	defer os.Remove(tempName)

	var written int64
	oversized := false
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		// DATA 正文同样要在缓冲前封顶：这里原本完全没有行长上限，一条超长
		// 行会让 bufio 按已收到的字节数持续分配。超限即置 oversized 并把
		// 该行丢弃到换行，保持与点行协议的同步。
		raw, truncated, rerr := readLine(reader, maxDataLine)
		if rerr != nil {
			// 对端在点行之前断开：丢弃半成品，连接随之关闭。
			f.Close()
			return
		}
		if truncated {
			oversized = true
			continue
		}
		body := raw
		crlf := ""
		switch {
		case strings.HasSuffix(string(body), "\r\n"):
			body = body[:len(body)-2]
			crlf = "\r\n"
		case strings.HasSuffix(string(body), "\n"):
			body = body[:len(body)-1]
			crlf = "\n"
		}
		if string(body) == "." {
			break
		}
		if len(body) > 0 && body[0] == '.' {
			body = body[1:] // 点消除
		}
		if !oversized {
			n, werr := f.Write(body)
			written += int64(n)
			if crlf != "" {
				nn, _ := f.WriteString(crlf)
				written += int64(nn)
			}
			if werr != nil || written > s.cfg.MaxMessageBytes {
				oversized = true
			}
		}
	}
	if oversized {
		f.Close()
		fmt.Fprintf(conn, "552 5.3.4 Message exceeds size limit %d\r\n", s.cfg.MaxMessageBytes)
		st.resetTransaction()
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		fmt.Fprint(conn, "451 4.3.0 Local spool error\r\n")
		st.resetTransaction()
		return
	}
	rerr := s.cfg.Receiver.Receive(ctx, mailin.Envelope{
		RemoteIP:   st.remoteIP,
		Helo:       st.helo,
		MailFrom:   st.mailFrom,
		Recipients: append([]string(nil), st.rcpts...),
		SPFResult:  st.spfResult,
	}, f)
	f.Close()
	if rerr != nil {
		replyReceiverError(conn, rerr)
	} else {
		fmt.Fprint(conn, "250 2.0.0 Ok queued\r\n")
	}
	st.resetTransaction()
}

// replyReceiverError 把 Receiver 的错误映射成 SMTP 应答：
// *StatusError 用其自带码，其它一律 451（让对方安全重试）。
func replyReceiverError(conn net.Conn, err error) {
	var se *mailin.StatusError
	if errors.As(err, &se) {
		fmt.Fprintf(conn, "%d %s\r\n", se.Code, sanitizeText(se.Message))
		return
	}
	fmt.Fprint(conn, "451 4.3.0 Local processing error\r\n")
}

// sanitizeText 防止业务错误文本里混入 CR/LF 做应答注入。
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}
