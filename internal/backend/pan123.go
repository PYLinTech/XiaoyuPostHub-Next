package backend

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// 123 开放平台的接口地址。各接口的频率上限见 NewPan123 里构造的 limiter：
// 上限来自官方文档的分级要求，超过会返回 429。
const (
	apiAccessToken       = "/api/v1/access_token"
	apiUserInfo          = "/api/v1/user/info"
	apiFileList          = "/api/v2/file/list"
	apiDownloadInfo      = "/api/v1/file/download_info"
	apiDirectLink        = "/api/v1/direct-link/url"
	apiDirectLinkEnable  = "/api/v1/direct-link/enable"
	apiDirectLinkDisable = "/api/v1/direct-link/disable"
	apiMkdir             = "/upload/v1/file/mkdir"
	apiTrash             = "/api/v1/file/trash"
	apiUploadCreate      = "/upload/v2/file/create"
	apiUploadFinish      = "/upload/v2/file/upload_complete"

	// 服务端解密器按加密块顺序读取；默认块为 1 MiB。4 MiB 窗口减少
	// 中转大文件的上游 Range 请求，同时把每个活跃读取器的缓冲控制在 4 MiB。
	rangeReadChunk = 4 << 20
)

// limiter 是滑窗式的接口频率限制器：保证任意两次调用之间至少间隔 interval。
//
// 自实现而不是引入依赖；重点是"排队"而不是"拒绝"，因为调用方都是后台任务。
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(qps int) *limiter {
	if qps <= 0 {
		return &limiter{}
	}
	return &limiter{interval: time.Second / time.Duration(qps)}
}

func (l *limiter) wait(ctx context.Context) error {
	if l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	wait := l.next.Sub(now)
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Pan123 是 123 云盘存储后端。
type Pan123 struct {
	cfg     Pan123Config
	apiBase string
	client  *http.Client
	upload  *http.Client

	tokenMu sync.Mutex
	token   string
	// tokenExpiry 是当前令牌的过期时刻。
	tokenExpiry time.Time
	// tokenRetryAt 是令牌刷新失败后的短暂退避截止时间。它避免上游故障时
	// 每个请求都立即重打令牌接口，同时允许后续请求自动恢复，不把一次瞬时
	// 网络错误变成进程生命周期内的永久停机。
	tokenBlocked bool
	tokenRetryAt time.Time
	// tokenInflight 非 nil 表示已有一次刷新在进行：后来的请求等待同一个
	// 结果而不是各自再打一遍令牌接口（singleflight）。刷新期间不持有
	// tokenMu，因此等待只阻塞需要新令牌的请求，拿着有效令牌的快路径
	// （以及与令牌无关的逻辑）不受影响。tokenErr 是该次刷新的结果。
	tokenInflight chan struct{}
	tokenErr      error

	uidMu sync.Mutex
	uid   uint64

	dirMu    sync.Mutex
	dirCache map[string]int64

	limAccessToken  *limiter
	limGlobal       *limiter
	limUserInfo     *limiter
	limFileList     *limiter
	limDownloadInfo *limiter
	limDirectLink   *limiter
	limMkdir        *limiter
	limMove         *limiter
	limRename       *limiter
	limTrash        *limiter
	limUploadCreate *limiter
}

// NewPan123 构造 123 后端。凭据缺失时返回错误：没有凭据的后端无法完成任何
// 实际读写，让它构造成功只会把问题推迟到第一次上传。
func NewPan123(cfg Pan123Config) (*Pan123, error) {
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, fmt.Errorf("backend: 未配置 123 开放平台凭据（pan123.client_id / pan123.client_secret）")
	}
	base, err := url.Parse(strings.TrimSpace(cfg.APIBase))
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("backend: APIBase 必须是完整的 http(s) 地址")
	}
	if cfg.DirectLinkTTL <= 0 {
		return nil, fmt.Errorf("backend: 直链有效期必须大于 0")
	}
	if cfg.UploadThreads <= 0 {
		return nil, fmt.Errorf("backend: 上传并发数必须大于 0")
	}
	if cfg.QPS < 0 {
		return nil, fmt.Errorf("backend: QPS 不得为负")
	}
	if cfg.PollInterval <= 0 || cfg.PollAttempts <= 0 {
		return nil, fmt.Errorf("backend: 上传完成轮询参数必须大于 0")
	}
	if root := strings.TrimSpace(cfg.RootDirID); root != "" && root != "0" {
		id, parseErr := strconv.ParseInt(root, 10, 64)
		if parseErr != nil || id <= 0 {
			return nil, fmt.Errorf("backend: 根目录编号必须是正整数或 0")
		}
	}
	return &Pan123{
		cfg:             cfg,
		apiBase:         strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/"),
		client:          &http.Client{Timeout: 60 * time.Second},
		upload:          &http.Client{Timeout: 0}, // 分片上传按 ctx 控制，不设整体超时
		dirCache:        map[string]int64{},
		limAccessToken:  newLimiter(1),
		limGlobal:       newLimiter(cfg.QPS),
		limUserInfo:     newLimiter(1),
		limFileList:     newLimiter(3),
		limDownloadInfo: newLimiter(5),
		limDirectLink:   newLimiter(5),
		limMkdir:        newLimiter(2),
		limMove:         newLimiter(1),
		limRename:       newLimiter(1),
		limTrash:        newLimiter(2),
		limUploadCreate: newLimiter(2),
	}, nil
}

func (b *Pan123) Kind() string { return "pan123" }

// Health 执行一次轻量自检：取用户信息。它同时验证了令牌获取与接口可达性，
// 而这两件事恰好是凭据配错时最先失败的地方。
func (b *Pan123) Health(ctx context.Context) (map[string]any, error) {
	var resp userInfoResp
	if err := b.call(ctx, apiUserInfo, b.limUserInfo, http.MethodGet, nil, &resp); err != nil {
		return nil, err
	}
	return map[string]any{
		"uid":            resp.Data.UID,
		"spaceUsed":      resp.Data.SpaceUsed,
		"spacePermanent": resp.Data.SpacePermanent,
		"spaceTemp":      resp.Data.SpaceTemp,
		"rootDirId":      b.cfg.RootDirID,
	}, nil
}

// PresignReady 只有配置 URL 鉴权私钥时才为真。
//
// 123 的自用下载地址不具备本系统票据的有效期与回源约束，不能作为安全直链
// 下发；缺少私钥时由业务层改走服务端解密。
func (b *Pan123) PresignReady() bool { return strings.TrimSpace(b.cfg.PrivateKey) != "" }

// SliceMD5Enabled 表示 create 必须提供密文哈希，因此上传链路必须先把密文落盘。
func (b *Pan123) SliceMD5Enabled() bool { return true }

