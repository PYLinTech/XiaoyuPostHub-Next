package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// 首次初始化（Setup）。
//
// "零配置可启动"带来一个新问题：**首个管理员是谁**。初始化统一由引导页
// 完成，避免把管理员口令复制到部署环境或启动参数中。
//
// 但尚未初始化的实例是公开可访问的，任何人都可能是"第一个调用初始化接口的
// 人"，所以初始化一律要求带上启动日志里打印的一次性 token——不区分本机与
// 远端，回环来源同样要自证身份。token 只在内存里，进程重启即换；初始化一旦
// 完成，整套入口立即失效。

// ErrAlreadyInitialized 表示站点已完成初始化。
var ErrAlreadyInitialized = errors.New("service: 站点已完成初始化")

// ErrSetupTokenRequired 表示初始化请求缺少或使用了错误的 token。
var ErrSetupTokenRequired = errors.New("service: 初始化需要一个有效的 setup token")

// SetupInvalidError 是引导页校验失败、需要把原因直接说给用户听的错误。
//
// 它仍然包装既有的错误（ErrBadRequest 等），错误链与调用方的判断都不受影响；
// 额外带上一句人话，是为了让 HTTP 层把它当作 message 返回，而不是通用的
// "请求参数不正确"。引导页的全部价值就在于当场说清是哪一项填错了，
// 到这一步还只回一句"参数不正确"等于白做。
type SetupInvalidError struct {
	err error
	msg string
}

func (e *SetupInvalidError) Error() string { return e.err.Error() }

// Unwrap 保留错误链，让 errors.Is 仍然按底层错误判断。
func (e *SetupInvalidError) Unwrap() error { return e.err }

// Message 返回可直接展示给用户的中文说明。
func (e *SetupInvalidError) Message() string { return e.msg }

// setupInvalid 构造一个带用户可读说明的校验失败。
func setupInvalid(err error, format string, args ...any) error {
	return &SetupInvalidError{err: err, msg: fmt.Sprintf(format, args...)}
}

// SetupStorage 是初始化时可选填写的存储凭据。
type SetupStorage struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	RootDirID    string `json:"rootDirId"`
	PrivateKey   string `json:"privateKey"`
	// AuthCallback 表示上游已配置把下载请求回源到本程序鉴权（pan123.auth_callback）。
	// 它只是"声明上游会回源"这个事实，用于管理端状态展示与排查；
	// 真正的拦截逻辑在 /api/cdn/auth，与它无关。
	AuthCallback bool `json:"authCallback"`
	// DirectLink 表示提交初始化时是否对存放根目录启用直链空间。勾选/取消勾选
	// 只在点按提交后由服务端对 123 执行一次启用/禁用，向导过程中不产生任何副作用。
	DirectLink bool `json:"directLink"`
}

// SetupRequest 是初始化请求。
type SetupRequest struct {
	Account  string `json:"account"`
	Password string `json:"password"`
	// SiteName 为空时保留默认站点名。
	SiteName string `json:"siteName"`
	// EncryptionKey 是主密钥（base64 编码的 32 字节）。留空时由服务端生成，
	// 并在响应里回传一次供离线保存。密钥只在初始化写入，之后不可更换。
	EncryptionKey string `json:"encryptionKey"`
	// RegisterMode 为空时保持默认（仅邀请码）。
	RegisterMode string `json:"registerMode"`
	// SystemMode 为空时保持默认（文件与邮件）。取值见 settings.SystemMode*。
	SystemMode string `json:"systemMode"`
	// Storage 可选：填写后立即尝试启用存储后端；留空则稍后在管理界面配置。
	Storage *SetupStorage `json:"storage"`
	// Token 是启动日志里打印的一次性初始化令牌。
	Token string `json:"token"`
}

