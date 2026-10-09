package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const uploadTaskColumns = `id, user_id, checksum, expected_checksum, size_plain, streaming, volume_size, chunk_size, chunk_total, received_mask,
	target_parent_path, target_name, conflict_action, expires_at, created_at, updated_at`

func scanUploadTask(row rowScanner) (UploadTask, error) {
	var t UploadTask
	var expires int64
	var streaming int
	err := row.Scan(&t.ID, &t.UserID, &t.Checksum, &t.ExpectedChecksum, &t.SizePlain, &streaming, &t.VolumeSize, &t.ChunkSize, &t.ChunkTotal,
		&t.ReceivedMask, &t.TargetParentPath, &t.TargetName, &t.ConflictAction,
		&expires, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return UploadTask{}, err
	}
	t.ExpiresAt = ToTime(expires)
	t.Streaming = streaming != 0
	return t, nil
}

// CreateUploadTask 建立一次上传会话。
//
// 分片尺寸与会话一起固定下来：断点续传必须按同一套边界续算，中途改变分片
// 粒度会让已接收位图的含义失效。
func CreateUploadTask(ctx context.Context, q Querier, t UploadTask) error {
	return createUploadTask(ctx, q, t, 0)
}

// CreateUploadTaskLimited 在创建会话的同一条写语句中检查用户的并发上限。
// 先 Count 再 INSERT 会在并发请求之间留下竞态，导致临时盘会话数超过上限。
func CreateUploadTaskLimited(ctx context.Context, q Querier, t UploadTask, limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("%w: 上传会话上限必须大于 0", ErrQuotaExceeded)
	}
	return createUploadTask(ctx, q, t, limit)
}

func createUploadTask(ctx context.Context, q Querier, t UploadTask, limit int64) error {
	now := Now()
	if t.CreatedAt == 0 {
		t.CreatedAt = now
	}
	query := `
		INSERT INTO upload_tasks (id, user_id, checksum, expected_checksum, size_plain, streaming, volume_size, chunk_size, chunk_total,
			received_mask, target_parent_path, target_name, conflict_action, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{
		t.ID, t.UserID, t.Checksum, t.ExpectedChecksum, t.SizePlain, boolToInt(t.Streaming), t.VolumeSize, t.ChunkSize, t.ChunkTotal,
		t.ReceivedMask, t.TargetParentPath, t.TargetName, t.ConflictAction,
		FromTime(t.ExpiresAt), t.CreatedAt, now,
	}
	if limit > 0 {
		query = `
			INSERT INTO upload_tasks (id, user_id, checksum, expected_checksum, size_plain, streaming, volume_size, chunk_size, chunk_total,
				received_mask, target_parent_path, target_name, conflict_action, expires_at, created_at, updated_at)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			WHERE (SELECT COUNT(*) FROM upload_tasks WHERE user_id = ?) < ?`
		args = append(args, t.UserID, limit)
	}
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 上传会话 %s 已存在", ErrConflict, t.ID)
		}
		return fmt.Errorf("创建上传会话失败: %w", err)
	}
	if limit > 0 {
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取上传会话创建结果失败: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: 进行中的上传会话已达上限 %d", ErrQuotaExceeded, limit)
		}
	}
	return nil
}

// GetUploadTask 取上传会话。
func GetUploadTask(ctx context.Context, q Querier, id string) (UploadTask, error) {
	row := q.QueryRowContext(ctx, `SELECT `+uploadTaskColumns+` FROM upload_tasks WHERE id = ?`, id)
	t, err := scanUploadTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadTask{}, ErrNotFound
	}
	if err != nil {
		return UploadTask{}, fmt.Errorf("读取上传会话失败: %w", err)
	}
	return t, nil
}

// SetUploadExpectedChecksum 绑定客户端在并行收片期间算出的摘要；同一会话只允许绑定一次。
func SetUploadExpectedChecksum(ctx context.Context, q Querier, id, checksum string) error {
	res, err := q.ExecContext(ctx, `UPDATE upload_tasks SET expected_checksum = ?, updated_at = ?
		WHERE id = ? AND (expected_checksum = '' OR expected_checksum = ?)`, checksum, Now(), id, checksum)
	if err != nil {
		return fmt.Errorf("保存上传校验码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: 上传会话校验码已绑定或会话不存在", ErrConflict)
	}
	return nil
}

// MarkChunkReceived 置位某个分片并返回置位后的会话。
//
// 位图整体读写而不是按位更新：SQLite 没有位操作函数，而位图本身很小
// （1 bit/片，8 MiB 分片的 1 TB 文件也只有 128 KB），整块读写的代价可忽略。
func MarkChunkReceived(ctx context.Context, q Querier, id string, index int) (UploadTask, error) {
	// 分片上传通常由前端并发发起。不能把位图做成普通的“读整块→写整块”
	// 更新，否则两个请求同时读取旧位图时，后写者会把先写者的位清掉。这里
	// 用旧位图作为条件做乐观 CAS；冲突时重新读取最新位图并合并自己的位。
	for attempt := 0; attempt < 16; attempt++ {
		task, err := GetUploadTask(ctx, q, id)
		if err != nil {
			return UploadTask{}, err
		}
		if index < 0 || index >= task.ChunkTotal {
			return UploadTask{}, fmt.Errorf("分片序号 %d 越界（共 %d 片）", index, task.ChunkTotal)
		}
		if HasBit(task.ReceivedMask, index) {
			return task, nil
		}
		oldMask := append([]byte(nil), task.ReceivedMask...)
		newMask := SetBit(append([]byte(nil), oldMask...), index)
		res, err := q.ExecContext(ctx,
			`UPDATE upload_tasks SET received_mask = ?, updated_at = ?
			 WHERE id = ? AND hex(received_mask) = hex(?)`,
			newMask, Now(), id, oldMask)
		if err != nil {
			return UploadTask{}, fmt.Errorf("更新分片位图失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return UploadTask{}, fmt.Errorf("读取分片位图更新结果失败: %w", err)
		}
		if n == 0 {
			// 另一个请求刚刚合并了位图。下一轮会重新读取并合并；若会话
			// 已被取消/清理，GetUploadTask 会返回明确的 NotFound。
			continue
		}
		task.ReceivedMask = newMask
		task.UpdatedAt = Now()
		return task, nil
	}
	return UploadTask{}, fmt.Errorf("更新分片位图冲突过多，请稍后重试")
}

// DeleteUploadTask 删除上传会话。
func DeleteUploadTask(ctx context.Context, q Querier, id string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM upload_tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除上传会话失败: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("读取上传会话删除结果失败: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountLiveUploadsByChecksum 统计某校验码上仍然有效的上传会话数。
//
// 用于识别"占位行还在、但上传者早已放弃"的僵局：没有这个判断，一次失败的
// 上传会让该内容永久无法再被上传。
func CountLiveUploadsByChecksum(ctx context.Context, q Querier, checksum string, now int64) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upload_tasks WHERE (checksum = ? OR expected_checksum = ?) AND expires_at > ?`,
		checksum, checksum, now).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计有效上传会话失败: %w", err)
	}
	return n, nil
}

