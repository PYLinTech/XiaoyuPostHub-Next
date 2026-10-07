// 邮箱地址解绑申请：用户申请，管理员审核，通过即删除地址行。
//
// 这一层只管数据，不含业务判断。两条硬约束落在服务层，这里只保证它们在
// 存储上可执行：
//
//	一、同一地址同时只能有一条待审申请（否则管理员会看到互相矛盾的两张单）
//	二、批准与删地址必须在同一事务里（否则删成功但单子还挂在 pending 上）
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 申请单状态。pending 是唯一可流转的起点。
const (
	UnbindPending  = "pending"
	UnbindApproved = "approved"
	UnbindRejected = "rejected"
)

// MailUnbindRequest 是用户对自己名下某个邮箱地址提出的解绑申请。
type MailUnbindRequest struct {
	ID         int64  `json:"id"`
	Address    string `json:"address"`
	UserID     int64  `json:"userId"`
	Account    string `json:"account"` // 联表带出，供管理端列表直接显示
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Note       string `json:"note"`
	ReviewedBy int64  `json:"reviewedBy"`
	CreatedAt  int64  `json:"createdAt"`
	ReviewedAt int64  `json:"reviewedAt"`
}

const mailUnbindColumns = `r.id, r.address, r.user_id, COALESCE(u.account, ''),
	r.status, r.reason, r.note, r.reviewed_by, r.created_at, r.reviewed_at`

func scanMailUnbindRequest(row rowScanner) (MailUnbindRequest, error) {
	var r MailUnbindRequest
	err := row.Scan(&r.ID, &r.Address, &r.UserID, &r.Account,
		&r.Status, &r.Reason, &r.Note, &r.ReviewedBy, &r.CreatedAt, &r.ReviewedAt)
	if err != nil {
		return MailUnbindRequest{}, err
	}
	return r, nil
}

// CreateUnbindRequest 新建一条待审申请。
//
// 同一地址已有待审申请时返 ErrConflict：管理员在一个列表里看到两张内容
// 不同的单子说不上哪张作数，而"最后一个赢"是更难解释的行为。
//
// 去重靠"插入时把已有待审单排除在外"这一条语句完成，而不是"先查后插"：
// 先查后插在两个请求同时到达时会双双通过检查。要让它真的成立，数据库
// 层必须有一条覆盖 (address, status=pending) 的唯一约束——现在没有，所以
// 这里用 NOT EXISTS 把判定压进同一条 INSERT，让写锁充当互斥。数据量是
// 单个用户的地址数，竞态窗口小到不值得为此加一个部分唯一索引。
func CreateUnbindRequest(ctx context.Context, q Querier, r MailUnbindRequest) error {
	res, err := q.ExecContext(ctx, `
		INSERT INTO mail_unbind_requests
			(address, user_id, status, reason, note, reviewed_by, created_at, reviewed_at)
		SELECT ?, ?, ?, ?, ?, 0, ?, 0
		WHERE NOT EXISTS (
			SELECT 1 FROM mail_unbind_requests WHERE address = ? AND status = ?)`,
		r.Address, r.UserID, UnbindPending, r.Reason, r.Note, Now(),
		r.Address, UnbindPending)
	if err != nil {
		return mapMailWriteError("创建解绑申请", err)
	}
	// 影响行数为 0 就是那条 NOT EXISTS 命中了：已经有单子在审。
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: 该地址已有待审核的解绑申请", ErrConflict)
	}
	return nil
}

