package store

import (
	"context"
	"fmt"
)

// 本文件是管理端概览页专用的聚合查询。
//
// 统一约束：概览是后台里第一个被打开的页面，它的每一条 SQL 都会乘以"每个
// 管理员每天登录若干次"。所以这里只允许两类查询——读预聚合表
// （traffic_daily），或者走以时间/状态开头的已有索引做窄范围扫描。
// 任何一条无索引的 COUNT(*) 都不该出现在这个文件里。
//
// 另有一条 JSON 约束：所有返回切片的函数都用 make 预分配而不是 var 声明。
// nil 切片会序列化成 null，而这份数据的消费者是前端，拿 null 直接 .map()
// 会让整个概览页崩掉。

// TrafficPoint 是某一天的流量合计。
type TrafficPoint struct {
	Day string `json:"day"`
	// 用明文口径：概览是给人看的趋势，存储侧对账另有页面用 wire 口径。
	UpPlain   int64 `json:"upPlain"`
	DownPlain int64 `json:"downPlain"`
}

// TrafficDailyRange 返回 [fromDay, toDay]（含端点，UTC 日期键）内每天的流量合计。
//
// 读 traffic_daily 而不是 traffic_logs：明细表随时间无界增长，按天 SUM 明细
// 会让这个页面随站点运行时长线性变慢。跨 actor_key 求和交给 SQL，返回行数
// 只与天数相关，与用户数无关。
//
// 缺失的日期不会出现在结果里（那几天确实没有流量），由调用方补零——
// 补零规则属于展示口径，不该混进存储层。
func TrafficDailyRange(ctx context.Context, q Querier, fromDay, toDay string) ([]TrafficPoint, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT day,
		       COALESCE(SUM(up_plain), 0)   AS up_plain,
		       COALESCE(SUM(down_plain), 0) AS down_plain
		FROM traffic_daily
		WHERE day >= ? AND day <= ?
		GROUP BY day
		ORDER BY day`, fromDay, toDay)
	if err != nil {
		return nil, fmt.Errorf("查询流量日聚合失败: %w", err)
	}
	defer rows.Close()

	out := make([]TrafficPoint, 0, 32)
	for rows.Next() {
		var p TrafficPoint
		if err := rows.Scan(&p.Day, &p.UpPlain, &p.DownPlain); err != nil {
			return nil, fmt.Errorf("读取流量日聚合失败: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历流量日聚合失败: %w", err)
	}
	return out, nil
}

// ActionStat 是某类流量的次数与字节合计。
type ActionStat struct {
	Action string `json:"action"`
	Count  int64  `json:"count"`
	Bytes  int64  `json:"bytes"`
}

// TrafficByActionSince 汇总 since 之后的流量按动作分类。
//
// 明细表在这里是可接受的：时间窗有界且走 idx_traffic_time，日聚合表按
// 动作维度并不存在，再造一张反而要为"每加一个动作就多写一个分支"付出代价。
func TrafficByActionSince(ctx context.Context, q Querier, since int64) ([]ActionStat, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT action, COUNT(*), COALESCE(SUM(bytes_plain), 0)
		FROM traffic_logs
		WHERE occurred_at >= ?
		GROUP BY action
		ORDER BY COUNT(*) DESC`, since)
	if err != nil {
		return nil, fmt.Errorf("查询流量动作分类失败: %w", err)
	}
	defer rows.Close()

	out := make([]ActionStat, 0, 8)
	for rows.Next() {
		var a ActionStat
		if err := rows.Scan(&a.Action, &a.Count, &a.Bytes); err != nil {
			return nil, fmt.Errorf("读取流量动作分类失败: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历流量动作分类失败: %w", err)
	}
	return out, nil
}

// GroupStat 是某个用户组的成员数。
type GroupStat struct {
	GroupName string `json:"groupName"`
	Count     int64  `json:"count"`
}