// ---------------------------------------------------------------- 令牌

// accessToken 返回可用的访问令牌。
//
// 刷新采用 singleflight：同一时刻只允许一次 flushToken（HTTP 调用）在途，
// 期间其他调用等待同一个结果。互斥锁只保护字段读写，绝不在 HTTP 期间持有——
// 旧实现持锁刷新会让令牌过期的那几百毫秒里，全站对 123 的调用（含互不相关的
// 上传分片）全部串行阻塞。
func (b *Pan123) accessToken(ctx context.Context, force bool) (string, error) {
	b.tokenMu.Lock()
	now := time.Now()
	if b.tokenBlocked && !force && now.Before(b.tokenRetryAt) {
		b.tokenMu.Unlock()
		return "", errTokenRefreshPaused
	}
	// 预留 5 分钟余量，避免令牌在请求途中过期。
	if !force && b.token != "" && now.Before(b.tokenExpiry.Add(-5*time.Minute)) {
		token := b.token
		b.tokenMu.Unlock()
		return token, nil
	}
	if ch := b.tokenInflight; ch != nil {
		// 已有刷新在途：等待它的结果，无论本次是不是 force。一次成功
		// 刷新足以修复过期令牌；若它失败了，由调用方自身的重试节奏决定
		// 下一次刷新，避免一批并发 401 各打一遍令牌接口。
		b.tokenMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ch:
		}
		b.tokenMu.Lock()
		err, token := b.tokenErr, b.token
		b.tokenMu.Unlock()
		if err != nil {
			return "", err
		}
		return token, nil
	}
	// 由本次调用发起刷新：登记在途通道后立即放锁，HTTP 在锁外进行。
	ch := make(chan struct{})
	b.tokenInflight = ch
	b.tokenErr = nil
	b.tokenMu.Unlock()

	newToken, expiry, err := b.flushToken(ctx)

	b.tokenMu.Lock()
	b.tokenInflight = nil
	b.tokenErr = err
	if err != nil {
		b.tokenBlocked = true
		b.tokenRetryAt = time.Now().Add(5 * time.Second)
	} else {
		b.token = newToken
		b.tokenExpiry = expiry
		b.tokenBlocked = false
		b.tokenRetryAt = time.Time{}
	}
	token := b.token
	close(ch)
	b.tokenMu.Unlock()
	if err != nil {
		return "", err
	}
	return token, nil
}

// flushToken 向 123 换取新令牌。它只返回结果、不写 b.token / b.tokenExpiry：
// 刷新在 tokenMu 之外进行（HTTP 不能持锁），字段写入必须由调用方回到锁内完成，
// 否则会与快路径的无锁读取形成数据竞争。
func (b *Pan123) flushToken(ctx context.Context) (string, time.Time, error) {
	if err := b.limAccessToken.wait(ctx); err != nil {
		return "", time.Time{}, err
	}
	payload := jsonBody{"clientID": b.cfg.ClientID, "clientSecret": b.cfg.ClientSecret}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiBase+apiAccessToken, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("platform", "open_platform")

	res, err := b.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("backend: 请求 123 令牌失败: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("backend: 读取 123 令牌响应失败: %w", err)
	}
	if len(raw) > 1<<20 {
		return "", time.Time{}, fmt.Errorf("backend: 123 令牌响应过大")
	}
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return "", time.Time{}, fmt.Errorf("backend: 获取 123 令牌失败，HTTP %d", res.StatusCode)
	}
	var parsed accessTokenResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("backend: 解析 123 令牌响应失败: %w", err)
	}
	if parsed.Code != 0 {
		return "", time.Time{}, fmt.Errorf("backend: 获取 123 令牌失败: %s", parsed.Message)
	}
	if parsed.Data.AccessToken == "" || parsed.Data.ExpiredAt == "" {
		return "", time.Time{}, fmt.Errorf("backend: 123 令牌响应缺少字段")
	}
	expiry, err := time.Parse(time.RFC3339, parsed.Data.ExpiredAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("backend: 解析 123 令牌过期时间失败: %w", err)
	}
	return parsed.Data.AccessToken, expiry.UTC(), nil
}

func (b *Pan123) uidValue(ctx context.Context) (uint64, error) {
	b.uidMu.Lock()
	if b.uid != 0 {
		uid := b.uid
		b.uidMu.Unlock()
		return uid, nil
	}
	b.uidMu.Unlock()

	var resp userInfoResp
	if err := b.call(ctx, apiUserInfo, b.limUserInfo, http.MethodGet, nil, &resp); err != nil {
		return 0, err
	}
	b.uidMu.Lock()
	b.uid = uint64(resp.Data.UID)
	b.uidMu.Unlock()
	return uint64(resp.Data.UID), nil
}

// ---------------------------------------------------------------- 通用请求

// call 发起一次带鉴权的 JSON 请求。
//
// 401（令牌失效）强制刷新后重试；429（过频）退避后重试；其余非 0 code 是
// 业务错误，重试不会让结果变好，直接返回。
func (b *Pan123) call(ctx context.Context, api string, lim *limiter, method string, payload jsonBody, out any) error {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := b.accessToken(ctx, attempt > 0 && isAuthErr(lastErr))
		if err != nil {
			return err
		}
		// 上传完成接口没有单独的限流器：nil 表示沿用请求本身的
		// 鉴权/退避逻辑，不应在这里解引用导致上传收尾 panic。
		if lim != nil {
			if err := lim.wait(ctx); err != nil {
				return err
			}
		}
		if b.limGlobal != nil {
			if err := b.limGlobal.wait(ctx); err != nil {
				return err
			}
		}

		body, err := marshalBody(payload)
		if err != nil {
			return err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, b.apiBase+api, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("platform", "open_platform")
		req.Header.Set("Content-Type", "application/json")

		res, err := b.client.Do(req)
		if err != nil {
			return fmt.Errorf("backend: 请求 %s 失败: %w", api, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
		res.Body.Close()
		if readErr != nil {
			return fmt.Errorf("backend: 读取 %s 响应失败: %w", api, readErr)
		}
		if len(raw) > 4<<20 {
			return fmt.Errorf("backend: %s 响应过大", api)
		}
		// 上游有时只通过 HTTP 状态码表达失效/限流，响应体甚至是
		// HTML。先看状态码，避免把这些可恢复情况误报成 JSON 解析错误。
		if res.StatusCode == http.StatusUnauthorized {
			lastErr = errAuthExpired
			continue
		}
		if res.StatusCode == http.StatusTooManyRequests {
			lastErr = errRateLimited
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
			}
			continue
		}
		if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("backend: %s 返回 HTTP %d", api, res.StatusCode)
		}

		var envelope baseResp
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return fmt.Errorf("backend: 解析 %s 响应失败: %w", api, err)
		}
		switch envelope.Code {
		case 0:
			if out != nil {
				if err := json.Unmarshal(raw, out); err != nil {
					return fmt.Errorf("backend: 解析 %s 数据失败: %w", api, err)
				}
			}
			return nil
		case 401:
			lastErr = errAuthExpired
		case 429:
			lastErr = errRateLimited
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
			}
		default:
			// 用户可见文案只留上游原因（如「parentFileID不存在」）；
			// 接口路径与错误码对排查有意义，但不进用户可见消息。
			if envelope.Message == "" {
				return fmt.Errorf("backend: 上游返回错误码 %d", envelope.Code)
			}
			return errors.New(envelope.Message)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("backend: %s 重试次数耗尽", api)
	}
	return lastErr
}

