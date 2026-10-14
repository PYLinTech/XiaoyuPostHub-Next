package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// deliveryBilling 是一次交付确定的计费桶。分享优先，额度不足时整笔转个人桶；
// 身份授权仍绑定访问者，不能为了让创建者承担额度而替换 actor。
type deliveryBilling struct {
	scope     store.CounterScope
	key       string
	userID    int64
	groupName string
	limit     int64
	limited   bool
}

func (s *Service) personalDeliveryBillingQ(ctx context.Context, q store.Querier, actor auth.Principal, at int64) (deliveryBilling, error) {
	limit, limited, err := s.dailyTrafficLimitQ(ctx, q, actor)
	return deliveryBilling{scope: store.ScopeTrafficDown, key: s.deliveryQuotaKey(actor, at), userID: actor.UserID(), groupName: actor.GroupName(), limit: limit, limited: limited}, err
}

// reserveDeliveryBillingQ 在同一写事务中选择、预扣并返回实际计费主体。
// 提供者限额失败的条件更新不写入计数，因此可以原子地转扣访问者；
// 数据库故障或分享授权失败则原样返回，不能被误当作额度不足。
func (s *Service) reserveDeliveryBillingQ(ctx context.Context, q store.Querier, actor auth.Principal, target DeliveryTarget, at, bytes int64) (deliveryBilling, error) {
	if target.ShareID != "" {
		share, err := store.GetShare(ctx, q, target.ShareID)
		if err != nil {
			return deliveryBilling{}, err
		}
		if share.OwnerID != target.OwnerUserID || (share.RootPath != target.Path && !vpath.IsWithin(share.RootPath, target.Path)) {
			return deliveryBilling{}, fmt.Errorf("%w: 分享与交付目标不匹配", ErrForbidden)
		}
		if share.Disabled || (share.ExpiresAt > 0 && share.ExpiresAt <= at) {
			return deliveryBilling{}, fmt.Errorf("%w: 分享已失效", ErrForbidden)
		}
		owner, err := store.GetUserByID(ctx, q, share.OwnerID)
		if err != nil {
			return deliveryBilling{}, err
		}
		if owner.Status != store.UserEnabled {
			return deliveryBilling{}, fmt.Errorf("%w: 分享创建者已停用", ErrForbidden)
		}
		if actor.IsGuest() || actor.UserID() != owner.ID {
			quotas, err := store.GroupQuotaMap(ctx, q, owner.GroupName)
			if err != nil {
				return deliveryBilling{}, fmt.Errorf("%w: 读取分享流量配额失败", ErrUnavailable)
			}
			limit, limited := quotas[store.QuotaTrafficDailyShare]
			billing := deliveryBilling{scope: store.ScopeTrafficShare, key: store.UserCounterKey(owner.ID, store.DayKey(at)), userID: owner.ID, groupName: owner.GroupName, limit: limit, limited: limited}
			err = s.reserveBillingQ(ctx, q, billing, bytes)
			if err == nil {
				return billing, nil
			}
			if !errors.Is(err, ErrQuotaExceeded) {
				return deliveryBilling{}, err
			}
		}
	}
	billing, err := s.personalDeliveryBillingQ(ctx, q, actor, at)
	if err != nil {
		return deliveryBilling{}, err
	}
	if err := s.reserveBillingQ(ctx, q, billing, bytes); err != nil {
		if target.ShareID != "" && errors.Is(err, ErrQuotaExceeded) && (actor.IsGuest() || actor.UserID() != target.OwnerUserID) {
			return deliveryBilling{}, fmt.Errorf("%w: 分享提供者与访问者的今日流量额度均不足", ErrQuotaExceeded)
		}
		return deliveryBilling{}, err
	}
	return billing, nil
}

func (s *Service) reserveBillingQ(ctx context.Context, q store.Querier, billing deliveryBilling, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	if !billing.limited {
		_, err := store.AddCounter(ctx, q, billing.scope, billing.key, bytes)
		return err
	}
	if _, err := store.ReserveCounter(ctx, q, billing.scope, billing.key, bytes, billing.limit); err != nil {
		if errors.Is(err, store.ErrQuotaExceeded) {
			return fmt.Errorf("%w: 今日流量额度不足", ErrQuotaExceeded)
		}
		return err
	}
	return nil
}

// ticketBillingActor 用于报表归属。旧票据没有计费快照，继续按旧访问者记账。
func ticketBillingActor(ticket store.Ticket) (string, int64, string) {
	if ticket.QuotaKey != "" {
		if ticket.BilledUserID > 0 {
			return store.UserCounterKey(ticket.BilledUserID, ""), ticket.BilledUserID, ticket.BilledGroupName
		}
		return store.GuestCounterKey(ticket.IPPrefix, ""), 0, ticket.BilledGroupName
	}
	if ticket.ActorType == store.ActorGuest {
		return store.GuestCounterKey(ticket.IPPrefix, ""), 0, ticket.GroupName
	}
	return store.UserCounterKey(ticket.UserID, ""), ticket.UserID, ticket.GroupName
}

// deliveryQuotaKey 冻结访问者个人桶的 UTC 日期。
func (s *Service) deliveryQuotaKey(actor auth.Principal, at int64) string {
	return actor.ActorKey() + ":" + store.DayKey(at)
}

func (s *Service) releaseDeliveryQuotaByTicketQ(ctx context.Context, q store.Querier, ticket store.Ticket, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	scope, key := ticket.QuotaScope, ticket.QuotaKey
	if key == "" {
		actorKey, _, _ := ticketBillingActor(ticket)
		scope = store.ScopeTrafficDown
		key = actorKey + ":" + store.DayKey(ticket.CreatedAt)
	}
	_, err := store.ReleaseCounter(ctx, q, scope, key, bytes)
	return err
}

// dailyTrafficLimitQ 返回个人下载日上限；缺失表示不限，0 表示桶不可用。
func (s *Service) dailyTrafficLimitQ(ctx context.Context, q store.Querier, actor auth.Principal) (int64, bool, error) {
	if actor.UnlimitedQuota() {
		return 0, false, nil
	}
	quotas, err := store.GroupQuotaMap(ctx, q, actor.GroupName())
	if err != nil {
		return 0, false, fmt.Errorf("%w: 读取流量配额失败", ErrUnavailable)
	}
	limit, ok := quotas[store.QuotaTrafficDailyDown]
	if !ok {
		return 0, false, nil
	}
	return limit, true, nil
}
