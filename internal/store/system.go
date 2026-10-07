package store

import (
	"context"
	"fmt"
	"strings"
)

// ListConfig 列出全部系统配置覆盖项。
func ListConfig(ctx context.Context, q Querier) ([]ConfigEntry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT key, value, value_type, updated_at, updated_by FROM system_config ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("列出系统配置失败: %w", err)
	}
	defer rows.Close()
	var out []ConfigEntry
	for rows.Next() {
		var e ConfigEntry
		if err := rows.Scan(&e.Key, &e.Value, &e.ValueType, &e.UpdatedAt, &e.UpdatedBy); err != nil {
			return nil, fmt.Errorf("扫描系统配置失败: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetConfig 写入或覆盖一项系统配置。
func SetConfig(ctx context.Context, q Querier, key, value, valueType string, by int64) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("配置键不得为空")
	}
	if valueType == "" {
		valueType = "string"
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO system_config (key, value, value_type, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, value_type = excluded.value_type,
			updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		key, value, valueType, Now(), by)
	if err != nil {
		return fmt.Errorf("写入系统配置失败: %w", err)
	}
	return nil
}

// DeleteConfig 删除一项覆盖，使其回到代码内置默认值。
func DeleteConfig(ctx context.Context, q Querier, key string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM system_config WHERE key = ?`, key); err != nil {
		return fmt.Errorf("删除系统配置失败: %w", err)
	}
	return nil
}

// InsertAuditLog 写入一条审计记录。
func InsertAuditLog(ctx context.Context, q Querier, l AuditLog) error {
	if l.OccurredAt == 0 {
		l.OccurredAt = Now()
	}
	if l.ActorType == "" {
		l.ActorType = "user"
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO audit_logs (actor_type, actor_id, client_ip, action, target, detail, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.ActorType, l.ActorID, l.ClientIP, l.Action, l.Target, l.Detail, l.OccurredAt)
	if err != nil {
		return fmt.Errorf("写入审计日志失败: %w", err)
	}
	return nil
}

// ListAuditLogs 分页查询审计记录。action 非空时精确匹配；action 为空且
// actionPrefix 非空时按命名空间前缀匹配（如 "admin.mail" 命中所有邮件动作）。
// 前缀中的 LIKE 通配符按字面量处理。
func ListAuditLogs(ctx context.Context, q Querier, action, actionPrefix string,
	from, to int64, limit, offset int) ([]AuditLog, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var where []string
	var args []any
	if action != "" {
		where = append(where, "action = ?")
		args = append(args, action)
	} else if prefix := strings.TrimSpace(actionPrefix); prefix != "" {
		where = append(where, "action LIKE ? ESCAPE '\\'")
		args = append(args, likeAuditPrefix(prefix))
	}
	if from > 0 {
		where = append(where, "occurred_at >= ?")
		args = append(args, from)
	}
	if to > 0 {
		where = append(where, "occurred_at <= ?")
		args = append(args, to)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计审计日志失败: %w", err)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, actor_type, actor_id, client_ip, action, target, detail, occurred_at
		FROM audit_logs`+clause+` ORDER BY occurred_at DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询审计日志失败: %w", err)
	}
	defer rows.Close()
	var out []AuditLog
	for rows.Next() {
		var l AuditLog
		if err := rows.Scan(&l.ID, &l.ActorType, &l.ActorID, &l.ClientIP, &l.Action, &l.Target, &l.Detail, &l.OccurredAt); err != nil {
			return nil, 0, fmt.Errorf("扫描审计日志失败: %w", err)
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// likeAuditPrefix 把审计动作前缀转成安全的 LIKE 参数：转义 \ % _，再加 %。
// 配合 SQL 中的 ESCAPE '\'，前缀里的通配符按字面量匹配。
func likeAuditPrefix(prefix string) string {
	return likeEscape(prefix) + "%"
}

// likeContains 转成"包含"语义的安全 LIKE 参数（%escaped%）。
// 所有来自用户输入的 LIKE 都必须走这里，避免 % _ 被当成通配符。
func likeContains(s string) string {
	return "%" + likeEscape(s) + "%"
}

// likeEscape 转义 LIKE 的三个特殊字符，配合 SQL 中的 ESCAPE '\'。
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// DeleteAuditLogsBeforeBatch 限制单次删除量，避免维护任务长时间占用写锁。
func DeleteAuditLogsBeforeBatch(ctx context.Context, q Querier, before int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	res, err := q.ExecContext(ctx, `
		DELETE FROM audit_logs
		WHERE id IN (SELECT id FROM audit_logs WHERE occurred_at < ? ORDER BY id LIMIT ?)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("分批清理审计日志失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
