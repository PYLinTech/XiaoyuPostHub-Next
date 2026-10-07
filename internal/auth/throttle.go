package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// ErrThrottleUnavailable 表示限流状态无法读取。限流是凭据入口的安全边界，
// 数据库异常时不能静默按“不限流”继续处理。
var ErrThrottleUnavailable = errors.New("auth: 限流状态不可用")

// ThrottleKind 是退避的维度。
//
// 两个维度缺一不可：只按账号退避，"同一 IP 刷大量账号"不受限；只按 IP 退避，
// "分布式撞库"不受限。
type ThrottleKind string

const (
	// ThrottleAccount 按账号退避，防针对某一账号的持续尝试。
	ThrottleAccount ThrottleKind = "account"
	// ThrottleIP 按 IP 退避，防同一来源轮换账号。
	ThrottleIP ThrottleKind = "ip"
	// ThrottleShare 按分享退避，防提取码爆破（码很短，必须限流）。
	ThrottleShare ThrottleKind = "share"
	// ThrottlePickup 按取件码退避，同上。
	ThrottlePickup ThrottleKind = "pickup"
)

// ThrottledError 表示当前处于退避期。调用方应把它映射成带 Retry-After
// 的响应，而不是暴露内部的计数细节。
type ThrottledError struct {
	Kind       ThrottleKind
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("auth: %s 维度处于退避期，还需等待 %s", e.Kind, e.RetryAfter.Round(time.Second))
}

// Throttler 负责失败退避的读写。
//
// 退避时长从配置读取而不是构造时固定：管理员调紧限流后应当立刻生效，
// 而不是重启才生效——被封禁攻击时，等重启的每一分钟都是成本。
type Throttler struct {
	db       *store.DB
	settings *settings.Store
}

// NewThrottler 构造退避器。
func NewThrottler(db *store.DB, st *settings.Store) *Throttler {
	return &Throttler{db: db, settings: st}
}

// baseDelay 返回某个维度当前的基础等待时长。
//
// 返回 0 表示**该维度不限流**：这是管理员的显式选择（例如内网部署），
// 与"没配置"必须区分开，后者应当走默认值而不是变成不限制。
func (t *Throttler) baseDelay(ctx context.Context, kind ThrottleKind) time.Duration {
	auth := t.settings.Runtime(ctx).Auth
	switch kind {
	case ThrottleIP:
		return auth.IPFailDelay
	case ThrottleShare, ThrottlePickup:
		// 提取码与取件码比口令短得多，爆破成本天然更低，因此退避要更陡。
		return auth.AccountFailDelay * 2
	default:
		return auth.AccountFailDelay
	}
}

// Key 构造退避键。键必须包含维度前缀，否则不同维度的同名值会互相污染。
func (t *Throttler) Key(kind ThrottleKind, value string) string {
	return string(kind) + ":" + strings.ToLower(strings.TrimSpace(value))
}

// Check 在退避期内返回 *ThrottledError。
//
// 读取失败时拒绝继续处理：退避是凭据入口的安全边界，故障时不能静默放开。
func (t *Throttler) Check(ctx context.Context, kind ThrottleKind, value string) error {
	if value == "" {
		return nil
	}
	state, err := store.GetThrottle(ctx, t.db.R(), t.Key(kind, value))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrThrottleUnavailable, err)
	}
	now := store.Now()
	if state.DelayUntil > now {
		return &ThrottledError{Kind: kind, RetryAfter: time.Duration(state.DelayUntil-now) * time.Second}
	}
	return nil
}

// CheckAll 逐个检查多个维度，任一处于退避期即返回错误。
func (t *Throttler) CheckAll(ctx context.Context, pairs ...[2]string) error {
	for _, p := range pairs {
		if err := t.Check(ctx, ThrottleKind(p[0]), p[1]); err != nil {
			return err
		}
	}
	return nil
}

// Fail 记一次失败并推进解禁时间，返回本次的等待时长。
//
// 基础时长配成 0 时直接返回，不写退避状态：这是"关闭限流"的显式语义。
func (t *Throttler) Fail(ctx context.Context, kind ThrottleKind, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	base := t.baseDelay(ctx, kind)
	if base <= 0 {
		return 0, nil
	}
	max := t.settings.Runtime(ctx).Auth.MaxFailDelay
	// 失败计数与解禁时间必须在同一个写事务里推进。否则并发失败会出现
	// “计数已加两次、较早请求却把较短的解禁时间写回去”的竞态，实际退避
	// 会低于配置值。
	state, err := store.BumpThrottleAtomic(ctx, t.db, t.Key(kind, value), base, max)
	if err != nil {
		return 0, err
	}
	remaining := time.Duration(state.DelayUntil-store.Now()) * time.Second
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

// FailAll 在多个维度上同时记一次失败。任一维度写入失败都返回错误；
// 调用方不能把限流状态不可用当成“关闭限流”继续处理。
func (t *Throttler) FailAll(ctx context.Context, pairs ...[2]string) error {
	var joined error
	for _, p := range pairs {
		if _, err := t.Fail(ctx, ThrottleKind(p[0]), p[1]); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

// Clear 清除某个维度的退避状态。
func (t *Throttler) Clear(ctx context.Context, kind ThrottleKind, value string) error {
	if value == "" {
		return nil
	}
	return store.ClearThrottle(ctx, t.db.W(), t.Key(kind, value))
}

// IsThrottled 判断错误是否是退避错误，便于 HTTP 层映射状态码。
func IsThrottled(err error) bool {
	var target *ThrottledError
	return errors.As(err, &target)
}

// RetryAfter 从错误中取出建议的等待时长。
func RetryAfter(err error) time.Duration {
	var target *ThrottledError
	if errors.As(err, &target) {
		return target.RetryAfter
	}
	return 0
}
