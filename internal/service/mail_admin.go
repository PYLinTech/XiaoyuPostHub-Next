// 管理端全局邮件视图：跨用户列表、只读详情、对单条归属行的回收/恢复/彻底删除。
// 与用户自助链路的区别：不校验属主、不自动已读、操作目标是 mailboxes.id。
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

// AdminListMailRequest 是全局邮件列表查询。
type AdminListMailRequest struct {
	UserID int64
	Owner  string // 属主账号子串
	Role   string // "" / inbox
	Status string // "" 默认 normal；可显式 archived / released
	Query  string
	Limit  int
	Offset int
}

// AdminListMailResult 是全局邮件分页结果。
type AdminListMailResult struct {
	Items  []store.AdminMailboxListItem `json:"items"`
	Total  int                          `json:"total"`
	Limit  int                          `json:"limit"`
	Offset int                          `json:"offset"`
	// Stats 是全站统计，**不受本次筛选影响**。Total 是当前筛选下的命中数，
	// 两者不是一回事：筛选框写满之后，统计条仍然是"整个系统里有多少邮件"。
	Stats store.AdminMailStats `json:"stats"`
}

// AdminMailboxDetail 是全局详情：归属行 + 属主账号 + 邮件元数据/收件人/部件清单。
// 不含正文交付通道——本期全局视图只读元数据，正文与附件下载不向管理员开放。
type AdminMailboxDetail struct {
	Box              store.Mailbox         `json:"box"`
	OwnerAccount     string                `json:"ownerAccount"`
	OwnerDisplayName string                `json:"ownerDisplayName"`
	Message          store.MailMessage     `json:"message"`
	Recipients       []store.MailRecipient `json:"recipients"`
	Parts            []store.MailPart      `json:"parts"`
}

var (
	adminMailRoles    = map[string]bool{"": true, store.MailboxRoleInbox: true}
	adminMailStatuses = map[string]bool{"": true, store.MailboxStatusNormal: true, store.MailboxStatusArchived: true, store.MailboxStatusReleased: true}
)

// AdminListMail 跨用户分页列出邮件归属。
func (s *Service) AdminListMail(ctx context.Context, actor auth.Principal,
	req AdminListMailRequest) (AdminListMailResult, error) {
	if err := auth.RequirePermission(actor, perm.AdminMail); err != nil {
		return AdminListMailResult{}, err
	}
	if !adminMailRoles[req.Role] || !adminMailStatuses[req.Status] {
		return AdminListMailResult{}, fmt.Errorf("%w: 筛选参数不合法", ErrBadRequest)
	}
	if req.UserID < 0 {
		return AdminListMailResult{}, fmt.Errorf("%w: 属主用户 ID 非法", ErrBadRequest)
	}
	// 归一化必须与 store 实际取值一致，否则回显的分页值与实际页长不符。
	limit, offset := clampPaging(req.Limit, req.Offset)
	items, total, err := store.ListAllMailboxes(ctx, s.DB.R(), store.AdminMailboxFilter{
		UserID: req.UserID,
		Owner:  req.Owner,
		Role:   req.Role,
		Status: req.Status,
		Query:  strings.TrimSpace(req.Query),
	}, limit, offset)
	if err != nil {
		return AdminListMailResult{}, err
	}
	// 统计条跟着列表一起返回：同属邮件管理、同一个权限位（AdminMail），
	// 且不受筛选影响——它回答的是"整个系统里有多少邮件"。
	stats, err := store.GetAdminMailStats(ctx, s.DB.R())
	if err != nil {
		return AdminListMailResult{}, err
	}
	return AdminListMailResult{
		Items: items, Total: total, Limit: limit, Offset: offset, Stats: stats,
	}, nil
}

// adminMailbox 按归属行 ID 取行，统一权限与不存在语义。
func (s *Service) adminMailbox(ctx context.Context, actor auth.Principal, boxID int64) (store.Mailbox, error) {
	if err := auth.RequirePermission(actor, perm.AdminMail); err != nil {
		return store.Mailbox{}, err
	}
	if boxID <= 0 {
		return store.Mailbox{}, fmt.Errorf("%w: 归属 ID 非法", ErrBadRequest)
	}
	box, err := store.GetMailboxByID(ctx, s.DB.R(), boxID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Mailbox{}, ErrNotFound
	}
	if err != nil {
		return store.Mailbox{}, err
	}
	return box, nil
}

