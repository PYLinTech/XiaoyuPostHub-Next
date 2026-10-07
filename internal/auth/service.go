package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/clientip"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

var (
	// ErrSessionInvalid 表示提供了令牌但它无效、已过期或已被吊销。
	ErrSessionInvalid = errors.New("auth: 会话无效或已过期")
	// ErrForbidden 表示身份有效但权限不足。
	ErrForbidden = errors.New("auth: 权限不足")
	// ErrNotAuthenticated 表示该操作需要登录。
	ErrNotAuthenticated = errors.New("auth: 需要登录")
)

// 会话缓存与活跃时间写回策略。
//
// 写池是单连接，若每个已鉴权请求都去更新 sessions.last_seen_at，读接口会被无关的
// 活跃时间更新拖成串行。因此会话解析结果缓存 sessionCacheTTL，活跃时间最多每
// sessionTouchInterval 写回一次。代价是封禁/改密最多延迟 sessionCacheTTL 生效，
// 所以该值取小。
const (
	sessionCacheTTL       = 10 * time.Second
	sessionTouchInterval  = 60 * time.Second
	principalCacheMaxSize = 4096
)

// Principal 是一次请求的身份判定结果。
type Principal struct {
	Actor    store.ActorType
	User     store.User
	Group    store.Group
	Session  store.Session
	ClientIP netip.Addr
	IPPrefix string
	// InvalidToken 为真表示请求带了令牌但它无效。上层据此区分"访客"与
	// "会话已失效"：前者对访客开放的接口照常处理，后者应返回 401 提示重新登录。
	InvalidToken bool
}

// IsGuest 表示本次请求以访客身份处理。
func (p Principal) IsGuest() bool { return p.Actor == store.ActorGuest }

// UserID 返回用户 id，访客为 0。
func (p Principal) UserID() int64 {
	if p.IsGuest() {
		return 0
	}
	return p.User.ID
}

// Permissions 返回该身份的有效权限掩码。
func (p Principal) Permissions() int64 { return p.Group.Permissions }

// Can 判断是否具备某个权限。
func (p Principal) Can(bit perm.Bit) bool { return perm.Has(p.Group.Permissions, bit) }

// UnlimitedQuota 表示该身份不受配额限制（但仍会记账）。
func (p Principal) UnlimitedQuota() bool { return perm.Has(p.Group.Permissions, perm.BypassQuota) }

// GroupName 返回所属组名。
func (p Principal) GroupName() string { return p.Group.Name }

// ActorKey 返回用于配额计数与流量归属的键前缀。
func (p Principal) ActorKey() string {
	if p.IsGuest() {
		return store.GuestCounterKey(p.IPPrefix, "")
	}
	return store.UserCounterKey(p.User.ID, "")
}

// Service 提供登录、会话解析与访客身份构造。
//
// 它持有配置存储而不是一份配置副本：账号上限、会话时长、退避时长都可能在
// 运行期被管理员改动，缓存一份副本会让"改了没生效"变成常态。
type Service struct {
	db       *store.DB
	settings *settings.Store
	throttle *Throttler

	cacheMu sync.RWMutex
	cache   map[string]cacheEntry
	touched map[string]int64

	guestMu    sync.RWMutex
	guestGroup store.Group
	guestAt    time.Time
}

type cacheEntry struct {
	principal Principal
	expiresAt time.Time
}

// NewService 构造鉴权服务。
//
// master secret（短凭据 HMAC 密钥）缺失不在这里报错：只有邀请码相关功能需要
// 它，缺了不影响登录与访客流程；而在这里失败会让服务完全起不来。
func NewService(db *store.DB, st *settings.Store) *Service {
	return &Service{
		db:       db,
		settings: st,
		throttle: NewThrottler(db, st),
		cache:    map[string]cacheEntry{},
		touched:  map[string]int64{},
	}
}

// Throttler 返回退避器，供上层在分享、取件码等场景复用同一套维度策略。
func (s *Service) Throttler() *Throttler { return s.throttle }

// IPPrefix 把一个地址折算成用于绑定与计量的前缀。
func (s *Service) IPPrefix(ctx context.Context, addr netip.Addr) string {
	rt := s.settings.Runtime(ctx).Delivery
	return clientip.Prefix(addr, rt.IPPrefixV4, rt.IPPrefixV6)
}

