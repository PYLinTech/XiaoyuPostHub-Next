package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const userColumns = `id, account, display_name, password_hash, group_name, status,
	last_action_at, created_at, updated_at`

func scanUser(row rowScanner) (User, error) {
	var u User
	var status int
	err := row.Scan(&u.ID, &u.Account, &u.DisplayName, &u.PasswordHash, &u.GroupName, &status,
		&u.LastActionAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return User{}, err
	}
	u.Status = UserStatus(status)
	return u, nil
}

// CreateUser 新建账号，account 重复返回 ErrConflict。
//
// passwordHash 必须是慢哈希编码串；明文密码不得进入本函数。
func CreateUser(ctx context.Context, q Querier, u User) (int64, error) {
	now := Now()
	res, err := q.ExecContext(ctx, `
		INSERT INTO users (account, display_name, password_hash, group_name, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.Account, u.DisplayName, u.PasswordHash, u.GroupName, int(u.Status), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("%w: 账号 %s 已存在", ErrConflict, u.Account)
		}
		return 0, fmt.Errorf("创建账号失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("读取新账号 id 失败: %w", err)
	}
	return id, nil
}

// GetUserByAccount 按账号名取用户。比较区分大小写，"Alice" 与 "alice" 是不同账号。
func GetUserByAccount(ctx context.Context, q Querier, account string) (User, error) {
	row := q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE account = ?`, account)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("读取账号失败: %w", err)
	}
	return u, nil
}

// GetUserByID 按主键取用户。
func GetUserByID(ctx context.Context, q Querier, id int64) (User, error) {
	row := q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("读取账号失败: %w", err)
	}
	return u, nil
}

// ListUsers 分页列出账号。
func ListUsers(ctx context.Context, q Querier, limit, offset int) ([]User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `SELECT `+userColumns+`
		FROM users ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("列出账号失败: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描账号失败: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers 统计账号总数。
func CountUsers(ctx context.Context, q Querier) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计账号失败: %w", err)
	}
	return n, nil
}

// CountEnabledUsersInGroup 统计某用户组内处于启用状态的账号数，可排除一个账号。
//
// 服务层用它保证"管理员组至少留一个可用账号"：把最后一个管理员移出组或
// 禁用掉，站点就失去了唯一能管理自身的身份，只能靠手工改库救回。
// 排除参数用于"这个账号本人不算"的场景（它即将被移出或禁用）。
func CountEnabledUsersInGroup(ctx context.Context, q Querier, groupName string, excludeID int64) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE group_name = ? AND id != ? AND status = ?`,
		groupName, excludeID, int(UserEnabled)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计组内可用账号失败: %w", err)
	}
	return n, nil
}

