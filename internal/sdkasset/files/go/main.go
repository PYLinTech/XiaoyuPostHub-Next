// XiaoyuPostHub-Next Go SDK 示例（v4）。
//
// 站点地址默认从 ../config.json 读取，可在 NewClient 的 baseURL 参数覆盖；
// API Key 只来自参数或 XIAOYU_API_KEY。
// 版本查询是公开接口，不需要 API Key；其余请求都需要 Key。
// 所有 path 参数都相对于 Key 的工作区，"/" 就是工作区根目录。
// 只做存储集成时直接使用 Upload/Download，无需创建分享或直链。
// 覆盖文件树、上传任务、下载/预览、分享与取件码、直链、归档和公告等用户侧操作。
// 密文计划在本地完成 XPH 解密并核对文件头与交付记录；明文计划同样核对
// 长度和 SHA-256，校验通过后才替换目标文件。
//
// 两类交付示例：
//
//	--serve-relay   方式 1：后端直连，由后端做中转（密文计划在本地解密，文件字节经本服务转发）；
//	--serve-direct  方式 2：后端只请求地址，浏览器拿 123 地址直接取密文并本地解密。
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	uploadWorkers = 3
	partWorkers   = 4
)

// APIError 是服务端返回的业务错误；Status 为 0 表示网络层失败。
type APIError struct {
	Status     int
	Message    string
	Detail     string
	RetryAfter int
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s：%s", e.Message, e.Detail)
	}
	return e.Message
}

// Client 是一枚 API Key 对应的用户侧客户端。
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewClient 构造客户端。apiKey / baseURL 为空时回落到环境变量与 config.json。
func NewClient(apiKey, baseURL string) (*Client, error) {
	if apiKey == "" {
		apiKey = os.Getenv("XIAOYU_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("缺少 API Key：设置 XIAOYU_API_KEY 或传入 apiKey")
	}
	resolved, err := resolveBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL: resolved,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 0},
	}, nil
}

func resolveBaseURL(baseURL string) (string, error) {
	if baseURL == "" {
		var err error
		baseURL, err = loadBaseURL()
		if err != nil {
			return "", err
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("站点地址必须以 http:// 或 https:// 开头")
	}
	return baseURL, nil
}

func loadBaseURL() (string, error) {
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "config.json"))
	if err != nil {
		return "", fmt.Errorf("读取 config.json 失败: %w", err)
	}
	var cfg struct {
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("解析 config.json 失败: %w", err)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return "", errors.New("config.json 缺少 baseUrl")
	}
	return cfg.BaseURL, nil
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
	} `json:"error"`
}

func (c *Client) do(ctx context.Context, method, endpoint string, body any, out any) error {
	return requestJSON(ctx, c.HTTP, c.BaseURL, c.APIKey, method, endpoint, body, out)
}

func requestJSON(ctx context.Context, httpClient *http.Client, baseURL, apiKey, method, endpoint string, body, out any) error {
	var reader io.Reader
	contentType := ""
	switch value := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(value)
		contentType = "application/octet-stream"
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+endpoint, reader)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &APIError{Status: 0, Message: "网络连接失败，请检查网络后重试", Detail: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode >= 400 || env.Error != nil {
		message := fmt.Sprintf("HTTP %d", resp.StatusCode)
		detail := string(raw)
		if env.Error != nil {
			message = env.Error.Message
			detail = env.Error.Detail
		}
		return &APIError{
			Status: resp.StatusCode, Message: message, Detail: detail,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return err
		}
	}
	return nil
}

func parseRetryAfter(raw string) int {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds <= 0 {
		return 0
	}
	return seconds
}

// SDKVersion 查询站点公开的示例 SDK 版本，不需要 API Key。
func SDKVersion(ctx context.Context, baseURL string) (string, error) {
	resolved, err := resolveBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	return fetchSDKVersion(ctx, http.DefaultClient, resolved)
}

// SDKVersion 使用方法所属 Client 的地址查询版本；该公开接口不发送 API Key。
func (c *Client) SDKVersion(ctx context.Context) (string, error) {
	return fetchSDKVersion(ctx, c.HTTP, c.BaseURL)
}

