package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const uploadStagingReservationTimeout = 10 * time.Minute

// ReserveUploadStagingChunk 在写入分片前原子预占真实磁盘字节。
func ReserveUploadStagingChunk(ctx context.Context, db *DB, sessionID string, index int, size, limit int64) (bool, error) {
	if size <= 0 || limit <= 0 {
		return false, ErrStagingFull
	}
	reserved := false
	err := db.InTx(ctx, func(tx Querier) error {
		var existingState string
		var updatedAt int64
		err := tx.QueryRowContext(ctx, `SELECT state, updated_at FROM upload_staging_chunks WHERE session_id = ? AND chunk_index = ?`, sessionID, index).Scan(&existingState, &updatedAt)
		if err == nil {
			if existingState == "staged" {
				return nil
			}
			if time.Duration(Now()-updatedAt)*time.Second < uploadStagingReservationTimeout {
				return ErrBusy
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM upload_staging_chunks WHERE session_id = ? AND chunk_index = ? AND state = 'reserved'`, sessionID, index); err != nil {
				return err
			}
			err = sql.ErrNoRows
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT SUM(size_bytes) FROM upload_staging_chunks), 0) +
			COALESCE((SELECT SUM(bytes) FROM upload_staging_reservations), 0)`).Scan(&used); err != nil {
			return err
		}
		if size > limit-used {
			return ErrStagingFull
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO upload_staging_chunks (session_id, chunk_index, size_bytes, state, updated_at)
			VALUES (?, ?, ?, 'reserved', ?)`, sessionID, index, size, Now())
		if err != nil {
			return fmt.Errorf("预占上传暂存空间失败: %w", err)
		}
		reserved = true
		return nil
	})
	return reserved, err
}

// TouchUploadStagingChunk 保活仍在接收的分片预占，避免慢速网络上传被误判为失联。
func TouchUploadStagingChunk(ctx context.Context, q Querier, sessionID string, index int) error {
	_, err := q.ExecContext(ctx, `UPDATE upload_staging_chunks SET updated_at = ?
		WHERE session_id = ? AND chunk_index = ? AND state = 'reserved'`, Now(), sessionID, index)
	return err
}

// MarkUploadStagingChunkReady 标记原子落盘完成。
func MarkUploadStagingChunkReady(ctx context.Context, q Querier, sessionID string, index int) error {
	_, err := q.ExecContext(ctx, `UPDATE upload_staging_chunks SET state = 'staged', updated_at = ?
		WHERE session_id = ? AND chunk_index = ? AND state = 'reserved'`, Now(), sessionID, index)
	return err
}

// ReleaseUploadStagingChunk 在文件已删除或已被持久化收尾任务接管后归还字节额度。
func ReleaseUploadStagingChunk(ctx context.Context, q Querier, sessionID string, index int) error {
	_, err := q.ExecContext(ctx, `DELETE FROM upload_staging_chunks WHERE session_id = ? AND chunk_index = ?`, sessionID, index)
	return err
}

// ReserveUploadStaging 在事务内预留非分片临时文件容量。
func ReserveUploadStaging(ctx context.Context, q Querier, sessionID, kind string, size, limit int64) error {
	if size <= 0 || limit <= 0 {
		return ErrStagingFull
	}
	var used int64
	if err := q.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(size_bytes) FROM upload_staging_chunks), 0) +
		COALESCE((SELECT SUM(bytes) FROM upload_staging_reservations), 0)`).Scan(&used); err != nil {
		return err
	}
	if size > limit-used {
		return ErrStagingFull
	}
	_, err := q.ExecContext(ctx, `INSERT INTO upload_staging_reservations (session_id, kind, bytes) VALUES (?, ?, ?)`, sessionID, kind, size)
	return err
}

func UploadStagingUsage(ctx context.Context, q Querier) (int64, error) {
	var used int64
	err := q.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(size_bytes) FROM upload_staging_chunks), 0) +
		COALESCE((SELECT SUM(bytes) FROM upload_staging_reservations), 0)`).Scan(&used)
	return used, err
}
