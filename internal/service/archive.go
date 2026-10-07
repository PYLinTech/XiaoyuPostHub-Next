package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// 归档。
//
// 删除的完整生命周期被拆成三段：
//
//	用户删除 → 归档暂存（快照子树、删除节点行；引用与配额仍占用，可恢复）
//	清除/清空/满用户留存期 → 归档删除（释放引用与配额；远端对象仍在）
//	满管理留存期 / 管理员强制 → 存储删除（真删远端对象；记录保留）
//
// 可见性按状态划界：**用户只看得到暂存中的批次**。清除之后条目即离开用户侧
// （列表查不到、也无法恢复），因为此时它已不可操作，后续处置由留存期与管理员
// 决定——把它继续摆在用户面前，只会让人反复尝试那些注定无效的操作。
// 管理员只有"向存储删除推进"的能力，没有恢复能力；恢复只属于删除者本人。

// ListUserArchive 列出当前用户暂存中的归档批次。
func (s *Service) ListUserArchive(ctx context.Context, p auth.Principal, limit, offset int) ([]store.ArchiveBatch, int64, error) {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return nil, 0, err
	}
	return store.ListArchiveBatchesByUser(ctx, s.DB.R(), p.UserID(), limit, offset)
}

// RestoreArchiveBatch 把一个暂存中的批次整树恢复回原路径。
//
// 根路径被占用时按「名称 (2)」规则自动改名，后代随根整体平移；
// 父目录在这期间也被删掉时，缺失的祖先链会按原结构补建。
// 暂存期引用与配额都没有释放，恢复因此是零成本的纯节点重建。
func (s *Service) RestoreArchiveBatch(ctx context.Context, p auth.Principal, batchID string) error {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return err
	}
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		batch, err := store.GetArchiveBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if batch.UserID != p.UserID() {
			return ErrForbidden
		}
		if batch.State != store.ArchiveStaged {
			return fmt.Errorf("%w: 只有暂存中的条目可以恢复", ErrBadRequest)
		}
		nodes, err := store.ListArchiveNodes(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			return fmt.Errorf("%w: 批次缺少节点快照", ErrUnavailable)
		}
		if err := s.rebuildArchivedNodes(ctx, tx, batch, nodes); err != nil {
			return err
		}
		return store.DeleteArchiveBatch(ctx, tx, batchID)
	}); err != nil {
		return err
	}
	s.audit(ctx, p, "archive.restore", batchID, "")
	return nil
}

// rebuildArchivedNodes 按快照重建子树：父先子后，根路径占用时自动改名。
func (s *Service) rebuildArchivedNodes(ctx context.Context, tx store.Querier, batch store.ArchiveBatch, nodes []store.ArchiveNode) error {
	root := nodes[0]
	rootParent := vpath.Parent(root.OriginalPath)
	// 祖先链可能同样被删掉了：按原结构补建缺失的文件夹。
	if err := s.ensureAncestorsTx(ctx, tx, batch.UserID, rootParent); err != nil {
		return err
	}
	newRootName, err := store.FreeName(ctx, tx, batch.UserID, rootParent, root.Name)
	if err != nil {
		return err
	}
	newRoot, err := vpath.Join(rootParent, newRootName)
	if err != nil {
		return err
	}
	now := store.Now()
	for _, n := range nodes {
		newPath := newRoot
		if n.OriginalPath != root.OriginalPath {
			newPath = newRoot + strings.TrimPrefix(n.OriginalPath, root.OriginalPath)
		}
		if err := store.InsertNode(ctx, tx, store.Node{
			UserID:       batch.UserID,
			LogicalPath:  newPath,
			NodeType:     n.NodeType,
			FileChecksum: n.FileChecksum,
			Name:         vpath.Base(newPath),
			ParentPath:   vpath.Parent(newPath),
			SizePlain:    n.SizePlain,
			Mtime:        now,
			CreatedAt:    now,
		}); err != nil {
			return fmt.Errorf("恢复节点 %s 失败: %w", n.OriginalPath, err)
		}
	}
	return nil
}

