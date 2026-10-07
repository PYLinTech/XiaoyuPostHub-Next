// 入站邮件编排：RCPT 校验、SPF 策略、以及"解析 → 入池 → 配额 → 落信 →
// 流量账"的完整收件流程。它实现 mailin.Receiver，供 smtpd 在协议各阶段回调。
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/mail"
	"os"
	"regexp"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailin"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/mailparse"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/spf"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 编译期断言：*Service 必须满足协议层契约。
var _ mailin.Receiver = (*Service)(nil)

// inboundTarget 是一个通过校验的本地收件人解析结果。
// inboundTarget 是通过全部收件校验的一个投递目标。组不在这里单列：
// 用户与组是一对一，user.GroupName 就是它的组。
type inboundTarget struct {
	address store.MailAddress
	user    store.User
}

// ---------------------------------------------------------------- RCPT / SPF

// VerifyRecipient 校验单个 RCPT TO。
func (s *Service) VerifyRecipient(ctx context.Context, address string, declaredSize int64) error {
	t, err := s.resolveInboundTarget(ctx, address)
	if err != nil {
		return err
	}
	if declaredSize <= 0 {
		return nil
	}
	limit, limited, err := s.mailStorageLimit(ctx, t.user.GroupName)
	if err != nil {
		return err
	}
	if !limited {
		return nil
	}
	used, err := store.GetCounter(ctx, s.DB.R(),
		store.ScopeMailStorage, store.UserCounterKey(t.user.ID, ""))
	if err != nil {
		return fmt.Errorf("%w: 读取邮箱配额失败", ErrUnavailable)
	}
	if used+declaredSize > limit {
		return mailin.Reject(452, "收件人邮箱空间不足，请稍后让对方重试")
	}
	return nil
}

// CheckSender 按站点 SPF 策略在 MAIL FROM 后判定发件人。
func (s *Service) CheckSender(ctx context.Context, remoteIP, helo, mailFrom string) (string, error) {
	policy := s.Settings.Runtime(ctx).Mail.SPFPolicy
	if policy == settings.SPFOff {
		return "", nil
	}
	identity := spfIdentityDomain(mailFrom, helo)
	if identity == "" {
		// 无法取出发件域（空 HELO / IP 字面量）：没有可校验对象，记录后放行。
		return string(spf.None), nil
	}
	ip := net.ParseIP(strings.TrimSpace(remoteIP))
	if ip == nil {
		return string(spf.PermError), nil
	}
	res, _ := s.spfChecker().Check(ctx, identity, ip)
	switch res {
	case spf.Fail:
		if policy == settings.SPFHard {
			return string(res), mailin.Reject(550, "SPF 校验未通过，来信被拒收")
		}
		return string(res), nil
	case spf.TempError:
		return string(res), mailin.Reject(451, "SPF 临时校验失败，请稍后重试")
	default:
		return string(res), nil
	}
}

func (s *Service) spfChecker() *spf.Checker {
	return spf.NewChecker(s.spfResolver)
}

// spfIdentityDomain 取 SPF 校验身份：信封发件人域；空反向路径时退回 HELO 域。
func spfIdentityDomain(mailFrom, helo string) string {
	if a := strings.TrimSpace(mailFrom); a != "" {
		if _, domain, ok := strings.Cut(a, "@"); ok {
			return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
		}
	}
	h := strings.ToLower(strings.TrimSpace(helo))
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if h == "" || net.ParseIP(strings.TrimSuffix(h, ".")) != nil {
		return ""
	}
	// HELO 允许是本地主机名（无点），SPF 记录一般不存在，求值会得到 none。
	return strings.TrimSuffix(h, ".")
}

