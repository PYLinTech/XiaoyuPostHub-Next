package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

const nodeColumns = `user_id, logical_path, node_type, file_checksum, name, parent_path,
	size_plain, mtime, node_status, created_at`

func scanNode(row rowScanner) (Node, error) {
	var n Node
	var nodeType int
	var checksum sql.NullString
	err := row.Scan(&n.UserID, &n.LogicalPath, &nodeType, &checksum, &n.Name, &n.ParentPath,
		&n.SizePlain, &n.Mtime, &n.NodeStatus, &n.CreatedAt)
	if err != nil {
		return Node{}, err
	}
	n.NodeType = NodeType(nodeType)
	if checksum.Valid {
		n.FileChecksum = checksum.String
	}
	return n, nil
}

// GetNode 取指定用户的某个路径节点。
func GetNode(ctx context.Context, q Querier, userID int64, path string) (Node, error) {
	row := q.QueryRowContext(ctx, `SELECT `+nodeColumns+`
		FROM user_nodes WHERE user_id = ? AND logical_path = ?`, userID, path)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("读取节点失败: %w", err)
	}
	return n, nil
}

// NodeExists 判断路径是否已被占用。
func NodeExists(ctx context.Context, q Querier, userID int64, path string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM user_nodes WHERE user_id = ? AND logical_path = ?`, userID, path).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("判定路径占用失败: %w", err)
	}
	return true, nil
}

// ListChildren 列出某目录的直接子项。
func ListChildren(ctx context.Context, q Querier, userID int64, parentPath string) ([]Node, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+nodeColumns+`
		FROM user_nodes WHERE user_id = ? AND parent_path = ?
		ORDER BY node_type DESC, name`, userID, parentPath)
	if err != nil {
		return nil, fmt.Errorf("列出目录失败: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描目录项失败: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// InsertNode 插入节点。路径重复时返回 ErrConflict，由调用方决定重命名还是报错。
func InsertNode(ctx context.Context, q Querier, n Node) error {
	// 文件夹不指向内容池，必须写 NULL 而不是空串：CHECK 约束要求二者互斥。
	var checksum any
	if n.NodeType != NodeFolder {
		checksum = n.FileChecksum
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO user_nodes (user_id, logical_path, node_type, file_checksum, name, parent_path,
			size_plain, mtime, node_status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.UserID, n.LogicalPath, int(n.NodeType), checksum, n.Name, n.ParentPath,
		n.SizePlain, n.Mtime, n.NodeStatus, n.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 路径 %s 已存在", ErrConflict, n.LogicalPath)
		}
		return fmt.Errorf("插入节点失败: %w", err)
	}
	return nil
}

// FindNodeNameByChecksum 返回引用某内容池对象的任意一个节点名。
//
// 同一内容可能被多个用户以不同文件名引用，这里只保证"名字与内容同源"，
// 用于按扩展名派生 Content-Type（流式交付的响应头），不用于展示归属。
// 找不到引用节点时返回 ErrNotFound。
func FindNodeNameByChecksum(ctx context.Context, q Querier, checksum string) (string, error) {
	var name string
	err := q.QueryRowContext(ctx,
		`SELECT name FROM user_nodes WHERE file_checksum = ? LIMIT 1`, checksum).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("按校验码查找节点名失败: %w", err)
	}
	return name, nil
}

// SubtreeRefCounts 统计某个路径及其后代各自引用了哪些内容池对象、各引用几次。
//
// 删除目录前必须先取这份清单：内容池是按引用计数的，漏减会让物理对象永远
// 无法回收；重复减会让计数变负从而误回收仍在使用的对象。
func SubtreeRefCounts(ctx context.Context, q Querier, userID int64, path string) (map[string]int64, error) {
	clause, args := pathSelfOrDescendant("logical_path", path)
	rows, err := q.QueryContext(ctx, `
		SELECT file_checksum, COUNT(*) FROM user_nodes
		WHERE user_id = ? AND node_type = ? AND `+clause+`
		GROUP BY file_checksum`,
		append([]any{userID, int(NodeFile)}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("统计子树引用失败: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var checksum sql.NullString
		var n int64
		if err := rows.Scan(&checksum, &n); err != nil {
			return nil, fmt.Errorf("扫描子树引用失败: %w", err)
		}
		if checksum.Valid && checksum.String != "" {
			out[checksum.String] += n
		}
	}
	return out, rows.Err()
}

// DeleteSubtree 删除某路径及其全部后代，返回删除的行数。
func DeleteSubtree(ctx context.Context, q Querier, userID int64, path string) (int64, error) {
	clause, args := pathSelfOrDescendant("logical_path", path)
	res, err := q.ExecContext(ctx, `
		DELETE FROM user_nodes
		WHERE user_id = ? AND `+clause,
		append([]any{userID}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("删除子树失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// MoveSubtree 移动或重命名一个节点及其全部后代。
//
// 路径是主键，移动子树必然要批量改写主键。一次带前缀条件的 UPDATE 即可：
// substr(..., length(old) + 1) 对节点自身得到空串、对后代得到剩余后缀，
// 同一表达式同时覆盖两种情况。根节点的 parent_path 单独设为新父目录，
// 后代则从旧节点路径截取相对后缀；直接复用节点路径会把根节点当成父目录。
//
// length()/substr() 按字符计（不是字节），与 Go 侧 NFC 规范化后的字符数一致，
// 不能自己用 len() 算字节长度。
//
// 匹配范围用 substr 前缀等值而不是 LIKE：LIKE 对 ASCII 大小写不敏感，
// 会把 /Photos 一起卷进 /photos 的移动（见 pathclause.go 的说明）。
func MoveSubtree(ctx context.Context, q Querier, userID int64, oldPath, newPath, newParent, newName string) error {
	clause, args := pathSelfOrDescendant("logical_path", oldPath)
	moveArgs := append([]any{
		newPath, oldPath,
		oldPath, newParent, newPath, oldPath,
		oldPath, newName,
		oldPath, Now(),
		userID,
	}, args...)
	res, err := q.ExecContext(ctx, `
		UPDATE user_nodes SET
			logical_path = ? || substr(logical_path, length(?) + 1),
			parent_path  = CASE WHEN logical_path = ? THEN ? ELSE ? || substr(parent_path, length(?) + 1) END,
			name  = CASE WHEN logical_path = ? THEN ? ELSE name END,
			mtime = CASE WHEN logical_path = ? THEN ? ELSE mtime END
		WHERE user_id = ? AND `+clause, moveArgs...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 目标路径 %s 已存在", ErrConflict, newPath)
		}
		return fmt.Errorf("移动子树失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountSubtree 统计 path 之下（**不含 path 自身**）的文件数、文件夹数与明文总字节数。
//
// 不含自身是刻意的：这个统计服务于"这个目录里都有些什么"的展示，把一个空目录
// 报成"1 个文件夹"会让人以为库里多了一个东西。根目录没有对应的节点行，
// 因此两种口径下根目录的结果相同。
func CountSubtree(ctx context.Context, q Querier, userID int64, path string) (files, folders int64, bytes int64, err error) {
	clause, args := pathDescendant("logical_path", path)
	row := q.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN node_type = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN node_type = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN node_type = ? THEN size_plain ELSE 0 END), 0)
		FROM user_nodes
		WHERE user_id = ? AND `+clause,
		append([]any{int(NodeFile), int(NodeFolder), int(NodeFile), userID}, args...)...)
	if err := row.Scan(&files, &folders, &bytes); err != nil {
		return 0, 0, 0, fmt.Errorf("统计子树失败: %w", err)
	}
	return files, folders, bytes, nil
}