// SetUserStatus 启用或禁用账号。
func SetUserStatus(ctx context.Context, q Querier, id int64, status UserStatus) error {
	res, err := q.ExecContext(ctx, `UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
		int(status), Now(), id)
	if err != nil {
		return fmt.Errorf("变更账号状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserGroup 变更账号所属组。
func UpdateUserGroup(ctx context.Context, q Querier, id int64, groupName string) error {
	res, err := q.ExecContext(ctx, `UPDATE users SET group_name = ?, updated_at = ? WHERE id = ?`,
		groupName, Now(), id)
	if err != nil {
		return fmt.Errorf("变更账号分组失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserPassword 更新密码哈希。
func UpdateUserPassword(ctx context.Context, q Querier, id int64, passwordHash string) error {
	res, err := q.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, Now(), id)
	if err != nil {
		return fmt.Errorf("更新密码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchUserLastAction 更新账号的最近动作时间。
//
// 只在时间推进时写入：条件更新让重复请求不产生实质写入，避免把用户行
// 变成全站写热点（逐次请求的活跃时间记录在会话行上）。
func TouchUserLastAction(ctx context.Context, q Querier, id, at int64) error {
	_, err := q.ExecContext(ctx,
		`UPDATE users SET last_action_at = ? WHERE id = ? AND last_action_at < ?`, at, id, at)
	if err != nil {
		return fmt.Errorf("更新最近动作时间失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- 会话

const sessionColumns = `token_hash, user_id, created_at, last_seen_at, expires_at, client_ip, user_agent, revoked_at`

func scanSession(row rowScanner) (Session, error) {
	var s Session
	var created, lastSeen, expires int64
	err := row.Scan(&s.TokenHash, &s.UserID, &created, &lastSeen, &expires, &s.ClientIP, &s.UserAgent, &s.RevokedAt)
	if err != nil {
		return Session{}, err
	}
	s.CreatedAt = ToTime(created)
	s.LastSeenAt = ToTime(lastSeen)
	s.ExpiresAt = ToTime(expires)
	return s, nil
}

// CreateSession 写入一条会话。token 只以哈希形式落库：库被读走也无法直接使用。
func CreateSession(ctx context.Context, q Querier, s Session) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at, client_ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.TokenHash, s.UserID, FromTime(s.CreatedAt), FromTime(s.LastSeenAt), FromTime(s.ExpiresAt),
		s.ClientIP, s.UserAgent)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 会话令牌冲突", ErrConflict)
		}
		return fmt.Errorf("创建会话失败: %w", err)
	}
	return nil
}

// GetSession 按令牌哈希取会话。
func GetSession(ctx context.Context, q Querier, tokenHash string) (Session, error) {
	row := q.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE token_hash = ?`, tokenHash)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("读取会话失败: %w", err)
	}
	return s, nil
}

// TouchSession 更新会话的最近活跃时间。
func TouchSession(ctx context.Context, q Querier, tokenHash string, at time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`,
		FromTime(at), tokenHash)
	if err != nil {
		return fmt.Errorf("更新会话活跃时间失败: %w", err)
	}
	return nil
}

// RevokeSession 吊销单个会话。
func RevokeSession(ctx context.Context, q Querier, tokenHash string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at = 0`, Now(), tokenHash)
	if err != nil {
		return fmt.Errorf("吊销会话失败: %w", err)
	}
	return nil
}

// RestoreSession 在同一事务内恢复一条仍属于指定账号的会话。
//
// 改密会先吊销账号的全部会话，再恢复发起改密请求的这一条。恢复必须和吊销
// 在同一事务中完成；若拆成两次写入，中间失败会让用户的口令已更新但当前会话
// 无法继续使用，或者恢复了不属于该账号的令牌。
func RestoreSession(ctx context.Context, q Querier, userID int64, tokenHash string) error {
	res, err := q.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = 0
		 WHERE token_hash = ? AND user_id = ?`, tokenHash, userID)
	if err != nil {
		return fmt.Errorf("恢复当前会话失败: %w", err)
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		return fmt.Errorf("读取当前会话恢复结果失败: %w", rowsErr)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeUserSessions 吊销某账号的全部会话（改密、封禁、踢出时使用）。
func RevokeUserSessions(ctx context.Context, q Querier, userID int64) error {
	_, err := q.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at = 0`, Now(), userID)
	if err != nil {
		return fmt.Errorf("吊销账号会话失败: %w", err)
	}
	return nil
}

