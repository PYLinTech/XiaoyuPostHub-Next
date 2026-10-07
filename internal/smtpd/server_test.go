package smtpd

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailin"
)

// fakeReceiver 按预设规则应答，并记录落信调用。
type fakeReceiver struct {
	mu          sync.Mutex
	verifyErr   map[string]error
	checkResult string
	checkErr    error
	receiveErr  error
	verifyCalls []string
	envs        []mailin.Envelope
	raws        []string
}

func (f *fakeReceiver) VerifyRecipient(_ context.Context, address string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyCalls = append(f.verifyCalls, strings.ToLower(address))
	if err := f.verifyErr[strings.ToLower(address)]; err != nil {
		return err
	}
	return nil
}

func (f *fakeReceiver) CheckSender(_ context.Context, _, _, _ string) (string, error) {
	return f.checkResult, f.checkErr
}

func (f *fakeReceiver) Receive(_ context.Context, env mailin.Envelope, raw io.Reader) error {
	data, _ := io.ReadAll(raw)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envs = append(f.envs, env)
	f.raws = append(f.raws, string(data))
	if f.receiveErr != nil {
		return f.receiveErr
	}
	return nil
}

func (f *fakeReceiver) snapshot() ([]mailin.Envelope, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	envs := append([]mailin.Envelope(nil), f.envs...)
	raws := append([]string(nil), f.raws...)
	calls := append([]string(nil), f.verifyCalls...)
	return envs, raws, calls
}

func startServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	if cfg.Receiver == nil {
		cfg.Receiver = &fakeReceiver{}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	go func() { _ = srv.Serve(ln) }()
	return srv, ln.Addr().String()
}

type smtpClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialSmtp(t *testing.T, addr string) *smtpClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c := &smtpClient{conn: conn, r: bufio.NewReader(conn)}
	c.expect(t, "220")
	return c
}

func (c *smtpClient) send(t *testing.T, format string, args ...any) {
	t.Helper()
	line := fmt.Sprintf(format, args...)
	if _, err := c.conn.Write([]byte(strings.TrimRight(line, "\r\n") + "\r\n")); err != nil {
		t.Fatal(err)
	}
}

func (c *smtpClient) expect(t *testing.T, prefix string) string {
	t.Helper()
	line, err := c.r.ReadString('\n')
	if err != nil {
		t.Fatalf("读应答（期望 %s）失败: %v", prefix, err)
	}
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("应答 = %q，期望前缀 %s", strings.TrimRight(line, "\r\n"), prefix)
	}
	return line
}

// readEhlo 读取 EHLO 多行应答，返回全部行。
func (c *smtpClient) readEhlo(t *testing.T) []string {
	t.Helper()
	var lines []string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.TrimRight(line, "\r\n"))
		if len(line) >= 4 && line[3] == ' ' {
			return lines
		}
	}
}

func (c *smtpClient) close() { c.conn.Close() }

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}}
}

func TestSMTPServerHappyPath(t *testing.T) {
	rcv := &fakeReceiver{checkResult: "pass"}
	srv, addr := startServer(t, Config{Receiver: rcv, Hostname: "hub.example"})
	defer srv.Shutdown(context.Background())

	c := dialSmtp(t, addr)
	defer c.close()

	c.send(t, "EHLO mta.sender.example")
	ads := c.readEhlo(t)
	joined := strings.Join(ads, "\n")
	for _, want := range []string{"250-hub.example", "250-SIZE 26214400", "250-8BITMIME"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("EHLO 宣告缺少 %q:\n%s", want, joined)
		}
	}

	c.send(t, "MAIL FROM:<sender@sender.example> SIZE=123")
	c.expect(t, "250")
	c.send(t, "RCPT TO:<alice@example.com>")
	c.expect(t, "250")
	// 大小写不同的重复收件人：不重复校验、不重复进信封。
	c.send(t, "RCPT TO:<ALICE@example.com>")
	c.expect(t, "250")
	c.send(t, "RCPT TO:<bob@example.com>")
	c.expect(t, "250")
	c.send(t, "DATA")
	c.expect(t, "354")
	c.conn.Write([]byte("Subject: hello\r\nline one\r\n..dot stays\r\n.\r\n"))
	c.expect(t, "250")
	c.send(t, "QUIT")
	c.expect(t, "221")

	envs, raws, calls := rcv.snapshot()
	if len(envs) != 1 {
		t.Fatalf("落信次数 = %d，期望 1", len(envs))
	}
	env := envs[0]
	if env.Helo != "mta.sender.example" || env.MailFrom != "sender@sender.example" ||
		env.SPFResult != "pass" || len(env.Recipients) != 2 {
		t.Fatalf("信封错误: %+v", env)
	}
	if env.RemoteIP != "127.0.0.1" {
		t.Fatalf("RemoteIP = %q", env.RemoteIP)
	}
	if want := "Subject: hello\r\nline one\r\n.dot stays\r\n"; raws[0] != want {
		t.Fatalf("点消除后原文 = %q，期望 %q", raws[0], want)
	}
	if len(calls) != 2 {
		t.Fatalf("校验调用次数 = %d（重复地址不应再调）: %v", len(calls), calls)
	}
}

