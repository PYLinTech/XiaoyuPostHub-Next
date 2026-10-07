package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 部件类型、邮箱角色与状态常量。本系统只收信：没有方向常量，也没有
// 发件箱（sent）与草稿箱（drafts）两种角色。
const (
	MailPartBody       = "body"
	MailPartAttachment = "attachment"
	MailPartInline     = "inline"

	MailboxRoleInbox = "inbox"

	MailboxStatusNormal   = "normal"
	MailboxStatusArchived = "archived"
	MailboxStatusReleased = "released"

	MailRecipientTo = "to"
	MailRecipientCc = "cc"
)

// MailMessage 是一封邮件的元数据（不含正文内容）。
type MailMessage struct {
	ID              string `json:"id"`
	MessageID       string `json:"messageId,omitempty"`
	FromName        string `json:"fromName,omitempty"`
	FromAddress     string `json:"fromAddress"`
	Subject         string `json:"subject"`
	Snippet         string `json:"snippet,omitempty"`
	SentAt          int64  `json:"sentAt"`
	CreatedAt       int64  `json:"createdAt"`
	ThreadRoot      string `json:"threadRoot,omitempty"`
	InReplyTo       string `json:"inReplyTo,omitempty"`
	SizePlain       int64  `json:"sizePlain"`
	AttachmentCount int    `json:"attachmentCount"`
	SPFResult       string `json:"spfResult,omitempty"`
}

const mailMessageColumns = `id, message_id, from_name, from_address, subject,
	snippet, sent_at, created_at, thread_root, in_reply_to, size_plain, attachment_count,
	spf_result`

func scanMailMessage(row rowScanner) (MailMessage, error) {
	var m MailMessage
	err := row.Scan(
		&m.ID, &m.MessageID, &m.FromName, &m.FromAddress,
		&m.Subject, &m.Snippet, &m.SentAt, &m.CreatedAt, &m.ThreadRoot, &m.InReplyTo, &m.SizePlain,
		&m.AttachmentCount, &m.SPFResult,
	)
	if err != nil {
		return MailMessage{}, err
	}
	return m, nil
}

// InsertMailMessage 插入一封邮件的元数据。RFC Message-ID 重复返 ErrConflict，
// 供入站链路做幂等判定（对方重试不产生重复邮件）。
func InsertMailMessage(ctx context.Context, q Querier, m MailMessage) error {
	if m.ID == "" {
		return fmt.Errorf("邮件内部 ID 不得为空")
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO mail_messages (
			id, message_id, from_name, from_address, subject,
			snippet, sent_at, created_at, thread_root, in_reply_to, size_plain, attachment_count,
			spf_result
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.MessageID, m.FromName, m.FromAddress, m.Subject,
		m.Snippet, m.SentAt, nonZero(m.CreatedAt, Now()), m.ThreadRoot, m.InReplyTo, m.SizePlain,
		m.AttachmentCount, m.SPFResult)
	if err != nil {
		return mapMailWriteError("写入邮件元数据", err)
	}
	return nil
}

// GetMailMessage 按内部 ID 取邮件元数据。
func GetMailMessage(ctx context.Context, q Querier, id string) (MailMessage, error) {
	row := q.QueryRowContext(ctx, `SELECT `+mailMessageColumns+` FROM mail_messages WHERE id = ?`, id)
	m, err := scanMailMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MailMessage{}, ErrNotFound
	}
	if err != nil {
		return MailMessage{}, fmt.Errorf("读取邮件元数据失败: %w", err)
	}
	return m, nil
}

// GetMailMessageByRFCID 按 RFC Message-ID 取邮件；空串或无记录返 ErrNotFound。
// 本地直投后同一 rfcID 可能同时有 in/out 两行，因此入站幂等判定只关心
// "有没有过"，不关心方向：调用方按 err == nil 即视为已入库。
func GetMailMessageByRFCID(ctx context.Context, q Querier, rfcMessageID string) (MailMessage, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+mailMessageColumns+` FROM mail_messages WHERE message_id = ?`, rfcMessageID)
	m, err := scanMailMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MailMessage{}, ErrNotFound
	}
	if err != nil {
		return MailMessage{}, fmt.Errorf("按 Message-ID 读取邮件失败: %w", err)
	}
	return m, nil
}