// CountUsersByGroup 按用户组统计成员数。
//
// 走 idx_users_group。禁用账号一并计入：概览展示的是"这个组占多少人力"，
// 而不是"有多少人能登录"，后者是账号页的职责。
func CountUsersByGroup(ctx context.Context, q Querier) ([]GroupStat, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT group_name, COUNT(*)
		FROM users
		GROUP BY group_name
		ORDER BY COUNT(*) DESC, group_name`)
	if err != nil {
		return nil, fmt.Errorf("查询用户分组统计失败: %w", err)
	}
	defer rows.Close()

	out := make([]GroupStat, 0, 8)
	for rows.Next() {
		var g GroupStat
		if err := rows.Scan(&g.GroupName, &g.Count); err != nil {
			return nil, fmt.Errorf("读取用户分组统计失败: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户分组统计失败: %w", err)
	}
	return out, nil
}

// CountActiveUsersSince 统计 since 之后有过动作的启用账号数。
//
// 读 users.last_action_at 而不是去 traffic_logs 里 DISTINCT user_id：
// 前者走 idx_users_last_action 且只扫一行宽的索引，后者在明细表上做去重。
// 语义上也更准——"最近有动作"本来就由用户行记录。
func CountActiveUsersSince(ctx context.Context, q Querier, since int64) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users WHERE status = 1 AND last_action_at >= ?`, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计活跃账号失败: %w", err)
	}
	return n, nil
}

// SharesOverview 是分享与取件码的规模快照。
type SharesOverview struct {
	// Total 是全部分享；Alive 是未禁用且未过期的。
	Total   int64 `json:"total"`
	Alive   int64 `json:"alive"`
	Visits  int64 `json:"visits"`
	Pickups int64 `json:"pickups"`
	// Views/Downloads 是 since 之后的分享访问次数。
	Views     int64 `json:"views"`
	Downloads int64 `json:"downloads"`
}

// CountSharesOverview 汇总分享规模与 since 之后的访问次数。
//
// shares 与 share_accesses 都是小表（分享数是人工创建的，访问明细受
// idx_share_accesses_time 约束），直接聚合即可，不预聚合。
func CountSharesOverview(ctx context.Context, q Querier, now, since int64) (SharesOverview, error) {
	var out SharesOverview
	// 过期判定在 SQL 里做而不是取出来算：expires_at=0 表示永久，
	// 这个分支交给 SQL 比在 Go 里遍历更省事也更好读。
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN disabled = 0 AND (expires_at = 0 OR expires_at > ?)
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(visits), 0)
		FROM shares`, now).Scan(&out.Total, &out.Alive, &out.Visits)
	if err != nil {
		return out, fmt.Errorf("统计分享规模失败: %w", err)
	}

	pickups, err := CountAlivePickupCodes(ctx, q, now)
	if err != nil {
		return out, err
	}
	out.Pickups = pickups

	if err := q.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN action = 'view' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN action = 'download' THEN 1 ELSE 0 END), 0)
		FROM share_accesses
		WHERE occurred_at >= ?`, since).Scan(&out.Views, &out.Downloads); err != nil {
		return out, fmt.Errorf("统计分享访问失败: %w", err)
	}
	return out, nil
}

// MailOverview 是收件箱的规模快照。
type MailOverview struct {
	Total     int64 `json:"total"`
	Unread    int64 `json:"unread"`
	Starred   int64 `json:"starred"`
	Archived  int64 `json:"archived"`
	Bytes     int64 `json:"bytes"`
	Addresses int64 `json:"addresses"`
}

// CountMailOverview 汇总收件箱规模。
//
// 只统计 role='inbox' 且 status<>'released'：released 的行是已释放的物理
// 残留（部件引用与配额已结），算进来会让"收件箱里有多少封"凭空多出一截。
func CountMailOverview(ctx context.Context, q Querier) (MailOverview, error) {
	var out MailOverview
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN is_read = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN is_starred = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'archived' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(charged_bytes), 0)
		FROM mailboxes
		WHERE role = 'inbox' AND status <> 'released'`).
		Scan(&out.Total, &out.Unread, &out.Starred, &out.Archived, &out.Bytes); err != nil {
		return out, fmt.Errorf("统计收件箱失败: %w", err)
	}

	// 邮件功能关闭时 mail_domains 可能整表为空，此时不该报错——
	// 概览是所有人的入口，不能因为一个可选子系统缺数据就整页失败。
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mail_addresses`).Scan(&out.Addresses); err != nil {
		return out, fmt.Errorf("统计收件地址失败: %w", err)
	}
	return out, nil
}
