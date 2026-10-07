// Package mailparse 把一封入站 RFC822/MIME 邮件解析为入库所需的中立结构：
// 头部字段、text/html 与 text/plain 正文、以及已做传输解码并落临时盘的
// 附件 / 内嵌图。
//
// 刻意不做的事（M2 边界）：
//   - 不做字符集转码（项目纯标准库，无 x/text）：正文按原始字节保留，
//     只保证 UTF-8 / US-ASCII 语义；非 UTF-8 邮件在前端按声明处理；
//   - 不验 DKIM / 不改写内容 / 不折叠头部；
//   - 不做 HTML 清洗（阅读链 M3 在沙箱里渲染）。
package mailparse

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// maxDepth 限制 MIME 嵌套层数，防止恶意嵌套耗尽栈。
	maxDepth = 32
	// maxLeafParts 限制叶子部件总数。
	maxLeafParts = 200
)

// PartKind 是落盘部件的种类。
type PartKind string

const (
	// PartAttachment 是普通附件。
	PartAttachment PartKind = "attachment"
	// PartInline 是带 Content-ID 的内嵌资源（multipart/related 引用）。
	PartInline PartKind = "inline"
)

// Part 是一个已传输解码、落临时盘的二进制部件。
type Part struct {
	Kind        PartKind
	FileName    string
	ContentType string
	ContentID   string
	// Path 是临时文件路径，Size 是解码后字节数。
	Path string
	Size int64
}

// Message 是解析结果。
type Message struct {
	Subject     string
	FromName    string
	FromAddress string
	ReplyTo     string
	// ToRaw/CcRaw/BccRaw 是未展开的原始收件人头，由调用方按需 ParseAddressList。
	ToRaw  string
	CcRaw  string
	BccRaw string

	// MessageID 已剥掉尖括号；ThreadRoot 取 References 第一项否则 InReplyTo。
	MessageID  string
	InReplyTo  string
	ThreadRoot string

	// SentAt 取 Date 头；缺失或非法时由 Parse 填当前时间。
	SentAt time.Time

	// HTML / Text 是已传输解码的正文（第一份对应类型）。
	HTML []byte
	Text []byte

	// Parts 按出现顺序排列，不含正文。
	Parts []Part
}

// Parse 解析 RFC822 字节流。tempDir 用于存放部件临时文件。
// 返回的 cleanup 无论成功失败都会清理已产生的临时文件。
func Parse(raw io.Reader, tempDir string) (m *Message, cleanup func(), err error) {
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, func() {}, fmt.Errorf("创建邮件临时目录失败: %w", err)
	}
	msg, err := mail.ReadMessage(raw)
	if err != nil {
		return nil, func() {}, fmt.Errorf("读取邮件头失败: %w", err)
	}

	m = &Message{SentAt: time.Now()}
	paths := make([]string, 0, 4)
	cleanup = func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	dec := new(mime.WordDecoder)
	m.Subject = decodeHeader(dec, msg.Header.Get("Subject"))
	m.FromName, m.FromAddress = parseFrom(dec, msg.Header.Get("From"))
	m.ReplyTo = parseReplyTo(msg.Header.Get("Reply-To"))
	m.ToRaw = msg.Header.Get("To")
	m.CcRaw = msg.Header.Get("Cc")
	m.BccRaw = msg.Header.Get("Bcc")
	m.MessageID = stripAngles(msg.Header.Get("Message-ID"))
	m.InReplyTo = stripAngles(msg.Header.Get("In-Reply-To"))
	m.ThreadRoot = firstReference(msg.Header.Get("References"))
	if m.ThreadRoot == "" {
		m.ThreadRoot = m.InReplyTo
	}
	if when, dateErr := msg.Header.Date(); dateErr == nil {
		m.SentAt = when
	}

	leafCount := 0
	if err = m.walk(dec, msg.Header, msg.Body, 1, &leafCount, &paths, tempDir); err != nil {
		return nil, cleanup, err
	}
	return m, cleanup, nil
}

