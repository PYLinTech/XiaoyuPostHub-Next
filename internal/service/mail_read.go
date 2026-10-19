// 邮件阅读链路：列表、详情、已读/星标、归档两段式。
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

// ListMailRequest 是邮件列表查询。
type ListMailRequest struct {
	// View: inbox(默认)/outbox(已发送+草稿)/starred/trash。
	View       string
	OnlyUnread bool
	Query      string
	Limit      int
	Offset     int
}

// ListMailResult 是分页结果。
type ListMailResult struct {
	Items    []store.MailboxListItem `json:"items"`
	Total    int                     `json:"total"`
	Limit    int                     `json:"limit"`
	Offset   int                     `json:"offset"`
	Counters store.MailboxCounters   `json:"counters"`
}

// MailDetail 是一封邮件的完整阅读视图。
type MailDetail struct {
	Box        store.Mailbox         `json:"box"`
	Message    store.MailMessage     `json:"message"`
	Recipients []store.MailRecipient `json:"recipients"`
	Parts      []store.MailPart      `json:"parts"`
}

func (s *Service) requireMailAccess(actor auth.Principal) error {
	if !actor.Can(perm.MailAccess) {
		return fmt.Errorf("%w: 未开放邮件功能", ErrForbidden)
	}
	return nil
}

// ownedMail 取属主归属行；不存在一律按 Forbidden 回应（不泄露邮件是否存在），
// 已彻底释放的邮件同样不可读。
func (s *Service) ownedMail(ctx context.Context, actor auth.Principal, id string) (store.Mailbox, error) {
	if err := s.requireMailAccess(actor); err != nil {
		return store.Mailbox{}, err
	}
	box, err := store.GetMailboxForUser(ctx, s.DB.R(), actor.UserID(), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Mailbox{}, fmt.Errorf("%w: 邮件不存在或无权访问", ErrForbidden)
	}
	if err != nil {
		return store.Mailbox{}, err
	}
	if box.Status == store.MailboxStatusReleased {
		return store.Mailbox{}, fmt.Errorf("%w: 邮件已彻底删除", ErrForbidden)
	}
	return box, nil
}

// ListMail 按视图分页列出当前用户邮件。
func (s *Service) ListMail(ctx context.Context, actor auth.Principal, req ListMailRequest) (ListMailResult, error) {
	if err := s.requireMailAccess(actor); err != nil {
		return ListMailResult{}, err
	}
	f := store.MailboxListFilter{
		OnlyUnread: req.OnlyUnread,
		Query:      strings.TrimSpace(req.Query),
	}
	switch req.View {
	case "", "inbox":
		f.Role = store.MailboxRoleInbox
	case "starred":
		f.OnlyStarred = true
	case "trash":
		f.Status = store.MailboxStatusArchived
	default:
		return ListMailResult{}, fmt.Errorf("%w: 未知视图 %q", ErrBadRequest, req.View)
	}
	items, total, err := store.ListMailboxes(ctx, s.DB.R(), actor.UserID(), f, req.Limit, req.Offset)
	if err != nil {
		return ListMailResult{}, err
	}
	counters, err := store.GetMailboxCounters(ctx, s.DB.R(), actor.UserID())
	if err != nil {
		return ListMailResult{}, err
	}
	return ListMailResult{
		Items: items, Total: total, Limit: req.Limit, Offset: req.Offset, Counters: counters,
	}, nil
}

