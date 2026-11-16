package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// MaxAPIKeysPerUser 限制单个用户同时持有的有效 Key 数量。
// 它约束的是泄露面与列表可读性，不是配额。
const MaxAPIKeysPerUser = 10

// CreateAPIKeyRequest 是新建 Key 的输入。
type CreateAPIKeyRequest struct {
	Name      string `json:"name"`
	ExpiresAt int64  `json:"expiresAt"`
}

// CreatedAPIKey 是创建结果。Token 只在这里出现一次，之后无法再取回。
type CreatedAPIKey struct {
	Key   store.APIKey `json:"key"`
	Token string       `json:"token"`
}

// CreateAPIKey 创建一枚用户自建 API Key。
func (s *Service) CreateAPIKey(ctx context.Context, p auth.Principal, req CreateAPIKeyRequest) (CreatedAPIKey, error) {
	if err := s.requireAPIKeyManagement(ctx, p); err != nil {
		return CreatedAPIKey{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return CreatedAPIKey{}, fmt.Errorf("%w: 请填写 Key 名称", ErrBadRequest)
	}
	if utf8.RuneCountInString(name) > 64 {
		return CreatedAPIKey{}, fmt.Errorf("%w: Key 名称最多 64 个字符", ErrBadRequest)
	}
	now := s.Now()
	if req.ExpiresAt != 0 && req.ExpiresAt <= now {
		return CreatedAPIKey{}, fmt.Errorf("%w: 有效期必须晚于当前时间", ErrBadRequest)
	}
	count, err := store.CountActiveAPIKeys(ctx, s.DB.R(), p.UserID(), now)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	if count >= MaxAPIKeysPerUser {
		return CreatedAPIKey{}, fmt.Errorf("%w: 最多保留 %d 个有效 API Key", ErrConflict, MaxAPIKeysPerUser)
	}
	id, err := store.NewID("key")
	if err != nil {
		return CreatedAPIKey{}, err
	}
	token, keyHash, prefix, err := auth.NewAPIKeyToken()
	if err != nil {
		return CreatedAPIKey{}, err
	}
	key := store.APIKey{
		ID:        id,
		UserID:    p.UserID(),
		Name:      name,
		Prefix:    prefix,
		KeyHash:   keyHash,
		ExpiresAt: req.ExpiresAt,
		CreatedAt: now,
	}
	if err := store.CreateAPIKey(ctx, s.DB.W(), key); err != nil {
		return CreatedAPIKey{}, err
	}
	s.audit(ctx, p, "api_key.create", id, name)
	return CreatedAPIKey{Key: key, Token: token}, nil
}

// ListAPIKeys 返回当前用户的全部 Key。明文令牌不可再取回。
func (s *Service) ListAPIKeys(ctx context.Context, p auth.Principal) ([]store.APIKey, error) {
	if err := s.requireAPIKeyManagement(ctx, p); err != nil {
		return nil, err
	}
	return store.ListAPIKeys(ctx, s.DB.R(), p.UserID())
}

// DeleteAPIKey 硬删一枚 Key；删除后下一次请求即失效。
func (s *Service) DeleteAPIKey(ctx context.Context, p auth.Principal, id string) error {
	if err := s.requireAPIKeyManagement(ctx, p); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: 缺少 Key 标识", ErrBadRequest)
	}
	if err := store.DeleteAPIKey(ctx, s.DB.W(), id, p.UserID()); err != nil {
		if errors.Is(err, store.ErrNoRowsAffected) {
			return ErrNotFound
		}
		return err
	}
	s.audit(ctx, p, "api_key.delete", id, "")
	return nil
}

// requireAPIKeyManagement 是 Key 管理面的统一门禁。
//
// API Key 本身不能管理 Key：一枚泄露的凭据若能签发新凭据，回收就失去意义。
func (s *Service) requireAPIKeyManagement(ctx context.Context, p auth.Principal) error {
	if !s.Settings.Runtime(ctx).Site.APIEnabled {
		return ErrNotFound
	}
	if p.IsGuest() {
		return fmt.Errorf("%w: 需要登录", ErrForbidden)
	}
	if p.APIKeyID != "" {
		return fmt.Errorf("%w: API Key 不能管理 API Key", ErrForbidden)
	}
	if !p.Can(perm.API) {
		return fmt.Errorf("%w: 缺少 API 调用权限", ErrForbidden)
	}
	return nil
}