// CountPendingUnbindForAddress 统计某地址的待审申请数。用于配额外的去重与展示。
func CountPendingUnbindForAddress(ctx context.Context, q Querier, address string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mail_unbind_requests WHERE address = ? AND status = ?`,
		address, UnbindPending).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计待审解绑申请失败: %w", err)
	}
	return n, nil
}

// GetUnbindRequest 按主键取申请。
func GetUnbindRequest(ctx context.Context, q Querier, id int64) (MailUnbindRequest, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+mailUnbindColumns+`
		FROM mail_unbind_requests r LEFT JOIN users u ON u.id = r.user_id
		WHERE r.id = ?`, id)
	r, err := scanMailUnbindRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MailUnbindRequest{}, ErrNotFound
	}
	if err != nil {
		return MailUnbindRequest{}, fmt.Errorf("读取解绑申请失败: %w", err)
	}
	return r, nil
}

// PendingUnbindForAddress 取某地址的待审申请。用于"这个地址能不能再申请"判定。
func PendingUnbindForAddress(ctx context.Context, q Querier, address string) (MailUnbindRequest, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+mailUnbindColumns+`
		FROM mail_unbind_requests r LEFT JOIN users u ON u.id = r.user_id
		WHERE r.address = ? AND r.status = ? ORDER BY r.id LIMIT 1`,
		address, UnbindPending)
	r, err := scanMailUnbindRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MailUnbindRequest{}, ErrNotFound
	}
	if err != nil {
		return MailUnbindRequest{}, fmt.Errorf("读取待审解绑申请失败: %w", err)
	}
	return r, nil
}

// ListUnbindRequestsByUser 列出某用户的全部申请（含已处理），供用户端展示。
//
// 按地址分组是用户的真实心智：用户看的是"我那几个地址现在各自什么状态"，
// 而不是"我按时间提交过的申请流水"。
func ListUnbindRequestsByUser(ctx context.Context, q Querier, userID int64) ([]MailUnbindRequest, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+mailUnbindColumns+`
		FROM mail_unbind_requests r LEFT JOIN users u ON u.id = r.user_id
		WHERE r.user_id = ? ORDER BY r.address, r.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("列出用户解绑申请失败: %w", err)
	}
	defer rows.Close()
	out := make([]MailUnbindRequest, 0, 2)
	for rows.Next() {
		r, err := scanMailUnbindRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描解绑申请失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnbindListFilter 是管理端列表的筛选条件。Status 为空表示不限。
type UnbindListFilter struct {
	Status   string
	Address  string
	Search   string
	Limit    int
	HasLimit bool
}

// ListUnbindRequests 列出申请，供管理端审核页使用。
func ListUnbindRequests(ctx context.Context, q Querier, f UnbindListFilter) ([]MailUnbindRequest, error) {
	q2 := `SELECT ` + mailUnbindColumns + `
		FROM mail_unbind_requests r LEFT JOIN users u ON u.id = r.user_id
		WHERE 1 = 1`
	var args []any
	if f.Status != "" {
		q2 += ` AND r.status = ?`
		args = append(args, f.Status)
	}
	if f.Address != "" {
		q2 += ` AND r.address = ?`
		args = append(args, f.Address)
	}
	if f.Search != "" {
		// 模糊匹配地址与用户名。转义 LIKE 通配符，否则用户搜 "%" 会匹配全表。
		q2 += ` AND (r.address LIKE ? ESCAPE '\' OR u.account LIKE ? ESCAPE '\')`
		esc := escapeLike(f.Search)
		args = append(args, "%"+esc+"%", "%"+esc+"%")
	}
	// 待审在前；同状态内按提交时间倒序，最新的一批就是最需要处理的。
	q2 += ` ORDER BY CASE r.status WHEN 'pending' THEN 0 ELSE 1 END, r.created_at DESC, r.id DESC`
	if f.HasLimit {
		limit := f.Limit
		if limit <= 0 || limit > 500 {
			limit = 200
		}
		q2 += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := q.QueryContext(ctx, q2, args...)
	if err != nil {
		return nil, fmt.Errorf("列出解绑申请失败: %w", err)
	}
	defer rows.Close()
	out := make([]MailUnbindRequest, 0, 16)
	for rows.Next() {
		r, err := scanMailUnbindRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描解绑申请失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// escapeLike 转义 LIKE 模式里的特殊字符，让用户输入的 % 与 _ 按字面匹配。
func escapeLike(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// MailUnbindStats 是审核页顶部的统计条。
type MailUnbindStats struct {
	Pending  int64 `json:"pending"`
	Approved int64 `json:"approved"`
	Rejected int64 `json:"rejected"`
	// 今日新增，用于判断这一批积压是新产生的还是历史遗留。
	TodayPending int64 `json:"todayPending"`
}

// CountUnbindStats 按状态统计申请数。todayFrom 取当天零点。
func CountUnbindStats(ctx context.Context, q Querier, todayFrom int64) (MailUnbindStats, error) {
	var s MailUnbindStats
	rows, err := q.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM mail_unbind_requests GROUP BY status`)
	if err != nil {
		return MailUnbindStats{}, fmt.Errorf("统计解绑申请失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return MailUnbindStats{}, fmt.Errorf("扫描解绑统计失败: %w", err)
		}
		switch status {
		case UnbindPending:
			s.Pending = n
		case UnbindApproved:
			s.Approved = n
		case UnbindRejected:
			s.Rejected = n
		}
	}
	if err := rows.Err(); err != nil {
		return MailUnbindStats{}, err
	}
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mail_unbind_requests WHERE status = ? AND created_at >= ?`,
		UnbindPending, todayFrom).Scan(&s.TodayPending); err != nil {
		return MailUnbindStats{}, fmt.Errorf("统计今日待审失败: %w", err)
	}
	return s, nil
}

// ApproveUnbindRequest 批准申请并删除对应地址，两步在同一事务内完成。
//
// 顺序是**先删地址、再把单子标成已批准**：申请单的 address 是快照而非
// 外键，两个动作本无先后约束，但先删地址能让"地址已不在"这件事立刻暴露，
// 事务回滚，单子不会停留在"已批准但地址还在"这种自相矛盾的状态。
//
// userID 参与 WHERE 条件是第二道防线：申请提交后、审核之前，这个地址可能
// 已经被管理员手工删除、或者归属的用户被注销。此时不能让一次"通过"顺手
// 删掉别的用户后来申请的同名地址。
func ApproveUnbindRequest(ctx context.Context, db *DB, id int64,
	reviewerID int64, note string) (MailUnbindRequest, error) {

	var approved MailUnbindRequest
	err := db.InTx(ctx, func(tx Querier) error {
		req, err := getUnbindForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM mail_addresses WHERE address = ? AND user_id = ?`,
			req.Address, req.UserID)
		if err != nil {
			return fmt.Errorf("删除邮箱地址失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// 地址已不在（或已不归该用户）：不动数据，直接判这单已失去标的。
			return fmt.Errorf("%w: 邮箱地址已不存在或已变更归属", ErrConflict)
		}
		now := Now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE mail_unbind_requests
			SET status = ?, note = ?, reviewed_by = ?, reviewed_at = ?
			WHERE id = ? AND status = ?`,
			UnbindApproved, note, reviewerID, now, id, UnbindPending); err != nil {
			return fmt.Errorf("更新解绑申请状态失败: %w", err)
		}
		req.Status = UnbindApproved
		req.Note = note
		req.ReviewedBy = reviewerID
		req.ReviewedAt = now
		approved = req
		return nil
	})
	if err != nil {
		return MailUnbindRequest{}, err
	}
	return approved, nil
}

// RejectUnbindRequest 驳回申请，地址保留。
func RejectUnbindRequest(ctx context.Context, q Querier, id int64,
	reviewerID int64, note string) (MailUnbindRequest, error) {

	now := Now()
	res, err := q.ExecContext(ctx, `
		UPDATE mail_unbind_requests
		SET status = ?, note = ?, reviewed_by = ?, reviewed_at = ?
		WHERE id = ? AND status = ?`,
		UnbindRejected, note, reviewerID, now, id, UnbindPending)
	if err != nil {
		return MailUnbindRequest{}, fmt.Errorf("驳回解绑申请失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return MailUnbindRequest{}, fmt.Errorf("%w: 申请不存在或已处理", ErrConflict)
	}
	return GetUnbindRequest(ctx, q, id)
}

// CancelUnbindRequest 用户撤销自己的待审申请。
func CancelUnbindRequest(ctx context.Context, q Querier, id, userID int64) (MailUnbindRequest, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE mail_unbind_requests SET status = ?, reviewed_at = ?
		WHERE id = ? AND user_id = ? AND status = ?`,
		UnbindRejected, Now(), id, userID, UnbindPending)
	if err != nil {
		return MailUnbindRequest{}, fmt.Errorf("撤销解绑申请失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 别人的单子、已处理的单子，一律报同一句：申请不存在或已处理。
		return MailUnbindRequest{}, fmt.Errorf("%w: 申请不存在或已处理", ErrConflict)
	}
	return GetUnbindRequest(ctx, q, id)
}

// getUnbindForUpdate 在事务内取申请，状态必须仍是 pending。
func getUnbindForUpdate(ctx context.Context, q Querier, id int64) (MailUnbindRequest, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+mailUnbindColumns+`
		FROM mail_unbind_requests r LEFT JOIN users u ON u.id = r.user_id
		WHERE r.id = ? AND r.status = ?`, id, UnbindPending)
	r, err := scanMailUnbindRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MailUnbindRequest{}, fmt.Errorf("%w: 申请不存在或已处理", ErrConflict)
	}
	if err != nil {
		return MailUnbindRequest{}, fmt.Errorf("读取解绑申请失败: %w", err)
	}
	return r, nil
}