var (
	errAuthExpired = fmt.Errorf("backend: 123 令牌已过期")
	errRateLimited = fmt.Errorf("backend: 123 接口请求过频")
	// errTokenRefreshPaused 在刷新失败后的短暂退避窗口内返回，避免上游故障时
	// 每个请求都立刻重打令牌接口。
	errTokenRefreshPaused = fmt.Errorf("backend: 123 令牌此前刷新失败，已暂停重试")
)

// ErrUploadBandwidth 表示上传网关的 60s 请求体读取超时。
//
// 123 的上传网关（slice/single 端点共用）在 60s 内没有收完请求体就断开并
// 返回 500。分片大小由服务端固定为 16MB，因此该错误几乎总是意味着
// "服务器上行带宽不足以在时限内传完一个分片"——这是部署侧的网络条件
// 问题，重试与改代码都解决不了，必须向上透传给用户一个明确的指引。
var ErrUploadBandwidth = errors.New("backend: 上传网关读取请求体超时")

// sliceBodyTimeoutFloor 是"请求耗时达到该值即认为撞上网关读超时"的判定
// 阈值。网关实际超时约 60s，取 50s 留出握手与响应余量；部分 500 的响应体
// 为空，耗时是唯一可用的辅助信号。
var sliceBodyTimeoutFloor = 50 * time.Second

// IsUploadBandwidthError 判断错误链里是否携带带宽不足信号。
func IsUploadBandwidthError(err error) bool {
	return errors.Is(err, ErrUploadBandwidth)
}

func isAuthErr(err error) bool {
	return errors.Is(err, errAuthExpired)
}

// ---------------------------------------------------------------- 目录

// EnsureDir 确保逻辑目录存在并返回其 fileID。dir 形如 "2026/09/21"，空表示根。
//
// 结果被缓存：同一批上传会反复用到同一天目录，不缓存会让每上传一个文件
// 就多打一次列表接口，很快触发上游限流。
func (b *Pan123) EnsureDir(ctx context.Context, dir string) (int64, error) {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return b.rootID()
	}

	b.dirMu.Lock()
	if id, ok := b.dirCache[dir]; ok {
		b.dirMu.Unlock()
		return id, nil
	}
	b.dirMu.Unlock()

	parentID, err := b.rootID()
	if err != nil {
		return 0, err
	}
	acc := ""
	for _, seg := range strings.Split(dir, "/") {
		if acc == "" {
			acc = seg
		} else {
			acc = acc + "/" + seg
		}
		id, err := b.ensureChild(ctx, parentID, seg)
		if err != nil {
			return 0, err
		}
		parentID = id
		b.dirMu.Lock()
		b.dirCache[acc] = id
		b.dirMu.Unlock()
	}
	return parentID, nil
}

func (b *Pan123) rootID() (int64, error) {
	raw := strings.TrimSpace(b.cfg.RootDirID)
	if raw == "" || raw == "0" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("backend: pan123.root_dir_id 不是合法的 fileID: %w", err)
	}
	return id, nil
}

// ensureChild 在 parent 下找到或创建名为 name 的目录。
func (b *Pan123) ensureChild(ctx context.Context, parentID int64, name string) (int64, error) {
	if id, ok, err := b.findChildDir(ctx, parentID, name); err != nil {
		return 0, err
	} else if ok {
		return id, nil
	}

	var resp mkdirResp
	err := b.call(ctx, apiMkdir, b.limMkdir, http.MethodPost, jsonBody{
		"parentID": strconv.FormatInt(parentID, 10),
		"name":     name,
	}, &resp)
	if err != nil {
		return 0, err
	}
	if resp.Data.DirID != 0 {
		return resp.Data.DirID, nil
	}
	// 部分账号的 mkdir 不回传 dirID，此时回查一次列表。
	if id, ok, err := b.findChildDir(ctx, parentID, name); err != nil {
		return 0, err
	} else if ok {
		return id, nil
	}
	return 0, fmt.Errorf("backend: 创建目录 %q 后未能解析其 fileID", name)
}

// findChildDir 在 parent 下按名称查找目录。
func (b *Pan123) findChildDir(ctx context.Context, parentID int64, name string) (int64, bool, error) {
	files, err := b.listAll(ctx, parentID)
	if err != nil {
		return 0, false, err
	}
	for _, f := range files {
		if f.Type == 1 && f.FileName == name && f.Trashed == 0 {
			return f.FileID, true, nil
		}
	}
	return 0, false, nil
}

// listAll 取某目录下的全部条目（自动翻页）。
func (b *Pan123) listAll(ctx context.Context, parentID int64) ([]panFile, error) {
	var out []panFile
	lastID := int64(0)
	for {
		var resp listResp
		err := b.call(ctx, fmt.Sprintf("%s?parentFileId=%d&limit=100&lastFileId=%d&trashed=false",
			apiFileList, parentID, lastID), b.limFileList, http.MethodGet, nil, &resp)
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Data.FileList...)
		if resp.Data.LastFileID == -1 || resp.Data.LastFileID == 0 {
			return out, nil
		}
		lastID = resp.Data.LastFileID
	}
}

// ---------------------------------------------------------------- 写入

