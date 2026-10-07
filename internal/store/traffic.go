package store

import (
	"context"
	"fmt"
	"strings"
)

// InsertTrafficLog 写入一条流量明细。
//
// 明细只用于审计、申诉与对账，不参与配额判定：它会随时间无界增长，
// 拿它做 SUM 会让判定延迟随数据量恶化。
func InsertTrafficLog(ctx context.Context, q Querier, l TrafficLog) error {
	if l.OccurredAt == 0 {
		l.OccurredAt = Now()
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO traffic_logs (actor_type, user_id, client_ip, group_name, action,
			bytes_plain, bytes_wire, resource_path, share_id, pickup_code, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(l.ActorType), l.UserID, l.ClientIP, l.GroupName, l.Action,
		l.BytesPlain, l.BytesWire, l.ResourcePath, l.ShareID, l.PickupCode, l.OccurredAt)
	if err != nil {
		return fmt.Errorf("写入流量明细失败: %w", err)
	}
	return nil
}

// UpsertTrafficDaily 累加某主体某天的流量聚合。
//
// 双口径记账：对用户展示与配额判定用明文字节，对存储侧实际消耗用密文字节
// （密文 = 明文 + 16 字节/块）。两者都要记，否则无法解释"为什么 123 后台
// 的用量比站点统计的多"。
func UpsertTrafficDaily(ctx context.Context, q Querier, d TrafficDaily) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO traffic_daily (actor_key, day, group_name, up_plain, up_wire, down_plain, down_wire)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(actor_key, day) DO UPDATE SET
			group_name = excluded.group_name,
			up_plain   = up_plain   + excluded.up_plain,
			up_wire    = up_wire    + excluded.up_wire,
			down_plain = down_plain + excluded.down_plain,
			down_wire  = down_wire  + excluded.down_wire`,
		d.ActorKey, d.Day, d.GroupName, d.UpPlain, d.UpWire, d.DownPlain, d.DownWire)
	if err != nil {
		return fmt.Errorf("累加流量聚合失败: %w", err)
	}
	return nil
}

// TrafficFilter 是流量明细的查询条件。零值表示该维度不过滤。
type TrafficFilter struct {
	ActorType ActorType
	UserID    int64
	ClientIP  string
	GroupName string
	Action    string
	From      int64
	To        int64
	Limit     int
	Offset    int
}

// ListTrafficLogs 按条件分页查询流量明细。
func ListTrafficLogs(ctx context.Context, q Querier, f TrafficFilter) ([]TrafficLog, int64, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	var where []string
	var args []any
	if f.ActorType != "" {
		where = append(where, "actor_type = ?")
		args = append(args, string(f.ActorType))
	}
	if f.UserID != 0 {
		where = append(where, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.ClientIP != "" {
		where = append(where, "client_ip = ?")
		args = append(args, f.ClientIP)
	}
	if f.GroupName != "" {
		where = append(where, "group_name = ?")
		args = append(args, f.GroupName)
	}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.From > 0 {
		where = append(where, "occurred_at >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		where = append(where, "occurred_at <= ?")
		args = append(args, f.To)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_logs`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计流量明细失败: %w", err)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT id, actor_type, user_id, client_ip, group_name, action,
		       bytes_plain, bytes_wire, resource_path, share_id, pickup_code, occurred_at
		FROM traffic_logs`+clause+` ORDER BY occurred_at DESC LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询流量明细失败: %w", err)
	}
	defer rows.Close()

	var out []TrafficLog
	for rows.Next() {
		var l TrafficLog
		var actor string
		if err := rows.Scan(&l.ID, &actor, &l.UserID, &l.ClientIP, &l.GroupName, &l.Action,
			&l.BytesPlain, &l.BytesWire, &l.ResourcePath, &l.ShareID, &l.PickupCode, &l.OccurredAt); err != nil {
			return nil, 0, fmt.Errorf("扫描流量明细失败: %w", err)
		}
		l.ActorType = ActorType(actor)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// DeleteTrafficLogsBeforeBatch 限制单次删除量，避免保留期清理长时间占用
// SQLite 写锁。id 是自增主键，子查询可稳定选出一批再执行删除。
func DeleteTrafficLogsBeforeBatch(ctx context.Context, q Querier, before int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	res, err := q.ExecContext(ctx, `
		DELETE FROM traffic_logs
		WHERE id IN (SELECT id FROM traffic_logs WHERE occurred_at < ? ORDER BY id LIMIT ?)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("分批清理流量明细失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
