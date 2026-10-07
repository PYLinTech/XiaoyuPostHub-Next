package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// CreateInviteRequest 是新建邀请码的入参。
type CreateInviteRequest struct {
	GroupName string
	MaxUses   int
	ExpiresAt int64
	Note      string
}

// AdminCreateInviteCode 生成邀请码。
//
// 明文只在返回值里出现一次，库里只留 HMAC 哈希：短码不能用随机盐的慢哈希，
// 因为必须能"由明文反查"，因此改用带服务端 pepper 的 HMAC，使得仅有数据库
// 也无法离线枚举。
//
// 返回的第二个值是明文码，调用方必须当场展示给管理员，之后无法再取回。
func (s *Service) AdminCreateInviteCode(ctx context.Context, p auth.Principal, req CreateInviteRequest) (store.InviteCode, string, error) {
	if err := auth.RequirePermission(p, perm.AdminInvites); err != nil {
		return store.InviteCode{}, "", err
	}
	groupName := strings.TrimSpace(req.GroupName)
	if groupName == "" {
		groupName = perm.GroupNormal
	}
	if _, err := store.GetGroup(ctx, s.DB.R(), groupName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.InviteCode{}, "", fmt.Errorf("%w: 用户组 %s 不存在", ErrBadRequest, groupName)
		}
		return store.InviteCode{}, "", err
	}
	pepper, err := s.Auth.Pepper(ctx)
	if err != nil {
		return store.InviteCode{}, "", fmt.Errorf("%w: 主密钥未就绪，无法派生短凭据密钥", ErrUnavailable)
	}

	plaintext, err := store.NewInviteCode()
	if err != nil {
		return store.InviteCode{}, "", err
	}
	code := store.InviteCode{
		CodeHash:  auth.HashLookupSecret(string(pepper), plaintext),
		CodeHint:  hintOf(plaintext),
		GroupName: groupName,
		MaxUses:   req.MaxUses,
		ExpiresAt: req.ExpiresAt,
		CreatedBy: p.UserID(),
		Note:      strings.TrimSpace(req.Note),
	}
	if code.MaxUses <= 0 {
		code.MaxUses = 1
	}
	if code.ExpiresAt > 0 && code.ExpiresAt <= s.Now() {
		return store.InviteCode{}, "", fmt.Errorf("%w: 过期时间必须晚于当前时间", ErrBadRequest)
	}
	if err := store.CreateInviteCode(ctx, s.DB.W(), code); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.InviteCode{}, "", fmt.Errorf("%w: 邀请码冲突，请重试", ErrConflict)
		}
		return store.InviteCode{}, "", err
	}
	created, err := store.GetInviteCode(ctx, s.DB.R(), code.CodeHash)
	if err != nil {
		return store.InviteCode{}, "", err
	}
	s.audit(ctx, p, "invite.create", strconv.FormatInt(created.ID, 10),
		fmt.Sprintf("group=%s maxUses=%d", created.GroupName, created.MaxUses))
	return created, plaintext, nil
}

// AdminListInviteCodes 分页列出邀请码（不含哈希）。
func (s *Service) AdminListInviteCodes(ctx context.Context, p auth.Principal, limit, offset int) ([]store.InviteCode, error) {
	if err := auth.RequirePermission(p, perm.AdminInvites); err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	return store.ListInviteCodes(ctx, s.DB.R(), limit, offset)
}

// AdminSetInviteDisabled 停用或启用邀请码。
//
// 停用而不是删除是更常用的处置：已经发出去的码需要立刻失效，但使用记录
// 仍有审计价值。
func (s *Service) AdminSetInviteDisabled(ctx context.Context, p auth.Principal, id int64, disabled bool) error {
	if err := auth.RequirePermission(p, perm.AdminInvites); err != nil {
		return err
	}
	if err := store.SetInviteCodeDisabled(ctx, s.DB.W(), id, disabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, "invite.disable", strconv.FormatInt(id, 10), fmt.Sprintf("disabled=%v", disabled))
	return nil
}

// AdminDeleteInviteCode 删除邀请码（使用记录随之级联删除）。
func (s *Service) AdminDeleteInviteCode(ctx context.Context, p auth.Principal, id int64) error {
	if err := auth.RequirePermission(p, perm.AdminInvites); err != nil {
		return err
	}
	if err := store.DeleteInviteCode(ctx, s.DB.W(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, "invite.delete", strconv.FormatInt(id, 10), "")
	return nil
}

// AdminListInviteUses 列出某个邀请码的使用记录。
//
// 记录在注册时写入，这里是它唯一的读出口：能回答"这个码把谁放进来了"，
// 而"只写不读"的审计表等于没有审计。
func (s *Service) AdminListInviteUses(ctx context.Context, p auth.Principal, id int64, limit int) ([]store.InviteUse, error) {
	if err := auth.RequirePermission(p, perm.AdminInvites); err != nil {
		return nil, err
	}
	uses, err := store.ListInviteUsesByCodeID(ctx, s.DB.R(), id, limit)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return uses, nil
}

// hintOf 取明文码的后 4 位作为后台辨识提示。
//
// 只留后 4 位而不是前 4 位：前缀容易被误当作"码的规律"，后 4 位已足够让
// 管理员在列表里区分不同条目。
func hintOf(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[len(code)-4:]
}