// Put 把密文写入 123。
//
// 流程：create（传密文长度与密文 MD5）→ 逐片上传 → 轮询 upload_complete。
// create 返回的分片粒度必须校验是加密块大小的整数倍。
func (b *Pan123) Put(ctx context.Context, req PutRequest) (PutResult, error) {
	if req.Source == nil {
		return PutResult{}, fmt.Errorf("backend: 缺少上传数据源")
	}
	if req.SizePlain < 0 {
		return PutResult{}, fmt.Errorf("backend: 明文长度不得为负")
	}
	if req.SizeWire <= 0 {
		return PutResult{}, fmt.Errorf("backend: 对象长度必须为正")
	}
	if req.BlockSize <= 0 {
		return PutResult{}, fmt.Errorf("backend: 缺少加密块大小，无法校验分片对齐")
	}
	if _, err := hex.DecodeString(strings.TrimSpace(req.CipherMD5)); err != nil || len(strings.TrimSpace(req.CipherMD5)) != 32 {
		return PutResult{}, fmt.Errorf("backend: 密文 MD5 必须是 32 位十六进制")
	}
	maxPartSize := req.MaxPartSize
	if maxPartSize <= 0 || req.SizeWire <= maxPartSize {
		return b.putOne(ctx, req)
	}
	parts, err := cipherPartRanges(req.SizePlain, req.SizeWire, req.BlockSize, maxPartSize)
	if err != nil {
		return PutResult{}, err
	}
	refs := make([]ObjectPart, 0, len(parts))
	var uploaded int64
	for i, part := range parts {
		section := io.NewSectionReader(req.Source, part.offset, part.wireSize)
		sum := md5.New()
		if _, err := io.Copy(sum, section); err != nil {
			return PutResult{}, fmt.Errorf("backend: 计算第 %d 卷密文 MD5 失败: %w", i+1, err)
		}
		partReq := req
		partReq.LogicalName = fmt.Sprintf("%s.xph-%05d", req.LogicalName, i+1)
		partReq.SizePlain = part.plainSize
		partReq.SizeWire = part.wireSize
		partReq.CipherMD5 = hex.EncodeToString(sum.Sum(nil))
		partReq.Source = io.NewSectionReader(req.Source, part.offset, part.wireSize)
		partReq.MaxPartSize = 0
		base := uploaded
		if req.OnProgress != nil {
			partReq.OnProgress = func(n int64) { req.OnProgress(base + n) }
		}
		put, err := b.putOne(ctx, partReq)
		if err != nil {
			cleanupErr := deleteObjectParts(ctx, b, refs)
			return PutResult{}, errors.Join(err, cleanupErr)
		}
		refs = append(refs, ObjectPart{Ref: put.ObjectRef, Offset: uploaded, Size: part.wireSize})
		uploaded += part.wireSize
	}
	ref, err := ComposeObjectRef(refs)
	if err != nil {
		return PutResult{}, err
	}
	return PutResult{ObjectRef: ref}, nil
}

type cipherPartRange struct {
	offset    int64
	wireSize  int64
	plainSize int64
}

// cipherPartRanges 只在 XPH 的完整 GCM 块边界切卷，第一卷保留文件头。
func cipherPartRanges(sizePlain, sizeWire, blockSize, maxPart int64) ([]cipherPartRange, error) {
	headerSize := int64(xph.HeaderSize)
	tagSize := int64(xph.TagSize)
	if sizePlain < 0 || sizeWire < headerSize || blockSize <= 0 || blockSize > math.MaxInt64-tagSize || maxPart <= headerSize {
		return nil, fmt.Errorf("backend: 分卷参数无效")
	}
	unit := blockSize + tagSize
	blocks := int64(0)
	if sizePlain > 0 {
		blocks = 1 + (sizePlain-1)/blockSize
	}
	firstCapacity := (maxPart - headerSize) / unit
	otherCapacity := maxPart / unit
	if firstCapacity == 0 || (blocks > firstCapacity && otherCapacity == 0) {
		return nil, fmt.Errorf("backend: 分卷上限小于一个加密块")
	}
	var out []cipherPartRange
	var blockIndex, cipherOffset int64
	for len(out) == 0 || blockIndex < blocks {
		capacity := otherCapacity
		first := len(out) == 0
		if first {
			capacity = firstCapacity
		}
		count := blocks - blockIndex
		if count > capacity {
			count = capacity
		}
		partStart := headerSize + blockIndex*unit
		if first {
			partStart = 0
		}
		partEnd := headerSize + (blockIndex+count)*unit
		if blockIndex+count == blocks {
			partEnd = sizeWire
		}
		wire := partEnd - partStart
		plainStart := blockIndex * blockSize
		plainEnd := plainStart + count*blockSize
		if plainEnd > sizePlain {
			plainEnd = sizePlain
		}
		if blocks == 0 {
			wire = headerSize
		}
		out = append(out, cipherPartRange{offset: cipherOffset, wireSize: wire, plainSize: plainEnd - plainStart})
		cipherOffset += wire
		blockIndex += count
	}
	if cipherOffset != sizeWire {
		return nil, fmt.Errorf("backend: 分卷边界计算与密文长度不一致 (%d != %d)", cipherOffset, sizeWire)
	}
	return out, nil
}

func (b *Pan123) putOne(ctx context.Context, req PutRequest) (PutResult, error) {
	parentID, err := b.EnsureDir(ctx, req.ParentDir)
	if err != nil {
		return PutResult{}, err
	}

	var created uploadCreateResp
	err = b.call(ctx, apiUploadCreate, b.limUploadCreate, http.MethodPost, jsonBody{
		"parentFileId": parentID,
		"filename":     req.LogicalName,
		"etag":         strings.ToLower(req.CipherMD5),
		"size":         req.SizeWire,
		"duplicate":    2,
		"containDir":   false,
	}, &created)
	if err != nil {
		return PutResult{}, err
	}
	if created.Data.Reuse && created.Data.FileID != 0 {
		return PutResult{ObjectRef: strconv.FormatInt(created.Data.FileID, 10)}, nil
	}
	if created.Data.PreuploadID == "" || created.Data.SliceSize <= 0 || len(created.Data.Servers) == 0 {
		return PutResult{}, fmt.Errorf("backend: 123 创建上传任务返回字段不完整")
	}
	// 分片边界与加密块边界错位后密文流无法跨片连续解密，Range 与预览都会
	// 读到垃圾，因此不对齐直接失败，没有绕过选项。
	if created.Data.SliceSize%req.BlockSize != 0 {
		return PutResult{}, fmt.Errorf("%w: 存储分片 %d 字节，加密块 %d 字节",
			ErrSliceMisaligned, created.Data.SliceSize, req.BlockSize)
	}

	if err := b.uploadSlices(ctx, req, &created); err != nil {
		return PutResult{}, err
	}

	fileID, err := b.finishUpload(ctx, created.Data.PreuploadID)
	if err != nil {
		return PutResult{}, err
	}
	return PutResult{ObjectRef: strconv.FormatInt(fileID, 10)}, nil
}