// SetupStepValidateRequest 是引导页的分步校验请求。
//
// 引导页每一步点"下一步"都会带着这一步的字段发过来，校验通过才允许前进。
// 这样令牌填错、账号不合规、密钥格式不对都在当场暴露，而不是让人填完八步
// 到最后提交时才知道。
type SetupStepValidateRequest struct {
	// Step 是要校验的步骤标识：token / admin / encryption / storage。
	// 其余字段按步骤取用，未涉及的字段一律忽略。
	Step          string        `json:"step"`
	Token         string        `json:"token,omitempty"`
	Account       string        `json:"account,omitempty"`
	Password      string        `json:"password,omitempty"`
	EncryptionKey string        `json:"encryptionKey,omitempty"`
	Storage       *SetupStorage `json:"storage,omitempty"`
}

// ValidateSetupStep 对引导页的单步输入做服务端校验。
//
// 只做无副作用的检查：不写库、不改配置、也不对存储发起连通性探测（那一步
// 会上传并清理真实对象，属于提交时的动作）。已经初始化的实例一律拒绝，
// 与 POST /api/setup/init 保持同一套口径。
func (s *Service) ValidateSetupStep(ctx context.Context, req SetupStepValidateRequest) error {
	initialized, err := s.Initialized(ctx)
	if err != nil {
		return fmt.Errorf("%w: 读取初始化状态失败", ErrUnavailable)
	}
	if initialized {
		return ErrAlreadyInitialized
	}

	switch req.Step {
	case "token":
		return s.checkSetupToken(req.Token)
	case "admin":
		return validateSetupAccount(req.Account, req.Password)
	case "encryption":
		return validateSetupKey(req.EncryptionKey)
	case "storage":
		return validateSetupStorage(req.Storage)
	default:
		return setupInvalid(ErrBadRequest, "未知的校验步骤 %q", req.Step)
	}
}

// checkSetupToken 校验一次性初始化令牌。
func (s *Service) checkSetupToken(given string) error {
	s.setupMu.Lock()
	token := s.setupTokenLocked()
	s.setupMu.Unlock()
	if token == "" || token != strings.TrimSpace(given) {
		return ErrSetupTokenRequired
	}
	return nil
}

// validateSetupAccount 校验管理员账号与口令。规则与 CompleteSetup 完全一致。
func validateSetupAccount(account, password string) error {
	if msg := validateAccountText(strings.TrimSpace(account)); msg != "" {
		return setupInvalid(ErrBadRequest, "%s", msg)
	}
	// 口令的规则由 auth 包把关，它的错误在 HTTP 层已有专属映射
	// （过短/过长/字符集各自一句人话），这里只负责接上错误码。
	if err := auth.ValidatePassword(password); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	return nil
}

// validateSetupKey 校验主密钥格式；留空表示由服务端生成，同样算通过。
func validateSetupKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	if _, err := settings.ParseKey(key); err != nil {
		return setupInvalid(ErrBadRequest, "主密钥格式不正确：%v", err)
	}
	return nil
}

// validateSetupStorage 校验存储凭据的填写完整性。
//
// 这里刻意不校验根目录 ID 的格式：该字段在上游是一个 fileID 字符串，默认值
// 就是 "0"，凭空加一条数字约束只会把合法的填写挡在门外。直链空间与根目录
// 互斥是上游的真实限制，值得在这里就拦下。
func validateSetupStorage(st *SetupStorage) error {
	if st == nil {
		return nil
	}
	if strings.TrimSpace(st.ClientID) == "" || strings.TrimSpace(st.ClientSecret) == "" {
		return setupInvalid(ErrBadRequest, "请填写 Client ID 与 Client Secret")
	}
	root := strings.TrimSpace(st.RootDirID)
	if root == "" {
		return setupInvalid(ErrBadRequest, "请填写根目录 ID")
	}
	if st.DirectLink && root == "0" {
		return setupInvalid(ErrBadRequest, "网盘根目录（ID 为 0）不能启用直链空间，请填写具体的根目录编号")
	}
	return nil
}

