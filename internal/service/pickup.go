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
)

// pickupCodeGenAttempts 是码值撞上已存在主键时的换码次数。
//
// 过期/停用行仍占用主键但不在"存活码池"计数内，正常情况下撞上它们的概率
// 可以忽略；连续多次撞码只可能发生在码池事实上已被占满时，此时按码池耗尽
// 明确失败，不做无界重试。
const pickupCodeGenAttempts = 5

// pickupAliveAfter 返回有效期窗口下界：created_at 大于该值的码才算存活。
// TTL 为 0（永久）时返回 0，而 created_at 恒大于 0，即全部存活。
func pickupAliveAfter(now int64, ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	bound := now - int64(ttl/time.Second)
	if bound < 0 {
		return 0
	}
	return bound
}

// pickupExpiresAt 计算用于展示的绝对到期时间；0 表示永久。
func pickupExpiresAt(createdAt int64, ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return createdAt + int64(ttl/time.Second)
}

// pickupExpired 按"创建时间 + 当前配置的全局有效期"动态判定取件码是否过期。
// 判定不依赖任何落库的到期时间，因此管理员缩短有效期后超龄码立即失效。
func pickupExpired(pc store.PickupCode, now int64, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return pc.CreatedAt+int64(ttl/time.Second) <= now
}

// CreatePickupCode 为某个分享新建一个取件码。
//
// 取件码本质是"分享的短码别名"：这里只校验分享归属，提取码、有效期、访问
// 计数、路径穿越防护全部复用 shares 的实现，避免两套语义漂移。
//
// 单个取件码不能指定有效期：有效期是管理员统一配置的全局时长（默认 8 小时，
// 可设永久），自创建时起算、按当前配置动态判定。
func (s *Service) CreatePickupCode(ctx context.Context, p auth.Principal, shareID string, maxUses int) (store.PickupCode, error) {
	if err := auth.RequirePermission(p, perm.Pickup); err != nil {
		return store.PickupCode{}, err
	}
	share, err := s.ownShare(ctx, p.UserID(), shareID)
	if err != nil {
		return store.PickupCode{}, err
	}
	if maxUses <= 0 {
		return store.PickupCode{}, fmt.Errorf("%w: 使用次数必须大于 0", ErrBadRequest)
	}

	now := s.Now()
	ttl := s.Settings.Runtime(ctx).Share.PickupTTL
	aliveAfter := pickupAliveAfter(now, ttl)

	// 码池是全局共用的 28^6 个短码（大小写不区分、剔除易混字符）。有效期窗口
	// 内存活的码占满码空间时，生成注定失败，必须把原因明确返回给用户，而不是
	// 反复撞主键。
	occupied, err := store.CountAlivePickupCodes(ctx, s.DB.R(), aliveAfter)
	if err != nil {
		return store.PickupCode{}, err
	}
	if occupied >= store.PickupCodePoolSize {
		return store.PickupCode{}, fmt.Errorf(
			"%w: 有效期窗口内存活的取件码已达码空间上限 %d", ErrPickupPoolExhausted, store.PickupCodePoolSize)
	}

	record := store.PickupCode{
		ShareID:   share.ID,
		MaxUses:   maxUses,
		CreatedBy: p.UserID(),
		CreatedAt: now,
	}
	// 碰撞换码：过期/停用行仍占着唯一码值，随机撞上时换一个；连续撞码
	// 视为码池事实上已满。
	var raw string
	for attempt := 0; attempt < pickupCodeGenAttempts; attempt++ {
		raw, err = store.NewPickupCode(store.PickupCodeLength)
		if err != nil {
			return store.PickupCode{}, err
		}
		record.Code = raw
		if err = store.CreatePickupCode(ctx, s.DB.W(), record); err == nil {
			break
		}
		if !errors.Is(err, store.ErrConflict) {
			return store.PickupCode{}, err
		}
	}
	if err != nil {
		return store.PickupCode{}, fmt.Errorf(
			"%w: 连续生成的取件码均被占用，码空间可能已用尽", ErrPickupPoolExhausted)
	}
	record.ExpiresAt = pickupExpiresAt(now, ttl)
	s.audit(ctx, p, "pickup.create", record.Code, share.ID)
	return record, nil
}

