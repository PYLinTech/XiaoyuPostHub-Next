package store

import (
	"context"
	"fmt"
)

// CreateAPIKey 写入一条 API Key 记录。明文令牌不经过这里。
func CreateAPIKey(ctx context.Context, q Querier, key APIKey) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO api_keys (id, user_id, name, key_hash, prefix, expires_at, last_used_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.UserID, key.Name, key.KeyHash, key.Prefix,
		key.ExpiresAt, key.LastUsedAt, key.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: 创建 API Key 失败: %w", err)
	}
	return nil
}

// GetAPIKeyByHash 按令牌哈希查找凭据。
func GetAPIKeyByHash(ctx context.Context, q Querier, keyHash string) (APIKey, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, user_id, name, key_hash, prefix, expires_at, last_used_at, created_at
		FROM api_keys WHERE key_hash = ?`, keyHash)
	key, err := scanAPIKey(row)
	if err != nil {
		if isNoRows(err) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, fmt.Errorf("store: 读取 API Key 失败: %w", err)
	}
	return key, nil
}

// ListAPIKeys 返回某个用户的全部 API Key，最近创建的在前。
func ListAPIKeys(ctx context.Context, q Querier, userID int64) ([]APIKey, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, user_id, name, key_hash, prefix, expires_at, last_used_at, created_at
		FROM api_keys WHERE user_id = ?
		ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 API Key 失败: %w", err)
	}
	defer rows.Close()
	out := make([]APIKey, 0)
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描 API Key 失败: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 API Key 失败: %w", err)
	}
	return out, nil
}

// DeleteAPIKey 删除用户自己的 Key。条件更新未命中表示不存在或不属于该用户。
func DeleteAPIKey(ctx context.Context, q Querier, id string, userID int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("store: 删除 API Key 失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 删除 API Key 后读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: API Key 不存在", ErrNoRowsAffected)
	}
	return nil
}

// TouchAPIKey 记录最近使用时间。失败不影响请求本身。
func TouchAPIKey(ctx context.Context, q Querier, id string, now int64) error {
	_, err := q.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now, id)
	return err
}

type apiKeyScanner interface {
	Scan(dest ...any) error
}

func scanAPIKey(row apiKeyScanner) (APIKey, error) {
	var key APIKey
	if err := row.Scan(&key.ID, &key.UserID, &key.Name, &key.KeyHash, &key.Prefix,
		&key.ExpiresAt, &key.LastUsedAt, &key.CreatedAt); err != nil {
		return APIKey{}, err
	}
	return key, nil
}
