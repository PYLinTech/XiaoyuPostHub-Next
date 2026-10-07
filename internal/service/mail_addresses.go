// 用户自助邮箱地址：在管理员分配给本组的域名下按前缀创建（格式与配额）。
package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// localPartRe 允许小写字母数字与 . _ -，首尾必须字母数字，长度 1..32。
// 连续点在匹配后单独拒绝（RE2 不支持前瞻；点是邮件路由里最容易出歧义的字符）。
var localPartRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,30}[a-z0-9])?$`)

// ListMyMailAddresses 列出当前用户的全部地址（含冻结）。
func (s *Service) ListMyMailAddresses(ctx context.Context, actor auth.Principal) ([]store.MailAddress, error) {
	if err := s.requireMailAccess(actor); err != nil {
		return nil, err
	}
	return store.ListMailAddressesByUser(ctx, s.DB.R(), actor.UserID())
}

// MyMailDomain 是管理员分配给本组、且已开启收件的一个域名。
type MyMailDomain struct {
	Domain string `json:"domain"`
}

// ListMyMailDomains 返回本用户所在组可用于自助注册地址的域名。
//
// 存在的理由：新建地址的界面不该让用户手打域名。域名完全由管理员按组分配，
// 用户没有改它的余地，界面也只给得出"从列表里选"这一个诚实的做法——
// 让他手输只会挑到一个必然创建失败的值。列出可选项同时也是告知：这个域归你。
//
// 只返回本组已开启收信的绑定：暂停收件的域名不该出现在下拉里。
func (s *Service) ListMyMailDomains(ctx context.Context, actor auth.Principal) ([]MyMailDomain, error) {
	if err := s.requireMailAccess(actor); err != nil {
		return nil, err
	}
	bindings, err := store.ListGroupMailDomains(ctx, s.DB.R(), actor.GroupName())
	if err != nil {
		return nil, fmt.Errorf("%w: 读取用户组域名失败", ErrUnavailable)
	}
	out := make([]MyMailDomain, 0, len(bindings))
	for _, b := range bindings {
		if b.ReceiveEnabled {
			out = append(out, MyMailDomain{Domain: b.Domain})
		}
	}
	return out, nil
}

// CreateMyMailAddress 在管理员分配给本组、且已开启收件的域名下自助创建一个地址。
func (s *Service) CreateMyMailAddress(ctx context.Context, actor auth.Principal, localPart, domain string) (store.MailAddress, error) {
	if err := s.requireMailAccess(actor); err != nil {
		return store.MailAddress{}, err
	}
	localPart = strings.ToLower(strings.TrimSpace(localPart))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if strings.Contains(localPart, "..") || !localPartRe.MatchString(localPart) {
		return store.MailAddress{}, fmt.Errorf(
			"%w: 前缀只能含小写字母、数字与 ._-（首尾为字母数字），长度 1..32", ErrBadRequest)
	}

	// 域名必须由管理员分配给本组、且本组该绑定已开启收件。域名不存在、没分配
	// 给本组、未开启，三种情况统一报同一句 BadRequest：逐个区分等于把后台
	// 配置透给普通用户，而对他们而言结果完全一样——都建不了。
	binding, err := store.GetGroupMailDomain(ctx, s.DB.R(), actor.GroupName(), domain)
	if err != nil {
		return store.MailAddress{}, fmt.Errorf("%w: 该域名不可用", ErrBadRequest)
	}
	if !binding.ReceiveEnabled {
		return store.MailAddress{}, fmt.Errorf("%w: 该域名未对你的用户组开放收件", ErrBadRequest)
	}

	// 配额：组配额优先；无行时取全局默认（0 = 禁止创建）。
	limit := int64(s.Settings.Runtime(ctx).Mail.AddressesDefault)
	if quotas, qerr := store.GroupQuotaMap(ctx, s.DB.R(), actor.GroupName()); qerr == nil {
		if v, ok := quotas[store.QuotaMailAddresses]; ok {
			limit = v
		}
	}
	if limit <= 0 {
		return store.MailAddress{}, fmt.Errorf("%w: 你的用户组未开放自助邮箱地址", ErrForbidden)
	}
	used, err := store.CountMailAddresses(ctx, s.DB.R(), actor.UserID(), domain)
	if err != nil {
		return store.MailAddress{}, err
	}
	if used >= limit {
		return store.MailAddress{}, fmt.Errorf("%w: 地址数量已达组上限 %d", ErrQuotaExceeded, limit)
	}

	addr := store.MailAddress{
		Address:   localPart + "@" + domain,
		LocalPart: localPart,
		Domain:    domain,
		UserID:    actor.UserID(),
		Status:    store.MailAddressActive,
	}
	if err := store.CreateMailAddress(ctx, s.DB.W(), addr); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.MailAddress{}, fmt.Errorf("%w: 该地址已被占用", ErrConflict)
		}
		return store.MailAddress{}, err
	}
	return addr, nil
}
