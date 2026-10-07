package store

import (
	"context"
	"fmt"
	"strings"
)

// 管理端的全站文件检索。
//
// 与用户侧列目录的根本差别：这里跨 user_id。没有它，管理员在站点里
// 找不到一份"某个用户存了什么"的文件——原先文件管理页只有对象状态计数
// 和按校验码查单个对象，等于只能盲查。
//
// 检索打在 user_nodes 而不是 files：内容池对象没有文件名，也没有属主，
// 用户眼里的"文件"是节点。同一个对象可能被几十个人各引用一次，按对象
// 检索会漏掉"谁存了它"，而按节点检索才是管理员真正要问的问题。

// AdminNodeFilter 是全站节点检索条件。零值表示该维度不过滤。
type AdminNodeFilter struct {
	// Query 匹配文件名或完整逻辑路径。
	Query string
	// Owner 是属主账号子串。
	Owner string
	// NodeType 为 nil 时不限文件与文件夹。
	NodeType *NodeType
	// FileStatus 为 nil 时不限内容池状态；仅对文件节点有意义。
	FileStatus *FileStatus
	// UserID 精确限定单个用户，0 表示不限。
	UserID int64
	// MTimeFrom/MTimeTo 按修改时间过滤，0 表示不限。
	MTimeFrom int64
	MTimeTo   int64
}

// AdminNodeItem 是全站节点列表的一行。
type AdminNodeItem struct {
	UserID    int64  `json:"userId"`
	Account   string `json:"account"`
	GroupName string `json:"groupName"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	NodeType  string `json:"nodeType"`
	SizePlain int64  `json:"sizePlain"`
	MTime     int64  `json:"mtime"`
	// Checksum 仅文件节点有值。
	Checksum string `json:"checksum"`
	// FileStatus 是内容池状态。文件夹没有对应对象，用 -1 表示"不适用"。
	FileStatus    int    `json:"fileStatus"`
	DisableReason string `json:"disableReason"`
	// RefCount 是该对象被全站引用的次数——删掉一个节点不会立刻回收对象，
	// 管理员需要这个数判断"这条引用是不是最后一条"。
	RefCount int64 `json:"refCount"`
}

// CountAllNodes 是 AdminNodeFilter 的命中总数。
func (f AdminNodeFilter) where() ([]string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if f.UserID > 0 {
		where = append(where, "n.user_id = ?")
		args = append(args, f.UserID)
	}
	if owner := strings.TrimSpace(f.Owner); owner != "" {
		where = append(where, "u.account LIKE ? ESCAPE '\\'")
		args = append(args, likeContains(owner))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// 文件名与完整路径都匹配：管理员手上往往只有路径片段
		// （"2026/报告"），只搜 name 会漏掉整个目录。
		like := likeContains(q)
		where = append(where, "(n.name LIKE ? ESCAPE '\\' OR n.logical_path LIKE ? ESCAPE '\\')")
		args = append(args, like, like)
	}
	if f.NodeType != nil {
		where = append(where, "n.node_type = ?")
		args = append(args, int(*f.NodeType))
	}
	// 状态过滤走 LEFT JOIN 后的 COALESCE：文件夹在内容池里没有对应行，
	// 直接对 f.status 过滤会把所有文件夹一起筛掉。
	if f.FileStatus != nil {
		where = append(where, "COALESCE(f.status, -1) = ?")
		args = append(args, int(*f.FileStatus))
	}
	if f.MTimeFrom > 0 {
		where = append(where, "n.mtime >= ?")
		args = append(args, f.MTimeFrom)
	}
	if f.MTimeTo > 0 {
		where = append(where, "n.mtime <= ?")
		args = append(args, f.MTimeTo)
	}
	return where, args
}

// ListAllNodes 分页检索全站节点。
//
// 关键词走的是 LIKE '%x%'，用不上索引——这是刻意的：文件名的检索必须
// 支持中间片段（"报告" 命中 "2026年第三季度报告.pdf"），前缀索引帮不上。
// 与全局邮件检索保持同一口径，代价是数据量上来后要靠 limit/offset 兜住。
// 真到了扛不住的那天，该上的是 FTS5 而不是改成前缀匹配。
func ListAllNodes(ctx context.Context, q Querier, f AdminNodeFilter,
	limit, offset int) ([]AdminNodeItem, int64, error) {
	where, args := f.where()
	clause := strings.Join(where, " AND ")
	const join = `FROM user_nodes n
		JOIN users u ON u.id = n.user_id
		LEFT JOIN files f ON f.checksum = n.file_checksum`

	var total int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) `+join+` WHERE `+clause, args...).
		Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计全站文件失败: %w", err)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	// 排序固定为「最近修改优先 + 路径」：同一次检索翻页时行序必须稳定，
	// 只按 mtime 排会在同一秒内产生抖动，表现为某几行在翻页时重复出现。
	rows, err := q.QueryContext(ctx, `
		SELECT n.user_id, u.account, u.group_name, n.logical_path, n.name, n.node_type,
		       n.size_plain, n.mtime,
		       COALESCE(n.file_checksum, ''), COALESCE(f.status, -1),
		       COALESCE(f.disable_reason, ''), COALESCE(f.ref_count, 0)
		`+join+` WHERE `+clause+`
		ORDER BY n.mtime DESC, n.logical_path
		LIMIT ? OFFSET ?`, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询全站文件失败: %w", err)
	}
	defer rows.Close()

	out := make([]AdminNodeItem, 0, limit)
	for rows.Next() {
		var it AdminNodeItem
		var nodeType int
		if err := rows.Scan(&it.UserID, &it.Account, &it.GroupName, &it.Path, &it.Name,
			&nodeType, &it.SizePlain, &it.MTime, &it.Checksum, &it.FileStatus,
			&it.DisableReason, &it.RefCount); err != nil {
			return nil, 0, fmt.Errorf("读取全站文件失败: %w", err)
		}
		if NodeType(nodeType) == NodeFolder {
			it.NodeType = "folder"
		} else {
			it.NodeType = "file"
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历全站文件失败: %w", err)
	}
	return out, total, nil
}