func deleteObjectParts(ctx context.Context, b *Pan123, parts []ObjectPart) error {
	var errs []error
	for _, part := range parts {
		if err := b.Delete(ctx, part.Ref); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *Pan123) uploadSlices(ctx context.Context, req PutRequest, created *uploadCreateResp) error {
	sliceSize := created.Data.SliceSize
	uploadDomain := strings.TrimRight(created.Data.Servers[0], "/")
	total := (req.SizeWire + sliceSize - 1) / sliceSize
	threads := b.cfg.UploadThreads
	if int64(threads) > total {
		threads = int(total)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		done     int64
	)
	jobs := make(chan uploadJob, threads)
	for worker := 0; worker < threads; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var (
					job uploadJob
					ok  bool
				)
				select {
				case <-ctx.Done():
					return
				case job, ok = <-jobs:
					if !ok {
						return
					}
				}
				if err := b.uploadSliceWithRetry(ctx, uploadDomain, created.Data.PreuploadID,
					job.sliceNo, req.Source, job.offset, job.length); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					cancel()
					return
				}
				if req.OnProgress != nil {
					mu.Lock()
					done += job.length
					uploaded := done
					mu.Unlock()
					req.OnProgress(uploaded)
				}
			}
		}()
	}

	// 只创建固定数量的 worker。旧实现为每个分片都启动 goroutine，超大
	// 文件会一次性创建数万 goroutine，虽然真正上传受 semaphore 限制，
	// 但调度器、栈和闭包仍会先把内存吃光。
producer:
	for index := int64(0); index < total; index++ {
		offset := index * sliceSize
		length := sliceSize
		if offset+length > req.SizeWire {
			length = req.SizeWire - offset
		}
		select {
		case jobs <- uploadJob{sliceNo: index + 1, offset: offset, length: length}:
		case <-ctx.Done():
			break producer
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("backend: 上传分片失败: %w", firstErr)
	}
	return nil
}

type uploadJob struct {
	sliceNo int64
	offset  int64
	length  int64
}

// sliceFormName 是分片文件字段的表单文件名。dry-run 计算长度与真实请求
// 必须使用同一表达式，否则 Content-Length 会与实际体长不一致。
func sliceFormName(sliceNo int64) string {
	return fmt.Sprintf("slice.%d", sliceNo)
}

// multipartBodyLength 计算 slice 上传请求体的精确总长度与 Content-Type。
//
// dry-run 用固定边界把"字段 + 文件头 + 收尾边界"写入临时缓冲（文件内容
// 不参与写入，其长度单独累加），得到与真实请求体逐字节等长的总长度。
func multipartBodyLength(fileLen int64, preuploadID string, sliceNo int64, sliceMD5 string) (total int64, contentType, boundary string) {
	const fixedBoundary = "xphSliceUploadBoundary"
	var probe bytes.Buffer
	mw := multipart.NewWriter(&probe)
	_ = mw.SetBoundary(fixedBoundary)
	_ = mw.WriteField("preuploadID", preuploadID)
	_ = mw.WriteField("sliceNo", strconv.FormatInt(sliceNo, 10))
	_ = mw.WriteField("sliceMD5", sliceMD5)
	if _, err := mw.CreateFormFile("slice", sliceFormName(sliceNo)); err != nil {
		return 0, mw.FormDataContentType(), fixedBoundary
	}
	_ = mw.Close()
	return int64(probe.Len()) + fileLen, mw.FormDataContentType(), fixedBoundary
}

// uploadSliceWithRetry 对单片上传做最多两次重试（间隔 1s/2s）。
//
// 同一分片重复上传是安全的：服务端按 preuploadID+sliceNo 覆盖。重试主要
// 吸收两类瞬时失败：偶发网络抖动，以及弱上行下贴着网关超时边缘的请求。
// 若带宽不足以在超时窗口内传完单片，重试也无法成功，会如实返回最后一次
// 的错误。
func (b *Pan123) uploadSliceWithRetry(ctx context.Context, domain, preuploadID string, sliceNo int64, src io.ReaderAt, offset, length int64) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = b.uploadSlice(ctx, domain, preuploadID, sliceNo, src, offset, length)
		if lastErr == nil {
			return nil
		}
		// 带宽不足是部署侧的网络条件问题，重试解决不了，只会让用户白等
		// （三次尝试加退避约 3 秒，而结果注定相同）。立即透传。
		if IsUploadBandwidthError(lastErr) {
			return lastErr
		}
		if attempt+1 == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return lastErr
}