// ListPickupCodes 列出某个分享下的全部取件码。
//
// ExpiresAt 不持久化，这里按当前的全局有效期配置逐项算出后再返回，
// 管理界面看到的到期时间随配置实时变化。
func (s *Service) ListPickupCodes(ctx context.Context, p auth.Principal, shareID string) ([]store.PickupCode, error) {
	if err := auth.RequireUser(p); err != nil {
		return nil, err
	}
	share, err := s.ownShare(ctx, p.UserID(), shareID)
	if err != nil {
		return nil, err
	}
	items, err := store.ListPickupCodesByShare(ctx, s.DB.R(), share.ID)
	if err != nil {
		return nil, err
	}
	ttl := s.Settings.Runtime(ctx).Share.PickupTTL
	for i := range items {
		items[i].ExpiresAt = pickupExpiresAt(items[i].CreatedAt, ttl)
	}
	return items, nil
}

// DeletePickupCode 删除取件码。
func (s *Service) DeletePickupCode(ctx context.Context, p auth.Principal, code string) error {
	if err := auth.RequireUser(p); err != nil {
		return err
	}
	pc, err := s.ownPickupCode(ctx, p.UserID(), code)
	if err != nil {
		return err
	}
	if err := store.DeletePickupCode(ctx, s.DB.W(), pc.Code); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, "pickup.delete", pc.Code, pc.ShareID)
	return nil
}

// SetPickupDisabled 启用或停用取件码。
//
// 停用而不是删除：码可能已经分发出去，停用保留了"这个码曾经存在过"的
// 审计线索，也便于误操作后恢复。
func (s *Service) SetPickupDisabled(ctx context.Context, p auth.Principal, code string, disabled bool) error {
	if err := auth.RequireUser(p); err != nil {
		return err
	}
	pc, err := s.ownPickupCode(ctx, p.UserID(), code)
	if err != nil {
		return err
	}
	if err := store.SetPickupDisabled(ctx, s.DB.W(), pc.Code, disabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, "pickup.disable", pc.Code, fmt.Sprintf("disabled=%v", disabled))
	return nil
}

// PeekPickupCode 校验取件码并换出交付目标，但**不**消耗使用额度。
//
// 它服务于"先让用户看到内容信息、再决定是否下载"的界面流程。如果这一步也扣次数，
// 那么 maxUses=1 的取件码在用户点下下载之前就已经用尽，表现为"提取成功但永远
// 下载不了"——而 maxUses 的默认值正是 1。真正取数时再由 ClaimPickupUseForActor
// 核销，一次提取只记一次账。
//
// 不扣次数不会让这个入口变成取件码校验器：查询前的双维度退避与"码不存在即计入
// 退避"依然生效，反复试错会被迅速挡住。
func (s *Service) PeekPickupCode(ctx context.Context, code string, p auth.Principal) (DeliveryTarget, store.Share, error) {
	return s.resolvePickupCode(ctx, code, p, false)
}

func (s *Service) resolvePickupCode(ctx context.Context, code string, p auth.Principal, consume bool) (DeliveryTarget, store.Share, error) {
	forbid := func(why string) error { return fmt.Errorf("%w: %s", ErrForbidden, why) }

	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return DeliveryTarget{}, store.Share{}, forbid("取件码无效或已失效")
	}
	key := [2]string{string(auth.ThrottlePickup), code}
	ipKey := [2]string{string(auth.ThrottleIP), throttleIP(p)}
	if err := s.Auth.Throttler().CheckAll(ctx, key, ipKey); err != nil {
		return DeliveryTarget{}, store.Share{}, err
	}

	pc, err := store.GetPickupCode(ctx, s.DB.R(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 只有"码不存在"才计入退避：其余失败要么是码本身的状态，
			// 要么是分享侧的问题，把它们也算作爆破尝试会让正常用户
			// 因为一条过期的码被误伤。
			if throttleErr := s.Auth.Throttler().FailAll(ctx, key, ipKey); throttleErr != nil {
				return DeliveryTarget{}, store.Share{}, fmt.Errorf("%w: 记录取件码失败状态", ErrUnavailable)
			}
			return DeliveryTarget{}, store.Share{}, forbid("取件码无效或已失效")
		}
		return DeliveryTarget{}, store.Share{}, err
	}
	now := s.Now()
	ttl := s.Settings.Runtime(ctx).Share.PickupTTL
	if pc.Disabled || pickupExpired(pc, now, ttl) || pc.UsedCount >= pc.MaxUses {
		return DeliveryTarget{}, store.Share{}, forbid("取件码无效或已失效")
	}

	// 取件码本身就是一次凭据校验：持有它即视为已通过该分享的提取码。
	target, share, err := s.resolveShare(ctx, pc.ShareID, "", p, true, false, "pickup", !consume)
	if err != nil {
		return DeliveryTarget{}, store.Share{}, err
	}
	if consume {
		aliveAfter := pickupAliveAfter(now, ttl)
		err := s.DB.InTx(ctx, func(tx store.Querier) error {
			if err := s.ensurePickupShareActiveTx(ctx, tx, pc.ShareID); err != nil {
				return err
			}
			if _, err := store.ConsumePickupCode(ctx, tx, code, aliveAfter); err != nil {
				return err
			}
			if err := store.ConsumeShareVisit(ctx, tx, pc.ShareID, now); err != nil {
				return err
			}
			// 审计不是交付的前置条件；写失败不能把已经成功的核销回滚成
			// 重试时重复扣减的机会，但计数本身必须与核销同事务提交。
			_ = store.InsertShareAccess(ctx, tx, pc.ShareID, p.Actor, p.UserID(), p.ClientIP.String(), "pickup")
			return nil
		})
		if errors.Is(err, store.ErrNoRowsAffected) {
			return DeliveryTarget{}, store.Share{}, forbid("取件码或分享已失效、已用尽")
		}
		if err != nil {
			return DeliveryTarget{}, store.Share{}, err
		}
		share.Visits++
	}

	if err := s.Auth.Throttler().Clear(ctx, auth.ThrottlePickup, code); err != nil {
		return DeliveryTarget{}, store.Share{}, fmt.Errorf("%w: 清除取件码限流状态", ErrUnavailable)
	}
	// 不清 IP 维度：它的作用是"防同一来源轮换取件码"。取到自己的一个取件码
	// 就把整个来源的失败计数清零，等于给爆破提供了免费复位。
	target.PickupCode = code
	return target, share, nil
}