// Pepper 返回短凭据的 HMAC 密钥。
//
// 它从主密钥派生（见 settings 的 pepperFromKey），每次都从配置快照取而不是
// 缓存：管理员可能刚初始化完就来创建邀请码，缓存会让"密钥就绪了但还在用
// 旧快照"这类错配延迟暴露。
func (s *Service) Pepper(ctx context.Context) ([]byte, error) {
	pepper := s.settings.Runtime(ctx).Crypto.Pepper
	if len(pepper) == 0 {
		return nil, fmt.Errorf("auth: 主密钥未就绪，无法派生短凭据密钥")
	}
	return pepper, nil
}

// LoginRequest 是一次登录请求。
type LoginRequest struct {
	Account   string
	Password  string
	ClientIP  netip.Addr
	UserAgent string
}

// LoginResult 是登录结果。
type LoginResult struct {
	Token     string
	Principal Principal
}

// Login 校验凭据并建立会话。
//
// 失败路径始终返回同一个 ErrInvalidCredentials，并在账号与 IP 两个维度上
// 同时记账；账号不存在时也执行一次等价开销的哈希，抹平时间差。
func (s *Service) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	account := strings.TrimSpace(req.Account)
	ip := req.ClientIP.String()
	rt := s.settings.Runtime(ctx)
	authRT := rt.Auth

	// 退避检查在读取账号之前：撞库流量应当在最便宜的环节被挡掉。
	if err := s.throttle.CheckAll(ctx,
		[2]string{string(ThrottleAccount), account},
		[2]string{string(ThrottleIP), ip},
	); err != nil {
		return LoginResult{}, err
	}

	user, err := store.GetUserByAccount(ctx, s.db.R(), account)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			VerifyDummy(req.Password)
			if throttleErr := s.throttle.FailAll(ctx,
				[2]string{string(ThrottleAccount), account},
				[2]string{string(ThrottleIP), ip},
			); throttleErr != nil {
				return LoginResult{}, fmt.Errorf("%w: 记录登录失败状态: %v", ErrThrottleUnavailable, throttleErr)
			}
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, err
	}

	ok, err := VerifyPassword(user.PasswordHash, req.Password)
	if err != nil || !ok {
		// 失败计数必须在写事务里重新读取。登录请求会并发到达，直接使用
		// 上面读池里的快照再写回会让两个失败请求互相覆盖，退避次数因此
		// 被低估。写事务把“读取最新值 + 计算 + 写回”串成一个原子操作。
		if throttleErr := s.throttle.FailAll(ctx,
			[2]string{string(ThrottleAccount), account},
			[2]string{string(ThrottleIP), ip},
		); throttleErr != nil {
			return LoginResult{}, fmt.Errorf("%w: 记录登录失败状态: %v", ErrThrottleUnavailable, throttleErr)
		}
		return LoginResult{}, ErrInvalidCredentials
	}

	if user.Status != store.UserEnabled {
		// 账号被禁用是明确信息，不隐藏：用户需要知道去找管理员，而不是
		// 反复尝试一个"密码错误"。
		return LoginResult{}, ErrAccountDisabled
	}

	group, err := store.GetGroup(ctx, s.db.R(), user.GroupName)
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: 读取用户组失败: %w", err)
	}

	// 参数升级：登录成功是唯一能拿到明文口令的时机。
	if NeedsRehash(user.PasswordHash) {
		if rehashed, err := HashPassword(req.Password); err == nil {
			if updateErr := store.UpdateUserPassword(ctx, s.db.W(), user.ID, rehashed); updateErr != nil {
				log.Printf("auth: 登录成功但升级账号 %d 的口令哈希失败: %v", user.ID, updateErr)
			} else {
				user.PasswordHash = rehashed
			}
		}
	}

	// 只清账号维度。IP 维度的存在意义正是"防同一来源轮换账号"——在它上面
	// 做成功即清零，等于给攻击者一个免费的复位按钮：用一个自备账号登录成功
	// 就能把该 IP 累积的失败计数清零，重新拿回完整的爆破预算。共享出口
	// （公司 NAT、校园网）下，任意一个用户登录成功还会顺手替所有人清空退避。
	if err := s.throttle.Clear(ctx, ThrottleAccount, account); err != nil {
		return LoginResult{}, fmt.Errorf("%w: 清除账号限流状态失败", ErrThrottleUnavailable)
	}

	token, err := NewSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	now := time.Now().UTC()
	ttl := authRT.SessionTTL
	if ttl <= 0 {
		return LoginResult{}, fmt.Errorf("auth: 会话有效期配置无效")
	}
	session := store.Session{
		TokenHash:  HashSessionToken(token),
		UserID:     user.ID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(ttl),
		ClientIP:   ip,
		UserAgent:  truncate(req.UserAgent, 256),
	}
	if err := store.CreateSession(ctx, s.db.W(), session); err != nil {
		return LoginResult{}, err
	}
	if err := store.TouchUserLastAction(ctx, s.db.W(), user.ID, now.Unix()); err != nil {
		log.Printf("auth: 登录成功但更新账号 %d 最近动作失败: %v", user.ID, err)
	}

	principal := Principal{
		Actor:    store.ActorUser,
		User:     user,
		Group:    group,
		Session:  session,
		ClientIP: req.ClientIP,
		IPPrefix: s.IPPrefix(ctx, req.ClientIP),
	}
	s.storeCache(session.TokenHash, principal)
	return LoginResult{Token: token, Principal: principal}, nil
}