// uploadSlice 上传单片。片内 MD5 由本函数按实际读取的字节计算，
// 因此与调用方的缓冲策略无关。
func (b *Pan123) uploadSlice(ctx context.Context, domain, preuploadID string, sliceNo int64, src io.ReaderAt, offset, length int64) error {
	start := time.Now()
	section := io.NewSectionReader(src, offset, length)

	sum := md5.New()
	if _, err := io.Copy(sum, section); err != nil {
		return fmt.Errorf("计算分片 %d 的哈希失败: %w", sliceNo, err)
	}
	sliceMD5 := hex.EncodeToString(sum.Sum(nil))
	if _, err := section.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// 先完成所有可能在请求体建立前失败的步骤。否则后台写 Pipe 的
	// goroutine 会在 token/URL 构造失败时永远阻塞。
	token, err := b.accessToken(ctx, false)
	if err != nil {
		return err
	}
	if b.limGlobal != nil {
		if err := b.limGlobal.wait(ctx); err != nil {
			return err
		}
	}
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 请求必须带 Content-Length：123 的上传网关对 chunked 编码的大请求体
	// 会在约 60 秒后返回一个空响应体的 HTTP 500。长度用固定边界的 dry-run
	// 精确得出，真实请求体以同一边界构造，两者逐字节等长。
	contentLength, contentType, boundary := multipartBodyLength(length, preuploadID, sliceNo, sliceMD5)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	_ = mw.SetBoundary(boundary)
	done := make(chan struct{})
	defer func() {
		cancel()
		_ = pw.Close()
		<-done
	}()
	go func() {
		defer close(done)
		var werr error
		defer func() {
			if cerr := mw.Close(); cerr != nil && werr == nil {
				werr = cerr
			}
			_ = pw.CloseWithError(werr)
		}()
		for _, field := range [][2]string{
			{"preuploadID", preuploadID},
			{"sliceNo", strconv.FormatInt(sliceNo, 10)},
			{"sliceMD5", sliceMD5},
		} {
			if werr = mw.WriteField(field[0], field[1]); werr != nil {
				return
			}
		}
		var part io.Writer
		if part, werr = mw.CreateFormFile("slice", sliceFormName(sliceNo)); werr != nil {
			return
		}
		if _, werr = io.Copy(part, section); werr != nil {
			return
		}
	}()

	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, domain+"/upload/v2/file/slice", pr)
	if err != nil {
		_ = pw.CloseWithError(err)
		<-done
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("platform", "open_platform")
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = contentLength

	res, err := b.upload.Do(req)
	if err != nil {
		_ = pw.CloseWithError(err)
		cancel()
		<-done
		return fmt.Errorf("分片 %d 上传失败: %w", sliceNo, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		cancel()
		_ = pw.Close()
		<-done
		return fmt.Errorf("分片 %d 响应读取失败: %w", sliceNo, err)
	}
	if len(raw) > 1<<20 {
		cancel()
		_ = pw.Close()
		<-done
		return fmt.Errorf("分片 %d 响应过大", sliceNo)
	}
	if res.StatusCode != http.StatusOK {
		cancel()
		_ = pw.Close()
		<-done
		// 123 的分片端点失败原因只在响应体里（权益、大小、限流各不相同），
		// 丢弃响应体会把可定位的问题变成一句"HTTP 500"。raw 已是完整响应体。
		bodyText := strings.TrimSpace(string(raw))
		// 网关读超时的两个签名：响应体自带 "i/o timeout"，或响应体为空但
		// 请求耗时贴着 60s。命中即归类为带宽不足，向上给出专属提示。
		if res.StatusCode == http.StatusInternalServerError &&
			(strings.Contains(bodyText, "i/o timeout") ||
				strings.Contains(bodyText, "Read from request Body") ||
				time.Since(start) >= sliceBodyTimeoutFloor) {
			return fmt.Errorf("分片 %d 上传失败，HTTP %d: %s: %w", sliceNo, res.StatusCode, bodyText, ErrUploadBandwidth)
		}
		return fmt.Errorf("分片 %d 上传失败，HTTP %d: %s", sliceNo, res.StatusCode, bodyText)
	}
	var envelope baseResp
	if err := json.Unmarshal(raw, &envelope); err != nil {
		cancel()
		_ = pw.Close()
		<-done
		return fmt.Errorf("分片 %d 响应解析失败: %w", sliceNo, err)
	}
	if envelope.Code != 0 {
		cancel()
		_ = pw.Close()
		<-done
		return fmt.Errorf("分片 %d 上传失败: %s", sliceNo, envelope.Message)
	}
	<-done
	return nil
}

// finishUpload 轮询上传完成接口。接口文档要求返回 completed=false 时
// 间隔 1 秒继续轮询，直到拿到最终 fileID。
func (b *Pan123) finishUpload(ctx context.Context, preuploadID string) (int64, error) {
	attempts := b.cfg.PollAttempts
	interval := b.cfg.PollInterval
	var lastMsg string
	for i := 0; i < attempts; i++ {
		var resp uploadCompleteResp
		// 该接口在错误码未知时也会返回，因此错误只记录不立即失败。
		err := b.call(ctx, apiUploadFinish, nil, http.MethodPost, jsonBody{"preuploadID": preuploadID}, &resp)
		if err == nil && resp.Data.Completed && resp.Data.FileID != 0 {
			return resp.Data.FileID, nil
		}
		if err != nil {
			lastMsg = err.Error()
		}
		if i+1 == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(interval):
		}
	}
	if lastMsg != "" {
		return 0, fmt.Errorf("backend: 等待上传完成超时（最后错误：%s）", lastMsg)
	}
	return 0, fmt.Errorf("backend: 等待上传完成超时")
}

// Delete 删除对象。对象不存在视为成功（幂等），便于失败清理重试。
func (b *Pan123) Delete(ctx context.Context, ref string) error {
	id, err := parseFileID(ref)
	if err != nil {
		return err
	}
	if err := b.call(ctx, apiTrash, b.limTrash, http.MethodPost, jsonBody{"fileIDs": []int64{id}}, nil); err != nil && !strings.Contains(err.Error(), "不存在") {
		return err
	}
	return nil
}

// Open 打开对象用于顺序或随机读取（服务端解密通道使用）。
//
// 取流地址按"直链流量优先、自用流量兜底"的顺序获取：直链空间的流量独立
// 计费，不受自用下载每日 1GB 上限约束；直链通道任一环节不可用（未开启
// 直链空间、直链流量用尽，或 CDN 回源鉴权开启导致服务端自取被拒）时静默
// 改走自用下载通道，保证交付不中断。
func (b *Pan123) Open(ctx context.Context, ref string) (io.ReadSeekCloser, error) {
	id, err := parseFileID(ref)
	if err != nil {
		return nil, err
	}
	link, size, err := b.relayLink(ctx, id)
	if err != nil {
		return nil, err
	}
	// Open 的契约是可 Seek 的 ReadSeekCloser；没有对象长度就无法实现
	// SeekEnd，不能把 HEAD 失败静默降级成一个半可用的 reader。
	return &httpRangeReader{client: b.client, url: link, size: size, ctx: ctx}, nil
}

// Stat 返回对象在存储侧的字节数。
//
// 123 没有按 fileID 直接取元数据的接口，因此对读取地址做一次 HEAD；
// 地址通道与 Open 完全相同（直链流量优先、自用流量兜底）。
func (b *Pan123) Stat(ctx context.Context, ref string) (int64, error) {
	id, err := parseFileID(ref)
	if err != nil {
		return 0, err
	}
	_, size, err := b.relayLink(ctx, id)
	return size, err
}

// relayLink 为服务端中转换取一个已经过 HEAD 验证的读取地址。
//
// 决策链固定为：直链流量（direct-link/url + auth_key）→ 自用流量
// （download_info）。回退只在这一层决策：直链地址即使签发成功，也可能
// 在 CDN 侧被拒（例如开启了回源鉴权回调、服务端请求无法携带访客票据），
// 因此必须以 HEAD 实际验证通过后才算通道可用。
func (b *Pan123) relayLink(ctx context.Context, id int64) (string, int64, error) {
	if strings.TrimSpace(b.cfg.PrivateKey) != "" {
		link, size, err := b.relayLinkVia(ctx, id, true)
		if err == nil {
			return link, size, nil
		}
		log.Printf("backend: fileID=%d 直链流量通道不可用，回退自用下载通道: %v", id, err)
	}
	return b.relayLinkVia(ctx, id, false)
}

// relayLinkVia 按指定通道换取地址并做 HEAD 验证。
func (b *Pan123) relayLinkVia(ctx context.Context, id int64, direct bool) (string, int64, error) {
	var (
		link string
		err  error
	)
	if direct {
		// 服务端自取不附票据参数：票据核销在本系统的服务端中转入口已经
		// 完成，CDN 回源回调不能也不应再消费同一张票据。
		link, err = b.signedDirectLink(ctx, id, b.cfg.DirectLinkTTL, "")
	} else {
		link, err = b.selfDownloadURL(ctx, id)
	}
	if err != nil {
		return "", 0, err
	}
	size, err := b.headSize(ctx, link)
	if err != nil {
		return "", 0, err
	}
	return link, size, nil
}