// resolveInboundTarget 把一个全址解析为"地址 + 用户"，任何一关不过
// 都按 550 永久拒收（让对方立刻退信而不是反复重试）。
func (s *Service) resolveInboundTarget(ctx context.Context, address string) (inboundTarget, error) {
	addr := normalizeEnvelopeAddress(address)
	if addr == "" || !strings.Contains(addr, "@") {
		return inboundTarget{}, mailin.Reject(550, "收件地址格式不正确")
	}
	a, err := store.GetMailAddress(ctx, s.DB.R(), addr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return inboundTarget{}, mailin.Reject(550, "收件地址不存在")
		}
		return inboundTarget{}, fmt.Errorf("%w: 读取收件地址失败", ErrUnavailable)
	}
	if a.Status != store.MailAddressActive {
		return inboundTarget{}, mailin.Reject(550, "该收件地址已暂停收件")
	}
	u, err := store.GetUserByID(ctx, s.DB.R(), a.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return inboundTarget{}, mailin.Reject(550, "收件账号不存在")
		}
		return inboundTarget{}, fmt.Errorf("%w: 读取收件账号失败", ErrUnavailable)
	}
	if u.Status == store.UserDisabled {
		return inboundTarget{}, mailin.Reject(550, "收件账号已停用")
	}
	// 授权的真正事实是"该地址所属用户的组，绑定了这个域名且该绑定开着收件"。
	// 绑定行的外键指向域名，因此查到绑定即证明域名存在，不必再单独查一次域名。
	// 地址的 status 只是这条事实的派生缓存，这里独立复核一次，使没跑过
	// SyncMailAddressStatus 的数据（旧库残留，或运维直接改 users.group_name）
	// 同样拒收，而不是只信任可能陈旧的 status。
	binding, err := store.GetGroupMailDomain(ctx, s.DB.R(), u.GroupName, a.Domain)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return inboundTarget{}, mailin.Reject(550, "该收件地址所属用户不在该域名的用户组中")
		}
		return inboundTarget{}, fmt.Errorf("%w: 读取收件域名绑定失败", ErrUnavailable)
	}
	if !binding.ReceiveEnabled {
		return inboundTarget{}, mailin.Reject(550, "该域名已暂停收件")
	}
	return inboundTarget{address: a, user: u}, nil
}

// mailStorageLimit 读某组邮件存储上限；无配额行（或 ≤0）表示不限。
func (s *Service) mailStorageLimit(ctx context.Context, group string) (int64, bool, error) {
	quotas, err := store.GroupQuotaMap(ctx, s.DB.R(), group)
	if err != nil {
		return 0, false, fmt.Errorf("%w: 读取邮件配额失败", ErrUnavailable)
	}
	limit, ok := quotas[store.QuotaMailStorageTotal]
	if !ok || limit <= 0 {
		return 0, false, nil
	}
	return limit, true, nil
}

// normalizeEnvelopeAddress 规范化信封地址：去尖括号/空白/显示名，全小写。
func normalizeEnvelopeAddress(raw string) string {
	a := strings.TrimSpace(raw)
	a = strings.TrimPrefix(a, "<")
	a = strings.TrimSuffix(a, ">")
	a = strings.TrimSpace(a)
	// 容忍个别客户端在信封里塞 "Name <a@b>" 的越界写法。
	if parsed, err := mail.ParseAddress(a); err == nil {
		a = parsed.Address
	}
	return strings.ToLower(a)
}

// ---------------------------------------------------------------- DATA 落信