func TestSMTPServerRejectCodes(t *testing.T) {
	rcv := &fakeReceiver{
		verifyErr: map[string]error{
			"bad@example.com":  mailin.Reject(550, "no such user"),
			"full@example.com": mailin.Reject(452, "mailbox full"),
		},
		checkResult: "fail",
		checkErr:    mailin.Reject(550, "spf fail"),
	}
	srv, addr := startServer(t, Config{Receiver: rcv})
	defer srv.Shutdown(context.Background())
	c := dialSmtp(t, addr)
	defer c.close()
	c.send(t, "EHLO h")
	c.readEhlo(t)

	c.send(t, "MAIL FROM:<x@bad.example>")
	c.expect(t, "550")

	rcv.checkErr = nil
	rcv.checkResult = "temperror"
	rcv.checkErr = nil
	c.send(t, "MAIL FROM:<x@ok.example>")
	c.expect(t, "250")

	c.send(t, "RCPT TO:<bad@example.com>")
	c.expect(t, "550")
	c.send(t, "RCPT TO:<full@example.com>")
	c.expect(t, "452")
	c.send(t, "RCPT TO:<good@example.com>")
	c.expect(t, "250")

	// 普通 error 映射 451。
	rcv.receiveErr = io.ErrUnexpectedEOF
	c.send(t, "DATA")
	c.expect(t, "354")
	c.conn.Write([]byte("x\r\n.\r\n"))
	c.expect(t, "451")
}

// TestSMTPServerOversizeBodyLineAtCap 验证正好等于 maxDataLine 的正文行
// 仍被正常接收（上限是"包含"语义，不是"少一个字节"）。
func TestSMTPServerOversizeBodyLineAtCap(t *testing.T) {
	rcv := &fakeReceiver{}
	srv, addr := startServer(t, Config{Receiver: rcv, MaxMessageBytes: 8 << 20})
	defer srv.Shutdown(context.Background())

	c := dialSmtp(t, addr)
	c.send(t, "EHLO test.example")
	c.readEhlo(t)
	c.send(t, "MAIL FROM:<s@example.org>")
	c.expect(t, "250")
	c.send(t, "RCPT TO:<r@example.org>")
	c.expect(t, "250")
	c.send(t, "DATA")
	c.expect(t, "354")

	// 整行（含 CRLF）恰好 maxDataLine 字节——上限是"包含"语义。
	c.send(t, "%s", strings.Repeat("D", maxDataLine-2))
	c.send(t, ".")
	c.expect(t, "250")
	c.close()
	if envs, _, _ := rcv.snapshot(); len(envs) != 1 {
		t.Fatalf("上限内的邮件应正常落信，实际 %d 封", len(envs))
	}
}

func TestSMTPServerOversize(t *testing.T) {
	rcv := &fakeReceiver{}
	srv, addr := startServer(t, Config{Receiver: rcv, MaxMessageBytes: 64})
	defer srv.Shutdown(context.Background())
	c := dialSmtp(t, addr)
	defer c.close()
	c.send(t, "EHLO h")
	c.readEhlo(t)
	// MAIL 上声明超限：直接 552。
	c.send(t, "MAIL FROM:<a@b.example> SIZE=100")
	c.expect(t, "552")
	// 实体超限：读完 DATA 后 552，不调 Receive。
	c.send(t, "MAIL FROM:<a@b.example>")
	c.expect(t, "250")
	c.send(t, "RCPT TO:<z@example.com>")
	c.expect(t, "250")
	c.send(t, "DATA")
	c.expect(t, "354")
	c.conn.Write([]byte(strings.Repeat("x", 100) + "\r\n.\r\n"))
	c.expect(t, "552")
	envs, _, _ := rcv.snapshot()
	if len(envs) != 0 {
		t.Fatalf("超限邮件不应落信，得到 %d 封", len(envs))
	}
}

