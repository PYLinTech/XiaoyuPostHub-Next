package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// ActiveTicker 返回当前生效的顶部滚动公告；没有则返回 nil, nil。
//
// 不要求登录：滚动公告要在未登录的首页也能显示。过期判定必须在这里补上——
// store 层只看 enabled，只靠它会漏掉"已过期但没被停用"的公告。
func (s *Service) ActiveTicker(ctx context.Context) (*store.Announcement, error) {
	a, err := store.GetActiveTicker(ctx, s.DB.R())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if a.ExpireAt != 0 && a.ExpireAt <= s.Now() {
		return nil, nil
	}
	return &a, nil
}

// ListVisibleAnnouncements 列出对当前身份可见的公告。
//
// 可见性完全交给 store 层按 user_id 判定：访客没有账号，只能看到"全体"
// 范围的公告，定向消息天然不会泄露给匿名访问者。
func (s *Service) ListVisibleAnnouncements(ctx context.Context, p auth.Principal, kinds []store.AnnouncementKind) ([]store.Announcement, error) {
	// 逐个过滤未知类型：store 把"空 kinds"解释为"全部类型"，若把非法值透传
	// 下去并过滤成空列表，反而会退化成"返回所有公告"。
	filtered := make([]store.AnnouncementKind, 0, len(kinds))
	for _, k := range kinds {
		if validAnnouncementKind(k) {
			filtered = append(filtered, k)
		}
	}
	if len(kinds) > 0 && len(filtered) == 0 {
		return []store.Announcement{}, nil
	}
	return store.ListAnnouncementsFor(ctx, s.DB.R(), p.UserID(), filtered, s.Now())
}

// AdminListAnnouncements 后台分页列出公告（含停用与已过期）。
func (s *Service) AdminListAnnouncements(ctx context.Context, p auth.Principal, kind store.AnnouncementKind, limit, offset int) ([]store.Announcement, error) {
	if err := auth.RequirePermission(p, perm.AdminAnnouncements); err != nil {
		return nil, err
	}
	if kind != "" && !validAnnouncementKind(kind) {
		return nil, fmt.Errorf("%w: 未知的公告类型 %q", ErrBadRequest, kind)
	}
	if offset < 0 {
		offset = 0
	}
	return store.ListAnnouncements(ctx, s.DB.R(), kind, limit, offset)
}

// SaveAnnouncement 新建或修改一条公告。ID 为空表示新建。
//
// 顶部滚动公告的"有且仅有一条"由数据库的部分唯一索引保证，停用既有滚动公告
// 的逻辑封装在 store.CreateAnnouncement / store.UpdateAnnouncement 里，这里
// 不重复实现——两处各写一遍必然会漂移。
func (s *Service) SaveAnnouncement(ctx context.Context, p auth.Principal, a store.Announcement) (store.Announcement, error) {
	if err := auth.RequirePermission(p, perm.AdminAnnouncements); err != nil {
		return store.Announcement{}, err
	}
	if !validAnnouncementKind(a.Kind) {
		return store.Announcement{}, fmt.Errorf("%w: 未知的公告类型 %q", ErrBadRequest, a.Kind)
	}
	if a.Audience != store.AudienceAll && a.Audience != store.AudienceUsers {
		return store.Announcement{}, fmt.Errorf("%w: 未知的接收范围 %q", ErrBadRequest, a.Audience)
	}
	a.Title = strings.TrimSpace(a.Title)
	if a.Title == "" {
		return store.Announcement{}, fmt.Errorf("%w: 标题不得为空", ErrBadRequest)
	}
	if a.ExpireAt < 0 {
		return store.Announcement{}, fmt.Errorf("%w: 过期时间不得为负", ErrBadRequest)
	}

	created := a.ID == ""
	if created {
		id, err := store.NewID("ann")
		if err != nil {
			return store.Announcement{}, err
		}
		a.ID = id
		a.CreatedBy = p.UserID()
		a.CreatedAt = s.Now()
		a.UpdatedAt = a.CreatedAt
		if err := store.CreateAnnouncement(ctx, s.DB.W(), a); err != nil {
			return store.Announcement{}, mapAnnouncementErr(err)
		}
	} else {
		// 先取原值：CreateBy / CreatedAt 不属于可变字段，更新时用请求里的
		// 零值覆盖会抹掉"谁在什么时候发的"这条线索。
		existing, err := store.GetAnnouncement(ctx, s.DB.R(), a.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return store.Announcement{}, ErrNotFound
			}
			return store.Announcement{}, err
		}
		a.CreatedBy = existing.CreatedBy
		a.CreatedAt = existing.CreatedAt
		if err := store.UpdateAnnouncement(ctx, s.DB.W(), a); err != nil {
			return store.Announcement{}, mapAnnouncementErr(err)
		}
	}

	action := "announcement.update"
	if created {
		action = "announcement.create"
	}
	s.audit(ctx, p, action, a.ID, fmt.Sprintf("%s enabled=%v", a.Kind, a.Enabled))

	// 回读以拿到权威值（含库内写入的 updated_at 与定向接收者）。不能在
	// 读取失败时返回请求侧的猜测快照，否则调用方会误以为这就是库内最终状态。
	saved, err := store.GetAnnouncement(ctx, s.DB.R(), a.ID)
	if err != nil {
		return store.Announcement{}, fmt.Errorf("%w: 公告已写入但无法读取最终状态", ErrUnavailable)
	}
	return saved, nil
}

// DeleteAnnouncement 删除公告及其定向接收者与已读记录。
func (s *Service) DeleteAnnouncement(ctx context.Context, p auth.Principal, id string) error {
	if err := auth.RequirePermission(p, perm.AdminAnnouncements); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: 缺少公告标识", ErrBadRequest)
	}
	if err := store.DeleteAnnouncement(ctx, s.DB.W(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, "announcement.delete", id, "")
	return nil
}

// mapAnnouncementErr 把 store 层的唯一约束冲突转成业务层的 Conflict。
//
// 触发它的是"已存在启用中的滚动公告"，这是正常的业务结果而不是故障，必须让
// 调用方能给出"请先停用现有滚动公告"这样的提示。
func mapAnnouncementErr(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: 已存在启用中的滚动公告", ErrConflict)
	}
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// validAnnouncementKind 判断公告类型是否已定义。
func validAnnouncementKind(kind store.AnnouncementKind) bool {
	switch kind {
	case store.KindTicker, store.KindAnnouncement, store.KindMessage:
		return true
	default:
		return false
	}
}
