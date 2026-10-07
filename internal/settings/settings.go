// Package settings 是"运行期可改配置"的唯一来源。
//
// 除监听端口与数据库路径这类启动前就必须知道的参数外，所有配置都存放在
// SQLite 里，由本包统一读取、校验、加密与热生效。
//
// 三条设计约定：
//
//  1. **默认值内置在代码里**，数据库表只存"覆盖项"。表为空或字段缺失时
//     使用默认值；数据库读取失败只保留最近一次完整快照并立即重试。
//  2. **敏感值加密后入库**：数据库文件会被复制、同步、被开发机拉走，原样
//     存储的密钥等于随文件一起泄露。
//  3. **读快照而不是逐项查找**：逐项查缓存会把锁竞争放大，也让"同一次操作
//     读到半新半旧的配置"成为可能。
package settings

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/secretbox"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// cacheTTL 是配置快照的存活时间。取 5 秒而不是更长：改完配置后管理员下一步就是
// 验证效果，等太久会让人以为没生效。写入路径会主动失效缓存，因此这个 TTL 实际
// 只兜住"多实例部署时另一实例的改动"。
const cacheTTL = 5 * time.Second

var (
	ErrUnknownKey   = errors.New("settings: 未知的配置项")
	ErrInvalidValue = errors.New("settings: 配置取值不合法")
)

// Store 是配置存储与缓存。
type Store struct {
	db  *store.DB
	box *secretbox.Box

	mu       sync.RWMutex
	raw      map[Key]string
	runtime  Runtime
	loadedAt time.Time

	hookMu sync.RWMutex
	hooks  []func(ctx context.Context, changed []Key)
}

// NewStore 构造配置存储。box 为 nil 时敏感配置不可写，避免意外落库明文。
func NewStore(db *store.DB, box *secretbox.Box) *Store {
	s := &Store{db: db, box: box, raw: map[Key]string{}}
	s.runtime = buildRuntime(Defaults())
	if box == nil {
		log.Printf("警告: 未配置 master secret，敏感配置不可用")
	}
	return s
}

// OnChange 注册配置变更回调。
//
// 回调同步执行：管理员改完配置后的下一步就是验证效果，异步执行会让"改完立刻
// 测试"读到旧配置，表现为随机的困惑。回调里不要做慢操作。
func (s *Store) OnChange(fn func(ctx context.Context, changed []Key)) {
	s.hookMu.Lock()
	s.hooks = append(s.hooks, fn)
	s.hookMu.Unlock()
}

// Invalidate 让缓存立即失效。
func (s *Store) Invalidate() {
	s.mu.Lock()
	s.loadedAt = time.Time{}
	s.mu.Unlock()
}

