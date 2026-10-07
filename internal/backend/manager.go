package backend

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Pan123Config 是 123 云盘后端的构造参数。
//
// 刻意不直接依赖 settings 包：配置从哪来、怎么加密存储都是上层的事，这层隔离
// 让后端可以被单独测试，也避免了 settings ↔ backend 的循环依赖。
type Pan123Config struct {
	ClientID     string
	ClientSecret string
	APIBase      string
	RootDirID    string
	PrivateKey   string
	// DirectLinkTTL 是签发给客户端的直链有效期。
	DirectLinkTTL time.Duration
	// UploadThreads 是单次上传的并发分片数。
	UploadThreads int
	// QPS 是各接口的调用频率上限；0 表示不限流。
	QPS int
	// PollInterval / PollAttempts 控制上传完成的轮询。
	PollInterval time.Duration
	PollAttempts int
}

// Status 描述当前后端状态，供管理端展示。
type Status struct {
	// Kind 是后端类型标识；未配置时为空。
	Kind string `json:"kind"`
	// Ready 表示凭据齐备、可以执行读写。
	Ready bool `json:"ready"`
	// Detail 说明不可用的原因或当前的关键参数。
	Detail string `json:"detail"`
	// RootDirID 是对象存放的根目录。
	RootDirID string `json:"rootDirId,omitempty"`
	// UpdatedAt 是最近一次重新配置的时间（Unix 秒）。
	UpdatedAt int64 `json:"updatedAt,omitempty"`
}

// Manager 持有一个可被替换的存储后端，并自身实现 Backend。
//
// 为什么要可变：配置在数据库里，而数据库需要先启动服务才能通过界面修改。
// 若后端起不来就让进程退出，管理员就永远没有入口去填凭据——这是一个死锁。
// 因此启动时允许"未配置"，由管理员在界面上补齐后热替换。
//
// 它同时实现 Backend（调用全部转发给当前实例），业务层因此不需要感知
// "后端可能被换掉"。
type Manager struct {
	mu      sync.RWMutex
	current Backend
	status  Status
	build   func(Pan123Config) (Backend, error)
}

// NewManager 构造管理器。build 用于构造具体后端，便于测试替换。
func NewManager() *Manager {
	m := &Manager{build: func(cfg Pan123Config) (Backend, error) { return NewPan123(cfg) }}
	m.current = unconfigured{}
	m.status = Status{Ready: false, Detail: "尚未配置存储后端"}
	return m
}

func (m *Manager) Current() Backend {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status.Ready
}

// Reconfigure 用新配置替换当前后端。
//
// 只有构造成功才替换：否则改错一个字段就会让整个站点的上传下载全部不可用，
// 而管理员只是想试一下别的值。
func (m *Manager) Reconfigure(cfg Pan123Config) error {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return fmt.Errorf("backend: ClientID 与 ClientSecret 均不得为空")
	}
	next, err := m.build(cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.current = next
	m.status = Status{
		Kind:      next.Kind(),
		Ready:     true,
		RootDirID: cfg.RootDirID,
		UpdatedAt: time.Now().Unix(),
		Detail:    "已配置",
	}
	m.mu.Unlock()
	return nil
}

// Unconfigure 停用当前后端并清除内存中的凭据。
//
// 清空设置必须真正撤销运行中的后端；如果只是保留旧实例，管理端会显示
// “未配置”而数据面仍可继续读写，既违反配置语义，也会让已删除的密钥继续有效。
func (m *Manager) Unconfigure() {
	m.mu.Lock()
	m.current = unconfigured{}
	m.status = Status{Ready: false, Detail: "尚未配置存储后端"}
	m.mu.Unlock()
}

// ---------------------------------------------------------------- Backend 实现

func (m *Manager) Kind() string { return m.Current().Kind() }

func (m *Manager) Put(ctx context.Context, req PutRequest) (PutResult, error) {
	return m.Current().Put(ctx, req)
}

func (m *Manager) Open(ctx context.Context, ref string) (io.ReadSeekCloser, error) {
	return m.Current().Open(ctx, ref)
}

func (m *Manager) Delete(ctx context.Context, ref string) error {
	return m.Current().Delete(ctx, ref)
}

func (m *Manager) Stat(ctx context.Context, ref string) (int64, error) {
	return m.Current().Stat(ctx, ref)
}

func (m *Manager) Presign(ctx context.Context, ref string, opt PresignOptions) (string, error) {
	return m.Current().Presign(ctx, ref, opt)
}

func (m *Manager) PresignReady() bool { return m.Current().PresignReady() }

func (m *Manager) SliceMD5Enabled() bool { return m.Current().SliceMD5Enabled() }

// Health 把自检转发给当前后端；后端未实现 HealthChecker 时返回 ErrNotSupported。
func (m *Manager) Health(ctx context.Context) (map[string]any, error) {
	checker, ok := m.Current().(HealthChecker)
	if !ok {
		return nil, ErrNotSupported
	}
	return checker.Health(ctx)
}

// SetDirectLink 把直链空间开关请求转发给当前后端；后端不支持（含"尚未配置"
// 占位实现）时返回 ErrNotSupported。生产装配中 Service.Backend 持有的就是
// *Manager，删除这个转发会让上层 DirectLinkSwitcher 断言失败、配置保存报 501，
// 因此它不是死代码。
func (m *Manager) SetDirectLink(ctx context.Context, enabled bool) (string, error) {
	switcher, ok := m.Current().(DirectLinkSwitcher)
	if !ok {
		return "", ErrNotSupported
	}
	return switcher.SetDirectLink(ctx, enabled)
}

// unconfigured 是"尚未配置存储后端"时的占位实现。
//
// 它把每个调用都变成一条明确的错误，而不是 panic 或静默失败：管理员在界面上
// 看到的应该是"存储后端尚未配置"，而不是 500。
type unconfigured struct{}

func (unconfigured) Kind() string { return "" }

func (unconfigured) Put(context.Context, PutRequest) (PutResult, error) {
	return PutResult{}, fmt.Errorf("%w: 存储后端尚未配置", ErrNotSupported)
}

func (unconfigured) Open(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, fmt.Errorf("%w: 存储后端尚未配置", ErrNotSupported)
}

func (unconfigured) Delete(context.Context, string) error {
	return fmt.Errorf("%w: 存储后端尚未配置", ErrNotSupported)
}

func (unconfigured) Stat(context.Context, string) (int64, error) {
	return 0, fmt.Errorf("%w: 存储后端尚未配置", ErrNotSupported)
}

func (unconfigured) Presign(context.Context, string, PresignOptions) (string, error) {
	return "", fmt.Errorf("%w: 存储后端尚未配置", ErrNotSupported)
}

func (unconfigured) PresignReady() bool { return false }

func (unconfigured) SliceMD5Enabled() bool { return false }

// 编译期确认：Manager 就是一个 Backend。
var _ Backend = (*Manager)(nil)