// Receive 消费一封完整邮件并完成落信。
func (s *Service) Receive(ctx context.Context, env mailin.Envelope, raw io.Reader) error {
	if !s.EncryptionReady(ctx) || !s.StorageReady() {
		// 密钥或存储未就绪时必须临时拒收：信还没落盘，重试是安全且唯一正确的反应。
		return mailin.Reject(451, "邮件存储暂不可用，请稍后重试")
	}

	dir, err := s.tempSubdir("mailin")
	if err != nil {
		return err
	}
	parsed, cleanup, err := mailparse.Parse(raw, dir)
	if err != nil {
		return mailin.Reject(451, fmt.Sprintf("邮件解析失败: %v", err))
	}
	defer cleanup()

	// RFC Message-ID 幂等：对方网关重试同一封邮件不产生第二封。
	if parsed.MessageID != "" {
		if _, err := store.GetMailMessageByRFCID(ctx, s.DB.R(), parsed.MessageID); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 查重失败", ErrUnavailable)
		}
	}

	// ① 部件逐一入池（正文容器 + 附件/内嵌图）。物理去重由 IngestPlaintext
	// 保证：群发重复附件只存一份，但每个收件人仍各计一份明文字节。
	bodyRaw, err := EncodeMailBody(MailBody{HTML: string(parsed.HTML), Text: string(parsed.Text)})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	bodyFile, err := s.IngestPlaintext(ctx, bytes.NewReader(bodyRaw), 0)
	if err != nil {
		return mailin.Reject(451, "正文写入失败，请稍后重试")
	}
	totalPlain := bodyFile.SizePlain
	totalWire := bodyFile.PanSizeWire
	parts := []store.MailPart{{
		Seq:          0,
		Kind:         store.MailPartBody,
		ContentType:  "application/json",
		SizePlain:    bodyFile.SizePlain,
		FileChecksum: bodyFile.Checksum,
	}}
	for i, p := range parsed.Parts {
		f, openErr := os.Open(p.Path)
		if openErr != nil {
			return mailin.Reject(451, "读取附件失败")
		}
		ing, ingestErr := s.IngestPlaintext(ctx, f, 0)
		_ = f.Close()
		if ingestErr != nil {
			return mailin.Reject(451, "附件写入失败，请稍后重试")
		}
		kind := store.MailPartAttachment
		if p.Kind == mailparse.PartInline {
			kind = store.MailPartInline
		}
		parts = append(parts, store.MailPart{
			Seq:          i + 1,
			Kind:         kind,
			FileName:     p.FileName,
			ContentType:  p.ContentType,
			ContentID:    p.ContentID,
			SizePlain:    ing.SizePlain,
			FileChecksum: ing.Checksum,
		})
		totalPlain += ing.SizePlain
		totalWire += ing.PanSizeWire
	}

	// ② 收件人（头表照实存，归属行只给通过校验的本地用户）。
	recipients := buildHeaderRecipients(parsed)

	// ③ 解析本地归属并逐人预留配额。别名共享收件箱：同一用户在本域的
	// 多个地址只建一条 mailbox、只计一份。
	now := s.Now()
	var boxes []store.Mailbox
	var acceptedUsers []mailAcceptance
	userSeen := map[int64]struct{}{}
	for _, ra := range env.Recipients {
		t, verr := s.resolveInboundTarget(ctx, ra)
		if verr != nil {
			// RCPT 之后状态被改（禁用/移组/封域名）：跳过此人，
			// 其余本地收件人照常收；若最终无人可投由调用方退信。
			continue
		}
		if _, dup := userSeen[t.user.ID]; dup {
			continue
		}
		userSeen[t.user.ID] = struct{}{}
		key := store.UserCounterKey(t.user.ID, "")
		if limit, limited, lerr := s.mailStorageLimit(ctx, t.user.GroupName); lerr != nil {
			s.releaseMailStorage(ctx, acceptedUsers, totalPlain)
			return lerr
		} else if limited {
			if _, rerr := store.ReserveCounter(ctx, s.DB.W(),
				store.ScopeMailStorage, key, totalPlain, limit); rerr != nil {
				if errors.Is(rerr, store.ErrQuotaExceeded) {
					s.releaseMailStorage(ctx, acceptedUsers, totalPlain)
					return mailin.Reject(452, "收件人邮箱空间不足，请稍后让对方重试")
				}
				s.releaseMailStorage(ctx, acceptedUsers, totalPlain)
				return fmt.Errorf("%w: 预留邮件配额失败", ErrUnavailable)
			}
		} else {
			if _, aerr := store.AddCounter(ctx, s.DB.W(),
				store.ScopeMailStorage, key, totalPlain); aerr != nil {
				s.releaseMailStorage(ctx, acceptedUsers, totalPlain)
				return fmt.Errorf("%w: 记账失败", ErrUnavailable)
			}
		}
		acceptedUsers = append(acceptedUsers, mailAcceptance{uid: t.user.ID, group: t.user.GroupName})
		boxes = append(boxes, store.Mailbox{
			UserID:       t.user.ID,
			Role:         store.MailboxRoleInbox,
			Status:       store.MailboxStatusNormal,
			ChargedBytes: totalPlain,
			CreatedAt:    now,
		})
	}
	if len(boxes) == 0 {
		// 所有 RCPT 都在 DATA 期间失效：临时失败，对方重试时会重新走 RCPT。
		return mailin.Reject(451, "当前无有效收件人，请稍后重试")
	}

	// ④ 单事务落信（message + recipients + parts/AddFileRef + mailboxes）。
	id, err := store.NewID("mm")
	if err != nil {
		s.releaseMailStorage(ctx, acceptedUsers, totalPlain)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	sentAt := parsed.SentAt.Unix()
	if sentAt <= 0 {
		sentAt = now
	}
	msg := store.MailMessage{
		ID:              id,
		MessageID:       parsed.MessageID,
		FromName:        parsed.FromName,
		FromAddress:     strings.ToLower(parsed.FromAddress),
		Subject:         parsed.Subject,
		Snippet:         buildSnippet(string(parsed.Text), string(parsed.HTML)),
		SentAt:          sentAt,
		CreatedAt:       now,
		ThreadRoot:      parsed.ThreadRoot,
		InReplyTo:       parsed.InReplyTo,
		SizePlain:       totalPlain,
		AttachmentCount: len(parsed.Parts),
		SPFResult:       env.SPFResult,
	}
	if txErr := store.CreateReceivedMailTx(ctx, s.DB, msg, recipients, parts, boxes); txErr != nil {
		// 并发重投同一 Message-ID：另一路已经落信，按幂等成功处理。
		if parsed.MessageID != "" && errors.Is(txErr, store.ErrConflict) {
			if _, gerr := store.GetMailMessageByRFCID(ctx, s.DB.R(), parsed.MessageID); gerr == nil {
				s.releaseMailStorage(ctx, acceptedUsers, totalPlain)
				return nil
			}
		}
		s.releaseMailStorage(ctx, acceptedUsers, totalPlain)
		return mailin.Reject(451, "邮件入库失败，请稍后重试")
	}

	// ⑤ 流量账（缓冲写入，不阻断）。入站字节计入"上行"口径。
	for _, a := range acceptedUsers {
		s.DB.Traffic().Record(
			store.TrafficLog{
				ActorType:    store.ActorUser,
				UserID:       a.uid,
				ClientIP:     env.RemoteIP,
				GroupName:    a.group,
				Action:       "mail_recv",
				BytesPlain:   totalPlain,
				BytesWire:    totalWire,
				ResourcePath: "mail:" + id,
			},
			store.TrafficDaily{
				ActorKey:  store.UserCounterKey(a.uid, ""),
				Day:       store.DayKey(now),
				GroupName: a.group,
				UpPlain:   totalPlain,
				UpWire:    totalWire,
			},
		)
	}
	return nil
}