// ListExpiredUploadTasksBatch 读取一批过期会话。删除和配额回收由调用方在
// 同一个写事务中完成，避免“会话已删但预扣额度仍在”的泄漏。
func ListExpiredUploadTasksBatch(ctx context.Context, q Querier, now int64, limit int) ([]UploadTask, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := q.QueryContext(ctx, `SELECT `+uploadTaskColumns+`
		FROM upload_tasks t WHERE expires_at < ?
		AND NOT EXISTS (SELECT 1 FROM upload_jobs j WHERE j.session_id = t.id AND j.state IN ('queued', 'processing'))
		ORDER BY expires_at LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("查询过期上传会话失败: %w", err)
	}
	var expired []UploadTask
	for rows.Next() {
		t, err := scanUploadTask(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("扫描过期上传会话失败: %w", err)
		}
		expired = append(expired, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("关闭过期上传会话查询失败: %w", err)
	}
	return expired, nil
}

// DeleteExpiredUploadTask 删除一张仍然过期的会话，并返回是否真的删除。
// 调用方应在同一事务中回收该会话的存储预扣；条件删除让维护任务可安全地
// 与取消/完成请求并发运行。
func DeleteExpiredUploadTask(ctx context.Context, q Querier, id string, before int64) (UploadTask, bool, error) {
	task, err := GetUploadTask(ctx, q, id)
	if errors.Is(err, ErrNotFound) {
		return UploadTask{}, false, nil
	}
	if err != nil {
		return UploadTask{}, false, err
	}
	if task.ExpiresAt.IsZero() || task.ExpiresAt.Unix() >= before {
		return task, false, nil
	}
	res, err := q.ExecContext(ctx,
		`DELETE FROM upload_tasks WHERE id = ? AND expires_at < ?
			AND NOT EXISTS (
				SELECT 1 FROM upload_jobs WHERE session_id = ? AND state IN ('queued', 'processing')
			)`, id, before, id)
	if err != nil {
		return UploadTask{}, false, fmt.Errorf("删除过期上传会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return UploadTask{}, false, fmt.Errorf("读取过期上传会话删除结果失败: %w", err)
	}
	if n > 0 {
		if _, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = 'error', error = '上传会话已过期', updated_at = ?
			WHERE session_id = ? AND state = 'receiving'`, Now(), id); err != nil {
			return UploadTask{}, false, fmt.Errorf("结束过期的流式上传任务失败: %w", err)
		}
	}
	return task, n > 0, nil
}
