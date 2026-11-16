package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// apiWorkspace 是 API Key 的路径视角。
//
// 对会话身份 root 为空，路径就是账号内的绝对路径；对绑定了工作区的 Key，
// 调用方看到的 "/" 是工作区目录本身，其余路径都相对于它。绑定根目录的 Key
// 与旧 Key 一样按账号绝对路径工作。这样同一套 SDK 代码既能在会话里用绝对
// 路径，也能在 Key 里用工作区相对路径，而越界在服务端统一收口，不依赖
// 客户端自觉。
type apiWorkspace struct {
	root string
}

func workspaceOf(p auth.Principal) apiWorkspace {
	root := p.APIKeyWorkspace
	if root == "" || root == vpath.Root {
		return apiWorkspace{}
	}
	return apiWorkspace{root: root}
}

func (w apiWorkspace) scoped() bool { return w.root != "" }

// resolve 把调用方路径解析成账号内的绝对路径。
func (w apiWorkspace) resolve(callerPath string) (string, error) {
	norm, err := vpath.Normalize(callerPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if !w.scoped() {
		return norm, nil
	}
	if norm == vpath.Root {
		return w.root, nil
	}
	abs, err := vpath.Normalize(w.root + norm)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if !vpath.IsAncestor(w.root, abs) {
		return "", fmt.Errorf("%w: 路径超出 Key 的工作区", ErrForbidden)
	}
	return abs, nil
}

// allows 判断一个账号内的绝对路径是否在工作区内（含工作区根）。
func (w apiWorkspace) allows(abs string) bool {
	if !w.scoped() {
		return true
	}
	return vpath.IsAncestor(w.root, abs)
}

// relative 把绝对路径还原成调用方视角。
func (w apiWorkspace) relative(abs string) string {
	if !w.scoped() {
		return abs
	}
	if abs == w.root {
		return vpath.Root
	}
	if vpath.IsWithin(w.root, abs) {
		return abs[len(w.root):]
	}
	return abs
}

func (w apiWorkspace) isRoot(abs string) bool {
	return w.scoped() && abs == w.root
}

func (w apiWorkspace) rejectRoot(action string) error {
	return fmt.Errorf("%w: 不能%s工作区根目录", ErrBadRequest, action)
}

// requireFolderFor 校验目录存在，并把错误里的路径换成调用方视角：
// 4xx 的 detail 会返回给客户端，工作区 Key 不应从错误文案里读到账号的
// 绝对目录结构。
func (s *Service) requireFolderFor(ctx context.Context, q store.Querier, p auth.Principal, path string) error {
	if err := s.requireFolderTx(ctx, q, p.UserID(), path); err != nil {
		shown := workspaceOf(p).relative(path)
		switch {
		case errors.Is(err, ErrNotFound):
			return fmt.Errorf("%w: 目录 %s 不存在", ErrNotFound, shown)
		case errors.Is(err, ErrBadRequest):
			return fmt.Errorf("%w: %s 不是目录", ErrBadRequest, shown)
		}
		return err
	}
	return nil
}

// mapNode 把节点里的绝对路径改成调用方视角。
func (w apiWorkspace) mapNode(node store.Node) store.Node {
	if !w.scoped() {
		return node
	}
	node.LogicalPath = w.relative(node.LogicalPath)
	if node.LogicalPath == vpath.Root {
		node.ParentPath = ""
	} else {
		node.ParentPath = w.relative(node.ParentPath)
	}
	return node
}

func (w apiWorkspace) mapListNode(item ListNode) ListNode {
	item.Path = w.relative(item.Path)
	return item
}

func (w apiWorkspace) mapShares(items []store.Share) {
	for i := range items {
		items[i].RootPath = w.relative(items[i].RootPath)
	}
}

func (w apiWorkspace) mapShareAccesses(items []store.ShareAccessRecord) {
	if !w.scoped() {
		return
	}
	for i := range items {
		if items[i].Target != "" {
			items[i].Target = w.relative(items[i].Target)
		}
	}
}

func (w apiWorkspace) mapDirectLink(link store.DirectLink) store.DirectLink {
	link.SourcePath = w.relative(link.SourcePath)
	return link
}

func (w apiWorkspace) mapDirectLinks(items []store.DirectLink) {
	for i := range items {
		items[i].SourcePath = w.relative(items[i].SourcePath)
	}
}

func (w apiWorkspace) mapArchiveBatch(batch store.ArchiveBatch) store.ArchiveBatch {
	batch.RootPath = w.relative(batch.RootPath)
	return batch
}