func fetchSDKVersion(ctx context.Context, httpClient *http.Client, baseURL string) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := requestJSON(ctx, httpClient, baseURL, "", http.MethodGet, "/api/sdk/version", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// ---------------------------------------------------------------- 文件树

func (c *Client) Profile(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/auth/profile", nil, &out)
	return out, err
}

func (c *Client) List(ctx context.Context, path string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/fs/list?path="+url.QueryEscape(path), nil, &out)
	return out, err
}

func (c *Client) Stat(ctx context.Context, path string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/fs/stat?path="+url.QueryEscape(path), nil, &out)
	return out, err
}

func (c *Client) Stats(ctx context.Context, path string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/fs/stats?path="+url.QueryEscape(path), nil, &out)
	return out, err
}

func (c *Client) Mkdir(ctx context.Context, parent, name string) error {
	return c.do(ctx, http.MethodPost, "/api/fs/mkdir", map[string]any{"parentPath": parent, "name": name}, nil)
}

func (c *Client) Rename(ctx context.Context, path, newName string) error {
	return c.do(ctx, http.MethodPost, "/api/fs/rename", map[string]any{"path": path, "newName": newName}, nil)
}

func (c *Client) Move(ctx context.Context, path, destParent string) error {
	return c.do(ctx, http.MethodPost, "/api/fs/move", map[string]any{"path": path, "destParent": destParent}, nil)
}

func (c *Client) Remove(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodPost, "/api/fs/delete", map[string]any{"path": path}, nil)
}

// ---------------------------------------------------------------- 上传

// UploadResult 是初始化秒传或收尾轮询的完整响应。
type UploadResult struct {
	Dedup         bool           `json:"dedup"`
	Node          map[string]any `json:"node,omitempty"`
	SessionID     string         `json:"sessionId,omitempty"`
	ChunkSize     int64          `json:"chunkSize,omitempty"`
	ChunkTotal    int            `json:"chunkTotal,omitempty"`
	Received      []int          `json:"received,omitempty"`
	ExpiresAt     int64          `json:"expiresAt,omitempty"`
	State         string         `json:"state,omitempty"`
	FileName      string         `json:"fileName,omitempty"`
	Message       string         `json:"message,omitempty"`
	Error         string         `json:"error,omitempty"`
	ProgressBytes int64          `json:"progressBytes,omitempty"`
	TotalBytes    int64          `json:"totalBytes,omitempty"`
}