// RevokeGroupSessions 吊销某用户组全部成员的会话。
//
// 组权限是鉴权结果的一部分。权限降级若只修改 user_groups，已有会话仍可
// 携带缓存的旧权限继续访问；把吊销放进同一写事务后，提交时权限变更与会话
// 失效同时生效，不留下可利用的时间窗口。
func RevokeGroupSessions(ctx context.Context, q Querier, groupName string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?
		WHERE revoked_at = 0 AND user_id IN (
			SELECT id FROM users WHERE group_name = ?
		)`, Now(), groupName)
	if err != nil {
		return fmt.Errorf("吊销用户组会话失败: %w", err)
	}
	return nil
}

// DeleteExpiredSessionsBatch 分批回收过期或已吊销会话，避免大量历史登录
// 记录一次性占用写锁。
func DeleteExpiredSessionsBatch(ctx context.Context, q Querier, before int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	res, err := q.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE token_hash IN (
			SELECT token_hash FROM sessions
			WHERE expires_at < ? OR revoked_at <> 0
			ORDER BY expires_at LIMIT ?
		)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("分批清理过期会话失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteStaleThrottles 清理已结束退避期的失败状态。账号不存在或攻击者轮换
// 账号时没有“成功登录”路径触发 ClearThrottle，若不清理这些键，throttle
// 表会被低成本地无限膨胀。
func DeleteStaleThrottles(ctx context.Context, q Querier, before int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	now := Now()
	res, err := q.ExecContext(ctx, `
		DELETE FROM throttle
		WHERE updated_at < ? AND delay_until <= ?
		AND key IN (SELECT key FROM throttle WHERE updated_at < ? AND delay_until <= ? ORDER BY updated_at, key LIMIT ?)`,
		before, now, before, now, limit)
	if err != nil {
		return 0, fmt.Errorf("清理过期退避状态失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------------------------------------------------------------- 退避

// GetThrottle 读取某个键维度的退避状态；不存在返回零值而非错误。
func GetThrottle(ctx context.Context, q Querier, key string) (Throttle, error) {
	var t Throttle
	err := q.QueryRowContext(ctx,
		`SELECT key, fail_count, delay_until, updated_at FROM throttle WHERE key = ?`, key).
		Scan(&t.Key, &t.FailCount, &t.DelayUntil, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Throttle{Key: key}, nil
	}
	if err != nil {
		return Throttle{}, fmt.Errorf("读取退避状态失败: %w", err)
	}
	return t, nil
}

// bumpThrottle 记一次失败并按指数退避推进解禁时间。
//
// base 是指数底数、max 是上限；两者由调用方按维度给出（账号维度与 IP 维度
// 可以配置成不同的强度）。退避在达到上限后保持，不会无限增长。
func bumpThrottle(ctx context.Context, q Querier, key string, base, max time.Duration) (Throttle, error) {
	now := Now()
	if _, err := q.ExecContext(ctx, `
		INSERT INTO throttle (key, fail_count, delay_until, updated_at) VALUES (?, 1, 0, ?)
		ON CONFLICT(key) DO UPDATE SET fail_count = fail_count + 1, updated_at = excluded.updated_at`,
		key, now); err != nil {
		return Throttle{}, fmt.Errorf("累加失败计数失败: %w", err)
	}
	current, err := GetThrottle(ctx, q, key)
	if err != nil {
		return Throttle{}, err
	}
	delay := backoffDelay(base, max, current.FailCount)
	current.DelayUntil = now + int64(delay.Seconds())
	if _, err := q.ExecContext(ctx, `UPDATE throttle SET delay_until = ? WHERE key = ?`,
		current.DelayUntil, key); err != nil {
		return Throttle{}, fmt.Errorf("更新解禁时间失败: %w", err)
	}
	current.UpdatedAt = now
	return current, nil
}

// BumpThrottleAtomic 在数据库写事务中推进退避状态，确保“累加失败次数、读取
// 最新次数、写入解禁时间”不会被并发失败请求交叉覆盖。
func BumpThrottleAtomic(ctx context.Context, db *DB, key string, base, max time.Duration) (Throttle, error) {
	var state Throttle
	if db == nil {
		return state, fmt.Errorf("推进退避状态失败: 数据库为空")
	}
	err := db.InTx(ctx, func(tx Querier) error {
		var err error
		state, err = bumpThrottle(ctx, tx, key, base, max)
		return err
	})
	return state, err
}

// backoffDelay 计算第 failCount 次失败对应的等待时长：base * 2^(n-1)，封顶 max。
func backoffDelay(base, max time.Duration, failCount int) time.Duration {
	if base <= 0 || failCount <= 0 {
		return 0
	}
	delay := base
	for i := 1; i < failCount; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
}

// ClearThrottle 清除退避状态（登录成功、提取码正确时调用）。
func ClearThrottle(ctx context.Context, q Querier, key string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM throttle WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("清除退避状态失败: %w", err)
	}
	return nil
}
