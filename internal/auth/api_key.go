package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// APIKeyTokenPrefix 是 API Key 的固定前缀，用于与会话令牌分流。
const APIKeyTokenPrefix = "xph_"

// APIKeyPrefixChars 是界面展示的 Key 前缀长度。它含 xph_，不足以用于认证。
const APIKeyPrefixChars = 12

// NewAPIKeyToken 生成一枚 API Key，返回值中的明文只应下发给用户一次。
//
// 令牌与邀请码不同：它是 256 位随机值，不需要 pepper，也不能被离线枚举。
// 因此库里存 SHA-256 即可，认证时按哈希反查。
func NewAPIKeyToken() (token, keyHash, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("auth: 生成 API Key 失败: %w", err)
	}
	token = APIKeyTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	keyHash = HashSessionToken(token)
	prefix = token
	if len(prefix) > APIKeyPrefixChars {
		prefix = prefix[:APIKeyPrefixChars]
	}
	return token, keyHash, prefix, nil
}

// resolveAPIKey 按 API Key 还原身份。
//
// 不缓存：Key 删除必须在下一次请求立刻生效，而 API 调用频率通常不高，
// 多读两次库换来确定性的回收语义是划算的。
func (s *Service) resolveAPIKey(ctx context.Context, token string, ip netip.Addr, ipPrefix string) (Principal, error) {
	invalidToken := func() (Principal, error) {
		guest, err := s.guestPrincipal(ctx, ip, ipPrefix)
		guest.InvalidToken = true
		return guest, err
	}

	key, err := store.GetAPIKeyByHash(ctx, s.db.R(), HashSessionToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return invalidToken()
		}
		return Principal{}, fmt.Errorf("auth: 读取 API Key 失败: %w", err)
	}
	if key.ExpiresAt > 0 && key.ExpiresAt <= time.Now().Unix() {
		return invalidToken()
	}
	user, err := store.GetUserByID(ctx, s.db.R(), key.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return invalidToken()
		}
		return Principal{}, err
	}
	if user.Status != store.UserEnabled {
		return invalidToken()
	}
	group, err := store.GetGroup(ctx, s.db.R(), user.GroupName)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: 读取用户组失败: %w", err)
	}
	s.maybeTouchAPIKey(ctx, key.ID)
	return Principal{
		Actor:      store.ActorUser,
		User:       user,
		Group:      group,
		ClientIP:   ip,
		IPPrefix:   ipPrefix,
		APIKeyID:   key.ID,
		APIKeyName: key.Name,
	}, nil
}

// maybeTouchAPIKey 把最近使用时间按会话同一套间隔写回，避免每个请求一次写。
func (s *Service) maybeTouchAPIKey(ctx context.Context, id string) {
	now := time.Now()
	if !s.touchDue("apikey:"+strings.TrimSpace(id), now) {
		return
	}
	_ = store.TouchAPIKey(ctx, s.db.W(), id, now.Unix())
}