// ClaimPickupUseForActor 在一次写事务里同时核销取件码与分享访问次数。
//
// 取件码“查看”不应消耗任一额度；只有交付准备成功后的最后一步才进入这里。
// 两个计数必须同事务提交，否则一个成功、另一个失败会出现“下载没发生但
// 次数被扣掉”或“分享访问上限被绕过”的不一致。
func (s *Service) ClaimPickupUseForActor(ctx context.Context, code string, p auth.Principal) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return fmt.Errorf("%w: 取件码无效或已失效", ErrForbidden)
	}
	pc, err := store.GetPickupCode(ctx, s.DB.R(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: 取件码无效或已失效", ErrForbidden)
		}
		return err
	}
	now := s.Now()
	ttl := s.Settings.Runtime(ctx).Share.PickupTTL
	aliveAfter := pickupAliveAfter(now, ttl)
	err = s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := s.ensurePickupShareActiveTx(ctx, tx, pc.ShareID); err != nil {
			return err
		}
		if _, err := store.ConsumePickupCode(ctx, tx, code, aliveAfter); err != nil {
			return err
		}
		if err := store.ConsumeShareVisit(ctx, tx, pc.ShareID, now); err != nil {
			return err
		}
		return store.InsertShareAccess(ctx, tx, pc.ShareID, p.Actor, p.UserID(), p.ClientIP.String(), "pickup")
	})
	if errors.Is(err, store.ErrNoRowsAffected) {
		return fmt.Errorf("%w: 取件码或分享已失效、已用尽", ErrForbidden)
	}
	return err
}

// ensurePickupShareActiveTx rechecks the share owner while the核销事务持有写锁。
// Peek 与核销之间可能发生封禁；如果只依赖 Peek 的快照，账号停用后仍会有
// 一个竞态窗口可以完成下载。
func (s *Service) ensurePickupShareActiveTx(ctx context.Context, tx store.Querier, shareID string) error {
	share, err := store.GetShare(ctx, tx, shareID)
	if errors.Is(err, store.ErrNotFound) {
		return store.ErrNoRowsAffected
	}
	if err != nil {
		return err
	}
	owner, err := store.GetUserByID(ctx, tx, share.OwnerID)
	if errors.Is(err, store.ErrNotFound) {
		return store.ErrNoRowsAffected
	}
	if err != nil {
		return err
	}
	if owner.Status != store.UserEnabled {
		return store.ErrNoRowsAffected
	}
	return nil
}

// ownPickupCode 取自己分享下的取件码。码是不区分大小写的短凭据，
// 因此这里先统一成大写再查，避免"用户抄的是同一个码却查不到"。
func (s *Service) ownPickupCode(ctx context.Context, userID int64, code string) (store.PickupCode, error) {
	pc, err := store.GetPickupCode(ctx, s.DB.R(), strings.ToUpper(strings.TrimSpace(code)))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.PickupCode{}, ErrNotFound
		}
		return store.PickupCode{}, err
	}
	if _, err := s.ownShare(ctx, userID, pc.ShareID); err != nil {
		return store.PickupCode{}, err
	}
	return pc, nil
}