func TestSMTPServerTransactionOrder(t *testing.T) {
	srv, addr := startServer(t, Config{})
	defer srv.Shutdown(context.Background())
	c := dialSmtp(t, addr)
	defer c.close()

	// 未 EHLO 先 MAIL。
	c.send(t, "MAIL FROM:<a@b.example>")
	c.expect(t, "503")
	c.send(t, "EHLO h")
	c.readEhlo(t)
	// 未 MAIL 先 RCPT/DATA。
	c.send(t, "RCPT TO:<a@example.com>")
	c.expect(t, "503")
	c.send(t, "DATA")
	c.expect(t, "503")
	// 重复 MAIL。
	c.send(t, "MAIL FROM:<a@b.example>")
	c.expect(t, "250")
	c.send(t, "MAIL FROM:<a@b.example>")
	c.expect(t, "503")
	// RSET 后可重新交易。
	c.send(t, "RSET")
	c.expect(t, "250")
	c.send(t, "MAIL FROM:<a@b.example>")
	c.expect(t, "250")
	c.send(t, "RCPT TO:<a@example.com>")
	c.expect(t, "250")
}

func TestSMTPServerSTARTTLS(t *testing.T) {
	rcv := &fakeReceiver{}
	cfgTLS := selfSignedTLS(t)
	srv, addr := startServer(t, Config{Receiver: rcv, TLSConfig: cfgTLS})
	defer srv.Shutdown(context.Background())

	c := dialSmtp(t, addr)
	defer c.close()
	c.send(t, "EHLO plain.example")
	if !strings.Contains(strings.Join(c.readEhlo(t), "\n"), "250-STARTTLS") {
		t.Fatal("明文 EHLO 应宣告 STARTTLS")
	}
	c.send(t, "STARTTLS")
	c.expect(t, "220")
	tlsConn := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	c.conn = tlsConn
	c.r = bufio.NewReader(tlsConn)

	// 升级后须重新 EHLO，且不再宣告 STARTTLS。
	c.send(t, "EHLO secure.example")
	ads := strings.Join(c.readEhlo(t), "\n")
	if strings.Contains(ads, "STARTTLS") {
		t.Fatalf("TLS 连接上不应再宣告 STARTTLS:\n%s", ads)
	}
	c.send(t, "MAIL FROM:<a@b.example>")
	c.expect(t, "250")
	c.send(t, "RCPT TO:<z@example.com>")
	c.expect(t, "250")
	c.send(t, "DATA")
	c.expect(t, "354")
	c.conn.Write([]byte("secure body\r\n.\r\n"))
	c.expect(t, "250")
	envs, _, _ := rcv.snapshot()
	if len(envs) != 1 {
		t.Fatalf("TLS 通道应正常落信，得到 %d", len(envs))
	}

	// 未配置 TLS 的服务器：STARTTLS → 502。
	srv2, addr2 := startServer(t, Config{})
	defer srv2.Shutdown(context.Background())
	c2 := dialSmtp(t, addr2)
	defer c2.close()
	c2.send(t, "EHLO h")
	c2.readEhlo(t)
	c2.send(t, "STARTTLS")
	c2.expect(t, "502")
}

