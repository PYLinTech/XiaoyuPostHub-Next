// Package service 是业务编排层：把存储、鉴权、配额、加密与存储后端组合成
// 可被 HTTP 层直接调用的用例。
//
// 分层约定：
//   - store 只做持久化，不含业务判断；
//   - backend 只做对象的读写，不关心谁在用；
//   - settings 只提供"当前生效的配置快照"；
//   - 本包负责全部跨模块的一致性（引用计数、配额预扣与结算、票据生命周期）。
//
// 配置一律通过 settings.Store.Runtime(ctx) 读取快照而不缓存副本：管理员改完
// 配置后应当立刻对新请求生效。
package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/spf"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// MaxPageLimit 是所有管理端列表接口共享的分页上限。
//
// 单一权威：早先 pagingOf 允许 500、文件端点又压到 200、service 里还各自
// 写一遍。两处各写一个数，改了一处忘了另一处，就会出现"回显的分页值"与
// 实际页长不符——分页控件显示第 3 页，内容却只有第 2 页那么多。
//
// 取 200 是因为前端分页器的可选项最大就是 200；接口允许多也没人会用到。
const MaxPageLimit = 200

// DefaultPageLimit 是未显式指定分页大小时的默认值。
const DefaultPageLimit = 50

// clampPaging 归一化分页参数。回显值必须与实际取用值一致，否则分页控件
// 会算错总页数。
func clampPaging(limit, offset int) (int, int) {
	if limit <= 0 || limit > MaxPageLimit {
		limit = DefaultPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// 业务错误。HTTP 层据此映射状态码，不向用户暴露内部细节。
var (
	ErrBadRequest    = errors.New("service: 请求参数非法")
	ErrForbidden     = errors.New("service: 无权访问")
	ErrNotFound      = errors.New("service: 资源不存在")
	ErrConflict      = errors.New("service: 目标状态冲突")
	ErrTooLarge      = errors.New("service: 文件超出上限")
	ErrQuotaExceeded = errors.New("service: 配额不足")
	ErrNotSupported  = errors.New("service: 当前配置不支持该操作")
	ErrUnavailable   = errors.New("service: 依赖的外部能力暂不可用")
	// ErrStorageNotReady 表示存储后端还没配置好。
	//
	// 与 ErrUnavailable 分开，是因为它需要一句不同的用户提示：泛化的
	// "服务暂时不可用，请稍后重试"会诱导用户反复重试一件永远不会成功的事，
	// 而真正能解决它的是管理员去填凭据。它同时被用来**在分片上传开始之前**
	// 就拦住请求——否则用户要把整份文件传完，才在最后一步收到失败。
	ErrStorageNotReady = errors.New("service: 存储后端尚未就绪")
	// ErrBusy 表示同校验码的对象已被其他上传占位，调用方应稍后重试。
	ErrBusy = errors.New("service: 该文件正在上传中")
	// ErrUploadBackpressure 表示暂存窗口已满，客户端应退避后重传当前分片。
	ErrUploadBackpressure = errors.New("service: 上传暂存窗口繁忙")
	// ErrPickupPoolExhausted 表示取件码的全局码空间（28^6）已被有效期窗口内
	// 存活的取件码占满，新码无法生成。这是需要明确告知最终用户的容量约束，
	// 不能用主键碰撞无限重试掩盖。
	ErrPickupPoolExhausted = errors.New("service: 取件码码池已用尽")
)

// Deps 是业务层依赖。
//
// DataDir / TempDir / MasterSecretSource 来自自举配置而非数据库：它们要在
// 读配置之前就可用（建目录、放密钥文件），属于无法自举的那一类。
type Deps struct {
	DB       *store.DB
	Auth     *auth.Service
	Backend  backend.Backend
	Settings *settings.Store
	// DataDir 是持久化根目录，数据库备份落到它的 backup/ 子目录。
	DataDir string
	// TempDir 是上传暂存与密文缓存目录。
	TempDir string
	// MasterSecretSource 说明 master secret 的来源（env / file / generated）。
	MasterSecretSource string
}

// Service 是业务服务。
type Service struct {
	Deps

	// setupMu 串行化一次性初始化：既保护内存里的初始化令牌，也保证
	// "检查库里有没有账号 → 建首个管理员"这一整段不会被并发请求穿过。
	// 令牌只在进程内存里，重启即更换，不需要落盘与清理。
	setupMu    sync.Mutex
	setupToken string

	// spfResolver 为 nil 时入站 SPF 使用系统 DNS；测试注入假解析器。
	spfResolver spf.Resolver

	// maintenanceMu 保证同一时刻只有一轮清理在跑。
	//
	// 清理是"读工作列表 → 逐条处理"两段式，多数步骤靠 WHERE 条件天然幂等，
	// 但步骤⑥的引用释放不是（详见 transitionBatchToArchiveDeleted）。两轮
	// 重叠时会把同一批次的引用减两次：一次是用户1的合法释放，一次是并发维护
	// 的重复释放，后者在共享内容上直接把 ref_count 打到 0，把另一个用户仍然
	// 在用的对象推进待回收——下一轮就被物理删除。
	//
	// 用 TryLock 而不是 Lock：维护是尽力而为的后台任务，失败可以等下一轮，
	// 不该把调用方（尤其是管理端手动触发）阻塞在锁上。
	maintenanceMu sync.Mutex
	finalizerWG   sync.WaitGroup
}

// New 构造业务服务。
//
// 加密或存储未配置时仍可启动：让服务因为数据库里少填一项就起不来，
// 会让管理员连"进去填"的入口都没有。
func New(deps Deps) (*Service, error) {
	if deps.DB == nil || deps.Auth == nil || deps.Settings == nil {
		return nil, fmt.Errorf("service: 缺少必要依赖")
	}
	if deps.Backend == nil {
		// 未配置存储后端时给一个明确的占位实现，而不是让每个调用点判空。
		deps.Backend = backend.NewManager()
	}
	if deps.TempDir == "" {
		return nil, fmt.Errorf("service: 缺少临时目录")
	}
	return &Service{Deps: deps}, nil
}

// EncryptionReady 表示是否可以加密新对象。
func (s *Service) EncryptionReady(ctx context.Context) bool {
	return s.Settings.Runtime(ctx).Crypto.KeyringReady
}

// KEKFor 按对象记录的 keyId 取主密钥。密钥只有一把且不可更换，
// keyId 恒为 settings.PrimaryKeyID；保留按版本查找是为了让对象元数据
// 与解密路径保持稳定。
func (s *Service) KEKFor(ctx context.Context, keyID string) ([]byte, error) {
	crypto := s.Settings.Runtime(ctx).Crypto
	if key, ok := crypto.KeyByID(keyID); ok {
		return key, nil
	}
	if crypto.KeyringError != "" {
		return nil, fmt.Errorf("%w: 主密钥配置有误: %s", ErrUnavailable, crypto.KeyringError)
	}
	return nil, fmt.Errorf("%w: 找不到主密钥版本 %q，该对象当前无法解密", ErrUnavailable, keyID)
}

// PrimaryKEK 返回用于加密新对象的主密钥与版本号。
func (s *Service) PrimaryKEK(ctx context.Context) (string, []byte, error) {
	crypto := s.Settings.Runtime(ctx).Crypto
	if !crypto.KeyringReady {
		if crypto.KeyringError != "" {
			return "", nil, fmt.Errorf("%w: 主密钥配置有误: %s", ErrUnavailable, crypto.KeyringError)
		}
		return "", nil, fmt.Errorf("%w: 尚未配置主密钥", ErrUnavailable)
	}
	id, key, _ := crypto.Primary()
	return id, key, nil
}

// Now 返回当前 Unix 秒。
func (s *Service) Now() int64 { return store.Now() }

// DayKey 返回流量聚合用的日期键。
func (s *Service) DayKey() string { return store.DayKey(store.Now()) }

// BlockLog2 返回当前使用的加密块大小指数。
func (s *Service) BlockLog2(ctx context.Context) byte {
	return s.Settings.Runtime(ctx).Crypto.BlockLog2
}

// BlockSize 返回加密块大小。
func (s *Service) BlockSize(ctx context.Context) int64 {
	return s.Settings.Runtime(ctx).Crypto.BlockSize
}

// ObjectName 生成对象名：YYYY/MM/DD/{HMAC 前缀}-{随机段}.xph
//
// 目录名是**上传日期**。秒传复用旧对象时不会迁移目录，所以对象所在日期
// 不代表引用发生的时间——按日期清理必须以引用计数为准。
func (s *Service) ObjectName(ctx context.Context, checksum string) (dir, name string, err error) {
	crypto := s.Settings.Runtime(ctx).Crypto
	if !crypto.KeyringReady {
		return "", "", fmt.Errorf("%w: 尚未配置主密钥", ErrUnavailable)
	}
	_, kek, _ := crypto.Primary()
	nameKey, err := xph.DeriveObjectNameKey(kek)
	if err != nil {
		return "", "", fmt.Errorf("service: 派生对象命名密钥失败: %w", err)
	}
	prefix, err := xph.ObjectNamePrefix(nameKey, checksum)
	if err != nil {
		return "", "", fmt.Errorf("service: 派生对象名前缀失败: %w", err)
	}
	suffix, err := xph.RandomToken(8)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	return now.Format("2006/01/02"), fmt.Sprintf("%s-%s%s", prefix, suffix, ObjectNameExt), nil
}

// ObjectNameExt 是对象文件的后缀，用于识别本系统写入的对象。
const ObjectNameExt = ".xph"

// tempDir 返回临时目录下的子目录路径（不存在时不创建）。
func (s *Service) tempDir(parts ...string) string {
	return filepath.Join(append([]string{s.TempDir}, parts...)...)
}

// audit 写一条管理动作审计。写入失败不阻断业务：动作本身已经生效，
// 此时报错只会造成"操作成功但界面报错"。
func (s *Service) audit(ctx context.Context, p auth.Principal, action, target, detail string) {
	actorType := string(p.Actor)
	if actorType == "" {
		actorType = string(store.ActorUser)
	}
	_ = store.InsertAuditLog(ctx, s.DB.W(), store.AuditLog{
		ActorType: actorType,
		ActorID:   p.UserID(),
		ClientIP:  p.ClientIP.String(),
		Action:    action,
		Target:    target,
		Detail:    detail,
	})
}