// AdminGetMail 返回全局详情。刻意不改动已读状态——管理员巡检不能污染用户未读。
// 已销毁（released）的归属仍可读元数据，供事后核查。
func (s *Service) AdminGetMail(ctx context.Context, actor auth.Principal, boxID int64) (*AdminMailboxDetail, error) {
	box, err := s.adminMailbox(ctx, actor, boxID)
	if err != nil {
		return nil, err
	}
	owner, err := store.GetUserByID(ctx, s.DB.R(), box.UserID)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取属主信息失败", ErrUnavailable)
	}
	msg, err := store.GetMailMessage(ctx, s.DB.R(), box.MessageID)
	if err != nil {
		return nil, err
	}
	rs, err := store.ListMailRecipients(ctx, s.DB.R(), box.MessageID)
	if err != nil {
		return nil, err
	}
	ps, err := store.ListMailParts(ctx, s.DB.R(), box.MessageID)
	if err != nil {
		return nil, err
	}
	detail := &AdminMailboxDetail{
		Box:              box,
		OwnerAccount:     owner.Account,
		OwnerDisplayName: owner.DisplayName,
		Message:          msg,
		Recipients:       rs,
		Parts:            ps,
	}
	// 管理员跨用户读取邮件元数据属敏感访问，逐条留痕。
	s.audit(ctx, actor, "admin.mail.message_view", box.MessageID,
		fmt.Sprintf("user=%d box=%d owner=%s", box.UserID, box.ID, owner.Account))
	return detail, nil
}

// AdminArchiveMail 把指定归属移入对应用户的归档并写计划清理时间
// （与用户自助同一留存设置）。已在归档视为幂等成功；已销毁视为不存在。
func (s *Service) AdminArchiveMail(ctx context.Context, actor auth.Principal, boxID int64) error {
	box, err := s.adminMailbox(ctx, actor, boxID)
	if err != nil {
		return err
	}
	if box.Status == store.MailboxStatusReleased {
		return ErrNotFound
	}
	if box.Status == store.MailboxStatusArchived {
		return nil
	}
	if err := store.SetMailboxStatus(ctx, s.DB.W(), box.UserID, box.MessageID, box.Role,
		store.MailboxStatusArchived, s.archivePurgeAt(ctx)); err != nil {
		return err
	}
	s.audit(ctx, actor, "admin.mail.message_archive", box.MessageID,
		fmt.Sprintf("user=%d box=%d", box.UserID, box.ID))
	return nil
}

// AdminRestoreMail 把指定归属从归档恢复。
func (s *Service) AdminRestoreMail(ctx context.Context, actor auth.Principal, boxID int64) error {
	box, err := s.adminMailbox(ctx, actor, boxID)
	if err != nil {
		return err
	}
	if box.Status == store.MailboxStatusReleased {
		return ErrNotFound
	}
	if box.Status != store.MailboxStatusArchived {
		return fmt.Errorf("%w: 该邮件不在归档", ErrBadRequest)
	}
	if err := store.SetMailboxStatus(ctx, s.DB.W(), box.UserID, box.MessageID, box.Role,
		store.MailboxStatusNormal, 0); err != nil {
		return err
	}
	s.audit(ctx, actor, "admin.mail.message_restore", box.MessageID,
		fmt.Sprintf("user=%d box=%d", box.UserID, box.ID))
	return nil
}

// AdminPurgeMail 立即彻底删除指定归属：归还属主配额；最后一个存活属主时释放
// 部件内容池引用（复用与到期清理相同的事务）。
func (s *Service) AdminPurgeMail(ctx context.Context, actor auth.Principal, boxID int64) error {
	box, err := s.adminMailbox(ctx, actor, boxID)
	if err != nil {
		return err
	}
	if box.Status == store.MailboxStatusReleased {
		return ErrNotFound
	}
	if box.Status != store.MailboxStatusArchived {
		return fmt.Errorf("%w: 请先把邮件移入归档", ErrBadRequest)
	}
	if err := s.purgeMailbox(ctx, box, store.PurgeGuard{RequireArchived: true}); err != nil {
		return err
	}
	s.audit(ctx, actor, "admin.mail.message_purge", box.MessageID,
		fmt.Sprintf("user=%d box=%d bytes=%d", box.UserID, box.ID, box.ChargedBytes))
	return nil
}
