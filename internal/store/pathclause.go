package store

import (
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// 路径子树谓词。
//
// 这里刻意**不用 LIKE**。SQLite 的 LIKE 对 ASCII 默认大小写不敏感
// （PRAGMA case_sensitive_like 未开启），而逻辑路径的比较是区分大小写的：
// 主键与 `=` 走 BINARY 排序规则。两者混用会造出静默的数据丢失——
// `/Photos` 与 `/photos` 是两棵互不相干的树（`=` 判它们不等），但
// `'/Photos/x' LIKE '/photos/%'` 返回 1，于是删除 `/photos` 会把
// `/Photos` 整棵树一起删掉，移动 `/photos` 会把两棵树合并。
//
// 改用 substr 前缀等值：`=` 是二进制比较，与路径语义一致；顺带也不需要
// 通配符转义。长度与截取都在 SQLite 内部完成（按字符计），与 vpath 的
// Unicode NFC 归一化结果兼容。

// pathSelfOrDescendant 返回"该行等于 path 本身，或位于 path 之下"的 SQL 条件。
func pathSelfOrDescendant(col, path string) (string, []any) {
	if path == vpath.Root {
		// 根之下即全部：所有逻辑路径都以 '/' 开头。
		return "substr(" + col + ", 1, 1) = '/'", nil
	}
	return "(" + col + " = ? OR substr(" + col + ", 1, length(?) + 1) = ? || '/')",
		[]any{path, path, path}
}

// pathDescendant 返回"该行位于 path 之下（不含 path 本身）"的 SQL 条件。
func pathDescendant(col, path string) (string, []any) {
	if path == vpath.Root {
		return "substr(" + col + ", 1, 1) = '/'", nil
	}
	return "substr(" + col + ", 1, length(?) + 1) = ? || '/'", []any{path, path}
}