// GetMail 返回详情并把收件箱邮件自动置为已读。
func (s *Service) GetMail(ctx context.Context, actor auth.Principal, id string) (*MailDetail, error) {
	box, err := s.ownedMail(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	msg, err := store.GetMailMessage(ctx, s.DB.R(), id)
	if err != nil {
		return nil, err
	}
	rs, err := store.ListMailRecipients(ctx, s.DB.R(), id)
	if err != nil {
		return nil, err
	}
	ps, err := store.ListMailParts(ctx, s.DB.R(), id)
	if err != nil {
		return nil, err
	}
	// 打开即已读：收件箱 normal 邮件自动置位。失败不阻断阅读，下一次打开还会重试。
	if !box.IsRead && box.Role == store.MailboxRoleInbox && box.Status == store.MailboxStatusNormal {
		if err := store.SetMailboxRead(ctx, s.DB.W(), actor.UserID(), id, box.Role, true); err == nil {
			box.IsRead = true
		}
	}
	// 阅读审计：不产生流量增量，只记动作（日聚合行零值）。
	s.DB.Traffic().Record(
		store.TrafficLog{
			ActorType:    store.ActorUser,
			UserID:       actor.UserID(),
			ClientIP:     actor.ClientIP.String(),
			GroupName:    actor.GroupName(),
			Action:       "mail_view",
			ResourcePath: "mail:" + id,
		},
		store.TrafficDaily{
			ActorKey:  store.UserCounterKey(actor.UserID(), ""),
			Day:       store.DayKey(s.Now()),
			GroupName: actor.GroupName(),
		},
	)
	return &MailDetail{Box: box, Message: msg, Recipients: rs, Parts: ps}, nil
}

// MarkMailRead 显式设置已读（工具栏用）。
func (s *Service) MarkMailRead(ctx context.Context, actor auth.Principal, id string, read bool) error {
	box, err := s.ownedMail(ctx, actor, id)
	if err != nil {
		return err
	}
	return store.SetMailboxRead(ctx, s.DB.W(), actor.UserID(), id, box.Role, read)
}

// StarMail 设置星标。
func (s *Service) StarMail(ctx context.Context, actor auth.Principal, id string, starred bool) error {
	box, err := s.ownedMail(ctx, actor, id)
	if err != nil {
		return err
	}
	return store.SetMailboxStarred(ctx, s.DB.W(), actor.UserID(), id, box.Role, starred)
}

// archivePurgeAt 按归档留存设置计算计划清理时间；未启用留存返回 0。
// 用户自助回收与管理员回收共用同一口径。
func (s *Service) archivePurgeAt(ctx context.Context) int64 {
	retention := s.Settings.Runtime(ctx).Mail.ArchiveRetention
	if retention <= 0 {
		return 0
	}
	return s.Now() + int64(retention.Seconds())
}

// ArchiveMyMail 把邮件移入用户归档，按设置写计划清理时间。占配额、可恢复。
func (s *Service) ArchiveMyMail(ctx context.Context, actor auth.Principal, id string) error {
	box, err := s.ownedMail(ctx, actor, id)
	if err != nil {
		return err
	}
	if box.Status == store.MailboxStatusArchived {
		return nil // 幂等
	}
	return store.SetMailboxStatus(ctx, s.DB.W(), actor.UserID(), id, box.Role,
		store.MailboxStatusArchived, s.archivePurgeAt(ctx))
}

// RestoreMyMail 从归档恢复（清空计划清理时间）。
func (s *Service) RestoreMyMail(ctx context.Context, actor auth.Principal, id string) error {
	box, err := s.ownedMail(ctx, actor, id)
	if err != nil {
		return err
	}
	if box.Status != store.MailboxStatusArchived {
		return fmt.Errorf("%w: 邮件不在归档", ErrBadRequest)
	}
	return store.SetMailboxStatus(ctx, s.DB.W(), actor.UserID(), id, box.Role,
		store.MailboxStatusNormal, 0)
}

// purgeMailbox 销毁单个归属：数存活属主、归还配额与释放部件引用全部在 store
// 的同一事务内完成。guard 表达这次销毁的合法前提（自助彻底删除 / 到期清理）。
// 重复调用是安全的空操作：已 released 的行匹配不到限定条件。
func (s *Service) purgeMailbox(ctx context.Context, box store.Mailbox, g store.PurgeGuard) error {
	return store.PurgeMailboxTx(ctx, s.DB, store.DueMailbox{
		ID:           box.ID,
		MessageID:    box.MessageID,
		UserID:       box.UserID,
		Role:         box.Role,
		ChargedBytes: box.ChargedBytes,
	}, g)
}

// PurgeMyMail 从归档彻底删除：立即释放属主配额；最后一个存活属主时释放部件引用。
func (s *Service) PurgeMyMail(ctx context.Context, actor auth.Principal, id string) error {
	box, err := s.ownedMail(ctx, actor, id)
	if err != nil {
		return err
	}
	if box.Status != store.MailboxStatusArchived {
		return fmt.Errorf("%w: 请先把邮件移入归档", ErrBadRequest)
	}
	return s.purgeMailbox(ctx, box, store.PurgeGuard{RequireArchived: true})
}
