package store

import (
	"context"
	"fmt"
)

// 配额预扣与结算。
//
// 判定走计数器而不是聚合明细表：明细表无界增长，SUM 全表的延迟会随数据量恶化。
// 预扣必须是原子条件更新——"先查再写"在并发下必然击穿上限。
//
// 语义区分：没有计数器行 = 从未使用过；没有配额限制行 = 不受限。两者不可混淆，
// 因此 ReserveCounter 只在调用方明确给出 limit 时才做上限判定。

// AddCounter 无条件累加计数器并返回新值。
func AddCounter(ctx context.Context, q Querier, scope CounterScope, key string, delta int64) (int64, error) {
	if _, err := q.ExecContext(ctx, `
		INSERT INTO quota_counters (scope, key, used, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(scope, key) DO UPDATE SET used = used + excluded.used, updated_at = excluded.updated_at`,
		string(scope), key, delta, Now()); err != nil {
		return 0, fmt.Errorf("累加计数器失败: %w", err)
	}
	return GetCounter(ctx, q, scope, key)
}

// ReserveCounter 在不超过 limit 的前提下累加计数器。
//
// 冲突与非冲突两个分支都必须做上限校验：UPSERT 的 WHERE 只作用于
// ON CONFLICT DO UPDATE，无冲突的纯 INSERT 若不校验，用户的第一笔
// 预扣（或按日计数时每天的第一笔）就能直接击穿配额。首次插入走
// SELECT ... WHERE 的条件形态，0 行即超限。
func ReserveCounter(ctx context.Context, q Querier, scope CounterScope, key string, delta, limit int64) (int64, error) {
	if delta < 0 {
		return 0, fmt.Errorf("预扣量不得为负")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("%w: 配额上限为 %d", ErrQuotaExceeded, limit)
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO quota_counters (scope, key, used, updated_at)
		SELECT ?, ?, ?, ? WHERE ? <= ?
		ON CONFLICT(scope, key) DO UPDATE SET used = used + excluded.used, updated_at = excluded.updated_at
		WHERE quota_counters.used + excluded.used <= ?`,
		string(scope), key, delta, Now(), delta, limit, limit)
	if err != nil {
		return 0, fmt.Errorf("预扣计数器失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取预扣结果失败: %w", err)
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: 需要 %d，上限 %d", ErrQuotaExceeded, delta, limit)
	}
	return GetCounter(ctx, q, scope, key)
}

// ReleaseCounter 回退计数器，下限为 0。
//
// 结算时段用：传输中断、失败、实际用量小于预扣量时都要把差额还回去，
// 否则额度会被永久占住。下限钳制是为了容忍重复结算——宁可少扣也不能变成负数。
func ReleaseCounter(ctx context.Context, q Querier, scope CounterScope, key string, delta int64) (int64, error) {
	if delta < 0 {
		return 0, fmt.Errorf("回退量不得为负")
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE quota_counters SET used = MAX(used - ?, 0), updated_at = ?
		WHERE scope = ? AND key = ?`,
		delta, Now(), string(scope), key); err != nil {
		return 0, fmt.Errorf("回退计数器失败: %w", err)
	}
	return GetCounter(ctx, q, scope, key)
}

// GetCounter 读取计数器当前值；不存在返回 0。
func GetCounter(ctx context.Context, q Querier, scope CounterScope, key string) (int64, error) {
	var used int64
	err := q.QueryRowContext(ctx,
		`SELECT used FROM quota_counters WHERE scope = ? AND key = ?`, string(scope), key).Scan(&used)
	if err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取计数器失败: %w", err)
	}
	return used, nil
}

// UserCounterKey / GuestCounterKey 构造计数器键。
//
// 访客没有账号，按 IP 当"用户"计量；IPv6 需要按前缀聚合，否则同一台设备
// 换一个地址就能重置额度。
func UserCounterKey(userID int64, suffix string) string {
	if suffix == "" {
		return fmt.Sprintf("user:%d", userID)
	}
	return fmt.Sprintf("user:%d:%s", userID, suffix)
}

// GuestCounterKey 构造访客的计数器键。
func GuestCounterKey(ipPrefix, suffix string) string {
	if suffix == "" {
		return "ip:" + ipPrefix
	}
	return fmt.Sprintf("ip:%s:%s", ipPrefix, suffix)
}