// AdminNodeStats 是文件管理页顶部统计条的两种口径。
//
// 分成两半是因为它们数的是**不同的东西**：节点是"用户目录里的一个条目"，
// 对象是"内容池里的一份内容"。同一份内容被 3 个人各存一次，是 1 个对象、
// 3 个节点。早期这两个数字混在一排里，管理员对着表格数行数永远对不上。
type AdminNodeStats struct {
	// NodeTotal / FileNodes / FolderNodes 数的是 user_nodes。
	NodeTotal   int64 `json:"nodeTotal"`
	FileNodes   int64 `json:"fileNodes"`
	FolderNodes int64 `json:"folderNodes"`
	// FileObjectTotal / MailObjectTotal 是 files 表按入库来源拆开的两份口径。
	//
	// 不提供合计字段是刻意的：管理员真正要回答的问题是"文件占多少、邮件占
	// 多少"，给一个总数反而会让人去和下面的节点表对账（对不上，因为两者
	// 本来就不是同一批东西）。两个值不重叠且覆盖全表，需要总量时自行相加。
	FileObjectTotal int64 `json:"fileObjectTotal"`
	MailObjectTotal int64 `json:"mailObjectTotal"`
	// ObjectsByStatus 是内容池对象的状态分布。
	ObjectsByStatus map[string]int64 `json:"objectsByStatus"`
}

// CountAdminNodesByType 统计全站节点按类型的分布。
func CountAdminNodesByType(ctx context.Context, q Querier) (fileNodes, folderNodes int64, err error) {
	rows, err := q.QueryContext(ctx, `SELECT node_type, COUNT(*) FROM user_nodes GROUP BY node_type`)
	if err != nil {
		return 0, 0, fmt.Errorf("统计全站节点类型失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeType int
		var n int64
		if err := rows.Scan(&nodeType, &n); err != nil {
			return 0, 0, fmt.Errorf("扫描节点类型统计失败: %w", err)
		}
		if NodeType(nodeType) == NodeFolder {
			folderNodes = n
		} else {
			fileNodes = n
		}
	}
	return fileNodes, folderNodes, rows.Err()
}

// GetAdminNodeStats 取文件管理页的全局统计。不接受任何筛选参数：
// 统计条回答的是"站点里有什么"，一旦跟着筛选条件变，就没人能拿它当基准。
func GetAdminNodeStats(ctx context.Context, q Querier) (AdminNodeStats, error) {
	fileNodes, folderNodes, err := CountAdminNodesByType(ctx, q)
	if err != nil {
		return AdminNodeStats{}, err
	}
	fileObjects, mailObjects, err := CountFilesByOrigin(ctx, q)
	if err != nil {
		return AdminNodeStats{}, err
	}
	byStatus, err := CountFilesByStatus(ctx, q)
	if err != nil {
		return AdminNodeStats{}, err
	}
	stats := AdminNodeStats{
		NodeTotal:        fileNodes + folderNodes,
		FileNodes:        fileNodes,
		FolderNodes:      folderNodes,
		FileObjectTotal:  fileObjects,
		MailObjectTotal:  mailObjects,
		ObjectsByStatus: make(map[string]int64, len(byStatus)),
	}
	for st, n := range byStatus {
		stats.ObjectsByStatus[st.String()] = n
	}
	return stats, nil
}

// CountFilesByOrigin 按入库来源统计内容池对象，两个返回值不重叠且覆盖全表。
//
// created_by>0 是用户主动上传：走上传会话，有明确的用户主体。
// created_by=0 是系统自动入库：邮件正文与附件走 IngestPlaintext 时显式传 0，
// 将来邮件草稿同样走这条原语（它没有上传会话，也没有用户主体），会自动
// 落进同一口径，不需要为此加字段或改判断。
func CountFilesByOrigin(ctx context.Context, q Querier) (fileObjects, mailObjects int64, err error) {
	// COALESCE 兜住空表：SUM 在无行时返回 NULL，直接 Scan 进 int64 会报
	// "converting NULL to int64 is unsupported"。
	err = q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN created_by > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN created_by = 0 THEN 1 ELSE 0 END), 0)
		FROM files`).
		Scan(&fileObjects, &mailObjects)
	if err != nil {
		return 0, 0, fmt.Errorf("统计对象入库来源失败: %w", err)
	}
	return fileObjects, mailObjects, nil
}
