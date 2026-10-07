package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const announcementColumns = `id, kind, audience, title, body, enabled, pinned, expire_at,
	created_by, created_at, updated_at`

func scanAnnouncement(row rowScanner) (Announcement, error) {
	var a Announcement
	var kind, audience string
	var enabled, pinned int
	err := row.Scan(&a.ID, &kind, &audience, &a.Title, &a.Body, &enabled, &pinned,
		&a.ExpireAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Announcement{}, err
	}
	a.Kind = AnnouncementKind(kind)
	a.Audience = Audience(audience)
	a.Enabled = enabled != 0
	a.Pinned = pinned != 0
	return a, nil
}

// CreateAnnouncement 新建一条公告或消息。
//
// 顶部滚动公告的"有且仅有一条"由数据库的部分唯一索引保证；这里先把既有的
// 启用中滚动公告停用，让"换一条置顶公告"成为一个正常操作而不是唯一约束报错。
func CreateAnnouncement(ctx context.Context, q Querier, a Announcement) error {
	now := Now()
	if a.ID == "" {
		id, err := NewID("ann")
		if err != nil {
			return err
		}
		a.ID = id
	}
	if a.Kind == KindTicker {
		if err := DisableActiveTickers(ctx, q, a.ID); err != nil {
			return err
		}
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO announcements (id, kind, audience, title, body, enabled, pinned, expire_at,
			created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, string(a.Kind), string(a.Audience), a.Title, a.Body,
		boolToInt(a.Enabled), boolToInt(a.Pinned), a.ExpireAt, a.CreatedBy, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: 已存在启用中的滚动公告", ErrConflict)
		}
		return fmt.Errorf("创建公告失败: %w", err)
	}
	if a.Audience == AudienceUsers {
		if err := SetAnnouncementTargets(ctx, q, a.ID, a.Targets); err != nil {
			return err
		}
	}
	return nil
}

// DisableActiveTickers 停用全部启用中的滚动公告（可排除某一条）。
func DisableActiveTickers(ctx context.Context, q Querier, exceptID string) error {
	query := `UPDATE announcements SET enabled = 0, updated_at = ? WHERE kind = 'ticker' AND enabled = 1`
	args := []any{Now()}
	if exceptID != "" {
		query += ` AND id <> ?`
		args = append(args, exceptID)
	}
	if _, err := q.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("停用滚动公告失败: %w", err)
	}
	return nil
}

// SetAnnouncementTargets 覆盖定向接收者名单。
func SetAnnouncementTargets(ctx context.Context, q Querier, id string, userIDs []int64) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM announcement_targets WHERE announcement_id = ?`, id); err != nil {
		return fmt.Errorf("清理定向接收者失败: %w", err)
	}
	seen := map[int64]bool{}
	for _, uid := range userIDs {
		if uid == 0 || seen[uid] {
			continue
		}
		seen[uid] = true
		if _, err := q.ExecContext(ctx,
			`INSERT INTO announcement_targets (announcement_id, user_id) VALUES (?, ?)`, id, uid); err != nil {
			return fmt.Errorf("写入定向接收者失败: %w", err)
		}
	}
	return nil
}

// GetAnnouncement 取单条公告（含定向接收者）。
func GetAnnouncement(ctx context.Context, q Querier, id string) (Announcement, error) {
	row := q.QueryRowContext(ctx, `SELECT `+announcementColumns+` FROM announcements WHERE id = ?`, id)
	a, err := scanAnnouncement(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Announcement{}, ErrNotFound
	}
	if err != nil {
		return Announcement{}, fmt.Errorf("读取公告失败: %w", err)
	}
	targets, err := listAnnouncementTargets(ctx, q, id)
	if err != nil {
		return Announcement{}, err
	}
	a.Targets = targets
	return a, nil
}

func listAnnouncementTargets(ctx context.Context, q Querier, id string) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT user_id FROM announcement_targets WHERE announcement_id = ? ORDER BY user_id`, id)
	if err != nil {
		return nil, fmt.Errorf("读取定向接收者失败: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("扫描定向接收者失败: %w", err)
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

// GetActiveTicker 取当前启用中的滚动公告。没有则返回 ErrNotFound。
func GetActiveTicker(ctx context.Context, q Querier) (Announcement, error) {
	row := q.QueryRowContext(ctx, `SELECT `+announcementColumns+`
		FROM announcements WHERE kind = 'ticker' AND enabled = 1 LIMIT 1`)
	a, err := scanAnnouncement(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Announcement{}, ErrNotFound
	}
	if err != nil {
		return Announcement{}, fmt.Errorf("读取滚动公告失败: %w", err)
	}
	return a, nil
}

// ListAnnouncementsFor 列出对指定访问者可见的公告。
//
// userID 为 0 表示访客：访客只能看到"全体"范围的公告，因为定向消息的接收者
// 是账号，访客没有账号。
func ListAnnouncementsFor(ctx context.Context, q Querier, userID int64, kinds []AnnouncementKind, now int64) ([]Announcement, error) {
	var (
		where = []string{"enabled = 1", "(expire_at = 0 OR expire_at > ?)"}
		args  = []any{now}
	)
	if len(kinds) > 0 {
		placeholders := make([]string, 0, len(kinds))
		for _, k := range kinds {
			placeholders = append(placeholders, "?")
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(placeholders, ",")+")")
	}
	if userID > 0 {
		where = append(where, `(audience = 'all' OR id IN (
			SELECT announcement_id FROM announcement_targets WHERE user_id = ?))`)
		args = append(args, userID)
	} else {
		where = append(where, `audience = 'all'`)
	}

	rows, err := q.QueryContext(ctx, `SELECT `+announcementColumns+` FROM announcements
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY pinned DESC, created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("查询公告失败: %w", err)
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		a, err := scanAnnouncement(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描公告失败: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAnnouncements 后台分页列出全部公告（含停用）。
func ListAnnouncements(ctx context.Context, q Querier, kind AnnouncementKind, limit, offset int) ([]Announcement, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + announcementColumns + ` FROM announcements`
	args := []any{}
	if kind != "" {
		query += ` WHERE kind = ?`
		args = append(args, string(kind))
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询公告失败: %w", err)
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		a, err := scanAnnouncement(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描公告失败: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAnnouncement 修改公告内容与状态。
func UpdateAnnouncement(ctx context.Context, q Querier, a Announcement) error {
	if a.Kind == KindTicker && a.Enabled {
		if err := DisableActiveTickers(ctx, q, a.ID); err != nil {
			return err
		}
	}
	res, err := q.ExecContext(ctx, `
		UPDATE announcements SET kind = ?, audience = ?, title = ?, body = ?,
			enabled = ?, pinned = ?, expire_at = ?, updated_at = ?
		WHERE id = ?`,
		string(a.Kind), string(a.Audience), a.Title, a.Body,
		boolToInt(a.Enabled), boolToInt(a.Pinned), a.ExpireAt, Now(), a.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: 已存在启用中的滚动公告", ErrConflict)
		}
		return fmt.Errorf("更新公告失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if a.Audience == AudienceUsers {
		if err := SetAnnouncementTargets(ctx, q, a.ID, a.Targets); err != nil {
			return err
		}
	} else if _, err := q.ExecContext(ctx,
		`DELETE FROM announcement_targets WHERE announcement_id = ?`, a.ID); err != nil {
		return fmt.Errorf("清理定向接收者失败: %w", err)
	}
	return nil
}

// DeleteAnnouncement 删除公告（定向接收者与已读记录随之级联删除）。
func DeleteAnnouncement(ctx context.Context, q Querier, id string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM announcements WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除公告失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
