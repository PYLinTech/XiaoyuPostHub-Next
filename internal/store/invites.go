package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const inviteColumns = `id, code_hash, code_hint, group_name, max_uses, used_count, expires_at,
	disabled, created_by, created_at, note`

// CreateInviteCode 新建邀请码。code_hash 为明文码的慢哈希，明文不落库。
func CreateInviteCode(ctx context.Context, q Querier, c InviteCode) error {
	if c.CodeHash == "" {
		return fmt.Errorf("邀请码哈希不得为空")
	}
	if c.MaxUses <= 0 {
		c.MaxUses = 1
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO invite_codes (code_hash, code_hint, group_name, max_uses, used_count,
			expires_at, disabled, created_by, created_at, note)
		VALUES (?, ?, ?, ?, 0, ?, 0, ?, ?, ?)`,
		c.CodeHash, c.CodeHint, c.GroupName, c.MaxUses, c.ExpiresAt, c.CreatedBy, Now(), c.Note)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 邀请码已存在", ErrConflict)
		}
		return fmt.Errorf("创建邀请码失败: %w", err)
	}
	return nil
}

// GetInviteCode 按哈希取邀请码。
func GetInviteCode(ctx context.Context, q Querier, codeHash string) (InviteCode, error) {
	row := q.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invite_codes WHERE code_hash = ?`, codeHash)
	c, err := scanInviteCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return InviteCode{}, ErrNotFound
	}
	if err != nil {
		return InviteCode{}, fmt.Errorf("读取邀请码失败: %w", err)
	}
	return c, nil
}

// GetInviteCodeByID 按代理主键取邀请码，供管理端使用。
func GetInviteCodeByID(ctx context.Context, q Querier, id int64) (InviteCode, error) {
	row := q.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invite_codes WHERE id = ?`, id)
	c, err := scanInviteCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return InviteCode{}, ErrNotFound
	}
	if err != nil {
		return InviteCode{}, fmt.Errorf("读取邀请码失败: %w", err)
	}
	return c, nil
}

func scanInviteCode(row rowScanner) (InviteCode, error) {
	var c InviteCode
	var disabled int
	err := row.Scan(&c.ID, &c.CodeHash, &c.CodeHint, &c.GroupName, &c.MaxUses, &c.UsedCount,
		&c.ExpiresAt, &disabled, &c.CreatedBy, &c.CreatedAt, &c.Note)
	if err != nil {
		return InviteCode{}, err
	}
	c.Disabled = disabled != 0
	return c, nil
}

// ConsumeInviteCode 原子消费一次邀请码。
//
// 用量递增与"未超发、未过期、未停用"三个条件放在同一条 UPDATE 里：分开写
// 在并发注册下必然超发，而超发的邀请码意味着不受控的账号被创建出来。
// 命中 0 行即表示拒绝，调用方据此区分"码无效"与"码已用尽"。
func ConsumeInviteCode(ctx context.Context, q Querier, codeHash string, now int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE invite_codes SET used_count = used_count + 1
		WHERE code_hash = ? AND disabled = 0 AND used_count < max_uses
		  AND (expires_at = 0 OR expires_at > ?)`, codeHash, now)
	if err != nil {
		return fmt.Errorf("消费邀请码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: 邀请码无效、已用尽或已过期", ErrNoRowsAffected)
	}
	return nil
}

// InsertInviteUse 记录一次使用。
func InsertInviteUse(ctx context.Context, q Querier, u InviteUse) error {
	if u.UsedAt == 0 {
		u.UsedAt = Now()
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO invite_uses (code_hash, user_id, user_account, used_at, client_ip)
		VALUES (?, ?, ?, ?, ?)`, u.CodeHash, u.UserID, u.UserAccount, u.UsedAt, u.ClientIP)
	if err != nil {
		return fmt.Errorf("记录邀请码使用失败: %w", err)
	}
	return nil
}

// ListInviteCodes 分页列出邀请码。
func ListInviteCodes(ctx context.Context, q Querier, limit, offset int) ([]InviteCode, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `SELECT `+inviteColumns+`
		FROM invite_codes ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("列出邀请码失败: %w", err)
	}
	defer rows.Close()
	var out []InviteCode
	for rows.Next() {
		c, err := scanInviteCode(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描邀请码失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetInviteCodeDisabled 启用或停用邀请码。
func SetInviteCodeDisabled(ctx context.Context, q Querier, id int64, disabled bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE invite_codes SET disabled = ? WHERE id = ?`, boolToInt(disabled), id)
	if err != nil {
		return fmt.Errorf("变更邀请码状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteInviteCode 删除邀请码（使用记录随之级联删除）。
func DeleteInviteCode(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM invite_codes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除邀请码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListInviteUsesByCodeID 按代理主键列出使用记录，供管理端使用。
func ListInviteUsesByCodeID(ctx context.Context, q Querier, id int64, limit int) ([]InviteUse, error) {
	code, err := GetInviteCodeByID(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return ListInviteUses(ctx, q, code.CodeHash, limit)
}

// ListInviteUses 列出某个邀请码的使用记录。
func ListInviteUses(ctx context.Context, q Querier, codeHash string, limit int) ([]InviteUse, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, code_hash, user_id, user_account, used_at, client_ip
		FROM invite_uses WHERE code_hash = ? ORDER BY used_at DESC LIMIT ?`, codeHash, limit)
	if err != nil {
		return nil, fmt.Errorf("查询邀请码使用记录失败: %w", err)
	}
	defer rows.Close()
	var out []InviteUse
	for rows.Next() {
		var u InviteUse
		if err := rows.Scan(&u.ID, &u.CodeHash, &u.UserID, &u.UserAccount, &u.UsedAt, &u.ClientIP); err != nil {
			return nil, fmt.Errorf("扫描邀请码使用记录失败: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