// ensureAncestorsTx 补建路径上缺失的祖先文件夹（不含路径自身）。
func (s *Service) ensureAncestorsTx(ctx context.Context, tx store.Querier, userID int64, dirPath string) error {
	norm, err := vpath.Normalize(dirPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if norm == vpath.Root {
		return nil
	}
	// 自上而下补建：/a/b/c 缺失时先建 /a 再建 /a/b。
	var missing []string
	for p := norm; p != vpath.Root; p = vpath.Parent(p) {
		exists, err := store.NodeExists(ctx, tx, userID, p)
		if err != nil {
			return err
		}
		if exists {
			break
		}
		missing = append(missing, p)
	}
	now := store.Now()
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := store.InsertNode(ctx, tx, store.Node{
			UserID:      userID,
			LogicalPath: p,
			NodeType:    store.NodeFolder,
			Name:        vpath.Base(p),
			ParentPath:  vpath.Parent(p),
			Mtime:       now,
			CreatedAt:   now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ClearArchiveBatch 清除单个批次：暂存 → 归档删除（不真删）。
func (s *Service) ClearArchiveBatch(ctx context.Context, p auth.Principal, batchID string) error {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return err
	}
	rt := s.Settings.Runtime(ctx)
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		batch, err := store.GetArchiveBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if batch.UserID != p.UserID() {
			return ErrForbidden
		}
		return s.transitionBatchToArchiveDeleted(ctx, tx, batch, rt.Archive.AdminRetention)
	}); err != nil {
		return err
	}
	s.audit(ctx, p, "archive.clear", batchID, "")
	return nil
}

// ClearAllArchive 清空当前用户的全部暂存批次。
func (s *Service) ClearAllArchive(ctx context.Context, p auth.Principal) (int, error) {
	if err := auth.RequirePermission(p, perm.ManageOwnNodes); err != nil {
		return 0, err
	}
	rt := s.Settings.Runtime(ctx)
	cleared := 0
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		batches, err := store.ListStagedBatchesByUser(ctx, tx, p.UserID(), maintenanceBatch)
		if err != nil {
			return err
		}
		for _, batch := range batches {
			if err := s.transitionBatchToArchiveDeleted(ctx, tx, batch, rt.Archive.AdminRetention); err != nil {
				return err
			}
			cleared++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if cleared > 0 {
		s.audit(ctx, p, "archive.clear_all", "", fmt.Sprintf("batches=%d", cleared))
	}
	return cleared, nil
}

// transitionBatchToArchiveDeleted 把暂存批次推进为归档删除：
// 释放内容池引用（归零转待回收）、按删除时间 + 管理留存期排定存储清理、
// 归还配额。必须在使用写锁的事务内调用。
func (s *Service) transitionBatchToArchiveDeleted(ctx context.Context, tx store.Querier, batch store.ArchiveBatch, adminRetention time.Duration) error {
	if batch.State != store.ArchiveStaged {
		return fmt.Errorf("%w: 批次 %s 不处于暂存状态", ErrBadRequest, batch.ID)
	}
	refs, err := store.CountArchiveFileRefs(ctx, tx, batch.ID)
	if err != nil {
		return err
	}
	purgeAt := batch.DeletedAt + int64(adminRetention.Seconds())
	for checksum, times := range refs {
		if file, err := store.GetFile(ctx, tx, checksum); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return err
		} else if file.Status == store.FilePurged {
			// 内容此前已被真实删除，本批次只剩记录层面的收尾。
			continue
		}
		if _, _, err := store.ReleaseFileRefs(ctx, tx, checksum, times); err != nil {
			return err
		}
		if err := store.SetFileArchivePurgeAt(ctx, tx, checksum, purgeAt); err != nil {
			return err
		}
	}
	if batch.SizeTotal > 0 {
		if _, err := store.ReleaseCounter(ctx, tx, store.ScopeStorage,
			store.UserCounterKey(batch.UserID, ""), batch.SizeTotal); err != nil {
			return err
		}
	}
	return store.UpdateArchiveBatchState(ctx, tx, batch.ID, store.ArchiveDeleted, purgeAt, 0)
}

// AdminListArchive 列出全局归档批次。
func (s *Service) AdminListArchive(ctx context.Context, p auth.Principal, state store.ArchiveState, limit, offset int) ([]store.ArchiveBatch, int64, error) {
	if err := auth.RequirePermission(p, perm.AdminFiles); err != nil {
		return nil, 0, err
	}
	return store.ListArchiveBatches(ctx, s.DB.R(), state, limit, offset)
}

// AdminPurgeArchiveBatches 把选中的批次推进为存储删除。
//
// 管理员没有恢复能力，只有单向的"向存储删除推进"。真实删除远端对象的前提
// 是该内容已无任何引用：去重共享的内容即使本批次被清理，只要别人还在用，
// 存储侧对象就必须保留，批次只做记录层面的关闭。
func (s *Service) AdminPurgeArchiveBatches(ctx context.Context, p auth.Principal, ids []string) (int, error) {
	if err := auth.RequirePermission(p, perm.AdminFiles); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: 未选择要删除的条目", ErrBadRequest)
	}
	now := store.Now()
	for _, id := range ids {
		if err := s.DB.InTx(ctx, func(tx store.Querier) error {
			batch, err := store.GetArchiveBatch(ctx, tx, id)
			if err != nil {
				return err
			}
			switch batch.State {
			case store.ArchiveStaged:
				// 暂存中的内容还占用引用与配额：先完成"归档删除"的释放，
				// 再把存储清理时间提前到现在。
				if err := s.transitionBatchToArchiveDeleted(ctx, tx, batch, 0); err != nil {
					return err
				}
				return store.UpdateArchiveBatchState(ctx, tx, batch.ID, store.ArchiveDeleted, now, 0)
			case store.ArchiveDeleted:
				return store.UpdateArchiveBatchState(ctx, tx, batch.ID, store.ArchiveDeleted, now, 0)
			default:
				return nil
			}
		}); err != nil {
			return 0, err
		}
	}
	due, err := store.ListPurgeDueBatches(ctx, s.DB.R(), now, len(ids))
	if err != nil {
		return 0, err
	}
	_, _, failures := s.purgeArchiveStorage(ctx, due)
	if failures > 0 {
		return 0, fmt.Errorf("%w: 部分对象删除失败，已保留记录待重试", ErrUnavailable)
	}
	s.audit(ctx, p, "archive.purge", "", fmt.Sprintf("batches=%d", len(ids)))
	return len(ids), nil
}