// Resolve 解析请求身份。
//
// 令牌缺失直接按访客处理（这是"没有密钥视为访客"的落点）；令牌存在但无效时
// 同样返回访客身份，但会把 InvalidToken 置位，让上层能区分"本来就该当访客"
// 与"登录已过期"。
func (s *Service) Resolve(ctx context.Context, token string, ip netip.Addr, userAgent string) (Principal, error) {
	token = strings.TrimSpace(token)
	ipPrefix := s.IPPrefix(ctx, ip)
	if token == "" {
		return s.guestPrincipal(ctx, ip, ipPrefix)
	}

	tokenHash := HashSessionToken(token)
	if cached, ok := s.loadCache(tokenHash); ok {
		cached.ClientIP = ip
		cached.IPPrefix = ipPrefix
		s.maybeTouch(ctx, tokenHash)
		return cached, nil
	}

	// 令牌失效、会话过期、用户被删或禁用这几种情况对外是同一件事：按访客继续，
	// 但置位 InvalidToken，让需要登录的接口返回 401 而不是静默降级。
	invalidToken := func() (Principal, error) {
		guest, err := s.guestPrincipal(ctx, ip, ipPrefix)
		guest.InvalidToken = true
		return guest, err
	}

	session, err := store.GetSession(ctx, s.db.R(), tokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return invalidToken()
		}
		return Principal{}, err
	}
	if session.Expired(time.Now().UTC()) {
		return invalidToken()
	}

	user, err := store.GetUserByID(ctx, s.db.R(), session.UserID)
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

	principal := Principal{
		Actor:    store.ActorUser,
		User:     user,
		Group:    group,
		Session:  session,
		ClientIP: ip,
		IPPrefix: ipPrefix,
	}
	s.storeCache(tokenHash, principal)
	s.maybeTouch(ctx, tokenHash)
	return principal, nil
}

// RequireUser 在身份不是登录用户时返回 ErrNotAuthenticated。
func RequireUser(p Principal) error {
	if p.IsGuest() {
		if p.InvalidToken {
			return ErrSessionInvalid
		}
		return ErrNotAuthenticated
	}
	return nil
}

// RequirePermission 校验权限位。
func RequirePermission(p Principal, bit perm.Bit) error {
	if err := RequireUser(p); err != nil {
		return err
	}
	if !p.Can(bit) {
		return fmt.Errorf("%w: 缺少权限 %d", ErrForbidden, bit)
	}
	return nil
}

// Logout 吊销当前会话。
func (s *Service) Logout(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	tokenHash := HashSessionToken(token)
	if err := store.RevokeSession(ctx, s.db.W(), tokenHash); err != nil {
		return err
	}
	s.dropCache(tokenHash)
	return nil
}

// RevokeUserSessions 吊销某账号的全部会话，并清掉对应缓存。
func (s *Service) RevokeUserSessions(ctx context.Context, userID int64) error {
	if err := store.RevokeUserSessions(ctx, s.db.W(), userID); err != nil {
		return err
	}
	s.InvalidateUserCache(userID)
	return nil
}

// InvalidateUserCache 丢弃指定用户的鉴权缓存。数据库状态变更若已在调用方
// 事务中完成，应在提交后调用它，避免缓存继续提供旧权限。
func (s *Service) InvalidateUserCache(userID int64) {
	s.cacheMu.Lock()
	for hash, entry := range s.cache {
		if entry.principal.User.ID == userID {
			delete(s.cache, hash)
		}
	}
	for hash := range s.touched {
		if _, ok := s.cache[hash]; !ok {
			delete(s.touched, hash)
		}
	}
	s.cacheMu.Unlock()
}