// SetupResult 是初始化的结果。
type SetupResult struct {
	AdminAccount string `json:"adminAccount"`
	SiteName     string `json:"siteName"`
	KeyID        string `json:"keyId,omitempty"`
	// EncryptionKey 只在服务端自动生成时返回，且只返回这一次。
	// 它是灾难恢复的最后凭据：master.key 丢失后，只有它能把存量文件救回来。
	EncryptionKey string `json:"encryptionKey,omitempty"`
	// EncryptionGenerated 表示主密钥由服务端生成（需要离线保存上面那一项）。
	EncryptionGenerated bool `json:"encryptionGenerated"`
	StorageConfigured   bool `json:"storageConfigured"`
	// StorageProbe 是提交时对存储链路执行的探针结果（上传/下载/校验/删除）。
	StorageProbe *SetupProbeResult `json:"storageProbe,omitempty"`
	// StorageWarning 在存储凭据填写了但链路验证或直链开关未通过时给出原因。
	StorageWarning string `json:"storageWarning,omitempty"`
}

// SetupState 描述站点当前的初始化状态，供前端决定是否展示引导页。
//
// 它返回"前端画引导页需要知道的一切"：站点名、口令长度要求、注册模式、
// 加密与存储是否就绪。引导页因此不需要额外请求，也就不会
// 出现"引导页和实际规则不一致"。
type SetupState struct {
	// Initialized 为真表示已完成初始化，引导入口应当关闭。
	Initialized bool `json:"initialized"`

	SiteName string `json:"siteName"`
	// EncryptionReady 为真表示已配置主密钥，可以加密新文件。
	EncryptionReady bool `json:"encryptionReady"`
	// StorageConfigured 为真表示存储凭据已填写。
	StorageConfigured bool `json:"storageConfigured"`
	// MasterSecretSource 说明 master secret 从哪来（env / file / generated），
	// 引导页据此提示"请备份 master.key"。
	MasterSecretSource string `json:"masterSecretSource"`

	RegisterMode string `json:"registerMode"`
	SystemMode   string `json:"systemMode"`
	// MailReceiveEnabled 供前端判断"用户侧邮件是否可用"。用户侧门禁要求模式
	// 与收件开关同时打开（见 settings.Runtime.MailAvailable），前端必须拿到
	// 同一份事实，否则侧栏会显示一个点进去就是 404 的邮件入口。
	MailReceiveEnabled bool `json:"mailReceiveEnabled"`
	MinAccountLen      int  `json:"minAccountLen"`
	MaxAccountLen      int  `json:"maxAccountLen"`
	MinPasswordLen     int  `json:"minPasswordLen"`
	MaxPasswordLen     int  `json:"maxPasswordLen"`

	// DataDir 与 DBPath 只在未初始化时返回，便于运维确认落盘位置。
	DataDir string `json:"dataDir,omitempty"`
	DBPath  string `json:"dbPath,omitempty"`
}

