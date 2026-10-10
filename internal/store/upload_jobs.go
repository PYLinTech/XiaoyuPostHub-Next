package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CreateUploadJob 入队；重复提交收尾请求返回已有任务，不制造重复收尾。
func CreateUploadJob(ctx context.Context, db *DB, job UploadJob, limit int64) (UploadJob, error) {
	now := Now()
	if job.CreatedAt == 0 {
		job.CreatedAt = now
	}
	err := db.InTx(ctx, func(tx Querier) error {
		if existing, err := GetUploadJob(ctx, tx, job.SessionID); err == nil {
			job = existing
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var pending int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM upload_jobs WHERE state IN ('queued', 'processing')`).Scan(&pending); err != nil {
			return fmt.Errorf("统计待处理上传任务失败: %w", err)
		}
		if limit > 0 && pending >= limit {
			return ErrBusy
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO upload_jobs
			(session_id, user_id, client_ip, state, total_bytes, created_at, updated_at) VALUES (?, ?, ?, 'queued', ?, ?, ?)`,
			job.SessionID, job.UserID, job.ClientIP, job.TotalBytes, job.CreatedAt, now)
		return err
	})
	if err != nil {
		return UploadJob{}, fmt.Errorf("创建上传收尾任务失败: %w", err)
	}
	return GetUploadJob(ctx, db.R(), job.SessionID)
}

// CreateReceivingUploadJob 建立大文件的收片状态；准备好一个完整加密卷后才进入公平队列。
func CreateReceivingUploadJob(ctx context.Context, q Querier, job UploadJob) error {
	if job.CreatedAt == 0 {
		job.CreatedAt = Now()
	}
	_, err := q.ExecContext(ctx, `INSERT INTO upload_jobs
		(session_id, user_id, client_ip, state, total_bytes, created_at, updated_at)
		VALUES (?, ?, ?, 'receiving', ?, ?, ?)`, job.SessionID, job.UserID, job.ClientIP, job.TotalBytes, job.CreatedAt, Now())
	if err != nil {
		return fmt.Errorf("建立流式上传任务失败: %w", err)
	}
	return nil
}

// QueueReceivingUploadJob 只把可处理的收片任务放入公平 worker 队列。
func QueueReceivingUploadJob(ctx context.Context, q Querier, sessionID string) error {
	_, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = 'queued', updated_at = ?
		WHERE session_id = ? AND state = 'receiving'`, Now(), sessionID)
	return err
}

// SetUploadJobReceiving 在当前窗口处理完但后续输入尚未到齐时释放 worker。
func SetUploadJobReceiving(ctx context.Context, q Querier, sessionID string) error {
	_, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = 'receiving', updated_at = ?
		WHERE session_id = ? AND state = 'processing'`, Now(), sessionID)
	return err
}

type UploadStreamState struct {
	NextPlainOffset int64
	SHA256State     []byte
}

func CreateUploadStreamState(ctx context.Context, q Querier, sessionID string, hashState []byte) error {
	_, err := q.ExecContext(ctx, `INSERT INTO upload_stream_state (session_id, next_plain_offset, sha256_state)
		VALUES (?, 0, ?)`, sessionID, hashState)
	return err
}

func GetUploadStreamState(ctx context.Context, q Querier, sessionID string) (UploadStreamState, error) {
	var state UploadStreamState
	err := q.QueryRowContext(ctx, `SELECT next_plain_offset, sha256_state FROM upload_stream_state WHERE session_id = ?`, sessionID).
		Scan(&state.NextPlainOffset, &state.SHA256State)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadStreamState{}, ErrNotFound
	}
	return state, err
}

func AdvanceUploadStream(ctx context.Context, q Querier, sessionID string, expectedOffset, nextOffset int64, hashState []byte, part UploadPart) error {
	if nextOffset <= expectedOffset || part.PlainOffset != expectedOffset || part.PlainSize != nextOffset-expectedOffset {
		return fmt.Errorf("上传流水线游标无效")
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO upload_job_parts
		(session_id, part_no, object_ref, object_name, plain_offset, plain_size, wire_offset, wire_size, cipher_md5, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, part.SessionID, part.PartNo, part.ObjectRef, part.ObjectName,
		part.PlainOffset, part.PlainSize, part.WireOffset, part.WireSize, part.CipherMD5, Now()); err != nil {
		return fmt.Errorf("记录已上传物理卷失败: %w", err)
	}
	res, err := q.ExecContext(ctx, `UPDATE upload_stream_state SET next_plain_offset = ?, sha256_state = ?
		WHERE session_id = ? AND next_plain_offset = ?`, nextOffset, hashState, sessionID, expectedOffset)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("上传流水线游标已被其他 worker 推进")
	}
	return nil
}

