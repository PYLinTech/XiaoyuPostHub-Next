package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// ListNode 是面向前端的目录项。
type ListNode struct {
	Path          string           `json:"path"`
	Name          string           `json:"name"`
	NodeType      store.NodeType   `json:"nodeType"`
	IsFolder      bool             `json:"isFolder"`
	Size          int64            `json:"size"`
	Mtime         int64            `json:"mtime"`
	Checksum      string           `json:"checksum,omitempty"`
	Status        store.FileStatus `json:"status,omitempty"`
	DisableReason string           `json:"disableReason,omitempty"`
}

// ListDir 列出某目录的直接子项。
func (s *Service) ListDir(ctx context.Context, p auth.Principal, dirPath string) ([]ListNode, error) {
	if err := auth.RequireUser(p); err != nil {
		return nil, err
	}
	path, err := vpath.Normalize(dirPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if path != vpath.Root {
		node, err := store.GetNode(ctx, s.DB.R(), p.UserID(), path)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if !node.IsFolder() {
			return nil, fmt.Errorf("%w: %s 不是目录", ErrBadRequest, path)
		}
	}

	nodes, err := store.ListChildren(ctx, s.DB.R(), p.UserID(), path)
	if err != nil {
		return nil, err
	}
	return s.listNodesWithStatus(ctx, nodes)
}

// listNodesWithStatus 把节点映射成面向前端的目录项，并附上内容池状态。
//
// ListDir 与分享浏览共用这一份映射：同一个目录在"自己的网盘"与"别人的分享"
// 里看到的字段不同，会让前端出现两套渲染分支。
//
// 内容池状态用一条 IN 查询批量取回，而不是逐条 GetFile——目录列表是调用最
// 频繁的读接口，逐条查会让它随目录条目数线性往返。
func (s *Service) listNodesWithStatus(ctx context.Context, nodes []store.Node) ([]ListNode, error) {
	checksums := make([]string, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.FileChecksum != "" && !seen[n.FileChecksum] {
			seen[n.FileChecksum] = true
			checksums = append(checksums, n.FileChecksum)
		}
	}
	found, err := store.GetFileStatuses(ctx, s.DB.R(), checksums)
	if err != nil {
		return nil, err
	}

	out := make([]ListNode, 0, len(nodes))
	for _, n := range nodes {
		item := ListNode{
			Path:     n.LogicalPath,
			Name:     n.Name,
			NodeType: n.NodeType,
			IsFolder: n.IsFolder(),
			Size:     n.SizePlain,
			Mtime:    n.Mtime,
			Checksum: n.FileChecksum,
		}
		if n.FileChecksum != "" {
			// 内容池记录缺失说明数据不一致：标成禁用而不是隐藏条目，
			// 让用户看到"这个文件有问题"而不是"文件凭空消失"。
			if info, ok := found[n.FileChecksum]; ok {
				item.Status = info.Status
				item.DisableReason = info.DisableReason
			} else {
				item.Status = store.FileDisabled
				item.DisableReason = "内容池记录缺失"
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// StatNode 取单个节点的信息。
func (s *Service) StatNode(ctx context.Context, p auth.Principal, path string) (store.Node, error) {
	if err := auth.RequireUser(p); err != nil {
		return store.Node{}, err
	}
	norm, err := vpath.Normalize(path)
	if err != nil {
		return store.Node{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if norm == vpath.Root {
		return store.Node{
			UserID:      p.UserID(),
			LogicalPath: vpath.Root,
			NodeType:    store.NodeFolder,
			Name:        "",
			ParentPath:  "",
		}, nil
	}
	node, err := store.GetNode(ctx, s.DB.R(), p.UserID(), norm)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Node{}, ErrNotFound
		}
		return store.Node{}, err
	}
	return node, nil
}

// MakeDir 新建目录。
func (s *Service) MakeDir(ctx context.Context, p auth.Principal, parentPath, name string) (store.Node, error) {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return store.Node{}, err
	}
	parent, err := vpath.Normalize(parentPath)
	if err != nil {
		return store.Node{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	full, err := vpath.Join(parent, name)
	if err != nil {
		return store.Node{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	node := store.Node{
		UserID:      p.UserID(),
		LogicalPath: full,
		NodeType:    store.NodeFolder,
		Name:        vpath.Base(full),
		ParentPath:  parent,
		Mtime:       s.Now(),
		CreatedAt:   s.Now(),
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		// 父目录检查与插入必须在同一写事务内；否则并发删除父目录会
		// 留下无法访问的孤儿节点。
		if err := s.requireFolderTx(ctx, tx, p.UserID(), parent); err != nil {
			return err
		}
		return store.InsertNode(ctx, tx, node)
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.Node{}, fmt.Errorf("%w: %s 已存在", ErrConflict, full)
		}
		return store.Node{}, err
	}
	return node, nil
}

// RenameNode 重命名节点（保持父目录不变）。
func (s *Service) RenameNode(ctx context.Context, p auth.Principal, path, newName string) (string, error) {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return "", err
	}
	node, err := s.requireOwnNode(ctx, p.UserID(), path)
	if err != nil {
		return "", err
	}
	return s.moveNode(ctx, p, node, node.ParentPath, newName)
}

// MoveNode 把节点移动到另一个目录。
func (s *Service) MoveNode(ctx context.Context, p auth.Principal, path, destParent string) (string, error) {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return "", err
	}
	node, err := s.requireOwnNode(ctx, p.UserID(), path)
	if err != nil {
		return "", err
	}
	dest, err := vpath.Normalize(destParent)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return s.moveNode(ctx, p, node, dest, node.Name)
}

func (s *Service) moveNode(ctx context.Context, p auth.Principal, node store.Node, destParent, newName string) (string, error) {
	if node.LogicalPath == vpath.Root {
		return "", fmt.Errorf("%w: 不能移动根目录", ErrBadRequest)
	}
	// 环检测：目标目录不能是被移动节点自身或其后代，否则子树会被接到自己
	// 下面，路径前缀更新会变成自我引用。
	if vpath.IsAncestor(node.LogicalPath, destParent) {
		return "", fmt.Errorf("%w: 不能把目录移动到它自己或其子目录下", ErrBadRequest)
	}
	name, err := vpath.ValidateName(newName)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	newPath, err := vpath.Join(destParent, name)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if newPath == node.LogicalPath {
		return node.LogicalPath, nil
	}

	// 路径改写与关联分享失效必须在同一写事务内完成。否则第二步失败时，
	// 已移动的内容仍可能被旧分享访问，形成“链接看似有效但目标已变”的状态。
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := s.requireFolderTx(ctx, tx, p.UserID(), destParent); err != nil {
			return err
		}
		// 先按旧快照定位并吊销票据，再改写节点路径；移动后旧分享根
		// 已无法通过路径反查其内容。
		if _, err := store.RevokeTicketsByShareRoot(ctx, tx, p.UserID(), node.LogicalPath); err != nil {
			return err
		}
		if err := store.MoveSubtree(ctx, tx, p.UserID(), node.LogicalPath, newPath, destParent, name); err != nil {
			return err
		}
		// 分享的根路径是快照，移动后不会自动跟随。这里让覆盖旧路径的分享失效，
		// 避免出现“分享指向一个已不存在的路径”这种无法解释的状态；已签发的
		// 交付票据也必须一并吊销，否则它们绕过分享解析仍能继续取数。
		_, err := store.DisableSharesByRootPrefix(ctx, tx, p.UserID(), node.LogicalPath)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return "", fmt.Errorf("%w: %s 已存在", ErrConflict, newPath)
		}
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	return newPath, nil
}

// DeleteNode 删除节点及其全部后代。
//
// 删除是**进入归档**而不是立即释放：单事务内先整树快照进归档批次，
// 再删除节点行。引用计数与配额在暂存期保持占用（内容还能恢复），
// 由清除/清空/留存期到期统一释放（见 service/archive.go）。
// 分享与下载票据仍然立即失效：删除后内容对分享访客不可见。
func (s *Service) DeleteNode(ctx context.Context, p auth.Principal, path string) error {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return err
	}
	norm, err := vpath.Normalize(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if norm == vpath.Root {
		return fmt.Errorf("%w: 不能删除根目录", ErrBadRequest)
	}
	// 节点、快照、引用清单和关联分享必须在同一写事务内完成，避免
	// 两个并发删除先后读取同一份引用清单后把计数减两次。
	err = s.DB.InTx(ctx, func(tx store.Querier) error {
		root, err := s.requireOwnNodeTx(ctx, tx, p.UserID(), norm)
		if err != nil {
			return err
		}
		refs, err := store.SubtreeRefCounts(ctx, tx, p.UserID(), norm)
		if err != nil {
			return err
		}
		nodes, err := store.EnumerateSubtree(ctx, tx, p.UserID(), norm)
		if err != nil {
			return err
		}
		batchID, err := store.NewID("rcy")
		if err != nil {
			return err
		}
		var sizeTotal int64
		snapshot := make([]store.ArchiveNode, 0, len(nodes))
		for _, n := range nodes {
			sizeTotal += n.SizePlain
			snapshot = append(snapshot, store.ArchiveNode{
				OriginalPath: n.LogicalPath,
				Name:         n.Name,
				NodeType:     n.NodeType,
				FileChecksum: n.FileChecksum,
				SizePlain:    n.SizePlain,
			})
		}
		if err := store.InsertArchiveBatch(ctx, tx, store.ArchiveBatch{
			ID:          batchID,
			UserID:      p.UserID(),
			UserAccount: p.User.Account,
			RootName:    root.Name,
			RootPath:    norm,
			NodeType:    root.NodeType,
			SizeTotal:   sizeTotal,
			DeletedAt:   store.Now(),
			State:       store.ArchiveStaged,
		}); err != nil {
			return err
		}
		if err := store.InsertArchiveNodes(ctx, tx, batchID, snapshot); err != nil {
			return err
		}
		if _, err := store.DeleteSubtree(ctx, tx, p.UserID(), norm); err != nil {
			return err
		}
		for checksum := range refs {
			if _, err := store.RevokeTicketsByChecksum(ctx, tx, checksum); err != nil {
				return err
			}
		}
		if _, err := store.DisableSharesByRootPrefix(ctx, tx, p.UserID(), norm); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) requireOwnNodeTx(ctx context.Context, tx store.Querier, userID int64, path string) (store.Node, error) {
	norm, err := vpath.Normalize(path)
	if err != nil {
		return store.Node{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if norm == vpath.Root {
		return store.Node{}, fmt.Errorf("%w: 不能对根目录执行该操作", ErrBadRequest)
	}
	node, err := store.GetNode(ctx, tx, userID, norm)
	if errors.Is(err, store.ErrNotFound) {
		return store.Node{}, ErrNotFound
	}
	return node, err
}

// NodeStats 返回某目录子树的数量与体积统计。
func (s *Service) NodeStats(ctx context.Context, p auth.Principal, path string) (files, folders, bytes int64, err error) {
	if err := auth.RequireUser(p); err != nil {
		return 0, 0, 0, err
	}
	norm, err := vpath.Normalize(path)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return store.CountSubtree(ctx, s.DB.R(), p.UserID(), norm)
}

// requireOwnNode 是 requireOwnNodeTx 的读池版本。
//
// 两个实现在语义上完全相同，差别只在一个显式传 `s.DB.R()`。保留薄封装而不是
// 各写一份：节点归属判定（规范化 → 拒绝根目录 → 取节点 → 把 not-found 映射成
// ErrNotFound）只有一处定义，不会各自漂移。
func (s *Service) requireOwnNode(ctx context.Context, userID int64, path string) (store.Node, error) {
	return s.requireOwnNodeTx(ctx, s.DB.R(), userID, path)
}

// requireFolder 确认目录存在（根目录视为恒存在）。
// requireFolder 是 requireFolderTx 的读池版本，语义完全相同。
func (s *Service) requireFolder(ctx context.Context, userID int64, path string) error {
	return s.requireFolderTx(ctx, s.DB.R(), userID, path)
}

func (s *Service) requireFolderTx(ctx context.Context, q store.Querier, userID int64, path string) error {
	if path == vpath.Root {
		return nil
	}
	node, err := store.GetNode(ctx, q, userID, path)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 目录 %s 不存在", ErrNotFound, path)
		}
		return err
	}
	if !node.IsFolder() {
		return fmt.Errorf("%w: %s 不是目录", ErrBadRequest, path)
	}
	return nil
}