// UserStorageUsage 返回某用户当前占用的明文与密文字节数。
func UserStorageUsage(ctx context.Context, q Querier, userID int64) (plain, wire int64, err error) {
	row := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(n.size_plain), 0), COALESCE(SUM(f.pan_size_wire), 0)
		FROM user_nodes n
		JOIN files f ON f.checksum = n.file_checksum
		WHERE n.user_id = ? AND n.node_type = ?`, userID, int(NodeFile))
	if err := row.Scan(&plain, &wire); err != nil {
		return 0, 0, fmt.Errorf("统计用户存储占用失败: %w", err)
	}
	return plain, wire, nil
}

// FreeName 在指定目录下找一个未被占用的名称。
//
// 同名冲突时按 "名称 (2)"、"名称 (3)" 递增；达到尝试上限后退化为追加随机后缀，
// 保证不会因为目录里塞满了同名文件而失败。
func FreeName(ctx context.Context, q Querier, userID int64, parentPath, name string) (string, error) {
	path, err := vpath.Join(parentPath, name)
	if err != nil {
		return "", err
	}
	used, err := NodeExists(ctx, q, userID, path)
	if err != nil {
		return "", err
	}
	if !used {
		return name, nil
	}

	base := name
	ext := ""
	// 保留扩展名，把序号插在主名之后，符合"文件名 (2).ext"的一般预期。
	if idx := strings.LastIndex(name, "."); idx > 0 {
		base, ext = name[:idx], name[idx:]
	}
	for i := 2; i <= 200; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		p, err := vpath.Join(parentPath, candidate)
		if err != nil {
			return "", err
		}
		exists, err := NodeExists(ctx, q, userID, p)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	suffix, err := GenerateToken(6)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s%s", base, suffix, ext), nil
}
