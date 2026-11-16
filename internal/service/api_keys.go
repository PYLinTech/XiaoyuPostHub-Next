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
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/vpath"
)

// CreateAPIKeyRequest 是新建 Key 的输入。
type CreateAPIKeyRequest struct {
	Name          string `json:"name"`
	WorkspacePath string `json:"workspacePath"`
	ExpiresAt     int64  `json:"expiresAt"`
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
	workspaceRaw := strings.TrimSpace(req.WorkspacePath)
	if workspaceRaw == "" {
		return CreatedAPIKey{}, fmt.Errorf("%w: 请选择 Key 的工作区文件夹", ErrBadRequest)
	}
	workspace, err := vpath.Normalize(workspaceRaw)
	if err != nil {
		return CreatedAPIKey{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	now := s.Now()
	if req.ExpiresAt != 0 && req.ExpiresAt <= now {
		return CreatedAPIKey{}, fmt.Errorf("%w: 有效期必须晚于当前时间", ErrBadRequest)
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
		ID:            id,
		UserID:        p.UserID(),
		Name:          name,
		Prefix:        prefix,
		KeyHash:       keyHash,
		WorkspacePath: workspace,
		ExpiresAt:     req.ExpiresAt,
		CreatedAt:     now,
	}
	// 工作区必须是一个仍然存在的文件夹，且与 Key 写入同事务，避免刚选完
	// 目录就被并发删掉，留下一枚指向不存在路径的 Key。
	if err := s.DB.InTx(ctx, func(tx store.Querier) error {
		if err := s.requireFolderTx(ctx, tx, p.UserID(), workspace); err != nil {
			return err
		}
		return store.CreateAPIKey(ctx, tx, key)
	}); err != nil {
		return CreatedAPIKey{}, err
	}
	s.audit(ctx, p, "api_key.create", id, fmt.Sprintf("name=%s workspace=%s", name, workspace))
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