// Initialized 判断站点是否已完成初始化。
//
// 判据是"是否已存在任何账号"而不是某个标志位：标志位可能因为一次失败的
// 初始化被误置，而"有账号"是一个不可伪造的事实。这也让手工插库创建管理员
// 的老办法仍然有效。
func (s *Service) Initialized(ctx context.Context) (bool, error) {
	count, err := store.CountUsers(ctx, s.DB.R())
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// SetupToken 返回一次性初始化令牌；站点已初始化时返回空串。
//
// 令牌在首次访问时生成并缓存在内存里，进程重启即更换。
func (s *Service) SetupToken(ctx context.Context) string {
	initialized, err := s.Initialized(ctx)
	if err != nil || initialized {
		return ""
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.setupTokenLocked()
}

// setupTokenLocked 取一次性初始化令牌，必要时生成。调用方必须已持有 setupMu。
func (s *Service) setupTokenLocked() string {
	if s.setupToken == "" {
		token, err := store.GenerateToken(16)
		if err != nil {
			return ""
		}
		s.setupToken = token
	}
	return s.setupToken
}

// SetupState 返回初始化状态。
func (s *Service) SetupState(ctx context.Context) (SetupState, error) {
	rt := s.Settings.Runtime(ctx)
	initialized, err := s.Initialized(ctx)
	if err != nil {
		return SetupState{}, fmt.Errorf("读取初始化状态失败: %w", err)
	}
	state := SetupState{
		Initialized:        initialized,
		SiteName:           rt.Site.Name,
		EncryptionReady:    rt.Crypto.KeyringReady,
		StorageConfigured:  rt.Pan123.Configured(),
		MasterSecretSource: s.MasterSecretSource,
		RegisterMode:       rt.Auth.RegisterMode,
		SystemMode:         rt.Site.SystemMode,
		MailReceiveEnabled: rt.Mail.ReceiveEnabled,
		// 账号与密码的长度规则是固定值（见 accounts.go 的常量），
		// 不再来自配置；这里回传是为了让界面提示与实际校验一致。
		MinAccountLen:  AccountMinChars,
		MaxAccountLen:  AccountMaxChars,
		MinPasswordLen: auth.MinPasswordChars,
		MaxPasswordLen: auth.MaxPasswordChars,
	}
	if !initialized {
		// 未初始化时才暴露路径信息：引导页需要它来让运维确认"东西落在哪"，
		// 初始化之后这些信息就属于管理端接口的范畴了。
		state.DataDir = s.DataDir
		state.DBPath = s.DB.Path()
	}
	return state, nil
}

// CompleteSetup 执行一次性初始化。
//
// 顺序刻意是"先写配置、后建账号"：反过来的话，账号建好而配置写入失败会留下
// "已有管理员但站点没初始化完"的状态，此时初始化入口已经关闭，管理员只能
// 登录后台去补齐。按当前顺序，最坏情况是配置写好了但没有账号，重试即可。
func (s *Service) CompleteSetup(ctx context.Context, req SetupRequest) (SetupResult, error) {
	// 全程互斥：判据是"库里还没有账号"，而"检查 → 建号"之间若有第二个请求
	// 挤进来，它会看到同样为空的库并一起成功，最终留下两个管理员，
	// 配置按 last-writer-wins 被静默覆盖。整个过程必须一把锁走完。
	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	initialized, err := s.Initialized(ctx)
	if err != nil {
		return SetupResult{}, fmt.Errorf("读取初始化状态失败: %w", err)
	}
	if initialized {
		return SetupResult{}, ErrAlreadyInitialized
	}
	// 先占住令牌校验，再做任何事：否则任何人都能在管理员之前把这个实例初始化掉。
	// 这里刻意不调 checkSetupToken：setupMu 已经被本函数持有，再取一次锁会自锁。
	token := s.setupTokenLocked()
	if token == "" || token != strings.TrimSpace(req.Token) {
		return SetupResult{}, ErrSetupTokenRequired
	}

	// 账号与密钥的规则与分步校验共用同一份实现，避免两处判断漂移。
	if err := validateSetupAccount(req.Account, req.Password); err != nil {
		return SetupResult{}, err
	}
	if err := validateSetupStorage(req.Storage); err != nil {
		return SetupResult{}, err
	}

	// ① 主密钥：未提供则生成。生成而不是"要求运维自己想一个"——
	//    后者在实践中的结果往往是弱口令，而弱口令保护的是全部文件内容。
	//    密钥只在初始化写入，此后没有任何接口可以更换它。
	key := strings.TrimSpace(req.EncryptionKey)
	generated := false
	if key == "" {
		key, err = newCryptoKey()
		if err != nil {
			return SetupResult{}, err
		}
		generated = true
	} else if err := validateSetupKey(key); err != nil {
		return SetupResult{}, err
	}

	// ② 组装并整体写入配置。SetMany 会先全部校验再落库，
	//    因此不存在"写了一半"的中间态。短凭据密钥由主密钥派生，无需单独写入。
	values := map[settings.Key]string{
		settings.KeyCryptokeys: key,
	}
	if name := strings.TrimSpace(req.SiteName); name != "" {
		values[settings.KeySiteName] = name
	}
	if mode := strings.TrimSpace(req.RegisterMode); mode != "" {
		values[settings.KeyRegisterMode] = mode
	}
	// 系统模式留空即保持内置默认（both），避免旧客户端不传该字段时写入空串。
	if mode := strings.TrimSpace(req.SystemMode); mode != "" {
		values[settings.KeySystemMode] = mode
	}
	if req.Storage != nil {
		if v := strings.TrimSpace(req.Storage.ClientID); v != "" {
			values[settings.KeyPan123ClientID] = v
		}
		if v := strings.TrimSpace(req.Storage.ClientSecret); v != "" {
			values[settings.KeyPan123ClientSecret] = v
		}
		if v := strings.TrimSpace(req.Storage.RootDirID); v != "" {
			values[settings.KeyPan123RootDirID] = v
		}
		if v := strings.TrimSpace(req.Storage.PrivateKey); v != "" {
			values[settings.KeyPan123PrivateKey] = v
		}
		if req.Storage.AuthCallback {
			values[settings.KeyPan123AuthCallback] = "true"
		}
		if req.Storage.DirectLink {
			// false 即内置默认，不写覆盖，避免管理端显示无意义的"已自定义"。
			values[settings.KeyPan123DirectLink] = "true"
		}
	}
	if err := s.Settings.SetMany(ctx, values, 0); err != nil {
		return SetupResult{}, err
	}

	// ③ 创建管理员账号。
	account := strings.TrimSpace(req.Account)
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return SetupResult{}, err
	}
	_, err = store.CreateUser(ctx, s.DB.W(), store.User{
		Account:      account,
		DisplayName:  account,
		PasswordHash: hash,
		GroupName:    perm.GroupAdmin,
		Status:       store.UserEnabled,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return SetupResult{}, fmt.Errorf("%w: 账号 %s 已存在", ErrConflict, account)
		}
		return SetupResult{}, err
	}

	result := SetupResult{
		AdminAccount:        account,
		SiteName:            s.Settings.Runtime(ctx).Site.Name,
		KeyID:               settings.PrimaryKeyID,
		EncryptionGenerated: generated,
	}
	if generated {
		// 只回传这一次：它是 master.key 丢失后唯一的救命凭据。
		result.EncryptionKey = key
	}

	// ④ 存储链路验证：凭据写入即生效，提交时用与管理端自检同一套服务层
	//    探针体检上传/下载/校验/清理数据面，再按勾选项执行一次直链空间
	//    开关。任何一步失败都不算初始化失败：凭据可稍后在「系统配置 →
	//    123 云盘」修正后重试，不应让"管理员账号已建好"这一事实回滚。
	directLinkFolder := ""
	probeOK := false
	if req.Storage != nil && req.Storage.ClientID != "" && req.Storage.ClientSecret != "" {
		var warnings []string
		if err := s.ApplyStorageSettings(ctx); err != nil {
			warnings = append(warnings, "存储后端构建失败: "+err.Error())
		} else if result.StorageConfigured = s.StorageReady(); result.StorageConfigured {
			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			probe, probeErr := s.runSetupProbe(probeCtx)
			cancel()
			if probeErr != nil {
				// 探针错误自带失败阶段（上传/下载/校验/清理），不再加前缀。
				warnings = append(warnings, probeErr.Error())
			} else {
				result.StorageProbe = &probe
				probeOK = true
			}

			action := "关闭"
			if req.Storage.DirectLink {
				action = "启用"
			}
			folder, err := s.ApplyDirectLinkSetting(ctx, req.Storage.DirectLink)
			switch {
			case err != nil:
				warnings = append(warnings, fmt.Sprintf("直链空间%s失败: %v", action, err))
			case folder != "":
				directLinkFolder = folder
			}
		}
		result.StorageWarning = strings.Join(warnings, "；")
	}

	// ⑤ 审计与令牌作废（已在 setupMu 保护下）。
	s.setupToken = ""
	directLinkAudit := "none"
	if result.StorageConfigured {
		directLinkAudit = fmt.Sprintf("on=%v folder=%q probe=%v warn=%v",
			req.Storage.DirectLink, directLinkFolder, probeOK, result.StorageWarning != "")
	}
	_ = store.InsertAuditLog(ctx, s.DB.W(), store.AuditLog{
		ActorType: "system",
		Action:    "setup.complete",
		Target:    account,
		Detail: fmt.Sprintf("keyId=%s generated=%v storage=%v directLink=%s",
			settings.PrimaryKeyID, generated, result.StorageConfigured, directLinkAudit),
	})
	return result, nil
}

// newCryptoKey 生成一把新的主密钥，返回 URL 安全 Base64 编码（43 字符）。
func newCryptoKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("%w: 生成主密钥失败", ErrUnavailable)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