// mailAcceptance 是一封入站邮件中已成功预留空间的本地用户。
type mailAcceptance struct {
	uid   int64
	group string
}

// releaseMailStorage 回退已预留/记账的邮件空间（下限钳 0，重复调用安全）。
func (s *Service) releaseMailStorage(ctx context.Context, users []mailAcceptance, bytes int64) {
	for _, u := range users {
		if _, err := store.ReleaseCounter(ctx, s.DB.W(),
			store.ScopeMailStorage, store.UserCounterKey(u.uid, ""), bytes); err != nil {
			// 计数器与邮件行会由维护路径对账，这里只留日志。
			log.Printf("service: 回退用户 %d 邮件配额失败: %v", u.uid, err)
		}
	}
}

// buildHeaderRecipients 把 To/Cc/Bcc 头展开为库内行；解析失败的头按裸值保留。
func buildHeaderRecipients(m *mailparse.Message) []store.MailRecipient {
	var out []store.MailRecipient
	seq := 0
	add := func(raw, kind string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		list, err := mail.ParseAddressList(raw)
		if err != nil {
			out = append(out, store.MailRecipient{Kind: kind, Address: strings.ToLower(raw), Seq: seq})
			seq++
			return
		}
		for _, a := range list {
			out = append(out, store.MailRecipient{
				Kind: kind, Name: a.Name, Address: strings.ToLower(a.Address), Seq: seq,
			})
			seq++
		}
	}
	// 只收信，不外发：Bcc 只对发件人有意义，收进来时按普通收件人留存即可。
	add(m.ToRaw, store.MailRecipientTo)
	add(m.CcRaw, store.MailRecipientCc)
	add(m.BccRaw, store.MailRecipientCc)
	return out
}

// maxSnippetRunes 是列表摘要的最大展示长度。
const maxSnippetRunes = 160

var (
	// 先整体剔除 script/style 块，避免剥标签后残留脚本与样式文本。
	snippetBlockRe    = regexp.MustCompile(`(?is)<(?:script|style)\b[^>]*>.*?</(?:script|style)>`)
	snippetTagRe      = regexp.MustCompile(`(?s)<[^>]*>`)
	snippetSpaceRe    = regexp.MustCompile(`\s+`)
	snippetEntityNbsp = strings.NewReplacer("&nbsp;", " ")
)

// buildSnippet 从明文正文抽取列表摘要。优先用 text/plain；只有它为空时
// 才从 HTML 剥标签兜底。摘要是明文元数据，与主题同级。
func buildSnippet(text, htmlBody string) string {
	s := strings.TrimSpace(text)
	if s == "" && htmlBody != "" {
		stripped := snippetBlockRe.ReplaceAllString(htmlBody, " ")
		stripped = snippetTagRe.ReplaceAllString(stripped, " ")
		stripped = snippetEntityNbsp.Replace(stripped)
		s = strings.TrimSpace(html.UnescapeString(stripped))
	}
	s = strings.TrimSpace(snippetSpaceRe.ReplaceAllString(s, " "))
	if runes := []rune(s); len(runes) > maxSnippetRunes {
		s = string(runes[:maxSnippetRunes]) + "…"
	}
	return s
}