func (b *Pan123) headSize(ctx context.Context, link string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link, nil)
	if err != nil {
		return 0, err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("backend: 探测对象长度失败: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("backend: 探测对象长度失败，HTTP %d", res.StatusCode)
	}
	if res.ContentLength < 0 {
		return 0, fmt.Errorf("backend: 对象长度未知")
	}
	return res.ContentLength, nil
}

// selfDownloadURL 通过自用下载通道（download_info）取对象地址。
func (b *Pan123) selfDownloadURL(ctx context.Context, id int64) (string, error) {
	var resp downloadInfoResp
	if err := b.call(ctx, fmt.Sprintf("%s?fileId=%d", apiDownloadInfo, id), b.limDownloadInfo, http.MethodGet, nil, &resp); err != nil {
		return "", err
	}
	if resp.Data.DownloadURL == "" {
		return "", fmt.Errorf("backend: 123 未返回下载地址")
	}
	return resp.Data.DownloadURL, nil
}

// Presign 返回带鉴权信息的直链。
func (b *Pan123) Presign(ctx context.Context, ref string, opt PresignOptions) (string, error) {
	if strings.TrimSpace(b.cfg.PrivateKey) == "" {
		return "", ErrPresignUnavailable
	}
	id, err := parseFileID(ref)
	if err != nil {
		return "", err
	}
	ttl := opt.TTL
	if ttl <= 0 {
		ttl = b.cfg.DirectLinkTTL
	}
	return b.signedDirectLink(ctx, id, ttl, opt.TicketID)
}

// PresignParts 为逻辑对象中的每个物理卷分别签发直链。
func (b *Pan123) PresignParts(ctx context.Context, ref string, size int64, opt PresignOptions) ([]PresignedPart, error) {
	parts, err := SplitObjectRef(ref, size)
	if err != nil {
		return nil, err
	}
	out := make([]PresignedPart, 0, len(parts))
	for _, part := range parts {
		url, err := b.Presign(ctx, part.Ref, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, PresignedPart{URL: url, Offset: part.Offset, Size: part.Size})
	}
	return out, nil
}

// signedDirectLink 经直链流量通道换取原始地址并完成 URL 鉴权签名。
//
// ticketID 非空时追加为票据参数：浏览器直连跨域 CDN 时自定义请求头会触发
// 预检，票据只能走 URL；服务端自取（服务端中转）传空。
func (b *Pan123) signedDirectLink(ctx context.Context, id int64, ttl time.Duration, ticketID string) (string, error) {
	var resp directLinkResp
	if err := b.call(ctx, fmt.Sprintf("%s?fileID=%d", apiDirectLink, id), b.limDirectLink, http.MethodGet, nil, &resp); err != nil {
		return "", err
	}
	raw := strings.TrimSpace(resp.Data.URL)
	if raw == "" {
		return "", ErrPresignUnavailable
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("backend: 直链无法解析: %w", err)
	}
	query := parsed.Query()
	uid, err := b.uidValue(ctx)
	if err != nil {
		return "", err
	}
	authKey, err := signAuthKey(parsed.EscapedPath(), b.cfg.PrivateKey, uid, ttl)
	if err != nil {
		return "", err
	}
	query.Set("auth_key", authKey)
	if ticketID != "" {
		query.Set("xph", ticketID)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// SetDirectLink 对配置的存放根目录启用或关闭直链空间。
//
// 123 要求文件所在文件夹先开启直链空间，direct-link/url 才能为其中文件签发
// 直链；启用与关闭接口都幂等，重复执行仍返回文件夹名。网盘根目录（fileID 0）
// 不能作为开关目标，必须显式配置一个根目录编号。
func (b *Pan123) SetDirectLink(ctx context.Context, enabled bool) (string, error) {
	endpoint := apiDirectLinkEnable
	action := "启用"
	if !enabled {
		endpoint = apiDirectLinkDisable
		action = "关闭"
	}
	id, err := b.rootID()
	if err != nil {
		return "", err
	}
	if id == 0 {
		return "", fmt.Errorf("backend: 未配置根目录编号，不能对网盘根目录%s直链空间", action)
	}
	var resp directLinkSwitchResp
	if err := b.call(ctx, endpoint, b.limDirectLink, http.MethodPost,
		jsonBody{"fileID": id}, &resp); err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Data.Filename) == "" {
		return "", fmt.Errorf("backend: %s直链空间未返回文件夹名称", action)
	}
	return resp.Data.Filename, nil
}

func parseFileID(ref string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(ref), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("backend: 非法对象定位符 %q", ref)
	}
	return id, nil
}

// signAuthKey 按 123 的 URL 鉴权规则生成 auth_key。
//
// 规则：待签串为 path-timestamp-rand-uid-privateKey，取其 md5，
// 最终参数形如 timestamp-rand-uid-md5。
// path 必须是 URL 的**原始转义形态**（url.URL.EscapedPath）：CDN 按请求
// URL 里的字面路径验签，用解码后的 Path 参签会在路径含转义字符时验不过。
func signAuthKey(path, privateKey string, uid uint64, ttl time.Duration) (string, error) {
	ts := time.Now().Add(ttl).Unix()
	randPart, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return authKeyOf(path, privateKey, uid, ts, randPart), nil
}

