package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 直链按内容哈希绑定，不持有内容池引用。用户删除文件时只停用自己的直链。

const directLinkColumns = `id, owner_id, file_checksum, source_path, source_name, mime_type,
	size_plain, expires_at, max_hits, hits, disabled, created_at, updated_at`

// 查询统一附带内容可用性，列表、详情与按内容反查共用同一套扫描。
const directLinkSelect = `SELECT ` + directLinkColumns + `,
 EXISTS (SELECT 1 FROM files WHERE checksum = direct_links.file_checksum AND status = 1)
 FROM direct_links`

func scanDirectLink(row rowScanner) (DirectLink, error) {
	var d DirectLink
	var disabled, available int
	err := row.Scan(&d.ID, &d.OwnerID, &d.FileChecksum, &d.SourcePath, &d.SourceName, &d.MimeType,
		&d.SizePlain, &d.ExpiresAt, &d.MaxHits, &d.Hits, &disabled, &d.CreatedAt, &d.UpdatedAt, &available)
	if err != nil {
		return DirectLink{}, err
	}
	d.Disabled = disabled != 0
	d.TargetAvailable = available != 0
	return d, nil
}

// CreateDirectLink 新建直链。
//
// id 是随机不可枚举的 token：可枚举的短 id 等于把"遍历全部直链"变成一次
// 简单的循环。直链是纯凭据入口，泄露一条就是泄露一份文件。
func CreateDirectLink(ctx context.Context, q Querier, d DirectLink) error {
	if d.ID == "" {
		id, err := GenerateToken(16)
		if err != nil {
			return err
		}
		d.ID = id
	}
	// updated_at 由调用方给初值而不是这里盖时间戳：返回的是调用方手里那个
	// 结构体，它必须与落库版本一致，否则紧接着的一次乐观锁更新会拿着
	// 一个库里根本不存在的版本号自撞冲突。与 CreateShare 同理。
	_, err := q.ExecContext(ctx, `
		INSERT INTO direct_links (id, owner_id, file_checksum, source_path, source_name, mime_type,
			size_plain, expires_at, max_hits, hits, disabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		d.ID, d.OwnerID, d.FileChecksum, d.SourcePath, d.SourceName, d.MimeType,
		d.SizePlain, d.ExpiresAt, d.MaxHits, Now(), d.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 直链标识冲突", ErrConflict)
		}
		return fmt.Errorf("创建直链失败: %w", err)
	}
	return nil
}

// GetDirectLink 按标识取直链。
func GetDirectLink(ctx context.Context, q Querier, id string) (DirectLink, error) {
	row := q.QueryRowContext(ctx, directLinkSelect+` WHERE id = ?`, id)
	d, err := scanDirectLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectLink{}, ErrNotFound
	}
	if err != nil {
		return DirectLink{}, fmt.Errorf("读取直链失败: %w", err)
	}
	return d, nil
}

// ListDirectLinksByOwner 列出某用户创建的直链。
//
// 一并带上绑定内容的可用状态：内容被删除或拉黑后直链记录仍然存在（可查可删），
// 但已经交付不出字节。列表页必须把两者区分开，否则用户会反复复制一条
// 必然失败的链接。
func ListDirectLinksByOwner(ctx context.Context, q Querier, ownerID int64, limit, offset int) ([]DirectLink, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, directLinkSelect+`
  WHERE owner_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`, ownerID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("列出直链失败: %w", err)
	}
	defer rows.Close()
	return collectDirectLinks(rows)
}

// ListDirectLinksByOwnerAndChecksums 按内容哈希反查某用户的直链，服务
// 「文件 → 直链」的双向绑定：文件详情里要能列出这份内容上的全部直链。
func ListDirectLinksByOwnerAndChecksums(ctx context.Context, q Querier, ownerID int64, checksums []string) ([]DirectLink, error) {
	if len(checksums) == 0 {
		return nil, nil
	}
	clause, args := inPlaceholders(checksums)
	rows, err := q.QueryContext(ctx, directLinkSelect+`
		WHERE owner_id = ? AND file_checksum IN (`+clause+`) ORDER BY created_at DESC`,
		append([]any{ownerID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("按内容反查直链失败: %w", err)
	}
	defer rows.Close()
	return collectDirectLinks(rows)
}

// UpdateDirectLinkIfUnchanged 按乐观锁更新直链。
//
// 谓词 updated_at = expectUpdatedAt 保证写入基于调用方读到的同一版本：
// 并发期间已有别人提交，本次陈旧写回命中 0 行。0 行需区分"直链不存在"
// 与"版本冲突"——再做一次存在性检查映射成对应错误。
func UpdateDirectLinkIfUnchanged(ctx context.Context, q Querier, d DirectLink, expectUpdatedAt int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE direct_links SET expires_at = ?, max_hits = ?, disabled = ?, updated_at = updated_at + 1
		WHERE id = ? AND updated_at = ?`,
		d.ExpiresAt, d.MaxHits, boolToInt(d.Disabled), d.ID, expectUpdatedAt)
	if err != nil {
		return fmt.Errorf("更新直链失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		return nil
	}
	if _, getErr := GetDirectLink(ctx, q, d.ID); getErr != nil {
		return getErr
	}
	return fmt.Errorf("%w: 直链已被其他人修改，请刷新后重试", ErrConflict)
}

// DeleteDirectLink 删除直链。
//
// 不碰 files：直链不持有内容池引用，删掉它不该改变任何对象的存亡。
func DeleteDirectLink(ctx context.Context, q Querier, id string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM direct_links WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除直链失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ConsumeDirectLinkHit 记一次命中。
//
// 有效性、过期、次数上限、**绑定内容是否仍可用**与计数递增放在同一条 UPDATE：
// 分开判会并发超发。命中 0 行即表示拒绝。
//
// 绑定内容可用性也写进谓词，是为了让"命中"严格等于一次成功交付的取件动作。
// 内容被删除或拉黑时交付会以 403 收场，若这里仍然计数，用户会看着自己的
// 次数上限被一批必然失败的请求白白吃掉。
func ConsumeDirectLinkHit(ctx context.Context, q Querier, id string, now int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE direct_links SET hits = hits + 1
		WHERE id = ? AND disabled = 0
		  AND (expires_at = 0 OR expires_at > ?)
		  AND (max_hits = 0 OR hits < max_hits)
		  AND EXISTS (
			SELECT 1 FROM files
			 WHERE files.checksum = direct_links.file_checksum
			   AND files.status = ?
		  )
		  AND EXISTS (
			SELECT 1 FROM users
			 WHERE users.id = direct_links.owner_id
			   AND users.status = 1
		  )`, id, now, int(FileNormal))
	if err != nil {
		return fmt.Errorf("记录直链命中失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: 直链已失效、已过期、已达次数上限或绑定内容不可用", ErrNoRowsAffected)
	}
	return nil
}

// DisableDirectLinksByOwnerAndChecksums 停用某用户在指定内容上的全部直链。
//
// 用户删除文件时调用。它表达的是**用户的删除意图**，与对象是否还在无关：
// 内容池是跨用户去重共享的，即使别的用户或同用户的另一处路径仍持有这份内容，
// 这条直链也不该继续交付。
//
// owner_id 过滤不是可选的。直链绑的是内容哈希而不是路径，没有 DisableSharesByRootPrefix
// 那样的天然前缀隔离——不带 owner 就会让用户 A 删除自己的文件时，顺手停用
// 用户 B 指向同一份去重内容的直链。
func DisableDirectLinksByOwnerAndChecksums(ctx context.Context, q Querier, ownerID int64, checksums []string) (int64, error) {
	if len(checksums) == 0 {
		return 0, nil
	}
	clause, args := inPlaceholders(checksums)
	res, err := q.ExecContext(ctx, `
		UPDATE direct_links SET disabled = 1, updated_at = updated_at + 1
		WHERE owner_id = ? AND disabled = 0 AND file_checksum IN (`+clause+`)`,
		append([]any{ownerID}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("失效直链失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// collectDirectLinks 收拢查询结果。
func collectDirectLinks(rows *sql.Rows) ([]DirectLink, error) {
	var out []DirectLink
	for rows.Next() {
		d, err := scanDirectLink(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描直链失败: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// inPlaceholders 按值生成 n 个占位符与对应实参，供 IN 子句拼装。
func inPlaceholders(values []string) (string, []any) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return marks, args
}

// ReleaseDirectLinkHit 撤回未能开流的请求；链接已被删除时无需处理。
func ReleaseDirectLinkHit(ctx context.Context, q Querier, id string) error {
	_, err := q.ExecContext(ctx, `UPDATE direct_links SET hits = MAX(0, hits - 1) WHERE id = ?`, id)
	return err
}
