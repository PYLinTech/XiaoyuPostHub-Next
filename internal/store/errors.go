package store

import (
	"database/sql"
	"errors"
)

var (
	ErrNotFound = errors.New("store: 记录不存在")
	ErrConflict = errors.New("store: 记录已存在")
	// ErrQuotaExceeded 表示配额不足（预扣被条件更新拒绝）。
	ErrQuotaExceeded = errors.New("store: 配额不足")
	ErrBusy          = errors.New("store: 处理队列已满")
	ErrStagingFull   = errors.New("store: 上传暂存空间已满")
	// ErrNoRowsAffected 表示条件更新未命中任何行，通常意味着并发竞争或状态不符。
	ErrNoRowsAffected = errors.New("store: 条件更新未命中")
)

// isNoRows 判断是否为"无结果"错误。集中一处，避免各仓储重复判断。
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
