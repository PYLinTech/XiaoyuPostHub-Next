package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
)

// 配额键的命名空间约定。
//
// 用键值表而不是固定列：新增一个配额维度只是多插一行，不需要迁移结构。
const (
	// QuotaStorageTotal 是存储总量上限（明文字节）。
	QuotaStorageTotal = "storage.total"
	// QuotaFileMax 是单文件上限（明文字节）。
	QuotaFileMax = "file.max"
	// QuotaTrafficDailyDown 是每日下载流量上限（明文字节）。
	QuotaTrafficDailyDown = "traffic.daily.download"
	// QuotaPendingUploads 是该组每个用户可同时进行的上传任务上限。
	QuotaPendingUploads = "count.pending_uploads"

	// ---- 邮件配额（与文件配额相互独立）----

	// QuotaMailStorageTotal 是邮件存储总量上限（明文字节，按人头计费）。
	QuotaMailStorageTotal = "mail.storage.total"
	// QuotaMailAddresses 是单个用户在单个域名下可创建的邮箱地址数上限。
	// 无行时取全局设置 mail.addresses_default（默认 1），因此这里**不**写默认行。
	QuotaMailAddresses = "count.mail_addresses"
)

const groupColumns = `name, display_name, is_builtin, permissions, priority, resource_scheduling_priority, created_at`

func scanGroup(row rowScanner) (Group, error) {
	var g Group
	var builtin int
	err := row.Scan(&g.Name, &g.DisplayName, &builtin, &g.Permissions, &g.Priority, &g.ResourceSchedulingPriority, &g.CreatedAt)
	if err != nil {
		return Group{}, err
	}
	g.IsBuiltin = builtin != 0
	return g, nil
}

// EnsureBuiltinGroups 写入三个预设组及其默认配额。
//
// 只补缺失的行，不覆盖既有配置：管理员改过的权限与配额不能被重启抹掉。
func EnsureBuiltinGroups(ctx context.Context, q Querier) error {
	now := Now()
	for _, g := range perm.BuiltinGroups() {
		if _, err := q.ExecContext(ctx, `
			INSERT OR IGNORE INTO user_groups (name, display_name, is_builtin, permissions, priority, created_at)
			VALUES (?, ?, 1, ?, ?, ?)`,
			g.Name, g.DisplayName, g.Permissions, g.Priority, now); err != nil {
			return fmt.Errorf("写入预设用户组 %s 失败: %w", g.Name, err)
		}
	}
	for _, q0 := range builtinQuotas() {
		if _, err := q.ExecContext(ctx, `
			INSERT OR IGNORE INTO group_quotas (group_name, quota_key, limit_value)
			VALUES (?, ?, ?)`, q0.GroupName, q0.QuotaKey, q0.LimitValue); err != nil {
			return fmt.Errorf("写入预设配额 %s/%s 失败: %w", q0.GroupName, q0.QuotaKey, err)
		}
	}
	return nil
}

// builtinQuotas 是预设组的默认配额。数值是零值兜底，实际部署应按需调整。
func builtinQuotas() []GroupQuota {
	return []GroupQuota{
		// 访客没有账号，"配额"靠按 IP 的计数器生效；这里给的是同一 IP 的日流量上限。
		{perm.GroupGuest, QuotaTrafficDailyDown, 2 << 30},
		{perm.GroupGuest, QuotaFileMax, 0},
		{perm.GroupGuest, QuotaPendingUploads, 0},

		{perm.GroupNormal, QuotaStorageTotal, 10 << 30},
		{perm.GroupNormal, QuotaFileMax, 4 << 30},
		{perm.GroupNormal, QuotaTrafficDailyDown, 100 << 30},
		{perm.GroupNormal, QuotaPendingUploads, 8}, // 每位用户最多同时进行 8 个上传任务。
		// 邮件存储与文件分开计量：默认 1GiB 邮件空间。
		// count.mail_addresses 刻意不写行——默认上限 1 由全局设置兜底，
		// 写死行会让"全局默认值"调整对存量组失效。
		{perm.GroupNormal, QuotaMailStorageTotal, 1 << 30},

		// 管理员组不写配额行 = 不受限（0 与"没有行"语义必须区分开：0 是禁止，无行是无限）。
		{perm.GroupAdmin, QuotaFileMax, 100 << 30},
	}
}

// IsBuiltinGroup 判断是否为不可删除、不可改名的预设组。
func IsBuiltinGroup(name string) bool {
	return name == perm.GroupGuest || name == perm.GroupNormal || name == perm.GroupAdmin
}

// ListGroups 列出全部用户组，按优先级降序。
func ListGroups(ctx context.Context, q Querier) ([]Group, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+groupColumns+` FROM user_groups ORDER BY priority DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("列出用户组失败: %w", err)
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描用户组失败: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup 按名称取用户组。
func GetGroup(ctx context.Context, q Querier, name string) (Group, error) {
	row := q.QueryRowContext(ctx, `SELECT `+groupColumns+` FROM user_groups WHERE name = ?`, name)
	g, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("读取用户组失败: %w", err)
	}
	return g, nil
}

// CreateGroup 新建用户组。名称重复返回 ErrConflict。
func CreateGroup(ctx context.Context, q Querier, g Group) error {
	if g.Name == "" || strings.TrimSpace(g.DisplayName) == "" {
		return fmt.Errorf("用户组名与显示名不得为空")
	}
	if IsBuiltinGroup(g.Name) {
		return fmt.Errorf("%w: %s 是预设组，不能以同名新建", ErrConflict, g.Name)
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO user_groups (name, display_name, is_builtin, permissions, priority, resource_scheduling_priority, created_at)
		VALUES (?, ?, 0, ?, ?, ?, ?)`,
		g.Name, g.DisplayName, g.Permissions, g.Priority, g.ResourceSchedulingPriority, Now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 用户组 %s 已存在", ErrConflict, g.Name)
		}
		return fmt.Errorf("创建用户组失败: %w", err)
	}
	return nil
}