// Runtime 返回当前配置快照。
func (s *Store) Runtime(ctx context.Context) Runtime {
	s.refresh(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runtime
}

// Get 返回某项配置的当前生效值。
func (s *Store) Get(ctx context.Context, key Key) string {
	return s.Runtime(ctx).Raw[key]
}

// Overridden 判断某项配置是否被数据库覆盖（false 表示仍是内置默认值）。
func (s *Store) Overridden(ctx context.Context, key Key) bool {
	overlays := s.overlayValues(ctx)
	_, ok := overlays[key]
	return ok
}

// Set 写入一项配置。
func (s *Store) Set(ctx context.Context, key Key, value string, by int64) error {
	return s.SetMany(ctx, map[Key]string{key: value}, by)
}

// SealSecret 用站点主密钥加密一段业务敏感文本（M4：mail_domains 的
// SMTP 密码加密列）。aad 是绑定的附加认证数据，打开时必须原样传入；
// plain 为空时透传空串，便于"未配置"与"已配置"共用一列。
func (s *Store) SealSecret(plain, aad string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if s.box == nil {
		return "", fmt.Errorf("加密业务密钥需要 master secret")
	}
	return s.box.Seal(plain, aad)
}

// OpenSecret 是 SealSecret 的逆操作；sealed 为空时透传空串。
func (s *Store) OpenSecret(sealed, aad string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if s.box == nil {
		return "", fmt.Errorf("解密业务密钥需要 master secret")
	}
	return s.box.Open(sealed, aad)
}

// SetMany 原子写入多项配置。
//
// 先全部校验，再在同一个写事务里落库：一半写入成功、一半失败会让系统停在
// 一个既不是旧配置也不是新配置的状态，比整体失败更难恢复。
func (s *Store) SetMany(ctx context.Context, values map[Key]string, by int64) error {
	if len(values) == 0 {
		return nil
	}
	type pending struct {
		key    Key
		desc   Descriptor
		stored string
	}
	prepared := make([]pending, 0, len(values))
	for key, raw := range values {
		desc, ok := Lookup(key)
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownKey, key)
		}
		if err := desc.Validate(raw); err != nil {
			return fmt.Errorf("%w: 配置项 %s 取值不合法: %v", ErrInvalidValue, desc.Title, err)
		}
		normalized := desc.Normalize(raw)
		// 与内置默认相同的值不落库：覆盖行的存在与否是管理端"已自定义"徽标的
		// 判据，若把默认值原样写成覆盖，走完初始化向导的站点会满屏"已自定义"。
		// 此时删除覆盖，效果与重置等价（见下方事务里的空值分支）。
		stored := ""
		if normalized != desc.Default {
			stored = normalized
			if desc.Secret() {
				if s.box == nil {
					return fmt.Errorf("配置项 %s 需要 master secret", desc.Title)
				}
				sealed, err := s.box.Seal(normalized, string(key))
				if err != nil {
					return fmt.Errorf("加密配置项 %s 失败: %w", desc.Title, err)
				}
				stored = sealed
			}
		}
		prepared = append(prepared, pending{key: key, desc: desc, stored: stored})
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].key < prepared[j].key })

	// stored 为空表示"清除覆盖，回到默认"，而不是写入空串：有些配置项的合法
	// 默认值就是空（例如 ClientID 未配置）。配置写入与删除必须共享同一个事务，
	// 否则第二项失败时第一项已经对请求生效。
	if err := s.db.InTx(ctx, func(tx store.Querier) error {
		for _, p := range prepared {
			if p.stored == "" {
				if err := store.DeleteConfig(ctx, tx, string(p.key)); err != nil {
					return err
				}
				continue
			}
			if err := store.SetConfig(ctx, tx, string(p.key), p.stored, string(p.desc.Kind), by); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	changed := make([]Key, 0, len(prepared))
	for _, p := range prepared {
		changed = append(changed, p.key)
	}
	s.Invalidate()
	s.notify(ctx, changed)
	return nil
}

// Reset 删除一项覆盖，使其回到内置默认值。
func (s *Store) Reset(ctx context.Context, key Key) error {
	if desc, ok := Lookup(key); ok && desc.Internal {
		return fmt.Errorf("%w: %s 是内部配置项，不能重置", ErrInvalidValue, key)
	}
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	if err := store.DeleteConfig(ctx, s.db.W(), string(key)); err != nil {
		return err
	}
	s.Invalidate()
	s.notify(ctx, []Key{key})
	return nil
}

// Entry 是面向前端的配置项视图。
type Entry struct {
	Key         Key      `json:"key"`
	Section     string   `json:"section"`
	Title       string   `json:"title"`
	Help        string   `json:"help"`
	Kind        Kind     `json:"kind"`
	Scope       Scope    `json:"scope"`
	Enum        []string `json:"enum,omitempty"`
	Min         int64    `json:"min,omitempty"`
	Max         int64    `json:"max,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Warn        string   `json:"warn,omitempty"`
	Secret      bool     `json:"secret"`
	// Value 是展示值。敏感项为掩码；未设置时为空串。
	Value string `json:"value"`
	// HasValue 区分"已设置"与"未设置"——掩码后的空串无法表达这个区别，
	// 而管理员需要知道某个密钥到底配没配。
	HasValue bool `json:"hasValue"`
	// Overridden 为真表示当前生效值与内置默认值不同。以"值是否不同"而不是
	// "覆盖行是否存在"为准：早先版本会把与默认相同的值也落成覆盖行，
	// 按"行存在"判据会让满屏默认值挂着"已自定义"徽标。
	Overridden bool `json:"overridden"`
	// DefaultValue 是内置默认值；敏感项不返回。
	DefaultValue string `json:"defaultValue,omitempty"`
}

// Entries 返回全部配置项及其当前值，供管理端展示。
//
// 内部项（主密钥等）不出现在返回值里：它们不在界面上编辑，
// 列出它们只会诱导管理员尝试修改一条"改了就不可逆"的配置。
func (s *Store) Entries(ctx context.Context) []Entry {
	rt := s.Runtime(ctx)
	overlays := s.overlayValues(ctx)
	out := make([]Entry, 0, len(registry))
	for _, d := range registry {
		if d.Internal {
			continue
		}
		value := rt.Raw[d.Key]
		entry := Entry{
			Key: d.Key, Section: d.Section, Title: d.Title, Help: d.Help,
			Kind: d.Kind, Scope: d.Scope, Enum: d.Enum, Min: d.Min, Max: d.Max,
			Placeholder: d.Placeholder, Warn: d.Warn, Secret: d.Secret(),
			HasValue: value != "",
		}
		if v, ok := overlays[d.Key]; ok && v != d.Default {
			entry.Overridden = true
		}
		if d.Secret() {
			entry.Value = secretbox.Mask(value)
		} else {
			entry.Value = value
			entry.DefaultValue = d.Default
		}
		out = append(out, entry)
	}
	return out
}

// 导出永不包含敏感项：导出件通常会被贴进工单、放进仓库或发给同事，把密钥一起
// 带出去是最常见的泄露路径。敏感项的转移只走"在引导页重新录入"一条路。
func (s *Store) Export(ctx context.Context) (map[string]string, error) {
	rt := s.Runtime(ctx)
	out := map[string]string{}
	for _, d := range registry {
		if d.Secret() {
			continue
		}
		out[string(d.Key)] = rt.Raw[d.Key]
	}
	return out, nil
}

// Import 导入配置，返回实际写入的项数。
//
// 未知键直接拒绝，导入只处理当前版本明确支持的配置。
// 内部项同样拒绝：主密钥这类只在初始化写入的值，不应能从一份导入件混进来。
func (s *Store) Import(ctx context.Context, values map[string]string, by int64) (int, error) {
	batch := map[Key]string{}
	for rawKey, value := range values {
		key := Key(rawKey)
		desc, ok := Lookup(key)
		if !ok {
			return 0, fmt.Errorf("%w: %s", ErrUnknownKey, key)
		}
		if desc.Internal {
			return 0, fmt.Errorf("%w: %s 是内部配置项，不能导入", ErrInvalidValue, key)
		}
		batch[key] = value
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if err := s.SetMany(ctx, batch, by); err != nil {
		return 0, err
	}
	return len(batch), nil
}

// overlayValues 返回数据库中实际存在的覆盖项（明文，未解密失败时跳过）。
func (s *Store) overlayValues(ctx context.Context) map[Key]string {
	s.refresh(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[Key]string, len(s.raw))
	for k, v := range s.raw {
		out[k] = v
	}
	return out
}

// refresh 按 TTL 重新加载配置。
func (s *Store) refresh(ctx context.Context) {
	s.mu.RLock()
	fresh := !s.loadedAt.IsZero() && time.Since(s.loadedAt) < cacheTTL
	s.mu.RUnlock()
	if fresh {
		return
	}

	entries, err := store.ListConfig(ctx, s.db.R())
	if err != nil {
		// 保留最近一次完整快照，但不推进 loadedAt；下一次请求会立即重试，
		// 不把一次数据库故障伪装成五秒有效的“新配置”。
		log.Printf("读取运行期配置失败，继续使用最近快照并立即重试: %v", err)
		return
	}

	raw := make(map[Key]string, len(entries))
	var unknown []string
	for _, e := range entries {
		key := Key(e.Key)
		desc, ok := Lookup(key)
		if !ok {
			// 数据库不应保存当前版本不存在的配置。不能把它当成正常覆盖项，
			// 也不能静默吞掉，否则管理员会误以为设置仍然生效。
			unknown = append(unknown, e.Key)
			continue
		}
		value := e.Value
		if desc.Secret() {
			if s.box == nil {
				log.Printf("配置项 %s 需要 master secret，当前按未设置处理", key)
				continue
			}
			plain, err := s.box.Open(value, string(key))
			if err != nil {
				// 解密失败通常意味着 master secret 换了：明确告警而不是静默当作未设置，
				// 否则会表现为"配置莫名消失"。
				log.Printf("配置项 %s 解密失败（master secret 可能已更换）: %v", key, err)
				continue
			}
			value = plain
		}
		raw[key] = value
	}
	if len(unknown) > 0 {
		log.Printf("配置库包含当前版本不支持的键（需手动清理）: %s", strings.Join(unknown, ", "))
	}

	s.mu.Lock()
	s.raw = raw
	s.runtime = buildRuntime(raw)
	s.loadedAt = time.Now()
	s.mu.Unlock()
}

func (s *Store) notify(ctx context.Context, changed []Key) {
	s.hookMu.RLock()
	hooks := append([]func(context.Context, []Key){}, s.hooks...)
	s.hookMu.RUnlock()
	for _, fn := range hooks {
		func() {
			defer func() {
				// 一个回调出错不应该影响其它回调，也不能让写入请求失败：
				// 配置已经落库了，此时报错只会让管理员以为没保存成功。
				if rec := recover(); rec != nil {
					log.Printf("配置变更回调 panic: %v", rec)
				}
			}()
			fn(ctx, changed)
		}()
	}
}
