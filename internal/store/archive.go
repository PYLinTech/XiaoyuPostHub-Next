package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 归档持久化。批次 = 一次删除操作（一棵子树）；批次内的每个节点都有
// 快照行，恢复时整树重插，存储删除时按 checksum 释放内容池引用。

const archiveBatchColumns = `id, user_id, user_account, root_name, root_path, node_type, size_total, deleted_at, state, purge_at, purged_at`

func scanArchiveBatch(row rowScanner) (ArchiveBatch, error) {
	var b ArchiveBatch
	var nodeType, state int
	err := row.Scan(&b.ID, &b.UserID, &b.UserAccount, &b.RootName, &b.RootPath,
		&nodeType, &b.SizeTotal, &b.DeletedAt, &state, &b.PurgeAt, &b.PurgedAt)
	if err != nil {
		return ArchiveBatch{}, err
	}
	b.NodeType = NodeType(nodeType)
	b.State = ArchiveState(state)
	return b, nil
}

// EnumerateSubtree 按路径列出子树全部节点（自身 + 后代）。
//
// 排序保证父节点排在子节点之前：父路径是子路径的前缀，同前缀时更短者在前，
// 快照与恢复都依赖这个顺序。
func EnumerateSubtree(ctx context.Context, q Querier, userID int64, path string) ([]Node, error) {
	clause, args := pathSelfOrDescendant("logical_path", path)
	rows, err := q.QueryContext(ctx, `SELECT `+nodeColumns+`
		FROM user_nodes
		WHERE user_id = ? AND `+clause+`
		ORDER BY logical_path`,
		append([]any{userID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("枚举子树失败: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描子树节点失败: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// InsertArchiveBatch 写入一个删除批次。
func InsertArchiveBatch(ctx context.Context, q Querier, b ArchiveBatch) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO archive_batches (id, user_id, user_account, root_name, root_path, node_type, size_total, deleted_at, state, purge_at, purged_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.UserID, b.UserAccount, b.RootName, b.RootPath,
		int(b.NodeType), b.SizeTotal, b.DeletedAt, int(b.State), b.PurgeAt, b.PurgedAt)
	if err != nil {
		return fmt.Errorf("写入归档批次失败: %w", err)
	}
	return nil
}

// InsertArchiveNodes 写入批次内的全部节点快照。
func InsertArchiveNodes(ctx context.Context, q Querier, batchID string, nodes []ArchiveNode) error {
	for _, n := range nodes {
		var checksum any
		if n.FileChecksum != "" {
			checksum = n.FileChecksum
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO archive_nodes (batch_id, original_path, name, node_type, file_checksum, size_plain)
			VALUES (?, ?, ?, ?, ?, ?)`,
			batchID, n.OriginalPath, n.Name, int(n.NodeType), checksum, n.SizePlain); err != nil {
			return fmt.Errorf("写入归档节点快照失败: %w", err)
		}
	}
	return nil
}

// GetArchiveBatch 按 id 取批次。
func GetArchiveBatch(ctx context.Context, q Querier, id string) (ArchiveBatch, error) {
	row := q.QueryRowContext(ctx, `SELECT `+archiveBatchColumns+` FROM archive_batches WHERE id = ?`, id)
	b, err := scanArchiveBatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ArchiveBatch{}, ErrNotFound
	}
	if err != nil {
		return ArchiveBatch{}, fmt.Errorf("读取归档批次失败: %w", err)
	}
	return b, nil
}

// ListArchiveBatchesByUser 分页列出某用户**暂存中**的归档批次。
//
// 固定只取暂存态，不接受调用方指定状态：清除之后条目已不属于用户，
// 它既不可恢复、后续处置也不再由用户决定，留下只会让人以为还能操作。
// 归档删除及之后的批次只在管理端可见（见 ListArchiveBatches）。
func ListArchiveBatchesByUser(ctx context.Context, q Querier, userID int64, limit, offset int) ([]ArchiveBatch, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `SELECT `+archiveBatchColumns+`
		FROM archive_batches WHERE user_id = ? AND state = ? ORDER BY deleted_at DESC LIMIT ? OFFSET ?`,
		userID, int(ArchiveStaged), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("列出归档批次失败: %w", err)
	}
	defer rows.Close()
	out := []ArchiveBatch{}
	for rows.Next() {
		b, err := scanArchiveBatch(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("扫描归档批次失败: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM archive_batches WHERE user_id = ? AND state = ?`,
		userID, int(ArchiveStaged)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计归档批次失败: %w", err)
	}
	return out, total, nil
}

// ListArchiveBatches 分页列出全局归档批次；state 非 0 时按状态过滤。
func ListArchiveBatches(ctx context.Context, q Querier, state ArchiveState, limit, offset int) ([]ArchiveBatch, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := ""
	args := []any{}
	if state != 0 {
		where = " WHERE state = ?"
		args = append(args, int(state))
	}
	rows, err := q.QueryContext(ctx, `SELECT `+archiveBatchColumns+`
		FROM archive_batches`+where+` ORDER BY deleted_at DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("列出归档批次失败: %w", err)
	}
	defer rows.Close()
	out := []ArchiveBatch{}
	for rows.Next() {
		b, err := scanArchiveBatch(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("扫描归档批次失败: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int64
	countArgs := args
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM archive_batches`+where, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计归档批次失败: %w", err)
	}
	return out, total, nil
}

// ListArchiveNodes 列出批次内的全部节点快照（父前子后）。
func ListArchiveNodes(ctx context.Context, q Querier, batchID string) ([]ArchiveNode, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT original_path, name, node_type, file_checksum, size_plain
		FROM archive_nodes WHERE batch_id = ? ORDER BY id`,
		batchID)
	if err != nil {
		return nil, fmt.Errorf("列出归档节点快照失败: %w", err)
	}
	defer rows.Close()
	var out []ArchiveNode
	for rows.Next() {
		var n ArchiveNode
		var checksum sql.NullString
		var nodeType int
		if err := rows.Scan(&n.OriginalPath, &n.Name, &nodeType, &checksum, &n.SizePlain); err != nil {
			return nil, fmt.Errorf("扫描归档节点快照失败: %w", err)
		}
		n.NodeType = NodeType(nodeType)
		if checksum.Valid {
			n.FileChecksum = checksum.String
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountArchiveFileRefs 统计批次内每个文件校验码被引用了几次（文件夹不计）。
func CountArchiveFileRefs(ctx context.Context, q Querier, batchID string) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT file_checksum, COUNT(*) FROM archive_nodes
		WHERE batch_id = ? AND file_checksum IS NOT NULL
		GROUP BY file_checksum`, batchID)
	if err != nil {
		return nil, fmt.Errorf("统计批次文件引用失败: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var checksum string
		var n int64
		if err := rows.Scan(&checksum, &n); err != nil {
			return nil, fmt.Errorf("扫描批次文件引用失败: %w", err)
		}
		out[checksum] = n
	}
	return out, rows.Err()
}

// ListStagedBatchesByUser 列出某用户全部暂存中的批次（清空归档用）。
func ListStagedBatchesByUser(ctx context.Context, q Querier, userID int64, limit int) ([]ArchiveBatch, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := q.QueryContext(ctx, `SELECT `+archiveBatchColumns+`
		FROM archive_batches WHERE user_id = ? AND state = ? ORDER BY deleted_at LIMIT ?`,
		userID, int(ArchiveStaged), limit)
	if err != nil {
		return nil, fmt.Errorf("列出暂存批次失败: %w", err)
	}
	defer rows.Close()
	return scanBatches(rows)
}

// ListExpiredStagedBatches 列出暂存期已满、应推进为"归档删除"的批次。
func ListExpiredStagedBatches(ctx context.Context, q Querier, before int64, limit int) ([]ArchiveBatch, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `SELECT `+archiveBatchColumns+`
		FROM archive_batches WHERE state = ? AND deleted_at < ? ORDER BY deleted_at LIMIT ?`,
		int(ArchiveStaged), before, limit)
	if err != nil {
		return nil, fmt.Errorf("查询到期暂存批次失败: %w", err)
	}
	defer rows.Close()
	return scanBatches(rows)
}

// ListPurgeDueBatches 列出管理留存期已满、应推进为"存储删除"的批次。
func ListPurgeDueBatches(ctx context.Context, q Querier, now int64, limit int) ([]ArchiveBatch, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `SELECT `+archiveBatchColumns+`
		FROM archive_batches WHERE state = ? AND purge_at > 0 AND purge_at <= ? ORDER BY purge_at LIMIT ?`,
		int(ArchiveDeleted), now, limit)
	if err != nil {
		return nil, fmt.Errorf("查询到期待清理批次失败: %w", err)
	}
	defer rows.Close()
	return scanBatches(rows)
}

// UpdateArchiveBatchState 推进批次状态（流转是单向的，调用方负责校验方向）。
func UpdateArchiveBatchState(ctx context.Context, q Querier, id string, state ArchiveState, purgeAt, purgedAt int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE archive_batches SET state = ?, purge_at = ?, purged_at = ? WHERE id = ?`,
		int(state), purgeAt, purgedAt, id)
	if err != nil {
		return fmt.Errorf("更新归档批次状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteArchiveBatch 恢复成功后移除批次与快照（节点行由外键级联删除）。
func DeleteArchiveBatch(ctx context.Context, q Querier, id string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM archive_batches WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除归档批次失败: %w", err)
	}
	return nil
}

func scanBatches(rows *sql.Rows) ([]ArchiveBatch, error) {
	var out []ArchiveBatch
	for rows.Next() {
		b, err := scanArchiveBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描归档批次失败: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
