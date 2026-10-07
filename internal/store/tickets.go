package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const ticketColumns = `id, file_checksum, actor_type, user_id, client_ip, ip_prefix, group_name,
	purpose, delivery_mode, reserved_bytes, settled_bytes, use_count, max_uses, expires_at, revoked, created_at`

func scanTicket(row rowScanner) (Ticket, error) {
	var t Ticket
	var actor, purpose, mode string
	var expires int64
	var revoked int
	err := row.Scan(&t.ID, &t.FileChecksum, &actor, &t.UserID, &t.ClientIP, &t.IPPrefix, &t.GroupName,
		&purpose, &mode, &t.ReservedBytes, &t.SettledBytes, &t.UseCount, &t.MaxUses, &expires, &revoked, &t.CreatedAt)
	if err != nil {
		return Ticket{}, err
	}
	t.ActorType = ActorType(actor)
	t.Purpose = Purpose(purpose)
	t.DeliveryMode = TicketDeliveryMode(mode)
	t.ExpiresAt = ToTime(expires)
	t.Revoked = revoked != 0
	return t, nil
}

// CreateTicket 签发一张下载/预览票据。
func CreateTicket(ctx context.Context, q Querier, t Ticket) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO download_tickets (id, file_checksum, actor_type, user_id, client_ip, ip_prefix,
			group_name, purpose, delivery_mode, reserved_bytes, settled_bytes, use_count, max_uses, expires_at, revoked, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, 0, ?)`,
		t.ID, t.FileChecksum, string(t.ActorType), t.UserID, t.ClientIP, t.IPPrefix,
		t.GroupName, string(t.Purpose), string(t.DeliveryMode), t.ReservedBytes, t.MaxUses, FromTime(t.ExpiresAt), t.CreatedAt)
	if err != nil {
		return fmt.Errorf("签发下载票据失败: %w", err)
	}
	return nil
}

// SetTicketDeliveryMode 原子改写票据的交付模式，仅用于准备交付时的安全降级
// 与中转解密覆盖。条件更新保证并发下不会把已结算/已取消的票据错误翻转。
func SetTicketDeliveryMode(ctx context.Context, q Querier, id string, mode TicketDeliveryMode) error {
	res, err := q.ExecContext(ctx,
		`UPDATE download_tickets SET delivery_mode = ? WHERE id = ? AND settled_bytes = 0 AND revoked = 0`,
		string(mode), id)
	if err != nil {
		return fmt.Errorf("更新票据交付模式失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: 票据已失效，无法变更交付模式", ErrNoRowsAffected)
	}
	return nil
}

// GetTicket 取票据。
func GetTicket(ctx context.Context, q Querier, id string) (Ticket, error) {
	row := q.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM download_tickets WHERE id = ?`, id)
	t, err := scanTicket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("读取票据失败: %w", err)
	}
	return t, nil
}