// walk 递归处理一个 MIME 实体。
func (m *Message) walk(dec *mime.WordDecoder, h headerReader, body io.Reader,
	depth int, leafCount *int, paths *[]string, tempDir string) error {
	if depth > maxDepth {
		return fmt.Errorf("MIME 嵌套超过 %d 层", maxDepth)
	}
	mediaType, params := parseContentType(h.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart 缺少 boundary")
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("读取 multipart 部件失败: %w", err)
			}
			err = m.walk(dec, part.Header, part, depth+1, leafCount, paths, tempDir)
			// Close 负责排空未读完的部分，保证下一个 NextPart 边界正确。
			_ = part.Close()
			if err != nil {
				return err
			}
		}
	}

	(*leafCount)++
	if *leafCount > maxLeafParts {
		return fmt.Errorf("部件数量超过 %d 个", maxLeafParts)
	}

	disposition, dispParams := parseDisposition(h.Get("Content-Disposition"))
	// Content-ID 语法自带尖括号，而 HTML 里的 cid: 引用不带，统一剥掉。
	contentID := stripAngles(h.Get("Content-Id"))
	decoded := decodeTransfer(body, h.Get("Content-Transfer-Encoding"))

	// 文本正文：text/plain、text/html，且没有显式 attachment 处置。
	if disposition != "attachment" {
		switch mediaType {
		case "text/plain":
			if m.Text == nil {
				data, err := io.ReadAll(decoded)
				if err != nil {
					return fmt.Errorf("读取纯文本正文失败: %w", err)
				}
				m.Text = data
				return nil
			}
		case "text/html":
			if m.HTML == nil {
				data, err := io.ReadAll(decoded)
				if err != nil {
					return fmt.Errorf("读取 HTML 正文失败: %w", err)
				}
				m.HTML = data
				return nil
			}
		}
	}

	// 其余一切落盘成附件 / 内嵌图。带 cid 的 related 资源即使没写
	// Content-Disposition 也算内嵌图（常见客户端就这么发）。
	name := dispParams["filename"]
	if name == "" {
		name = params["name"]
	}
	kind := PartAttachment
	if contentID != "" {
		kind = PartInline
	}
	if name == "" {
		name = "attachment.bin"
	}
	name = filepath.Base(name)

	f, err := os.CreateTemp(tempDir, "part-*")
	if err != nil {
		return fmt.Errorf("创建部件临时文件失败: %w", err)
	}
	n, copyErr := io.Copy(f, decoded)
	syncErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("写入部件 %q 失败: %w", name, copyErr)
	}
	if syncErr != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("部件 %q 落盘失败: %w", name, syncErr)
	}
	*paths = append(*paths, f.Name())
	m.Parts = append(m.Parts, Part{
		Kind:        kind,
		FileName:    name,
		ContentType: mediaType,
		ContentID:   contentID,
		Path:        f.Name(),
		Size:        n,
	})
	return nil
}

// headerReader 是 textproto.MIMEHeader 与 mail.Header 的共同形状。
type headerReader interface {
	Get(key string) string
}

// decodeTransfer 按 Content-Transfer-Encoding 包一层解码器。
//
// base64 走宽容解码：标准解码器遇到第一个非法字节就返回 CorruptInputError，
// 而被截断或畸形编码的附件在真实世界里很常见（各家客户端的 bug、转发表中途
// 被裁剪）。让它一路失败会把**整封信**变成可重试的 451：既丢信，又让对方
// 反复重投。宽容模式跳过非法字符继续还原，语义是"尽力还原这个部件"，
// 而不是"因为一个附件坏了就拒收整封信"。
func decodeTransfer(r io.Reader, te string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(te)) {
	case "base64":
		return newLenientBase64Reader(r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	default:
		// 7bit / 8bit / binary / 未知：原样。
		return r
	}
}

// b64Alphabet 标记标准 base64 字母表内的字符。
var b64Alphabet = func() (t [256]bool) {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < len(alpha); i++ {
		t[alpha[i]] = true
	}
	return
}()

// lenientBase64Reader 逐字符跳过不在 base64 字母表内的字节（空白由调用方
// 的换行处理覆盖），并在每凑满 4 个有效字符时产出 3 字节。
//
// 不处理填充 '='：邮件体的 base64 常常没有或只有尾部 '='，忽略它能让被
// 裁剪的正文还原出可用前缀，而不是整段丢弃。
type lenientBase64Reader struct {
	src io.Reader
	buf [3]byte // 待输出的解码字节
	n   int     // buf 中的有效字节数
	pos int     // buf 中已消费的位置
	eof bool
	// pend 保存上次从 src 读到但还没凑进解码组的字节。必须跨 fill 保留：
	// 一次 Read 可能带回远超一个解码组的数据，丢掉余量就会少解内容。
	pend []byte
	// chunk 是复用的读缓冲，避免每组解码都分配一次。
	chunk [4096]byte
	// emptyReads 统计连续的空读，(0, nil) 连续达到上限即判定源不可用。
	emptyReads int
}

// maxEmptyReads 是允许的连续空读次数上限，仅用于防御病态 reader。
const maxEmptyReads = 1024

func newLenientBase64Reader(r io.Reader) io.Reader { return &lenientBase64Reader{src: r} }

func (l *lenientBase64Reader) Read(p []byte) (int, error) {
	for {
		if l.pos < l.n {
			n := copy(p, l.buf[l.pos:l.n])
			l.pos += n
			return n, nil
		}
		if l.fill() {
			l.pos = 0
			continue
		}
		// fill 已尽力：源读完且残留不足 1 字节（6 位无法成字节），到此为止。
		return 0, io.EOF
	}
}