func ListUploadParts(ctx context.Context, q Querier, sessionID string) ([]UploadPart, error) {
	rows, err := q.QueryContext(ctx, `SELECT session_id, part_no, object_ref, object_name, plain_offset, plain_size,
		wire_offset, wire_size, cipher_md5 FROM upload_job_parts WHERE session_id = ? ORDER BY part_no`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []UploadPart
	for rows.Next() {
		var p UploadPart
		if err := rows.Scan(&p.SessionID, &p.PartNo, &p.ObjectRef, &p.ObjectName, &p.PlainOffset, &p.PlainSize,
			&p.WireOffset, &p.WireSize, &p.CipherMD5); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

// GetUploadJob 查询持久化收尾状态。
func GetUploadJob(ctx context.Context, q Querier, sessionID string) (UploadJob, error) {
	var job UploadJob
	err := q.QueryRowContext(ctx, `SELECT session_id, user_id, client_ip, state, error, result_json, total_bytes, progress_bytes, created_at, updated_at
		FROM upload_jobs WHERE session_id = ?`, sessionID).Scan(
		&job.SessionID, &job.UserID, &job.ClientIP, &job.State, &job.Error, &job.ResultJSON, &job.TotalBytes, &job.ProgressBytes, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadJob{}, ErrNotFound
	}
	if err != nil {
		return UploadJob{}, fmt.Errorf("读取上传收尾任务失败: %w", err)
	}
	return job, nil
}

// ListActiveUploadJobsForUser 返回本人仍未结束的上传任务。
func ListActiveUploadJobsForUser(ctx context.Context, q Querier, userID int64) ([]UploadJob, error) {
	rows, err := q.QueryContext(ctx, `SELECT j.session_id, j.user_id, j.client_ip, j.state, j.error,
		j.result_json, j.total_bytes, j.progress_bytes, j.created_at, j.updated_at, t.target_name
		FROM upload_jobs j JOIN upload_tasks t ON t.id = j.session_id
		WHERE j.user_id = ? AND j.state IN ('receiving', 'queued', 'processing')
		ORDER BY j.created_at, j.session_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("读取用户活动上传任务失败: %w", err)
	}
	defer rows.Close()

	jobs := make([]UploadJob, 0)
	for rows.Next() {
		var job UploadJob
		if err := rows.Scan(&job.SessionID, &job.UserID, &job.ClientIP, &job.State, &job.Error,
			&job.ResultJSON, &job.TotalBytes, &job.ProgressBytes, &job.CreatedAt, &job.UpdatedAt, &job.TargetName); err != nil {
			return nil, fmt.Errorf("读取用户活动上传任务失败: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户活动上传任务失败: %w", err)
	}
	return jobs, nil
}

func SetUploadJobProgress(ctx context.Context, q Querier, sessionID string, plainBytes int64) error {
	_, err := q.ExecContext(ctx, `UPDATE upload_jobs SET progress_bytes = MAX(progress_bytes, ?), updated_at = ?
		WHERE session_id = ? AND state IN ('queued', 'processing', 'receiving')`, plainBytes, Now(), sessionID)
	return err
}

// ClaimNextUploadJob 按用户组资源调度优先级领取任务；同优先级按更新时间轮转。
// 同一用户已有收尾任务运行时跳过其余任务，避免单个用户占满全站 worker。
func (db *DB) ClaimNextUploadJob(ctx context.Context) (UploadJob, bool, error) {
	var claimed UploadJob
	found := false
	err := db.InTx(ctx, func(tx Querier) error {
		var sessionID string
		err := tx.QueryRowContext(ctx, `SELECT j.session_id
			FROM upload_jobs j
			JOIN users u ON u.id = j.user_id
			JOIN user_groups g ON g.name = u.group_name
			WHERE j.state = 'queued'
			  AND NOT EXISTS (
				SELECT 1 FROM upload_jobs active
				WHERE active.user_id = j.user_id AND active.state = 'processing'
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM upload_jobs earlier
				WHERE earlier.user_id = j.user_id AND earlier.state = 'queued'
				  AND (earlier.created_at < j.created_at OR
					(earlier.created_at = j.created_at AND earlier.session_id < j.session_id))
			  )
			  AND (
				NOT EXISTS (
					SELECT 1 FROM upload_jobs active
					JOIN upload_tasks active_task ON active_task.id = active.session_id
					WHERE active.state = 'processing' AND active_task.streaming = 1
				) OR NOT EXISTS (
					SELECT 1 FROM upload_tasks candidate_task
					WHERE candidate_task.id = j.session_id AND candidate_task.streaming = 1
				)
			  )
			ORDER BY g.resource_scheduling_priority DESC, j.updated_at, j.created_at, j.session_id LIMIT 1`).Scan(&sessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("选择上传收尾任务失败: %w", err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE upload_jobs SET state = 'processing', updated_at = ?
			WHERE session_id = ? AND state = 'queued'`, Now(), sessionID)
		if err != nil {
			return fmt.Errorf("领取上传收尾任务失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		claimed, err = GetUploadJob(ctx, tx, sessionID)
		found = err == nil
		return err
	})
	return claimed, found, err
}

// FinishUploadJob 保存收尾终态。结果为空表示失败或取消。
func FinishUploadJob(ctx context.Context, q Querier, sessionID, state, message, result string) error {
	if state != "done" && state != "error" {
		return fmt.Errorf("非法上传任务终态 %q", state)
	}
	res, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = ?, error = ?, result_json = ?, updated_at = ?
		WHERE session_id = ? AND state IN ('queued', 'processing')`, state, message, result, Now(), sessionID)
	if err != nil {
		return fmt.Errorf("更新上传收尾任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CancelReceivingUploadJob 只在任务仍收片时取消，不能越过 worker 领取竞态。
func CancelReceivingUploadJob(ctx context.Context, q Querier, sessionID string) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = 'error', error = '已取消', updated_at = ?
		WHERE session_id = ? AND state = 'receiving'`, Now(), sessionID)
	if err != nil {
		return false, fmt.Errorf("取消收片中的上传任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CancelQueuedUploadJob 仅取消尚未被 worker 领取的任务，原子地阻止领取竞态。
func CancelQueuedUploadJob(ctx context.Context, q Querier, sessionID string) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = 'error', error = '已取消', updated_at = ?
		WHERE session_id = ? AND state = 'queued'`, Now(), sessionID)
	if err != nil {
		return false, fmt.Errorf("取消排队中的上传任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteOldUploadJobs 清理已结束超过保留期的状态记录。
func DeleteOldUploadJobs(ctx context.Context, q Querier, before int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	res, err := q.ExecContext(ctx, `DELETE FROM upload_jobs WHERE session_id IN (
		SELECT session_id FROM upload_jobs WHERE state IN ('done', 'error') AND updated_at < ?
		ORDER BY updated_at LIMIT ?
	)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("清理过期上传任务记录失败: %w", err)
	}
	return res.RowsAffected()
}

// CompleteUploadJobInTx 完成文件节点登记时同步写入结果，避免进程在两步之间退出后
// 出现“文件已完成但队列仍处理中”的状态。
func CompleteUploadJobInTx(ctx context.Context, q Querier, sessionID, result string) error {
	_, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = 'done', error = '', result_json = ?, updated_at = ?
		WHERE session_id = ? AND state = 'processing'`, result, Now(), sessionID)
	if err != nil {
		return fmt.Errorf("提交上传收尾结果失败: %w", err)
	}
	return nil
}

// CompleteDedupUploadJob 持久化收片阶段的秒传结果，供重试和断点恢复复用。
// 调用者必须在同一事务内确认任务尚未被 worker 领取。
func CompleteDedupUploadJob(ctx context.Context, q Querier, job UploadJob, result string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO upload_jobs
		(session_id, user_id, state, total_bytes, result_json, created_at, updated_at)
		VALUES (?, ?, 'done', ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET state = 'done', error = '', result_json = excluded.result_json,
		updated_at = excluded.updated_at`, job.SessionID, job.UserID, job.TotalBytes, result, Now(), Now())
	return err
}

// ResetInterruptedUploadJobs 进程启动时重新排入上次中断的任务。
func ResetInterruptedUploadJobs(ctx context.Context, q Querier) error {
	if _, err := q.ExecContext(ctx, `UPDATE upload_jobs SET state = 'queued', updated_at = ?
		WHERE state = 'processing'`, Now()); err != nil {
		return fmt.Errorf("恢复中断的上传收尾任务失败: %w", err)
	}
	return nil
}