// ConsumeTicketUse 记一次票据使用。
//
// 一次播放会产生大量 Range 请求，每个都可能触发 CDN 回源鉴权，所以票据不能做成
// 一次性消费：这里用"次数上限 + 有效期"约束，条件更新保证并发下不超上限，命中 0 行
// 即表示拒绝。
//
// 只做次数与有效性判定；IP 一致性与文件状态由调用方校验，它们需要票据之外的上下文。
func ConsumeTicketUse(ctx context.Context, q Querier, id string, now int64) (Ticket, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE download_tickets SET use_count = use_count + 1
		WHERE id = ? AND revoked = 0 AND settled_bytes = 0
		  AND expires_at > ? AND use_count < max_uses
		  AND EXISTS (
			SELECT 1 FROM files
			 WHERE files.checksum = download_tickets.file_checksum
			   AND files.status = 1
		  )`,
		id, now)
	if err != nil {
		return Ticket{}, fmt.Errorf("消费票据失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Ticket{}, fmt.Errorf("%w: 票据无效、已过期或已达使用上限", ErrNoRowsAffected)
	}
	return GetTicket(ctx, q, id)
}

// SettleTicket 记录票据实际消耗的字节数，用于差额回补配额。
//
// settled_bytes=0 表示尚未结算（密文对象至少包含文件头，因此合法的实际
// 用量不会是 0）。条件更新让并发的 Range/失败回调只有一个能拿到退款资格，
// 避免重复结算把配额释放到负数。
func SettleTicket(ctx context.Context, q Querier, id string, settledBytes int64) (bool, error) {
	if settledBytes <= 0 {
		return false, fmt.Errorf("结算密文字节数必须大于 0")
	}
	res, err := q.ExecContext(ctx,
		`UPDATE download_tickets SET settled_bytes = ? WHERE id = ? AND settled_bytes = 0`, settledBytes, id)
	if err != nil {
		return false, fmt.Errorf("结算票据失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CancelTicket 取消一张尚未结算的票据。-1 是内部的“已取消”哨兵值，
// 与合法密文用量（至少 64 字节）区分，并保持后续取消调用幂等。
//
// 取消必须同时吊销票据。仅写 settled_bytes=-1 会让 ConsumeTicketUse 仍然
// 能够命中，导致“已经回滚的票据”继续被 CDN 或服务端流消费。
func CancelTicket(ctx context.Context, q Querier, id string) (bool, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE download_tickets SET settled_bytes = -1, revoked = 1
		 WHERE id = ? AND settled_bytes = 0`, id)
	if err != nil {
		return false, fmt.Errorf("取消票据失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteTicket 立即物理删除一张票据。
//
// 仅供"票据寿命被一次短任务完整包住"的场景（存储自检：2 分钟 TTL、
// ReservedBytes=0、不承载真实流量）在收尾时调用：内容池行对 tickets 有
// 外键约束，只做软吊销（CancelTicket）会让紧接着的 files 行删除触发
// FOREIGN KEY 失败。常规交付不得使用——那里要靠软吊销与过期清理保留
// 对账痕迹。票据不存在视为已删除，返回 nil。
func DeleteTicket(ctx context.Context, q Querier, id string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM download_tickets WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除票据失败: %w", err)
	}
	return nil
}

// RevokeTicket 吊销单张票据。
func RevokeTicket(ctx context.Context, q Querier, id string) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE download_tickets SET revoked = 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("吊销票据失败: %w", err)
	}
	return nil
}