// UpdateGroup 修改组权限与显示名。预设组的名称不可变（这里不接受改名，
// 调用方若需要改名必须走"新建 + 迁移成员"）。
func UpdateGroup(ctx context.Context, q Querier, name, displayName string, permissions int64, priority, resourceSchedulingPriority int) error {
	res, err := q.ExecContext(ctx, `
		UPDATE user_groups SET display_name = ?, permissions = ?, priority = ?, resource_scheduling_priority = ?
		WHERE name = ?`, displayName, permissions, priority, resourceSchedulingPriority, name)
	if err != nil {
		return fmt.Errorf("更新用户组失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteGroup 删除用户组。预设组拒绝删除；组内仍有成员时也拒绝，
// 避免把成员悬置到一个不存在的组上。
func DeleteGroup(ctx context.Context, q Querier, name string) error {
	if IsBuiltinGroup(name) {
		return fmt.Errorf("%w: %s 是预设组，不能删除", ErrConflict, name)
	}
	var members int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE group_name = ?`, name).Scan(&members); err != nil {
		return fmt.Errorf("统计组成员失败: %w", err)
	}
	if members > 0 {
		return fmt.Errorf("%w: 用户组 %s 仍有 %d 个成员", ErrConflict, name, members)
	}
	var inviteRefs int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM invite_codes WHERE group_name = ?`, name).Scan(&inviteRefs); err != nil {
		return fmt.Errorf("统计邀请码引用失败: %w", err)
	}
	if inviteRefs > 0 {
		return fmt.Errorf("%w: 用户组 %s 仍被 %d 个邀请码引用", ErrConflict, name, inviteRefs)
	}
	res, err := q.ExecContext(ctx, `DELETE FROM user_groups WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("删除用户组失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListGroupQuotas 列出某组的全部配额项。
func ListGroupQuotas(ctx context.Context, q Querier, groupName string) ([]GroupQuota, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT group_name, quota_key, limit_value FROM group_quotas WHERE group_name = ? ORDER BY quota_key`,
		groupName)
	if err != nil {
		return nil, fmt.Errorf("列出组配额失败: %w", err)
	}
	defer rows.Close()
	// 空也返回非 nil 切片：这个结果会作为嵌套字段进 JSON，nil 会被编码成
	// null，前端对 null 取 .length 会直接抛错（表现为整页卡在加载态）。
	out := make([]GroupQuota, 0, 8)
	for rows.Next() {
		var gq GroupQuota
		if err := rows.Scan(&gq.GroupName, &gq.QuotaKey, &gq.LimitValue); err != nil {
			return nil, fmt.Errorf("扫描组配额失败: %w", err)
		}
		out = append(out, gq)
	}
	return out, rows.Err()
}

// ListAllGroupQuotas 一次取出全部用户组的配额，返回 组名 → 配额项。
//
// 管理端列组时每组各查一次就是 2N 条查询；这里一条搞定，调用方按组名取。
// 空组不会出现在结果里，调用方用缺省即可（"没有行"与"有行且为 0"语义不同）。
func ListAllGroupQuotas(ctx context.Context, q Querier) (map[string][]GroupQuota, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT group_name, quota_key, limit_value FROM group_quotas ORDER BY group_name, quota_key`)
	if err != nil {
		return nil, fmt.Errorf("列出全部组配额失败: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]GroupQuota, 8)
	for rows.Next() {
		var gq GroupQuota
		if err := rows.Scan(&gq.GroupName, &gq.QuotaKey, &gq.LimitValue); err != nil {
			return nil, fmt.Errorf("扫描组配额失败: %w", err)
		}
		out[gq.GroupName] = append(out[gq.GroupName], gq)
	}
	return out, rows.Err()
}

// CountUsersInAllGroups 一次统计每个用户组的成员数，返回 组名 → 人数。
func CountUsersInAllGroups(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT group_name, COUNT(*) FROM users GROUP BY group_name`)
	if err != nil {
		return nil, fmt.Errorf("统计各组人数失败: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64, 8)
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("扫描组人数失败: %w", err)
		}
		out[name] = n
	}
	return out, rows.Err()
}

// GroupQuotaMap 返回某组的配额映射，便于一次读取多处判定。
func GroupQuotaMap(ctx context.Context, q Querier, groupName string) (map[string]int64, error) {
	quotas, err := ListGroupQuotas(ctx, q, groupName)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(quotas))
	for _, gq := range quotas {
		out[gq.QuotaKey] = gq.LimitValue
	}
	return out, nil
}

// SetGroupQuota 设置（或覆盖）某项配额。
func SetGroupQuota(ctx context.Context, q Querier, groupName, key string, limit int64) error {
	if limit < 0 {
		return fmt.Errorf("配额不得为负")
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO group_quotas (group_name, quota_key, limit_value) VALUES (?, ?, ?)
		ON CONFLICT(group_name, quota_key) DO UPDATE SET limit_value = excluded.limit_value`,
		groupName, key, limit)
	if err != nil {
		return fmt.Errorf("设置组配额失败: %w", err)
	}
	return nil
}

// DeleteGroupQuota 删除某项配额。删除与设置为 0 语义不同：
// 没有行表示不受限，值为 0 表示完全禁止。
func DeleteGroupQuota(ctx context.Context, q Querier, groupName, key string) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM group_quotas WHERE group_name = ? AND quota_key = ?`, groupName, key); err != nil {
		return fmt.Errorf("删除组配额失败: %w", err)
	}
	return nil
}
