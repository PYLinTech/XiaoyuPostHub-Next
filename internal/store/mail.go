package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 邮件地址 / 域名的状态常量。
const (
	MailAddressActive = "active"
	MailAddressFrozen = "frozen"
)

// MailDomain 是一个收件域名本身，只回答"存在吗"。
// 组归属与收件开关都在 GroupMailDomain（绑定）上。域名行建成就不会再被改动，
// 因此没有 UpdatedAt——"什么时候被谁绑过最后一次"属于绑定的历史，不是域名的。
type MailDomain struct {
	Domain    string `json:"domain"`
	CreatedAt int64  `json:"createdAt"`
}

// GroupMailDomain 是「用户组 × 收件域名」的绑定。收件开关挂在绑定上：
// 同一个域名可以被多个组同时使用，各组独立决定收不收。
type GroupMailDomain struct {
	GroupName      string `json:"groupName"`
	Domain         string `json:"domain"`
	ReceiveEnabled bool   `json:"receiveEnabled"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

const mailDomainColumns = `domain, created_at`
const groupMailDomainColumns = `group_name, domain, receive_enabled, created_at, updated_at`

func scanMailDomain(row rowScanner) (MailDomain, error) {
	var d MailDomain
	err := row.Scan(&d.Domain, &d.CreatedAt)
	if err != nil {
		return MailDomain{}, err
	}
	return d, nil
}

func scanGroupMailDomain(row rowScanner) (GroupMailDomain, error) {
	var d GroupMailDomain
	var receive int
	err := row.Scan(&d.GroupName, &d.Domain, &receive, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return GroupMailDomain{}, err
	}
	d.ReceiveEnabled = receive != 0
	return d, nil
}

// EnsureMailDomain 保证域名行存在；已存在则原样保留。
//
// 域名行只表示"这个域名存在"，绑定关系与收件开关都在 group_mail_domains。
// 因此冲突时什么都不写——域名行建成之后本就不该再变。
func EnsureMailDomain(ctx context.Context, q Querier, domain string) error {
	if _, err := q.ExecContext(ctx, `
		INSERT INTO mail_domains (domain, created_at) VALUES (?, ?)
		ON CONFLICT(domain) DO NOTHING`,
		domain, Now()); err != nil {
		return mapMailWriteError("写入收件域名", err)
	}
	return nil
}

// GetMailDomain 按域名取域名行；不存在返 ErrNotFound。
func GetMailDomain(ctx context.Context, q Querier, domain string) (MailDomain, error) {
	row := q.QueryRowContext(ctx, `SELECT `+mailDomainColumns+` FROM mail_domains WHERE domain = ?`, domain)
	d, err := scanMailDomain(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MailDomain{}, ErrNotFound
	}
	if err != nil {
		return MailDomain{}, fmt.Errorf("读取收件域名失败: %w", err)
	}
	return d, nil
}

// DeleteMailDomain 删除域名行。仍被绑定或仍挂着邮箱地址时外键拒绝。
func DeleteMailDomain(ctx context.Context, q Querier, domain string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM mail_domains WHERE domain = ?`, domain)
	if err != nil {
		return mapMailWriteError("删除收件域名", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BindGroupMailDomain 把域名绑给用户组；已绑定则只更新收件开关。
func BindGroupMailDomain(ctx context.Context, q Querier, groupName, domain string, receiveEnabled bool) error {
	now := Now()
	if _, err := q.ExecContext(ctx, `
		INSERT INTO group_mail_domains (group_name, domain, receive_enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(group_name, domain) DO UPDATE SET
			receive_enabled = excluded.receive_enabled,
			updated_at = excluded.updated_at`,
		groupName, domain, b2i(receiveEnabled), now, now); err != nil {
		return mapMailWriteError("绑定收件域名", err)
	}
	return nil
}

// UnbindGroupMailDomain 解除「组 × 域名」绑定。域名本身与其他组不受影响。
func UnbindGroupMailDomain(ctx context.Context, q Querier, groupName, domain string) error {
	res, err := q.ExecContext(ctx,
		`DELETE FROM group_mail_domains WHERE group_name = ? AND domain = ?`, groupName, domain)
	if err != nil {
		return mapMailWriteError("解除收件域名绑定", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetGroupMailDomain 取单条绑定；不存在返 ErrNotFound。
func GetGroupMailDomain(ctx context.Context, q Querier, groupName, domain string) (GroupMailDomain, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+groupMailDomainColumns+` FROM group_mail_domains
		 WHERE group_name = ? AND domain = ?`, groupName, domain)
	d, err := scanGroupMailDomain(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupMailDomain{}, ErrNotFound
	}
	if err != nil {
		return GroupMailDomain{}, fmt.Errorf("读取收件域名绑定失败: %w", err)
	}
	return d, nil
}

// ListGroupMailDomains 列出某用户组的全部绑定，按域名排序。
func ListGroupMailDomains(ctx context.Context, q Querier, groupName string) ([]GroupMailDomain, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+groupMailDomainColumns+` FROM group_mail_domains
		 WHERE group_name = ? ORDER BY domain`, groupName)
	if err != nil {
		return nil, fmt.Errorf("列出用户组收件域名失败: %w", err)
	}
	defer rows.Close()
	out := make([]GroupMailDomain, 0, 2)
	for rows.Next() {
		d, err := scanGroupMailDomain(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描用户组收件域名失败: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListAllGroupMailDomains 列出全部绑定，按组名与域名排序。
//
// 用户组列表要把每组各自的域名一起带出来；组数与域名数都不多，一次读全
// 再按组归拢，比逐组再查省掉 N 条查询。
func ListAllGroupMailDomains(ctx context.Context, q Querier) ([]GroupMailDomain, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+groupMailDomainColumns+` FROM group_mail_domains ORDER BY group_name, domain`)
	if err != nil {
		return nil, fmt.Errorf("列出收件域名绑定失败: %w", err)
	}
	defer rows.Close()
	out := make([]GroupMailDomain, 0, 8)
	for rows.Next() {
		d, err := scanGroupMailDomain(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描收件域名绑定失败: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDomainsInUse 统计还有绑定存在的域名数。解绑后判断域名是否可以一并删除。
func CountDomainsInUse(ctx context.Context, q Querier, domain string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM group_mail_domains WHERE domain = ?`, domain).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计域名绑定数失败: %w", err)
	}
	return n, nil
}

// MailAddress 是用户持有的一个邮箱地址。
type MailAddress struct {
	Address   string `json:"address"`
	LocalPart string `json:"localPart"`
	Domain    string `json:"domain"`
	UserID    int64  `json:"userId"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

const mailAddressColumns = `address, local_part, domain, user_id, status, created_at, updated_at`

func scanMailAddress(row rowScanner) (MailAddress, error) {
	var a MailAddress
	err := row.Scan(&a.Address, &a.LocalPart, &a.Domain, &a.UserID, &a.Status,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return MailAddress{}, err
	}
	return a, nil
}

// CreateMailAddress 创建邮箱地址。地址重名（全址或 local+domain）返 ErrConflict。
func CreateMailAddress(ctx context.Context, q Querier, a MailAddress) error {
	now := Now()
	_, err := q.ExecContext(ctx, `
		INSERT INTO mail_addresses (address, local_part, domain, user_id, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Address, a.LocalPart, a.Domain, a.UserID,
		nonEmpty(a.Status, MailAddressActive), now, now)
	if err != nil {
		return mapMailWriteError("创建邮箱地址", err)
	}
	return nil
}

// GetMailAddress 按全址取地址。
func GetMailAddress(ctx context.Context, q Querier, address string) (MailAddress, error) {
	row := q.QueryRowContext(ctx, `SELECT `+mailAddressColumns+` FROM mail_addresses WHERE address = ?`, address)
	a, err := scanMailAddress(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MailAddress{}, ErrNotFound
	}
	if err != nil {
		return MailAddress{}, fmt.Errorf("读取邮箱地址失败: %w", err)
	}
	return a, nil
}

// ListMailAddressesByUser 列出某用户的全部地址（含冻结地址）。
func ListMailAddressesByUser(ctx context.Context, q Querier, userID int64) ([]MailAddress, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+mailAddressColumns+` FROM mail_addresses WHERE user_id = ? ORDER BY domain, local_part`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("列出用户邮箱地址失败: %w", err)
	}
	defer rows.Close()
	out := make([]MailAddress, 0, 2)
	for rows.Next() {
		a, err := scanMailAddress(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描邮箱地址失败: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountMailAddresses 统计某用户在某域名下的**可用**地址数（配额上限判定用）。
//
// 早先这里有个 activeOnly 参数，false 分支的注释写着"历史占用视图用"——
// 那个视图并不存在，管理端的域名占用检查走的是 CountMailAddressesAll。
// 参数因此只剩 true 一种用法，删掉以免误导。
func CountMailAddresses(ctx context.Context, q Querier, userID int64, domain string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mail_addresses
		 WHERE user_id = ? AND domain = ? AND status = 'active'`,
		userID, domain).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计邮箱地址数失败: %w", err)
	}
	return n, nil
}

// CountMailAddressesAll 统计某域名下全部用户的邮箱地址数（含冻结）。
// 解除域名绑定 / 改名的前置检查用——地址是用户数据，不得随域静默消失。
func CountMailAddressesAll(ctx context.Context, q Querier, domain string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mail_addresses WHERE domain = ?`, domain).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计域名邮箱地址总数失败: %w", err)
	}
	return n, nil
}

// GroupMailDomainCount 是「组 × 域名」上属于该组用户的邮箱地址数。
type GroupMailDomainCount struct {
	GroupName    string
	Domain       string
	AddressCount int64
}

// CountGroupAddressesByDomain 一次统计每个绑定下属于该组用户的邮箱地址数。
//
// 注意统计口径是"该组用户的地址"而不是"该域名下全部地址"：域名可以被多个组
// 共用，B 组有地址并不意味着 A 组不能移除这个域名——把两者混同会让共用域名
// 下的任何一组都无法解绑。
func CountGroupAddressesByDomain(ctx context.Context, q Querier) ([]GroupMailDomainCount, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT g.group_name, g.domain, COUNT(a.address)
		FROM group_mail_domains g
		LEFT JOIN users u ON u.group_name = g.group_name
		LEFT JOIN mail_addresses a ON a.user_id = u.id AND a.domain = g.domain
		GROUP BY g.group_name, g.domain`)
	if err != nil {
		return nil, fmt.Errorf("统计各组域名地址数失败: %w", err)
	}
	defer rows.Close()
	out := make([]GroupMailDomainCount, 0, 8)
	for rows.Next() {
		var c GroupMailDomainCount
		if err := rows.Scan(&c.GroupName, &c.Domain, &c.AddressCount); err != nil {
			return nil, fmt.Errorf("扫描组域名地址统计失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountGroupMailAddresses 统计某组用户在某域名下的邮箱地址数（含冻结）。
// 移除绑定前的阻断检查用——地址是用户数据，不得随解绑静默失效。
func CountGroupMailAddresses(ctx context.Context, q Querier, groupName, domain string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mail_addresses
		WHERE domain = ?
		  AND user_id IN (SELECT id FROM users WHERE group_name = ?)`,
		domain, groupName).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计用户组域名地址数失败: %w", err)
	}
	return n, nil
}

// SetMailAddressStatus 冻结或恢复单个地址。
//
// 仅供测试与运维直改使用；常规的组归属变更走 SyncMailAddressStatus 重算全量。
func SetMailAddressStatus(ctx context.Context, q Querier, address, status string) error {
	if status != MailAddressActive && status != MailAddressFrozen {
		return fmt.Errorf("未知的邮箱地址状态 %q", status)
	}
	res, err := q.ExecContext(ctx,
		`UPDATE mail_addresses SET status = ?, updated_at = ? WHERE address = ?`,
		status, Now(), address)
	if err != nil {
		return fmt.Errorf("更新邮箱地址状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SyncMailAddressStatus 按用户当前所属组重算其全部邮箱地址的可用状态。
//
// 地址的可用性是组归属的派生结果，而不是独立维护的状态：域名经
// group_mail_domains 绑定给用户组（多对多），仍被本组任一绑定覆盖的地址为
// active，其余一律 frozen。移组只冻结、不删除——地址是用户数据，历史邮件
// 必须继续可读。
//
// 调用方须在同一事务内先写入新的 users.group_name，否则下面两条 UPDATE
// 读到的是旧组，算出的状态与刚落库的组不一致。
//
// 幂等：重复调用只产生与当前状态相同的写入。
func SyncMailAddressStatus(ctx context.Context, q Querier, userID int64, groupName string) error {
	now := Now()
	const boundByGroup = `
		SELECT 1 FROM group_mail_domains
		 WHERE group_mail_domains.group_name = ?
		   AND group_mail_domains.domain = mail_addresses.domain`
	if _, err := q.ExecContext(ctx, `
		UPDATE mail_addresses SET status = ?, updated_at = ?
		WHERE user_id = ?
		  AND EXISTS (`+boundByGroup+`)`,
		MailAddressActive, now, userID, groupName); err != nil {
		return fmt.Errorf("恢复邮箱地址可用状态失败: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE mail_addresses SET status = ?, updated_at = ?
		WHERE user_id = ?
		  AND NOT EXISTS (`+boundByGroup+`)`,
		MailAddressFrozen, now, userID, groupName); err != nil {
		return fmt.Errorf("冻结邮箱地址可用状态失败: %w", err)
	}
	return nil
}

// b2i 把布尔转成库内的 0/1。
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nonEmpty 返回 s，空串时返回 fallback。
func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// mapMailWriteError 把 SQLite 的约束错误翻译成仓储哨兵，其余错误包装中文语境。
func mapMailWriteError(action string, err error) error {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unique") || strings.Contains(msg, "primary key"):
		return fmt.Errorf("%w: %s", ErrConflict, err.Error())
	case strings.Contains(msg, "foreign key"):
		return fmt.Errorf("%w: 引用的域名、用户组或用户不存在", ErrConflict)
	}
	return fmt.Errorf("%s失败: %w", action, err)
}