func TestSMTPServerLimits(t *testing.T) {
	// 每 IP 2 连接。
	srv, addr := startServer(t, Config{MaxConnsPerIP: 2, MessagesPerMinute: 3})
	defer srv.Shutdown(context.Background())

	c1 := dialSmtp(t, addr)
	defer c1.close()
	c2 := dialSmtp(t, addr)
	defer c2.close()
	conn3, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn3.Close()
	r3 := bufio.NewReader(conn3)
	line, _ := r3.ReadString('\n')
	if !strings.HasPrefix(line, "421") {
		t.Fatalf("第 3 个连接应 421，得到 %q", line)
	}

	// 每分钟 3 封：第 4 封在 DATA 前 451。
	c1.send(t, "EHLO h")
	c1.readEhlo(t)
	deliver := func() string {
		c1.send(t, "MAIL FROM:<a@b.example>")
		c1.expect(t, "250")
		c1.send(t, "RCPT TO:<z@example.com>")
		c1.expect(t, "250")
		c1.send(t, "DATA")
		line, err := c1.r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "354") {
			c1.conn.Write([]byte("m\r\n.\r\n"))
			c1.expect(t, "250")
			return "250"
		}
		if !strings.HasPrefix(line, "451") {
			t.Fatalf("期望 354/451，得到 %q", line)
		}
		return "451"
	}
	for i := 0; i < 3; i++ {
		if got := deliver(); got != "250" {
			t.Fatalf("第 %d 封应成功", i+1)
		}
	}
	if got := deliver(); got != "451" {
		t.Fatalf("第 4 封应被速率限制 451，得到 %s", got)
	}
}

func TestSMTPServerMiscCommands(t *testing.T) {
	srv, addr := startServer(t, Config{})
	defer srv.Shutdown(context.Background())
	c := dialSmtp(t, addr)
	defer c.close()
	c.send(t, "NOOP")
	c.expect(t, "250")
	c.send(t, "VRFY x")
	c.expect(t, "252")
	c.send(t, "BOGUS")
	c.expect(t, "502")
	c.send(t, "QUIT")
	c.expect(t, "221")
	// QUIT 后再写应失败（连接已关）。
	if err := c.conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.r.ReadString('\n'); err == nil {
		t.Fatal("QUIT 后连接应已关闭")
	}
}

func TestSMTPServerShutdown(t *testing.T) {
	srv, addr := startServer(t, Config{})

	c := dialSmtp(t, addr)
	go func() {
		time.Sleep(100 * time.Millisecond)
		srv.Shutdown(context.Background())
	}()
	c.send(t, "QUIT")
	c.expect(t, "221")
	c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 应干净返回: %v", err)
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("Shutdown 后不应再接受新连接")
	}
}

// TestOverlongCommandLineRejected 验证超长命令行被拒收，且状态机不被打乱。
// 关键在于"在缓冲之前封顶"：一条没有换行符的超长行如果先读完再判长度，
// 内存会按已发送字节数增长（这里用一个远大于 maxCommandLine 的载荷复现）。
func TestOverlongCommandLineRejected(t *testing.T) {
	srv, addr := startServer(t, Config{})
	defer srv.Shutdown(context.Background())

	c := dialSmtp(t, addr)
	c.send(t, "EHLO test.example")
	c.readEhlo(t)

	// 一条 8MiB 的 NOOP（无换行结尾），远超 maxCommandLine。
	junk := strings.Repeat("A", 8<<20)
	c.send(t, "NOOP %s", junk)
	c.expect(t, "500")
	c.close()
}

// TestOverlongLineDoesNotDesyncState 验证丢弃超长行后，协议状态仍然同步：
// 紧随其后的正常命令必须被正常解析，而不是把超长行的残留当成命令。
func TestOverlongLineDoesNotDesyncState(t *testing.T) {
	srv, addr := startServer(t, Config{})
	defer srv.Shutdown(context.Background())

	c := dialSmtp(t, addr)
	c.send(t, "EHLO test.example")
	c.readEhlo(t)

	c.send(t, "NOOP %s", strings.Repeat("B", maxCommandLine+4096))
	c.expect(t, "500")

	// 状态机仍应工作：EHLO 之后 NOOP 必须得到 250。
	c.send(t, "NOOP")
	c.expect(t, "250")
	c.close()
}