// purgeArchiveStorage 对到期的批次执行真实存储删除。
//
// 同一份内容可能同时挂在多个批次：用 done 表去重，避免对同一远端对象
// 重复发起删除。ref_count 仍大于 0 或已被拉黑的内容跳过存储删除，
// 批次记录仍然关闭——存储侧对象的生命周期以引用计数为准。
func (s *Service) purgeArchiveStorage(ctx context.Context, batches []store.ArchiveBatch) (batchesDone, objectsPurged, failures int) {
	if s.Backend == nil {
		// 没有存储后端时无事可做（例如本地开发）。
		return 0, 0, failures
	}
	purged := map[string]bool{}
	for _, batch := range batches {
		nodes, err := store.ListArchiveNodes(ctx, s.DB.R(), batch.ID)
		if err != nil {
			failures++
			continue
		}
		for _, node := range nodes {
			if node.NodeType != store.NodeFile || node.FileChecksum == "" || purged[node.FileChecksum] {
				continue
			}
			purged[node.FileChecksum] = true
			file, err := store.GetFile(ctx, s.DB.R(), node.FileChecksum)
			if err != nil {
				failures++
				continue
			}
			// 仍被引用或已被拉黑的内容不能删：前者有人还在用，后者等待人工裁决。
			if file.Status != store.FileArchive || file.RefCount != 0 {
				continue
			}
			if file.PanFileID != "" {
				if err := s.Backend.Delete(ctx, file.PanFileID); err != nil {
					failures++
					delete(purged, node.FileChecksum)
					continue
				}
			}
			if err := store.MarkFilePurged(ctx, s.DB.W(), file.Checksum); err != nil {
				failures++
				continue
			}
			objectsPurged++
		}
		if err := store.UpdateArchiveBatchState(ctx, s.DB.W(), batch.ID,
			store.ArchiveStorageDeleted, batch.PurgeAt, store.Now()); err != nil {
			failures++
			continue
		}
		batchesDone++
	}
	return batchesDone, objectsPurged, failures
}