// Upload 完成一次完整上传：秒传探测、分片、收尾与状态轮询。
func (c *Client) Upload(ctx context.Context, localPath, parent, name, conflictAction string) (UploadResult, error) {
	if name == "" {
		name = filepath.Base(localPath)
	}
	if conflictAction == "" {
		conflictAction = "rename"
	}
	file, err := os.Open(localPath)
	if err != nil {
		return UploadResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return UploadResult{}, err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return UploadResult{}, err
	}
	var init UploadResult
	if err := c.do(ctx, http.MethodPost, "/api/upload/init", map[string]any{
		"checksum":       hex.EncodeToString(digest.Sum(nil)),
		"sizePlain":      info.Size(),
		"parentPath":     parent,
		"name":           name,
		"conflictAction": conflictAction,
	}, &init); err != nil {
		return UploadResult{}, err
	}
	if init.Dedup {
		return init, nil
	}
	received := make(map[int]bool, len(init.Received))
	for _, index := range init.Received {
		received[index] = true
	}
	pending := make([]int, 0, init.ChunkTotal)
	for index := 0; index < init.ChunkTotal; index++ {
		if !received[index] {
			pending = append(pending, index)
		}
	}
	session := url.PathEscape(init.SessionID)
	if len(pending) > 0 {
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		jobs := make(chan int, len(pending))
		for _, index := range pending {
			jobs <- index
		}
		close(jobs)
		var wg sync.WaitGroup
		var once sync.Once
		var firstErr error
		fail := func(err error) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
		for worker := 0; worker < min(uploadWorkers, len(pending)); worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range jobs {
					if runCtx.Err() != nil {
						return
					}
					offset := int64(index) * init.ChunkSize
					length := init.ChunkSize
					if remain := info.Size() - offset; remain < length {
						length = remain
					}
					if length <= 0 {
						fail(errors.New("读取本地文件失败：文件在上传过程中被截断"))
						return
					}
					buf := make([]byte, length)
					n, err := file.ReadAt(buf, offset)
					if err != nil && !errors.Is(err, io.EOF) {
						fail(err)
						return
					}
					if n != len(buf) {
						fail(fmt.Errorf("读取本地文件失败：偏移 %d 期望 %d 字节，实得 %d 字节",
							offset, len(buf), n))
						return
					}
					if err := c.do(runCtx, http.MethodPut,
						fmt.Sprintf("/api/upload/%s/chunk/%d", session, index), buf, nil); err != nil {
						fail(err)
						return
					}
				}
			}()
		}
		wg.Wait()
		if firstErr != nil {
			return UploadResult{}, firstErr
		}
	}
	if err := c.do(ctx, http.MethodPost, "/api/upload/"+session+"/complete", nil, nil); err != nil {
		return UploadResult{}, err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var job UploadResult
		if err := c.do(ctx, http.MethodGet, "/api/upload/"+session, nil, &job); err != nil {
			return UploadResult{}, err
		}
		switch job.State {
		case "done":
			return job, nil
		case "error":
			return UploadResult{}, fmt.Errorf("上传失败：%s", job.Error)
		}
		select {
		case <-ctx.Done():
			return UploadResult{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return UploadResult{}, errors.New("上传收尾超时")
}

func (c *Client) UploadTasks(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/upload/tasks", nil, &out)
	return out, err
}

func (c *Client) UploadStatus(ctx context.Context, session string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/upload/"+url.PathEscape(session), nil, &out)
	return out, err
}

func (c *Client) CancelUpload(ctx context.Context, session string) error {
	return c.do(ctx, http.MethodPost, "/api/upload/"+url.PathEscape(session)+"/cancel", nil, nil)
}

func (c *Client) ResolveUpload(ctx context.Context, session, checksum string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, "/api/upload/"+url.PathEscape(session)+"/resolve",
		map[string]any{"checksum": checksum}, &out)
	return out, err
}

// ---------------------------------------------------------------- 下载

type DeliveryMeta struct {
	ChunkLog2   int    `json:"chunkLog2"`
	PlainSize   int64  `json:"plainSize"`
	CipherSize  int64  `json:"cipherSize"`
	NoncePrefix uint32 `json:"noncePrefix"`
	Checksum    string `json:"checksum"`
	KeyEnvelope string `json:"keyEnvelope"`
}

type PresignedPart struct {
	URL    string `json:"url"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

type DeliveryPlan struct {
	Purpose       string          `json:"purpose"`
	FileName      string          `json:"fileName"`
	MimeType      string          `json:"mimeType"`
	Checksum      string          `json:"checksum"`
	Mode          string          `json:"mode"`
	URL           string          `json:"url"`
	Parts         []PresignedPart `json:"parts"`
	StreamURL     string          `json:"streamUrl"`
	ContentForm   string          `json:"contentForm"`
	Encryption    *DeliveryMeta   `json:"encryption"`
	TicketID      string          `json:"ticketId"`
	PlainSize     int64           `json:"plainSize"`
	ReservedBytes int64           `json:"reservedBytes"`
	ExpiresAt     int64           `json:"expiresAt"`
}

// PrepareDownload 请求交付计划；函数只协商通道与密钥，不下载任何字节。
//
// clientPublicKey 是调用方现场生成的临时公钥（base64 SPKI DER）。密文计划会
// 附带用该公钥封装的内容密钥信封；明文计划不含密钥材料。
func (c *Client) PrepareDownload(ctx context.Context, path, clientPublicKey string, preview bool) (DeliveryPlan, error) {
	endpoint := "/api/fs/download"
	if preview {
		endpoint = "/api/fs/preview"
	}
	var plan DeliveryPlan
	if err := c.do(ctx, http.MethodPost, endpoint, map[string]any{
		"path":            path,
		"clientPublicKey": clientPublicKey,
	}, &plan); err != nil {
		return DeliveryPlan{}, err
	}
	return plan, nil
}

// Settle 回执一张交付票据。密文通道按预扣额度结算，completed 只标记本次
// 客户端是否完整接收；明文通道在开流时由服务端实测结算，无需调用。
func (c *Client) Settle(ctx context.Context, ticketID string, completed bool) error {
	ticketID = strings.TrimSpace(ticketID)
	if ticketID == "" {
		return nil
	}
	return c.do(ctx, http.MethodPost, "/api/fs/settle", map[string]any{
		"ticketId": ticketID, "completed": completed,
	}, nil)
}

// Download 下载并（除服务端明文交付外）在本地解密，返回交付计划。
func (c *Client) Download(ctx context.Context, path, dest string, preview bool) (DeliveryPlan, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return DeliveryPlan{}, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return DeliveryPlan{}, err
	}
	plan, err := c.PrepareDownload(ctx, path, base64.StdEncoding.EncodeToString(pub), preview)
	if err != nil {
		return DeliveryPlan{}, err
	}
	if plan.ContentForm == "plaintext" {
		if plan.StreamURL == "" {
			return DeliveryPlan{}, errors.New("交付计划缺少可下载地址")
		}
		return plan, c.savePlain(ctx, plan.StreamURL, dest, plan)
	}
	if plan.Encryption == nil {
		return DeliveryPlan{}, errors.New("交付计划缺少解密材料")
	}
	temp, err := os.CreateTemp("", "xph-*.bin")
	if err != nil {
		return DeliveryPlan{}, err
	}
	tempPath := temp.Name()
	_ = temp.Close()
	defer os.Remove(tempPath)
	fail := func(err error) (DeliveryPlan, error) {
		if plan.TicketID != "" {
			_ = c.Settle(ctx, plan.TicketID, false)
		}
		return DeliveryPlan{}, err
	}
	parts, err := orderedParts(plan.Parts, plan.Encryption.CipherSize)
	if err != nil {
		return fail(err)
	}
	switch {
	case plan.Mode == "direct" && len(parts) > 0:
		if err := c.saveParts(ctx, parts, tempPath); err != nil {
			return fail(err)
		}
	case plan.Mode == "direct" && plan.URL != "":
		if err := c.saveURL(ctx, plan.URL, tempPath, false); err != nil {
			return fail(err)
		}
	case plan.StreamURL != "":
		if err := c.saveURL(ctx, plan.StreamURL, tempPath, true); err != nil {
			return fail(err)
		}
	default:
		return fail(errors.New("交付计划缺少可下载地址"))
	}
	if err := decryptFile(tempPath, dest, plan, privateKey); err != nil {
		return fail(err)
	}
	if plan.TicketID != "" {
		_ = c.Settle(ctx, plan.TicketID, true)
	}
	return plan, nil
}

func (c *Client) resolveDownloadURL(rawURL string) (string, error) {
	target := rawURL
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = c.BaseURL + target
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("不支持的下载地址协议")
	}
	return target, nil
}

// siteURL 只接受站内相对路径：认证请求不能把 API Key 发往交付计划里的外部地址。
func (c *Client) siteURL(raw string) (string, error) {
	if !strings.HasPrefix(raw, "/") {
		return "", errors.New("交付计划的中转地址不正确")
	}
	return c.BaseURL + raw, nil
}

func downloadResponseError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &APIError{
		Status: resp.StatusCode, Message: "下载失败",
		Detail:     strings.TrimSpace(string(raw)),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

func orderedParts(parts []PresignedPart, expectedSize int64) ([]PresignedPart, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	ordered := append([]PresignedPart(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Offset < ordered[j].Offset })
	var covered int64
	for _, part := range ordered {
		if strings.TrimSpace(part.URL) == "" {
			return nil, errors.New("分片下载地址为空")
		}
		if part.Offset != covered {
			return nil, fmt.Errorf("分片区间不连续：期望偏移 %d，实得 %d", covered, part.Offset)
		}
		if part.Size <= 0 {
			return nil, errors.New("分片长度非法")
		}
		if part.Size > math.MaxInt64-covered {
			return nil, errors.New("分片区间溢出")
		}
		covered += part.Size
	}
	if expectedSize > 0 && covered != expectedSize {
		return nil, fmt.Errorf("分片未覆盖完整密文：期望 %d 字节，实得 %d 字节", expectedSize, covered)
	}
	return ordered, nil
}

func (c *Client) saveURL(ctx context.Context, rawURL, dest string, auth bool) error {
	var target string
	var err error
	if auth {
		target, err = c.siteURL(rawURL)
	} else {
		target, err = c.resolveDownloadURL(rawURL)
	}
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Status: 0, Message: "网络连接失败，请检查网络后重试", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return downloadResponseError(resp)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func (c *Client) saveParts(ctx context.Context, parts []PresignedPart, dest string) error {
	if len(parts) == 0 {
		return nil
	}
	var total int64
	for _, part := range parts {
		if part.Size > math.MaxInt64-total {
			return errors.New("分片区间溢出")
		}
		total += part.Size
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(total); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan PresignedPart, len(parts))
	for _, part := range parts {
		jobs <- part
	}
	close(jobs)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for worker := 0; worker < min(partWorkers, len(parts)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for part := range jobs {
				if runCtx.Err() != nil {
					return
				}
				if err := c.savePart(runCtx, part, out); err != nil {
					once.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func (c *Client) savePart(ctx context.Context, part PresignedPart, out *os.File) error {
	target, err := c.resolveDownloadURL(part.URL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Status: 0, Message: "网络连接失败，请检查网络后重试", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return downloadResponseError(resp)
	}
	written, err := io.Copy(io.NewOffsetWriter(out, part.Offset), resp.Body)
	if err != nil {
		return err
	}
	if written != part.Size {
		return fmt.Errorf("分片长度不匹配：偏移 %d 期望 %d 字节，实得 %d 字节",
			part.Offset, part.Size, written)
	}
	return nil
}

func (c *Client) savePlain(ctx context.Context, rawURL, dest string, plan DeliveryPlan) error {
	target, err := c.siteURL(rawURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Status: 0, Message: "网络连接失败，请检查网络后重试", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return downloadResponseError(resp)
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".xph-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, digest), resp.Body)
	if err != nil {
		return err
	}
	if plan.PlainSize > 0 && written != plan.PlainSize {
		return fmt.Errorf("下载长度不匹配：期望 %d 字节，实得 %d 字节", plan.PlainSize, written)
	}
	expected := plan.Checksum
	if expected == "" && plan.Encryption != nil {
		expected = plan.Encryption.Checksum
	}
	if expected != "" && !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expected) {
		return errors.New("下载内容校验失败：SHA-256 与交付计划不一致")
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, dest); err != nil {
		return err
	}
	keep = true
	return nil
}

func decryptFile(cipherPath, dest string, plan DeliveryPlan, privateKey *rsa.PrivateKey) error {
	meta := plan.Encryption
	if meta == nil {
		return errors.New("交付计划缺少解密材料")
	}
	in, err := os.Open(cipherPath)
	if err != nil {
		return err
	}
	defer in.Close()
	header := make([]byte, 64)
	if _, err := io.ReadFull(in, header); err != nil {
		return fmt.Errorf("读取密文文件头失败: %w", err)
	}
	if string(header[:8]) != "XPHCRPT1" {
		return errors.New("密文文件头无效")
	}
	if header[8] != 1 {
		return fmt.Errorf("不支持的加密算法标识 %d", header[8])
	}
	blockLog2 := int(header[9])
	if blockLog2 < 9 || blockLog2 > 26 {
		return fmt.Errorf("非法的块大小指数 %d", blockLog2)
	}
	rawPlainSize := binary.LittleEndian.Uint64(header[28:36])
	if rawPlainSize > 1<<62 {
		return errors.New("非法的明文长度")
	}
	plainSize := int64(rawPlainSize)
	noncePrefix := binary.LittleEndian.Uint32(header[36:40])
	blockSize := int64(1) << blockLog2
	blockCount := int64(0)
	if plainSize > 0 {
		blockCount = (plainSize + blockSize - 1) / blockSize
	}
	cipherSize := int64(64) + plainSize + blockCount*16
	var problems []string
	if blockLog2 != meta.ChunkLog2 {
		problems = append(problems, fmt.Sprintf("块大小指数 头=%d 记录=%d", blockLog2, meta.ChunkLog2))
	}
	if plainSize != meta.PlainSize {
		problems = append(problems, fmt.Sprintf("明文长度 头=%d 记录=%d", plainSize, meta.PlainSize))
	}
	if cipherSize != meta.CipherSize {
		problems = append(problems, fmt.Sprintf("密文长度 头=%d 记录=%d", cipherSize, meta.CipherSize))
	}
	if noncePrefix != meta.NoncePrefix {
		problems = append(problems, fmt.Sprintf("nonce 前缀 头=%d 记录=%d", noncePrefix, meta.NoncePrefix))
	}
	if len(problems) > 0 {
		return fmt.Errorf("对象与记录不一致，已中止（%s）", strings.Join(problems, "；"))
	}
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if info.Size() != cipherSize {
		return fmt.Errorf("密文长度与文件头不一致：期望 %d 字节，实得 %d 字节", cipherSize, info.Size())
	}
	envelope, err := base64.StdEncoding.DecodeString(meta.KeyEnvelope)
	if err != nil {
		return err
	}
	dek, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, envelope, nil)
	if err != nil {
		return fmt.Errorf("解开内容密钥失败: %w", err)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	aad := header[:40]
	out, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".xph-*")
	if err != nil {
		return err
	}
	outPath := out.Name()
	keep := false
	defer func() {
		_ = out.Close()
		if !keep {
			_ = os.Remove(outPath)
		}
	}()
	digest := sha256.New()
	remaining := plainSize
	for index := int64(0); remaining > 0; index++ {
		take := blockSize
		if remaining < take {
			take = remaining
		}
		encrypted := make([]byte, int(take)+gcm.Overhead())
		if _, err := io.ReadFull(in, encrypted); err != nil {
			return fmt.Errorf("密文被截断: %w", err)
		}
		nonce := make([]byte, 12)
		binary.BigEndian.PutUint32(nonce[:4], noncePrefix)
		binary.BigEndian.PutUint64(nonce[4:], uint64(index))
		plain, err := gcm.Open(nil, nonce, encrypted, aad)
		if err != nil {
			return fmt.Errorf("第 %d 块解密失败: %w", index, err)
		}
		if _, err := out.Write(plain); err != nil {
			return err
		}
		digest.Write(plain)
		remaining -= take
	}
	expected := plan.Checksum
	if expected == "" {
		expected = meta.Checksum
	}
	if expected != "" && !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expected) {
		return errors.New("下载内容校验失败：SHA-256 与交付计划不一致")
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(outPath, dest); err != nil {
		return err
	}
	keep = true
	return nil
}

// ---------------------------------------------------------------- 分享与直链

type Share struct {
	CustomMarkdown string `json:"customMarkdown"`
	ID             string `json:"id"`
	OwnerID        int64  `json:"ownerId"`
	RootPath       string `json:"rootPath"`
	Kind           string `json:"kind"`
	AccessMode     string `json:"accessMode"`
	HasPassword    bool   `json:"hasPassword"`
	AllowDownload  bool   `json:"allowDownload"`
	AllowPreview   bool   `json:"allowPreview"`
	AllowSubpath   bool   `json:"allowSubpath"`
	ShowSharerName bool   `json:"showSharerName"`
	ExpiresAt      int64  `json:"expiresAt"`
	MaxDeliveries  int    `json:"maxDeliveries"`
	Deliveries     int    `json:"deliveries"`
	AccessVisits   int    `json:"accessVisits"`
	MaxVisits      int    `json:"maxVisits"`
	Visits         int    `json:"visits"`
	Disabled       bool   `json:"disabled"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// CreateShareOptions 是完整分享参数；nil 布尔字段由服务端按默认值开启。
type CreateShareOptions struct {
	Kind           string `json:"kind"`
	AccessMode     string `json:"accessMode"`
	Password       string `json:"password,omitempty"`
	AllowDownload  *bool  `json:"allowDownload,omitempty"`
	AllowPreview   *bool  `json:"allowPreview,omitempty"`
	AllowSubpath   *bool  `json:"allowSubpath,omitempty"`
	ShowSharerName *bool  `json:"showSharerName,omitempty"`
	CustomMarkdown string `json:"customMarkdown,omitempty"`
	ExpiresAt      int64  `json:"expiresAt,omitempty"`
	MaxDeliveries  int    `json:"maxDeliveries,omitempty"`
}

// UpdateShareOptions 只更新非 nil 字段。
type UpdateShareOptions struct {
	CustomMarkdown  *string `json:"customMarkdown,omitempty"`
	Path            *string `json:"path,omitempty"`
	Kind            *string `json:"kind,omitempty"`
	AccessMode      *string `json:"accessMode,omitempty"`
	Password        *string `json:"password,omitempty"`
	AllowDownload   *bool   `json:"allowDownload,omitempty"`
	AllowPreview    *bool   `json:"allowPreview,omitempty"`
	AllowSubpath    *bool   `json:"allowSubpath,omitempty"`
	ShowSharerName  *bool   `json:"showSharerName,omitempty"`
	ExpiresAt       *int64  `json:"expiresAt,omitempty"`
	MaxDeliveries   *int    `json:"maxDeliveries,omitempty"`
	Disabled        *bool   `json:"disabled,omitempty"`
	ExpectUpdatedAt int64   `json:"expectUpdatedAt,omitempty"`
}

type PickupCode struct {
	Code      string `json:"code"`
	ShareID   string `json:"shareId"`
	MaxUses   int    `json:"maxUses"`
	UsedCount int    `json:"usedCount"`
	ExpiresAt int64  `json:"expiresAt"`
	Disabled  bool   `json:"disabled"`
	CreatedBy int64  `json:"createdBy"`
	CreatedAt int64  `json:"createdAt"`
}

// CreateShare 创建分享；Kind 为空时按 file、AccessMode 为空时按 public。
func (c *Client) CreateShare(ctx context.Context, path string, opts CreateShareOptions) (Share, error) {
	if opts.Kind == "" {
		opts.Kind = "file"
	}
	if opts.AccessMode == "" {
		opts.AccessMode = "public"
	}
	payload := struct {
		Path string `json:"path"`
		CreateShareOptions
	}{Path: path, CreateShareOptions: opts}
	var data struct {
		Share Share `json:"share"`
	}
	err := c.do(ctx, http.MethodPost, "/api/share", payload, &data)
	return data.Share, err
}

func (c *Client) ListShares(ctx context.Context, limit, offset int) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/share?limit=%d&offset=%d", limit, offset), nil, &out)
	return out, err
}

func (c *Client) UpdateShare(ctx context.Context, id string, opts UpdateShareOptions) (Share, error) {
	var data struct {
		Share Share `json:"share"`
	}
	err := c.do(ctx, http.MethodPatch, "/api/share/"+url.PathEscape(id), opts, &data)
	return data.Share, err
}

func (c *Client) DeleteShare(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/share/"+url.PathEscape(id), nil, nil)
}

func (c *Client) ListShareAccesses(ctx context.Context, id string, limit, offset int, snapshotID *int64) (map[string]any, error) {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	if snapshotID != nil {
		query.Set("snapshotId", strconv.FormatInt(*snapshotID, 10))
	}
	var out map[string]any
	err := c.do(ctx, http.MethodGet,
		"/api/share/"+url.PathEscape(id)+"/accesses?"+query.Encode(), nil, &out)
	return out, err
}

func (c *Client) CreatePickup(ctx context.Context, shareID string, maxUses int) (PickupCode, error) {
	var data struct {
		Pickup PickupCode `json:"pickup"`
	}
	err := c.do(ctx, http.MethodPost, "/api/share/"+url.PathEscape(shareID)+"/pickup",
		map[string]any{"maxUses": maxUses}, &data)
	return data.Pickup, err
}

func (c *Client) ListPickups(ctx context.Context, shareID string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/share/"+url.PathEscape(shareID)+"/pickup", nil, &out)
	return out, err
}

func (c *Client) DeletePickup(ctx context.Context, code string) error {
	return c.do(ctx, http.MethodDelete, "/api/pickup/"+url.PathEscape(code), nil, nil)
}

func (c *Client) ShareURL(id string) string {
	return c.BaseURL + "/s/" + url.PathEscape(id)
}

type DirectLink struct {
	ID              string `json:"id"`
	OwnerID         int64  `json:"ownerId"`
	Checksum        string `json:"checksum"`
	SourcePath      string `json:"sourcePath"`
	SourceName      string `json:"sourceName"`
	MimeType        string `json:"mimeType"`
	Size            int64  `json:"size"`
	ExpiresAt       int64  `json:"expiresAt"`
	MaxHits         int    `json:"maxHits"`
	Hits            int    `json:"hits"`
	Disabled        bool   `json:"disabled"`
	TargetAvailable bool   `json:"targetAvailable"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

type UpdateDirectLinkOptions struct {
	ExpiresAt       *int64 `json:"expiresAt,omitempty"`
	MaxHits         *int   `json:"maxHits,omitempty"`
	Disabled        *bool  `json:"disabled,omitempty"`
	ExpectUpdatedAt int64  `json:"expectUpdatedAt,omitempty"`
}

func (c *Client) CreateDirectLink(ctx context.Context, path string, expiresAt int64, maxHits int) (DirectLink, error) {
	var data struct {
		Link DirectLink `json:"link"`
	}
	err := c.do(ctx, http.MethodPost, "/api/direct-link", map[string]any{
		"path": path, "expiresAt": expiresAt, "maxHits": maxHits,
	}, &data)
	return data.Link, err
}

func (c *Client) ListDirectLinks(ctx context.Context, limit, offset int) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/direct-link?limit=%d&offset=%d", limit, offset), nil, &out)
	return out, err
}

func (c *Client) UpdateDirectLink(ctx context.Context, id string, opts UpdateDirectLinkOptions) (DirectLink, error) {
	var data struct {
		Link DirectLink `json:"link"`
	}
	err := c.do(ctx, http.MethodPatch, "/api/direct-link/"+url.PathEscape(id), opts, &data)
	return data.Link, err
}

func (c *Client) DeleteDirectLink(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/direct-link/"+url.PathEscape(id), nil, nil)
}

func (c *Client) DirectLinkTarget(ctx context.Context, id string) (string, error) {
	var data struct {
		Path string `json:"path"`
	}
	err := c.do(ctx, http.MethodGet, "/api/direct-link/"+url.PathEscape(id)+"/target", nil, &data)
	return data.Path, err
}

func (c *Client) ListNodeDirectLinks(ctx context.Context, checksums []string) (map[string]any, error) {
	query := url.Values{}
	query.Set("checksums", strings.Join(checksums, ","))
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/direct-link/nodes?"+query.Encode(), nil, &out)
	return out, err
}

func (c *Client) DirectLinkURL(id string) string {
	return c.BaseURL + "/d/" + url.PathEscape(id)
}

// ---------------------------------------------------------------- 归档与公告

func (c *Client) ListArchive(ctx context.Context, limit, offset int) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/archive?limit=%d&offset=%d", limit, offset), nil, &out)
	return out, err
}

func (c *Client) RestoreArchive(ctx context.Context, batchID string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, "/api/archive/"+url.PathEscape(batchID)+"/restore", nil, &out)
	return out, err
}

func (c *Client) ClearArchive(ctx context.Context, batchID string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, "/api/archive/"+url.PathEscape(batchID)+"/clear", nil, &out)
	return out, err
}

func (c *Client) ClearAllArchive(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, "/api/archive/clear-all", nil, &out)
	return out, err
}

func (c *Client) ListAnnouncements(ctx context.Context, kinds []string) (map[string]any, error) {
	endpoint := "/api/announcements"
	if len(kinds) > 0 {
		query := url.Values{}
		query.Set("kinds", strings.Join(kinds, ","))
		endpoint += "?" + query.Encode()
	}
	var out map[string]any
	err := c.do(ctx, http.MethodGet, endpoint, nil, &out)
	return out, err
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "--version":
		version, err := SDKVersion(context.Background(), "")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(version)
		return
	case "--serve-direct":
		runDirectDeliveryServer(exampleAddr(os.Args[2:], "127.0.0.1:8080"))
		return
	case "--serve-relay":
		runRelayDeliveryServer(exampleAddr(os.Args[2:], "127.0.0.1:8081"))
		return
	}
	if strings.HasPrefix(os.Args[1], "-") {
		printUsage()
		os.Exit(2)
	}
	client, err := NewClient("", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	parent := "/"
	if len(os.Args) > 2 {
		parent = os.Args[2]
	}
	result, err := client.Upload(context.Background(), os.Args[1], parent, "", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))
}