// fill 从源里读到下一个解码组，产出解码字节到 l.buf。
// 返回 true 表示产出了字节（l.buf[l.pos:l.n] 有效）；false 表示再也产不出
// 字节，调用方应返回 EOF。
//
// 注意 l.eof 只是"源已读完"的内部标记，**不能**在 pend 里还有未解码字符时
// 让 Read 提前返回 EOF：multipart.Part 会在一次 Read 里同时交出全部数据
// 与 io.EOF（err 与 n 同时非零是合法的），若据此判定无数据可解，附件会被
// 截断到第一组。
func (l *lenientBase64Reader) fill() bool {
	var group [4]byte
	got := 0
	l.pos, l.n = 0, 0
	for got < 4 {
		for len(l.pend) > 0 && got < 4 {
			c := l.pend[0]
			l.pend = l.pend[1:]
			if b64Alphabet[c] {
				group[got] = c
				got++
			}
			// 换行、空白与任何畸形字符一律跳过，不中止解码。
		}
		if got == 4 {
			break
		}
		if l.eof {
			break
		}
		// (0, nil) 是合法的 Read 返回（multipart.Part 等包装 reader 会这样），
		// 只有 err != nil 才代表源真的读完。把 n==0 当 EOF 会让解码在
		// 第一次短读之后就提前收尾，附件内容被截断。
		n, err := l.src.Read(l.chunk[:])
		if n > 0 {
			l.pend = append(l.pend, l.chunk[:n]...)
			l.emptyReads = 0
		}
		if err != nil {
			// 标记源已读完但**不**退出：刚读进 pend 的字节还没参与解码，
			// 直接 break 会把它们连同这一组一起丢掉。
			l.eof = true
			continue
		}
		if n == 0 {
			// 防御病态 reader 反复返回 (0, nil) 造成的死循环。
			l.emptyReads++
			if l.emptyReads >= maxEmptyReads {
				l.eof = true
				continue
			}
		}
	}
	switch got {
	case 4:
		v := uint32(0)
		for i := 0; i < 4; i++ {
			v = v<<6 | uint32(b64Value(group[i]))
		}
		l.buf[0], l.buf[1], l.buf[2] = byte(v>>16), byte(v>>8), byte(v)
		l.n = 3
		return true
	case 2: // 1 个残留字节（6 位）
		v := uint32(b64Value(group[0]))<<6 | uint32(b64Value(group[1]))
		l.buf[0] = byte(v >> 4)
		l.n = 1
		return true
	case 3: // 2 个残留字节（12 位）
		v := uint32(b64Value(group[0]))<<12 |
			uint32(b64Value(group[1]))<<6 | uint32(b64Value(group[2]))
		l.buf[0], l.buf[1] = byte(v>>10), byte(v>>2)
		l.n = 2
		return true
	}
	// got==1（6 位）凑不出完整字节，丢弃。
	return false
}

func b64Value(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '+':
		return 62
	case c == '/':
		return 63
	default:
		return 0
	}
}

func parseContentType(raw string) (string, map[string]string) {
	mt, params, err := mime.ParseMediaType(raw)
	if err != nil {
		// 畸形头按纯文本兜底，保证整封邮件不被一个坏部件拖死。
		return "text/plain", map[string]string{}
	}
	return strings.ToLower(mt), params
}

func parseDisposition(raw string) (string, map[string]string) {
	if strings.TrimSpace(raw) == "" {
		return "", map[string]string{}
	}
	d, params, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", map[string]string{}
	}
	return strings.ToLower(d), params
}

func decodeHeader(dec *mime.WordDecoder, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	out, err := dec.DecodeHeader(raw)
	if err != nil {
		return raw
	}
	return out
}

func parseFrom(dec *mime.WordDecoder, raw string) (name, address string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	decoded := decodeHeader(dec, raw)
	if list, err := mail.ParseAddressList(decoded); err == nil && len(list) > 0 {
		return list[0].Name, list[0].Address
	}
	if a, err := mail.ParseAddress(decoded); err == nil {
		return a.Name, a.Address
	}
	// 彻底无法解析时保留裸值，发件人信息不能丢。
	return "", raw
}

func parseReplyTo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if list, err := mail.ParseAddressList(decodeHeader(new(mime.WordDecoder), raw)); err == nil && len(list) > 0 {
		return list[0].Address
	}
	return raw
}

// stripAngles 去掉 Message-ID / In-Reply-To 的尖括号与空白。
func stripAngles(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// firstReference 取 References 头的第一个 msg-id。
func firstReference(raw string) string {
	for _, f := range strings.Fields(raw) {
		if s := stripAngles(f); s != "" {
			return s
		}
	}
	return ""
}