// MailRecipient 是邮件的一个收件人。
type MailRecipient struct {
	MessageID string `json:"-"`
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	Address   string `json:"address"`
	Seq       int    `json:"seq"`
}

// InsertMailRecipients 批量插入收件人。
func InsertMailRecipients(ctx context.Context, q Querier, rs []MailRecipient) error {
	for _, r := range rs {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO mail_message_recipients (message_id, kind, name, address, seq)
			VALUES (?, ?, ?, ?, ?)`, r.MessageID, r.Kind, r.Name, r.Address, r.Seq); err != nil {
			return mapMailWriteError("写入收件人", err)
		}
	}
	return nil
}

// ListMailRecipients 取一封邮件的全部收件人（按 kind、seq 排序）。
func ListMailRecipients(ctx context.Context, q Querier, messageID string) ([]MailRecipient, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT message_id, kind, name, address, seq
		FROM mail_message_recipients WHERE message_id = ?
		ORDER BY CASE kind WHEN 'to' THEN 0 WHEN 'cc' THEN 1 ELSE 2 END, seq`,
		messageID)
	if err != nil {
		return nil, fmt.Errorf("列出收件人失败: %w", err)
	}
	defer rows.Close()
	out := make([]MailRecipient, 0, 4)
	for rows.Next() {
		var r MailRecipient
		if err := rows.Scan(&r.MessageID, &r.Kind, &r.Name, &r.Address, &r.Seq); err != nil {
			return nil, fmt.Errorf("扫描收件人失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MailPart 是邮件正文容器或一个附件 / 内嵌图，对内容池对象的引用。
type MailPart struct {
	ID           int64  `json:"id"`
	MessageID    string `json:"messageId"`
	Seq          int    `json:"seq"`
	Kind         string `json:"kind"`
	FileName     string `json:"fileName,omitempty"`
	ContentType  string `json:"contentType,omitempty"`
	ContentID    string `json:"contentId,omitempty"`
	SizePlain    int64  `json:"sizePlain"`
	FileChecksum string `json:"fileChecksum"`
}

// InsertMailParts 批量写入部件行。引用计数（AddFileRef）由更高层的事务编排负责，
// 这里只做行写入，保持"行"与"引用"两件事可以独立测试。
func InsertMailParts(ctx context.Context, q Querier, ps []MailPart) error {
	for _, p := range ps {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO mail_parts (message_id, seq, kind, file_name, content_type,
				content_id, size_plain, file_checksum)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.MessageID, p.Seq, p.Kind, p.FileName, p.ContentType, p.ContentID,
			p.SizePlain, p.FileChecksum); err != nil {
			return mapMailWriteError("写入邮件部件", err)
		}
	}
	return nil
}

// ListMailParts 取一封邮件的全部部件（按 seq 排序，正文容器固定在最前）。
func ListMailParts(ctx context.Context, q Querier, messageID string) ([]MailPart, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, message_id, seq, kind, file_name, content_type, content_id,
			size_plain, file_checksum
		FROM mail_parts WHERE message_id = ? ORDER BY seq`, messageID)
	if err != nil {
		return nil, fmt.Errorf("列出邮件部件失败: %w", err)
	}
	defer rows.Close()
	out := make([]MailPart, 0, 2)
	for rows.Next() {
		var p MailPart
		if err := rows.Scan(&p.ID, &p.MessageID, &p.Seq, &p.Kind, &p.FileName,
			&p.ContentType, &p.ContentID, &p.SizePlain, &p.FileChecksum); err != nil {
			return nil, fmt.Errorf("扫描邮件部件失败: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetMailPart 取单个部件（邮件交付借壳下载前的归属鉴权用）。
func GetMailPart(ctx context.Context, q Querier, partID int64) (MailPart, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, message_id, seq, kind, file_name, content_type, content_id,
			size_plain, file_checksum
		FROM mail_parts WHERE id = ?`, partID)
	var p MailPart
	err := row.Scan(&p.ID, &p.MessageID, &p.Seq, &p.Kind, &p.FileName,
		&p.ContentType, &p.ContentID, &p.SizePlain, &p.FileChecksum)
	if errors.Is(err, sql.ErrNoRows) {
		return MailPart{}, ErrNotFound
	}
	if err != nil {
		return MailPart{}, fmt.Errorf("读取邮件部件失败: %w", err)
	}
	return p, nil
}

// Mailbox 是一个用户对一封邮件的归属行。
type Mailbox struct {
	ID           int64  `json:"id"`
	MessageID    string `json:"messageId"`
	UserID       int64  `json:"userId"`
	Role         string `json:"role"`
	IsRead       bool   `json:"isRead"`
	IsStarred    bool   `json:"isStarred"`
	Status       string `json:"status"`
	ChargedBytes int64  `json:"chargedBytes"`
	ArchivedAt   int64  `json:"archivedAt,omitempty"`
	PurgeAt      int64  `json:"purgeAt,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

const mailboxColumns = `id, message_id, user_id, role, is_read, is_starred,
	status, charged_bytes, archived_at, purge_at, created_at`

func scanMailbox(row rowScanner) (Mailbox, error) {
	var b Mailbox
	var read, starred int
	err := row.Scan(&b.ID, &b.MessageID, &b.UserID, &b.Role, &read, &starred,
		&b.Status, &b.ChargedBytes, &b.ArchivedAt, &b.PurgeAt, &b.CreatedAt)
	if err != nil {
		return Mailbox{}, err
	}
	b.IsRead = read != 0
	b.IsStarred = starred != 0
	return b, nil
}

// InsertMailbox 写入一条邮件归属行。
func InsertMailbox(ctx context.Context, q Querier, b Mailbox) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO mailboxes (message_id, user_id, role, is_read, is_starred,
			status, charged_bytes, archived_at, purge_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.MessageID, b.UserID, b.Role, b2i(b.IsRead), b2i(b.IsStarred),
		nonEmpty(b.Status, MailboxStatusNormal), b.ChargedBytes, b.ArchivedAt, b.PurgeAt,
		nonZero(b.CreatedAt, Now()))
	if err != nil {
		return mapMailWriteError("写入邮件归属", err)
	}
	return nil
}

// GetMailbox 取某用户对某邮件在指定角色下的归属行。
func GetMailbox(ctx context.Context, q Querier, userID int64, messageID, role string) (Mailbox, error) {
	row := q.QueryRowContext(ctx, `SELECT `+mailboxColumns+`
		FROM mailboxes WHERE user_id = ? AND message_id = ? AND role = ?`,
		userID, messageID, role)
	b, err := scanMailbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, ErrNotFound
	}
	if err != nil {
		return Mailbox{}, fmt.Errorf("读取邮件归属失败: %w", err)
	}
	return b, nil
}

// GetMailboxForUser 取用户对一封邮件的归属行。阅读链路的所有鉴权都从这里进入。
// 当前写入路径（入站收信、本地直投、草稿转正）不会产生这种重复，但取行必须
// 仍然是确定的：裸 LIMIT 1 会让结果随存储层返回顺序变化，一旦将来出现
// 多角色行，"已释放"与"正常"行的取舍就会变成随机的。因此显式排序，
// 优先返回未释放的行——读取一条还能读的邮件不该取决于行顺序。
func GetMailboxForUser(ctx context.Context, q Querier, userID int64, messageID string) (Mailbox, error) {
	row := q.QueryRowContext(ctx, `SELECT `+mailboxColumns+`
		FROM mailboxes WHERE user_id = ? AND message_id = ?
		ORDER BY CASE status WHEN ? THEN 1 ELSE 0 END, id
		LIMIT 1`, userID, messageID, MailboxStatusReleased)
	b, err := scanMailbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, ErrNotFound
	}
	if err != nil {
		return Mailbox{}, fmt.Errorf("读取邮件归属失败: %w", err)
	}
	return b, nil
}

// MailboxListFilter 是邮件列表的筛选条件。零值字段不参与过滤。
type MailboxListFilter struct {
	Role string // inbox
	// Status 区分收件箱（normal）与归档（archived）/已释放（released）。
	// 零值默认按 normal 处理。
	Status      string
	OnlyStarred bool
	OnlyUnread  bool
	// Query 做元数据搜索：主题、发件人、收件人地址的子串匹配（不搜正文）。
	Query string
}

// MailboxListItem 是列表行：归属状态 + 对应邮件的展示元数据。
type MailboxListItem struct {
	Mailbox
	FromAddress     string `json:"fromAddress"`
	FromName        string `json:"fromName"`
	ToAddress       string `json:"toAddress,omitempty"`
	Subject         string `json:"subject"`
	Snippet         string `json:"snippet,omitempty"`
	SentAt          int64  `json:"sentAt"`
	AttachmentCount int    `json:"attachmentCount"`
}

// ListMailboxes 分页列出用户邮件，返回事务内一致的总数。
func ListMailboxes(ctx context.Context, q Querier, userID int64, f MailboxListFilter,
	limit, offset int) ([]MailboxListItem, int, error) {
	where := []string{"b.user_id = ?"}
	args := []any{userID}
	if f.Role != "" {
		where = append(where, "b.role = ?")
		args = append(args, f.Role)
	}
	if f.Status != "" {
		where = append(where, "b.status = ?")
		args = append(args, f.Status)
	} else {
		// 默认列表只看正常邮件；归档与已释放视图必须显式指定状态。
		where = append(where, "b.status = ?")
		args = append(args, MailboxStatusNormal)
	}
	if f.OnlyStarred {
		where = append(where, "b.is_starred = 1")
	}
	if f.OnlyUnread {
		where = append(where, "b.is_read = 0")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := likeContains(q)
		where = append(where,
			`(m.subject LIKE ? ESCAPE '\' OR m.from_address LIKE ? ESCAPE '\'
			  OR EXISTS (SELECT 1 FROM mail_message_recipients r
			             WHERE r.message_id = b.message_id
			               AND r.address LIKE ? ESCAPE '\'))`)
		args = append(args, like, like, like)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailboxes b JOIN mail_messages m ON m.id = b.message_id WHERE `+clause,
		args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计邮件列表失败: %w", err)
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := q.QueryContext(ctx, `
		SELECT b.id, b.message_id, b.user_id, b.role, b.is_read,
			b.is_starred, b.status, b.charged_bytes, b.archived_at, b.purge_at,
			b.created_at,
			m.from_address, m.from_name,
			(SELECT GROUP_CONCAT(r.address, ', ') FROM mail_message_recipients r
			   WHERE r.message_id = b.message_id AND r.kind = 'to') AS to_address,
			m.subject, m.snippet, m.sent_at,
			m.attachment_count
		FROM mailboxes b JOIN mail_messages m ON m.id = b.message_id
		WHERE `+clause+`
		ORDER BY m.sent_at DESC, b.id DESC
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询邮件列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]MailboxListItem, 0, limit)
	for rows.Next() {
		var item MailboxListItem
		var read, starred int
		var toAddress sql.NullString
		err := rows.Scan(
			&item.ID, &item.MessageID, &item.UserID, &item.Role, &read,
			&starred, &item.Status, &item.ChargedBytes, &item.ArchivedAt, &item.PurgeAt,
			&item.CreatedAt, &item.FromAddress, &item.FromName, &toAddress, &item.Subject,
			&item.Snippet, &item.SentAt, &item.AttachmentCount)
		if err != nil {
			return nil, 0, fmt.Errorf("扫描邮件列表行失败: %w", err)
		}
		item.ToAddress = toAddress.String
		item.IsRead = read != 0
		item.IsStarred = starred != 0
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// SetMailboxRead 更新已读标记。
func SetMailboxRead(ctx context.Context, q Querier, userID int64, messageID, role string, read bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE mailboxes SET is_read = ? WHERE user_id = ? AND message_id = ? AND role = ?`,
		b2i(read), userID, messageID, role)
	if err != nil {
		return fmt.Errorf("更新已读标记失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMailboxStarred 更新星标。
func SetMailboxStarred(ctx context.Context, q Querier, userID int64, messageID, role string, starred bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE mailboxes SET is_starred = ? WHERE user_id = ? AND message_id = ? AND role = ?`,
		b2i(starred), userID, messageID, role)
	if err != nil {
		return fmt.Errorf("更新星标失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMailboxStatus 变更归属状态（normal/archived/released）并写入回收时间点。
// 进入归档写 archived_at；released 时调用方还需释放部件引用与配额。
func SetMailboxStatus(ctx context.Context, q Querier, userID int64, messageID, role, status string, purgeAt int64) error {
	if status != MailboxStatusNormal && status != MailboxStatusArchived && status != MailboxStatusReleased {
		return fmt.Errorf("未知的邮件归属状态 %q", status)
	}
	var archivedAt int64
	if status == MailboxStatusArchived {
		archivedAt = Now()
	}
	res, err := q.ExecContext(ctx, `
		UPDATE mailboxes SET status = ?, archived_at = ?, purge_at = ?
		WHERE user_id = ? AND message_id = ? AND role = ?`,
		status, archivedAt, purgeAt, userID, messageID, role)
	if err != nil {
		return fmt.Errorf("更新邮件归属状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateReceivedMailTx 是入站 / 本地直投落信的单事务编排：
// 元数据 + 收件人 + 部件行（同时增加内容池引用计数）+ 每个收件人的归属行。
//
// 配额预扣必须在调用本事务**之前**完成（按人头），事务失败后由调用方回退。
// 这样收件处理在任何一步崩溃时都不会出现"有部件行无归属"或"引用计数泄漏"：
// 行与引用同生共死。
func CreateReceivedMailTx(ctx context.Context, db *DB, msg MailMessage,
	recipients []MailRecipient, parts []MailPart, boxes []Mailbox) error {
	return db.InTx(ctx, func(tx Querier) error {
		if err := InsertMailMessage(ctx, tx, msg); err != nil {
			return err
		}
		for i := range recipients {
			recipients[i].MessageID = msg.ID
		}
		if err := InsertMailRecipients(ctx, tx, recipients); err != nil {
			return err
		}
		for i := range parts {
			parts[i].MessageID = msg.ID
			if _, err := AddFileRef(ctx, tx, parts[i].FileChecksum); err != nil {
				return fmt.Errorf("登记邮件部件引用失败: %w", err)
			}
		}
		if err := InsertMailParts(ctx, tx, parts); err != nil {
			return err
		}
		for _, b := range boxes {
			b.MessageID = msg.ID
			if err := InsertMailbox(ctx, tx, b); err != nil {
				return err
			}
		}
		return nil
	})
}

// MailboxCounters 是侧栏徽标用的计数（只计 normal）。
type MailboxCounters struct {
	UnreadInbox int64 `json:"unreadInbox"`
}

// GetMailboxCounters 取收件箱未读数；归档/已释放不计。
// COALESCE 是必需的：没有正常行时 SUM 返回 NULL，扫描进 int64 会直接报错。
func GetMailboxCounters(ctx context.Context, q Querier, userID int64) (MailboxCounters, error) {
	var unread int64
	if err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN is_read = 0 THEN 1 ELSE 0 END), 0)
		FROM mailboxes
		WHERE user_id = ? AND status = ? AND role = ?`,
		userID, MailboxStatusNormal, MailboxRoleInbox).Scan(&unread); err != nil {
		return MailboxCounters{}, fmt.Errorf("统计邮件计数失败: %w", err)
	}
	return MailboxCounters{UnreadInbox: unread}, nil
}

// DueMailbox 是一条到达 purge_at 的归档归属行。
type DueMailbox struct {
	ID           int64  `json:"id"`
	MessageID    string `json:"messageId"`
	UserID       int64  `json:"userId"`
	Role         string `json:"role"`
	ChargedBytes int64  `json:"chargedBytes"`
}

// ListDueArchivedMailboxes 列出到达计划清理时间的归档邮件。
func ListDueArchivedMailboxes(ctx context.Context, q Querier, now int64, limit int) ([]DueMailbox, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, message_id, user_id, role, charged_bytes
		FROM mailboxes
		WHERE status = ? AND purge_at > 0 AND purge_at <= ?
		ORDER BY purge_at LIMIT ?`, MailboxStatusArchived, now, limit)
	if err != nil {
		return nil, fmt.Errorf("列出到期归档邮件失败: %w", err)
	}
	defer rows.Close()
	out := make([]DueMailbox, 0, 8)
	for rows.Next() {
		var d DueMailbox
		if err := rows.Scan(&d.ID, &d.MessageID, &d.UserID, &d.Role, &d.ChargedBytes); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountLiveMailboxes 统计一封邮件仍未释放的属主数（normal + archived）。
// 归零时才能释放部件的内容池引用——群发邮件一个属主删信不影响其他人。
func CountLiveMailboxes(ctx context.Context, q Querier, messageID string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailboxes WHERE message_id = ? AND status != ?`,
		messageID, MailboxStatusReleased).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计邮件存活属主失败: %w", err)
	}
	return n, nil
}

// ReleaseMailStorageQ 在给定事务句柄上归还邮件存储配额。
func ReleaseMailStorageQ(ctx context.Context, q Querier, userID int64, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	if _, err := ReleaseCounter(ctx, q, ScopeMailStorage, UserCounterKey(userID, ""), bytes); err != nil {
		return fmt.Errorf("归还邮件配额失败: %w", err)
	}
	return nil
}

// PurgeGuard 限定一次归属销毁的适用范围。两条销毁路径的合法前提不同：
//   - 用户自助彻底删除（必须是归档行）与草稿删除（任何存活行）；
//   - 到期清理批处理：还必须仍在归档且已到 purge_at。
//
// 到期路径需要更严的限定：ListDueArchivedMailboxes 的结果是事务外快照，
// 用户可能在批次扫过之后把邮件恢复出来；若清理只按 id 匹配，那封刚恢复的
// 邮件会被连带销毁并退还配额。
type PurgeGuard struct {
	// RequireArchived 要求该行当前位于归档。
	RequireArchived bool
	// RequireDue 在 RequireArchived 之上再要求已到计划清理时间。
	RequireDue bool
	// DueBefore 是判断"已到期"的时间水位（Unix 秒）。
	DueBefore int64
}

// whereClause 生成限定条件与对应的实参。条件片段都是代码常量、不含外部输入，
// 只有实参走占位符绑定。
func (g PurgeGuard) whereClause(d DueMailbox) (string, []any) {
	cond := `id = ? AND user_id = ? AND status != ?`
	args := []any{d.ID, d.UserID, MailboxStatusReleased}
	if g.RequireArchived {
		cond += ` AND status = ?`
		args = append(args, MailboxStatusArchived)
	}
	if g.RequireDue {
		cond += ` AND purge_at > 0 AND purge_at <= ?`
		args = append(args, g.DueBefore)
	}
	return cond, args
}

// PurgeMailboxTx 单事务完成一条归属的彻底删除：置 released、归还邮件配额；
// 若该邮件已无任何存活属主，逐部件释放内容池引用，归零对象被推进到"待回收"
// （物理删除由维护任务后续轮次完成）。
//
// 三件事刻意收进这个事务，而不是由调用方拼好再传进来：
//  1. 计费字节在事务内重读。调用方持有的 DueMailbox 是事务外快照，按它归还
//     配额会让 quota_counters 与 mailboxes.charged_bytes 长期漂移。
//  2. 存活属主数在置位之后、同一事务内统计。事先在读池里数一遍再把布尔值传
//     进来是有竞态的：群发邮件的两个属主并发删除时，双方都看到 live==2 而都不
//     释放引用（存储永久泄漏），或都看到 live==1 而重复释放（他人仍可读的内容
//     被推进回收）。
//  3. status/purge_at 参与限定。已 released 的行影响 0 行 → ErrNotFound，
//     重复销毁退化成安全的空操作，不会二次退还配额（SQLite 对值未变的 UPDATE
//     同样计入 rows affected，只靠 rows==0 判断幂等是靠不住的）。
func PurgeMailboxTx(ctx context.Context, db *DB, d DueMailbox, g PurgeGuard) error {
	return db.InTx(ctx, func(tx Querier) error {
		cond, args := g.whereClause(d)

		var charged int64
		if err := tx.QueryRowContext(ctx,
			`SELECT charged_bytes FROM mailboxes WHERE `+cond, args...).Scan(&charged); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("读取待销毁邮件归属失败: %w", err)
		}

		updateArgs := make([]any, 0, len(args)+1)
		updateArgs = append(updateArgs, MailboxStatusReleased)
		updateArgs = append(updateArgs, args...)
		res, err := tx.ExecContext(ctx, `
			UPDATE mailboxes SET status = ?, archived_at = 0, purge_at = 0
			WHERE `+cond, updateArgs...)
		if err != nil {
			return fmt.Errorf("释放邮件归属失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if err := ReleaseMailStorageQ(ctx, tx, d.UserID, charged); err != nil {
			return err
		}

		live, err := CountLiveMailboxes(ctx, tx, d.MessageID)
		if err != nil {
			return err
		}
		if live > 0 {
			return nil
		}
		parts, err := ListMailParts(ctx, tx, d.MessageID)
		if err != nil {
			return err
		}
		for _, p := range parts {
			if _, _, err := ReleaseFileRef(ctx, tx, p.FileChecksum); err != nil {
				return fmt.Errorf("释放邮件部件引用失败: %w", err)
			}
		}
		return nil
	})
}

// nonZero 返回 v，零值时返回 fallback。
func nonZero(v, fallback int64) int64 {
	if v == 0 {
		return fallback
	}
	return v
}

// AdminMailboxFilter 是管理端全局邮件视图的筛选条件。零值字段不参与过滤；
// Status 为空时默认只看 normal（与用户列表语义一致），archived/released 须显式指定。
type AdminMailboxFilter struct {
	UserID int64
	Role   string // inbox
	Status string // normal/archived/released
	// Owner 按属主账号子串过滤（u.account LIKE）。
	Owner string
	// Query 做元数据子串搜索：主题、发件人、收件人地址、邮件内部 ID、RFC Message-ID。
	Query string
}

// AdminMailboxListItem 是全局视图一行：归属 + 邮件元数据 + 属主账号信息。
type AdminMailboxListItem struct {
	MailboxListItem
	OwnerAccount     string `json:"ownerAccount"`
	OwnerDisplayName string `json:"ownerDisplayName"`
}

// AdminMailStats 是邮件管理页顶部统计条的口径。
//
// 一律是全站值，不接受任何筛选参数：统计条回答"整个系统的邮件里有什么"，
// 跟着筛选条件变的话它就只是表格上方的一个副本，对不上任何问题。
type AdminMailStats struct {
	Total     int64 `json:"total"`
	Unread    int64 `json:"unread"`
	Starred   int64 `json:"starred"`
	Archived  int64 `json:"archived"`
	Released  int64 `json:"released"`
	UserCount int64 `json:"userCount"`
}

// GetAdminMailStats 一次扫描算出全部统计项。
//
// 用一条聚合查询而不是五条 COUNT：mailboxes 上没有能同时覆盖这些维度的索引，
// 拆开查就是五次全表扫，而这一页每次翻页都会带着它。
func GetAdminMailStats(ctx context.Context, q Querier) (AdminMailStats, error) {
	var s AdminMailStats
	var total, unread, starred, archived, released, users sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN is_read = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN is_starred = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'archived' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'released' THEN 1 ELSE 0 END), 0),
		       COUNT(DISTINCT user_id)
		FROM mailboxes`).Scan(&total, &unread, &starred, &archived, &released, &users)
	if err != nil {
		return AdminMailStats{}, fmt.Errorf("统计全站邮件失败: %w", err)
	}
	s.Total = total.Int64
	s.Unread = unread.Int64
	s.Starred = starred.Int64
	s.Archived = archived.Int64
	s.Released = released.Int64
	s.UserCount = users.Int64
	return s, nil
}

// ListAllMailboxes 跨用户分页列出邮件归属，用于管理全局视图。
func ListAllMailboxes(ctx context.Context, q Querier, f AdminMailboxFilter,
	limit, offset int) ([]AdminMailboxListItem, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.UserID > 0 {
		where = append(where, "b.user_id = ?")
		args = append(args, f.UserID)
	}
	if f.Role != "" {
		where = append(where, "b.role = ?")
		args = append(args, f.Role)
	}
	if f.Status != "" {
		where = append(where, "b.status = ?")
		args = append(args, f.Status)
	} else {
		// 默认列表只看正常邮件；归档与已销毁视图必须显式指定状态。
		where = append(where, "b.status = ?")
		args = append(args, MailboxStatusNormal)
	}
	if owner := strings.TrimSpace(f.Owner); owner != "" {
		where = append(where, "u.account LIKE ? ESCAPE '\\'")
		args = append(args, likeContains(owner))
	}
	if query := strings.TrimSpace(f.Query); query != "" {
		like := likeContains(query)
		where = append(where,
			`(m.subject LIKE ? ESCAPE '\' OR m.from_address LIKE ? ESCAPE '\'
			  OR m.id LIKE ? ESCAPE '\' OR m.message_id LIKE ? ESCAPE '\'
			  OR EXISTS (SELECT 1 FROM mail_message_recipients r
			             WHERE r.message_id = b.message_id
			               AND r.address LIKE ? ESCAPE '\'))`)
		args = append(args, like, like, like, like, like)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM mailboxes b
		JOIN mail_messages m ON m.id = b.message_id
		JOIN users u ON u.id = b.user_id
		WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计全局邮件列表失败: %w", err)
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := q.QueryContext(ctx, `
		SELECT b.id, b.message_id, b.user_id, b.role, b.is_read,
			b.is_starred, b.status, b.charged_bytes, b.archived_at, b.purge_at,
			b.created_at,
			m.from_address, m.from_name, m.subject, m.sent_at,
			m.attachment_count,
			u.account, u.display_name
		FROM mailboxes b
		JOIN mail_messages m ON m.id = b.message_id
		JOIN users u ON u.id = b.user_id
		WHERE `+clause+`
		ORDER BY m.sent_at DESC, b.id DESC
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询全局邮件列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]AdminMailboxListItem, 0, limit)
	for rows.Next() {
		var item AdminMailboxListItem
		var read, starred int
		if err := rows.Scan(
			&item.ID, &item.MessageID, &item.UserID, &item.Role, &read,
			&starred, &item.Status, &item.ChargedBytes, &item.ArchivedAt, &item.PurgeAt,
			&item.CreatedAt, &item.FromAddress, &item.FromName, &item.Subject,
			&item.SentAt, &item.AttachmentCount,
			&item.OwnerAccount, &item.OwnerDisplayName); err != nil {
			return nil, 0, fmt.Errorf("扫描全局邮件列表行失败: %w", err)
		}
		item.IsRead = read != 0
		item.IsStarred = starred != 0
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// GetMailboxByID 按归属行主键取一条邮件归属（管理端处置入口）。
func GetMailboxByID(ctx context.Context, q Querier, id int64) (Mailbox, error) {
	row := q.QueryRowContext(ctx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE id = ?`, id)
	b, err := scanMailbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, ErrNotFound
	}
	if err != nil {
		return Mailbox{}, fmt.Errorf("按 ID 读取邮件归属失败: %w", err)
	}
	return b, nil
}