// ListActiveTicketsByChecksum 列出某内容对象上尚未结算（settled_bytes=0，
// 含未取消）的票据。
//
// 仅用于"内容对象即将被同校验码重传替换"的清理：这些票据的预扣额度必须
// 先逐张释放，再物理删除行，否则要么外键阻塞内容行删除，要么预扣额度
// 随票据行被删除而泄漏。
func ListActiveTicketsByChecksum(ctx context.Context, q Querier, checksum string) ([]Ticket, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+ticketColumns+` FROM download_tickets
		 WHERE file_checksum = ? AND settled_bytes = 0`, checksum)
	if err != nil {
		return nil, fmt.Errorf("查询对象活跃票据失败: %w", err)
	}
	defer rows.Close()
	var out []Ticket
	for rows.Next() {
		tk, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

// DeleteTicketsByChecksum 物理删除指向某内容对象的全部票据行。
// 调用方必须先处理完这些票据的预扣额度（见 ListActiveTicketsByChecksum）。
func DeleteTicketsByChecksum(ctx context.Context, q Querier, checksum string) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM download_tickets WHERE file_checksum = ?`, checksum); err != nil {
		return fmt.Errorf("删除对象票据失败: %w", err)
	}
	return nil
}

// RevokeTicketsByChecksum 吊销某个内容池对象的全部未失效票据。
//
// 拉黑文件时必须调用：只"不再签发新票据"是不够的，已签发但尚未过期的票据
// 在有效期内依然生效。
func RevokeTicketsByChecksum(ctx context.Context, q Querier, checksum string) (int64, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE download_tickets SET revoked = 1
		WHERE file_checksum = ? AND revoked = 0`, checksum)
	if err != nil {
		return 0, fmt.Errorf("吊销对象票据失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeTicketsByOwner 吊销某账号名下内容对应的全部票据。
//
// 分享访客票据的 actor 是 guest，封禁内容所有者时不能只按 actor=user
// 吊销；否则账号已被停用，原先发出的公开链接仍可继续下载。
func RevokeTicketsByOwner(ctx context.Context, q Querier, ownerID int64) (int64, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE download_tickets SET revoked = 1
		WHERE revoked = 0 AND file_checksum IN (
			SELECT file_checksum FROM user_nodes
			WHERE user_id = ? AND node_type = ? AND file_checksum IS NOT NULL
		)`, ownerID, int(NodeFile))
	if err != nil {
		return 0, fmt.Errorf("吊销账号内容票据失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeTicketsByShareRoot 吊销指向某分享根及其子树内容的票据。
// 分享没有单独的票据外键时，按分享快照中的节点集合定位票据；这是停用、
// 删除或移动分享根时唯一能阻断既有 CDN/中转票据的边界。
func RevokeTicketsByShareRoot(ctx context.Context, q Querier, ownerID int64, rootPath string) (int64, error) {
	clause, args := pathSelfOrDescendant("logical_path", rootPath)
	res, err := q.ExecContext(ctx, `
		UPDATE download_tickets SET revoked = 1
		WHERE revoked = 0 AND file_checksum IN (
			SELECT file_checksum FROM user_nodes
			WHERE user_id = ? AND node_type = ? AND file_checksum IS NOT NULL
			  AND `+clause+`
		)`, append([]any{ownerID, int(NodeFile)}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("吊销分享内容票据失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeTicketsByActor 吊销某个用户或访客 IP 前缀的全部票据（封禁、改密时使用）。
func RevokeTicketsByActor(ctx context.Context, q Querier, actorType ActorType, userID int64, ipPrefix string) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if actorType == ActorUser {
		res, err = q.ExecContext(ctx,
			`UPDATE download_tickets SET revoked = 1 WHERE actor_type = 'user' AND user_id = ? AND revoked = 0`,
			userID)
	} else {
		res, err = q.ExecContext(ctx,
			`UPDATE download_tickets SET revoked = 1 WHERE actor_type = 'guest' AND ip_prefix = ? AND revoked = 0`,
			ipPrefix)
	}
	if err != nil {
		return 0, fmt.Errorf("吊销主体票据失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListExpiredTicketsBatch 只读取一批过期票据，不改变状态。实际删除由
// DeleteExpiredTicket 在写事务中完成，以便和预扣额度回补保持原子。
func ListExpiredTicketsBatch(ctx context.Context, q Querier, before int64, limit int) ([]Ticket, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := q.QueryContext(ctx, `SELECT `+ticketColumns+`
		FROM download_tickets WHERE expires_at < ? ORDER BY expires_at LIMIT ?`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("读取过期票据失败: %w", err)
	}
	defer rows.Close()
	var out []Ticket
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描过期票据失败: %w", err)
		}
		out = append(out, ticket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取过期票据失败: %w", err)
	}
	return out, nil
}

// DeleteExpiredTicket 删除一张已经过期的票据，并报告它是否仍需要回收
// 预扣额度。调用方应在同一写事务中调用本函数与配额回补：如果回补失败，
// 整个事务回滚，下一轮维护仍能重试，不会出现票据消失而额度永久占用。
func DeleteExpiredTicket(ctx context.Context, q Querier, id string, before int64) (ticket Ticket, deleted, needsRefund bool, err error) {
	ticket, err = GetTicket(ctx, q, id)
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, false, false, nil
	}
	if err != nil {
		return Ticket{}, false, false, err
	}
	if ticket.ExpiresAt.IsZero() || ticket.ExpiresAt.Unix() >= before {
		return ticket, false, false, nil
	}

	res, err := q.ExecContext(ctx,
		`DELETE FROM download_tickets WHERE id = ? AND expires_at < ? AND settled_bytes = 0`, id, before)
	if err != nil {
		return Ticket{}, false, false, fmt.Errorf("删除过期票据失败: %w", err)
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		return Ticket{}, false, false, fmt.Errorf("读取过期票据删除结果失败: %w", rowsErr)
	} else if n > 0 {
		return ticket, true, true, nil
	}

	// 已结算或已取消的票据没有待回收额度，但同样需要删除，避免表无限增长。
	res, err = q.ExecContext(ctx,
		`DELETE FROM download_tickets WHERE id = ? AND expires_at < ? AND settled_bytes <> 0`, id, before)
	if err != nil {
		return Ticket{}, false, false, fmt.Errorf("删除已结算过期票据失败: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return Ticket{}, false, false, fmt.Errorf("读取已结算票据删除结果失败: %w", rowsErr)
	}
	return ticket, n > 0, false, nil
}