func authKeyOf(path, privateKey string, uid uint64, ts int64, randPart string) string {
	unsigned := fmt.Sprintf("%s-%d-%s-%d-%s", path, ts, randPart, uid, privateKey)
	sum := md5.Sum([]byte(unsigned))
	return fmt.Sprintf("%d-%s-%d-%x", ts, randPart, uid, sum)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("backend: 读取随机数失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ---------------------------------------------------------------- 远程随机读

// httpRangeReader 把远端对象包装成可随机读的流。
//
// 每次 Read 只在必要时发起一次 Range 请求并缓冲一个读取窗口，因此顺序读的
// 网络开销与窗口大小成反比，而随机跳读的代价与一次 Range 请求相当。
type httpRangeReader struct {
	client *http.Client
	url    string
	ctx    context.Context
	size   int64
	pos    int64
	buf    []byte
	bufAt  int64
	// 上游忽略 Range 并返回 200 时保留顺序响应流，避免后续窗口反复从
	// 对象开头下载并丢弃前缀，使大文件读取退化为 O(n²) 流量。
	fallback io.ReadCloser
	closed   bool
}

func (r *httpRangeReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.bufAt <= r.pos && r.pos < r.bufAt+int64(len(r.buf)) {
		n := copy(p, r.buf[r.pos-r.bufAt:])
		r.pos += int64(n)
		return n, nil
	}
	if err := r.fetchWindow(); err != nil {
		return 0, err
	}
	if len(r.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.buf)
	r.pos += int64(n)
	return n, nil
}

func (r *httpRangeReader) fetchWindow() error {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if r.pos < 0 || r.pos > int64(^uint64(0)>>1)-rangeReadChunk+1 {
		return fmt.Errorf("backend: 范围读取位置溢出")
	}
	// 对象长度在 Open 时已通过 HEAD 取得。读指针越过 EOF 时直接返回空窗口，
	// 让 Read 产生 io.EOF。
	if r.size >= 0 && r.pos >= r.size {
		r.buf = nil
		r.bufAt = r.pos
		return nil
	}
	if r.fallback != nil {
		return r.readFallbackWindow()
	}
	end := r.pos + rangeReadChunk - 1
	// 区间末端必须截断到对象最后一个字节：部分上游 CDN 不按 RFC 把越界区间
	// 收敛为实际长度，而是直接回 416，导致小于读取窗口的对象（含 1 字节
	// 探针）整段不可读。
	if r.size >= 0 && end >= r.size {
		end = r.size - 1
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}
	// 显式区间：后缀式 bytes=-N 会被 CDN 的预检规则拒绝。
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.pos, end))
	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("backend: 范围读取失败: %w", err)
	}
	keepBody := false
	defer func() {
		if !keepBody {
			_ = res.Body.Close()
		}
	}()
	if res.StatusCode != http.StatusPartialContent && res.StatusCode != http.StatusOK {
		return fmt.Errorf("backend: 范围读取失败，HTTP %d", res.StatusCode)
	}
	// 某些上游会忽略 Range 并返回 200 + 整个对象。不能直接 ReadAll，
	// 否则一次从文件尾部读取就会把整份大对象载入内存。先流式跳到起始
	// 偏移，再保留该响应流、逐窗口读取；对 206 响应拒绝超出窗口的异常响应。
	var data []byte
	if res.StatusCode == http.StatusOK {
		if r.pos > 0 {
			if _, err := io.CopyN(io.Discard, res.Body, r.pos); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					r.buf = nil
					r.bufAt = r.pos
					return nil
				}
				return fmt.Errorf("backend: 跳过对象前缀失败: %w", err)
			}
		}
		r.fallback = res.Body
		keepBody = true
		if r.size < 0 && res.ContentLength >= 0 {
			r.size = res.ContentLength
		}
		return r.readFallbackWindow()
	} else {
		data, err = io.ReadAll(io.LimitReader(res.Body, rangeReadChunk+1))
		if err == nil && len(data) > rangeReadChunk {
			return fmt.Errorf("backend: 范围响应超过读取窗口")
		}
		if err == nil {
			start, end, total, ok := parseContentRange(res.Header.Get("Content-Range"))
			if !ok || start != r.pos || end < start || end-start+1 != int64(len(data)) {
				return fmt.Errorf("backend: 上游范围响应与请求不一致")
			}
			if total > 0 && r.size >= 0 && total != r.size {
				return fmt.Errorf("backend: 上游对象长度发生变化")
			}
		}
	}
	if err != nil {
		return fmt.Errorf("backend: 读取响应体失败: %w", err)
	}

	r.buf = data
	r.bufAt = r.pos
	if r.size < 0 && res.ContentLength > 0 {
		if res.StatusCode == http.StatusOK {
			r.size = res.ContentLength
		} else {
			r.size = r.bufAt + res.ContentLength
		}
	}
	return nil
}

// readFallbackWindow 消费保留的顺序响应；短窗口标记 EOF，避免未知长度时重复请求。
func (r *httpRangeReader) readFallbackWindow() error {
	limit := int64(rangeReadChunk)
	if r.size >= 0 && r.size-r.pos < limit {
		limit = r.size - r.pos
	}
	data, err := io.ReadAll(io.LimitReader(r.fallback, limit))
	if err != nil || int64(len(data)) < limit || (r.size >= 0 && r.pos+int64(len(data)) >= r.size) {
		_ = r.fallback.Close()
		r.fallback = nil
	}
	if err != nil {
		return fmt.Errorf("backend: 顺序读取响应失败: %w", err)
	}
	if int64(len(data)) < limit && r.size < 0 {
		r.size = r.pos + int64(len(data))
	}
	r.buf, r.bufAt = data, r.pos
	return nil
}

// parseContentRange 解析标准的 `bytes start-end/total` 响应头。
// Range 读取若不确认起止位置，恶意或失配的 CDN 响应可能把另一段密文
// 喂给解密器，最终表现为随机认证失败而不是明确的上游错误。
func parseContentRange(raw string) (start, end, total int64, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "bytes ") {
		return 0, 0, 0, false
	}
	raw = strings.TrimPrefix(raw, "bytes ")
	rangePart, totalPart, found := strings.Cut(raw, "/")
	if !found {
		return 0, 0, 0, false
	}
	startRaw, endRaw, found := strings.Cut(strings.TrimSpace(rangePart), "-")
	if !found {
		return 0, 0, 0, false
	}
	var err error
	start, err = strconv.ParseInt(strings.TrimSpace(startRaw), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, 0, false
	}
	end, err = strconv.ParseInt(strings.TrimSpace(endRaw), 10, 64)
	if err != nil || end < start {
		return 0, 0, 0, false
	}
	totalRaw := strings.TrimSpace(totalPart)
	if totalRaw == "*" {
		return start, end, -1, true
	}
	total, err = strconv.ParseInt(totalRaw, 10, 64)
	if err != nil || total <= end {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

func (r *httpRangeReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		var ok bool
		target, ok = addInt64(r.pos, offset)
		if !ok {
			return 0, fmt.Errorf("backend: Seek 位置溢出")
		}
	case io.SeekEnd:
		if r.size < 0 {
			return 0, fmt.Errorf("backend: 尚未获知对象长度，无法从末尾定位")
		}
		var ok bool
		target, ok = addInt64(r.size, offset)
		if !ok {
			return 0, fmt.Errorf("backend: Seek 位置溢出")
		}
	default:
		return 0, fmt.Errorf("backend: 不支持的 Seek 模式")
	}
	if target < 0 {
		return 0, fmt.Errorf("backend: Seek 位置无效")
	}
	bufferEnd := r.bufAt + int64(len(r.buf))
	if r.fallback != nil && (target < r.bufAt || target > bufferEnd) {
		_ = r.fallback.Close()
		r.fallback = nil
	}
	r.pos = target
	return target, nil
}

func addInt64(a, b int64) (int64, bool) {
	const max = int64(^uint64(0) >> 1)
	const min = -max - 1
	if b > 0 && a > max-b {
		return 0, false
	}
	if b < 0 && a < min-b {
		return 0, false
	}
	return a + b, true
}

func (r *httpRangeReader) Close() error {
	r.closed = true
	r.buf = nil
	if r.fallback != nil {
		err := r.fallback.Close()
		r.fallback = nil
		return err
	}
	return nil
}