// TestCommandBudgetCapsSPFAmplification 验证单连接命令数封顶生效：
// MAIL FROM 会触发一次 SPF 求值，RSET 可无限次重置事务，没有封顶时
// 少量客户端流量就能换来对任意域的持续 DNS 放大。
func TestCommandBudgetCapsSPFAmplification(t *testing.T) {
	rcv := &fakeReceiver{checkResult: "pass"}
	srv, addr := startServer(t, Config{Receiver: rcv})
	defer srv.Shutdown(context.Background())

	c := dialSmtp(t, addr)
	c.send(t, "EHLO test.example")
	c.readEhlo(t)

	sawLimit := false
	for i := 0; i < maxCommandsPerConn+20; i++ {
		c.send(t, "MAIL FROM:<probe%d@example.org>", i)
		line, err := c.r.ReadString('\n')
		if err != nil {
			sawLimit = true // 服务端直接断开同样表示封顶生效
			break
		}
		if strings.HasPrefix(line, "421") {
			sawLimit = true
			break
		}
		c.send(t, "RSET")
		if _, err := c.r.ReadString('\n'); err != nil {
			sawLimit = true
			break
		}
	}
	c.close()
	if !sawLimit {
		t.Fatal("命令数超过上限后应回 421 并断开连接")
	}
	// 封顶后 CheckSender 不应再被调用：DNS 放大到此为止。
	_, _, calls := rcv.snapshot()
	if len(calls) > maxCommandsPerConn {
		t.Fatalf("CheckSender 调用次数 %d 超过连接命令上限 %d", len(calls), maxCommandsPerConn)
	}
}

// TestOverlongDataLineRejected 验证 DATA 正文里的超长行被丢弃并按超限处理。
// 原实现对 DATA 行完全没有行长上限，一条超长行会让缓冲按已收字节增长。
func TestOverlongDataLineRejected(t *testing.T) {
	rcv := &fakeReceiver{}
	srv, addr := startServer(t, Config{Receiver: rcv, MaxMessageBytes: 64 << 20})
	defer srv.Shutdown(context.Background())

	c := dialSmtp(t, addr)
	c.send(t, "EHLO test.example")
	c.readEhlo(t)
	c.send(t, "MAIL FROM:<s@example.org>")
	c.expect(t, "250")
	c.send(t, "RCPT TO:<r@example.org>")
	c.expect(t, "250")
	c.send(t, "DATA")
	c.expect(t, "354")

	// 一条 4MiB 的单行（大于 maxDataLine）。
	c.send(t, "X%s", strings.Repeat("C", 4<<20))
	c.send(t, ".")
	// 超限应回 552，且不落信。
	c.expect(t, "552")
	c.close()
	if envs, _, _ := rcv.snapshot(); len(envs) != 0 {
		t.Fatalf("超限邮件不应落信，实际 %d 封", len(envs))
	}
}

// TestLimiterReleasesIdleBuckets 验证空闲桶会被回收：限流表不应随
// "曾经连过的来源 IP 数"无界增长（IPv6 下单个 /64 就能派生出海量来源）。
func TestLimiterReleasesIdleBuckets(t *testing.T) {
	l := newLimiter(10, 60, time.Now)
	for i := 0; i < 500; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		if !l.acquire(ip) {
			t.Fatalf("第 %d 次 acquire 应成功", i)
		}
		l.release(ip)
	}
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("连接结束且无投递记录后限流表应为空，实际残留 %d 个桶", n)
	}
}

// TestLimiterEvictsWhenFull 验证桶数触顶时回收空闲桶而不是无界增长。
func TestLimiterEvictsWhenFull(t *testing.T) {
	l := newLimiter(10, 60, time.Now)
	for i := 0; i < maxTrackedIPs; i++ {
		if !l.acquire(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)) {
			t.Fatalf("第 %d 次 acquire 不应被拒", i)
		}
	}
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n > maxTrackedIPs {
		t.Fatalf("限流表 %d 个桶，超过上限 %d", n, maxTrackedIPs)
	}
	// 全部释放后新来源仍可接入（说明回收过）。
	for i := 0; i < maxTrackedIPs; i++ {
		l.release(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
	}
	if !l.acquire("203.0.113.9") {
		t.Fatal("表满回收后新来源应能接入")
	}
}

// TestConnTimeoutBoundsSlowClient 验证单连接总时长上限：只读超时会逐次重置，
// 慢速持续发送的对端永远不触发；总时长必须独立于每次读生效。
func TestConnTimeoutBoundsSlowClient(t *testing.T) {
	srv, addr := startServer(t, Config{ConnTimeout: 300 * time.Millisecond, ReadTimeout: 10 * time.Second})
	defer srv.Shutdown(context.Background())

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// 每 100ms 发一个 NOOP（每次都远小于 ReadTimeout），持续到总时长耗尽。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := conn.Write([]byte("NOOP\r\n")); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	// 读应答直到服务端因总时长上限而断开。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := r.ReadString('\n'); err != nil {
			return // 被断开，符合预期
		}
	}
	t.Fatal("超过单连接总时长上限后服务端应断开连接")
}