// InvalidateGroupCache 丢弃指定用户组的鉴权缓存。
// 会话已由数据库层吊销，但缓存仍可能暂存旧 Principal；两者都清除才能让
// 权限回收在下一次请求立即生效。
func (s *Service) InvalidateGroupCache(groupName string) {
	s.cacheMu.Lock()
	for hash, entry := range s.cache {
		if entry.principal.User.GroupName == groupName || entry.principal.Group.Name == groupName {
			delete(s.cache, hash)
			delete(s.touched, hash)
		}
	}
	s.cacheMu.Unlock()
}

// guestPrincipal 构造访客身份。
func (s *Service) guestPrincipal(ctx context.Context, ip netip.Addr, ipPrefix string) (Principal, error) {
	group, err := s.guestGroupOf(ctx)
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		Actor:    store.ActorGuest,
		Group:    group,
		ClientIP: ip,
		IPPrefix: ipPrefix,
	}, nil
}

// guestGroupOf 取访客组，带短缓存。
//
// 访客组是每次匿名请求都要用到的配置，直接查库会让纯静态资源请求也产生
// 一次数据库读取。
func (s *Service) guestGroupOf(ctx context.Context) (store.Group, error) {
	s.guestMu.RLock()
	if !s.guestAt.IsZero() && time.Since(s.guestAt) < time.Minute {
		g := s.guestGroup
		s.guestMu.RUnlock()
		return g, nil
	}
	s.guestMu.RUnlock()

	group, err := store.GetGroup(ctx, s.db.R(), perm.GroupGuest)
	if err != nil {
		return store.Group{}, fmt.Errorf("auth: 读取访客组失败: %w", err)
	}
	s.guestMu.Lock()
	s.guestGroup = group
	s.guestAt = time.Now()
	s.guestMu.Unlock()
	return group, nil
}

// InvalidateGuestGroup 在访客组配置变更后清掉缓存。
func (s *Service) InvalidateGuestGroup() {
	s.guestMu.Lock()
	s.guestAt = time.Time{}
	s.guestMu.Unlock()
}

func (s *Service) storeCache(tokenHash string, p Principal) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if len(s.cache) >= principalCacheMaxSize {
		// 容量兜底：直接清空而不是做 LRU。缓存只是优化，清空的代价是
		// 几个请求多读一次库，比引入淘汰逻辑的复杂度更划算。
		s.cache = map[string]cacheEntry{}
	}
	s.cache[tokenHash] = cacheEntry{principal: p, expiresAt: time.Now().Add(sessionCacheTTL)}
}

func (s *Service) loadCache(tokenHash string) (Principal, bool) {
	s.cacheMu.RLock()
	entry, ok := s.cache[tokenHash]
	s.cacheMu.RUnlock()
	if !ok {
		return Principal{}, false
	}
	if time.Now().After(entry.expiresAt) {
		s.dropCache(tokenHash)
		return Principal{}, false
	}
	return entry.principal, true
}

func (s *Service) dropCache(tokenHash string) {
	s.cacheMu.Lock()
	delete(s.cache, tokenHash)
	s.cacheMu.Unlock()
}

// maybeTouch 按间隔把活跃时间写回数据库。
func (s *Service) maybeTouch(ctx context.Context, tokenHash string) {
	now := time.Now()
	if !s.touchDue(tokenHash, now) {
		return
	}
	session, err := store.GetSession(ctx, s.db.R(), tokenHash)
	if err != nil {
		return
	}
	if err := store.TouchSession(ctx, s.db.W(), tokenHash, now.UTC()); err != nil {
		return
	}
	// 账号行上的最近动作时间只在推进时写入。
	if err := store.TouchUserLastAction(ctx, s.db.W(), session.UserID, now.Unix()); err != nil {
		log.Printf("auth: 更新账号 %d 最近动作失败: %v", session.UserID, err)
	}
}

// touchDue 判断该会话是否到了写回间隔，到期时就地登记时间戳。
//
// 登记必须在返回前完成：否则并发的同一会话请求会同时判定为"到期"，
// 把一次写放大成多次，正好抵消这个节流的目的。
func (s *Service) touchDue(tokenHash string, now time.Time) bool {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if last, ok := s.touched[tokenHash]; ok && now.Sub(time.Unix(last, 0)) < sessionTouchInterval {
		return false
	}
	s.touched[tokenHash] = now.Unix()
	if len(s.touched) > principalCacheMaxSize {
		s.touched = map[string]int64{tokenHash: now.Unix()}
	}
	return true
}

// truncate 按 Unicode 码点截断，避免把多字节 User-Agent 截在半个 UTF-8
// 序列上。User-Agent 只用于诊断，截断失败也不应影响登录。
func truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
