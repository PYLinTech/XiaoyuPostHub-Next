// XiaoyuPostHub-Next Go SDK 示例。
//
// 站点地址只来自 ../config.json；API Key 只来自参数或 XIAOYU_API_KEY。
// 下载在本地完成 XPH 解密与 SHA-256 校验。
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// APIError 是服务端返回的业务错误。
type APIError struct {
	Status  int
	Message string
	Detail  string
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
	if baseURL == "" {
		var err error
		baseURL, err = loadBaseURL()
		if err != nil {
			return nil, err
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("站点地址必须以 http:// 或 https:// 开头")
	}
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 0},
	}, nil
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
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
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
		return &APIError{Status: resp.StatusCode, Message: message, Detail: detail}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return err
		}
	}
	return nil
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

type uploadInit struct {
	Dedup      bool   `json:"dedup"`
	SessionID  string `json:"sessionId"`
	ChunkSize  int64  `json:"chunkSize"`
	ChunkTotal int    `json:"chunkTotal"`
	Received   []int  `json:"received"`
}

type uploadJob struct {
	State string `json:"state"`
	Error string `json:"error"`
}

// Upload 完成一次完整上传：秒传探测、分片、收尾与状态轮询。
func (c *Client) Upload(ctx context.Context, localPath, parent, name, conflictAction string) (map[string]any, error) {
	if name == "" {
		name = filepath.Base(localPath)
	}
	if conflictAction == "" {
		conflictAction = "rename"
	}
	file, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return nil, err
	}
	var init uploadInit
	if err := c.do(ctx, http.MethodPost, "/api/upload/init", map[string]any{
		"checksum":       hex.EncodeToString(digest.Sum(nil)),
		"sizePlain":      info.Size(),
		"parentPath":     parent,
		"name":           name,
		"conflictAction": conflictAction,
	}, &init); err != nil {
		return nil, err
	}
	result := map[string]any{"dedup": init.Dedup}
	if init.Dedup {
		return result, nil
	}
	received := make(map[int]bool, len(init.Received))
	for _, index := range init.Received {
		received[index] = true
	}
	for index := 0; index < init.ChunkTotal; index++ {
		if received[index] {
			continue
		}
		offset := int64(index) * init.ChunkSize
		length := init.ChunkSize
		if remain := info.Size() - offset; remain < length {
			length = remain
		}
		buf := make([]byte, length)
		if _, err := file.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if err := c.do(ctx, http.MethodPut,
			fmt.Sprintf("/api/upload/%s/chunk/%d", init.SessionID, index), buf, nil); err != nil {
			return nil, err
		}
	}
	if err := c.do(ctx, http.MethodPost, "/api/upload/"+init.SessionID+"/complete", nil, nil); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var job uploadJob
		if err := c.do(ctx, http.MethodGet, "/api/upload/"+init.SessionID, nil, &job); err != nil {
			return nil, err
		}
		switch job.State {
		case "done":
			result["state"] = "done"
			return result, nil
		case "error":
			return nil, fmt.Errorf("上传失败: %s", job.Error)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, errors.New("上传收尾超时")
}

func (c *Client) CancelUpload(ctx context.Context, session string) error {
	return c.do(ctx, http.MethodPost, "/api/upload/"+session+"/cancel", nil, nil)
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
	Mode        string          `json:"mode"`
	URL         string          `json:"url"`
	Parts       []PresignedPart `json:"parts"`
	StreamURL   string          `json:"streamUrl"`
	ContentForm string          `json:"contentForm"`
	Encryption  *DeliveryMeta   `json:"encryption"`
	TicketID    string          `json:"ticketId"`
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
	endpoint := "/api/fs/download"
	if preview {
		endpoint = "/api/fs/preview"
	}
	var plan DeliveryPlan
	if err := c.do(ctx, http.MethodPost, endpoint, map[string]any{
		"path":            path,
		"clientPublicKey": base64.StdEncoding.EncodeToString(pub),
	}, &plan); err != nil {
		return DeliveryPlan{}, err
	}
	if plan.ContentForm == "plaintext" {
		if plan.StreamURL == "" {
			return DeliveryPlan{}, errors.New("交付计划缺少可下载地址")
		}
		return plan, c.saveURL(ctx, plan.StreamURL, dest, true, false)
	}
	temp, err := os.CreateTemp("", "xph-*.bin")
	if err != nil {
		return DeliveryPlan{}, err
	}
	tempPath := temp.Name()
	_ = temp.Close()
	defer os.Remove(tempPath)
	switch {
	case plan.Mode == "direct" && len(plan.Parts) > 0:
		for _, part := range plan.Parts {
			if err := c.saveURL(ctx, part.URL, tempPath, false, true); err != nil {
				return DeliveryPlan{}, err
			}
		}
	case plan.Mode == "direct" && plan.URL != "":
		if err := c.saveURL(ctx, plan.URL, tempPath, false, false); err != nil {
			return DeliveryPlan{}, err
		}
	case plan.StreamURL != "":
		if err := c.saveURL(ctx, plan.StreamURL, tempPath, true, false); err != nil {
			return DeliveryPlan{}, err
		}
	default:
		return DeliveryPlan{}, errors.New("交付计划缺少可下载地址")
	}
	if plan.Encryption == nil {
		return DeliveryPlan{}, errors.New("交付计划缺少解密材料")
	}
	if err := decryptFile(tempPath, dest, *plan.Encryption, privateKey); err != nil {
		return DeliveryPlan{}, err
	}
	if plan.TicketID != "" {
		_ = c.do(ctx, http.MethodPost, "/api/fs/settle", map[string]any{
			"ticketId": plan.TicketID, "completed": true,
		}, nil)
	}
	return plan, nil
}

func (c *Client) saveURL(ctx context.Context, rawURL, dest string, auth, appendMode bool) error {
	target := rawURL
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = c.BaseURL + target
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("不支持的下载地址协议")
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
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Status: resp.StatusCode, Message: "下载失败", Detail: string(raw)}
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	out, err := os.OpenFile(dest, flags, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func decryptFile(cipherPath, dest string, meta DeliveryMeta, privateKey *rsa.PrivateKey) error {
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
	blockSize := int64(1) << header[9]
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	digest := sha256.New()
	remaining := meta.PlainSize
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
		binary.BigEndian.PutUint32(nonce[:4], meta.NoncePrefix)
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
	if meta.Checksum != "" && hex.EncodeToString(digest.Sum(nil)) != meta.Checksum {
		_ = os.Remove(dest)
		return errors.New("下载内容校验失败：SHA-256 与交付计划不一致")
	}
	return nil
}

// ---------------------------------------------------------------- 分享与直链

type Share struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	AccessMode string `json:"accessMode"`
	ExpiresAt  int64  `json:"expiresAt"`
}

func (c *Client) CreateShare(ctx context.Context, path, kind, accessMode, password string, expiresAt int64) (Share, error) {
	if kind == "" {
		kind = "file"
	}
	if accessMode == "" {
		accessMode = "public"
	}
	var data struct {
		Share Share `json:"share"`
	}
	err := c.do(ctx, http.MethodPost, "/api/share", map[string]any{
		"path": path, "kind": kind, "accessMode": accessMode,
		"password": password, "expiresAt": expiresAt,
		"allowDownload": true, "allowPreview": true,
	}, &data)
	return data.Share, err
}

func (c *Client) ListShares(ctx context.Context, limit, offset int) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/share?limit=%d&offset=%d", limit, offset), nil, &out)
	return out, err
}

func (c *Client) DeleteShare(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/share/"+id, nil, nil)
}

func (c *Client) ShareURL(id string) string { return c.BaseURL + "/s/" + id }

type DirectLink struct {
	ID         string `json:"id"`
	SourceName string `json:"sourceName"`
	ExpiresAt  int64  `json:"expiresAt"`
	MaxHits    int    `json:"maxHits"`
	Hits       int    `json:"hits"`
	Disabled   bool   `json:"disabled"`
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

func (c *Client) DeleteDirectLink(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/direct-link/"+id, nil, nil)
}

func (c *Client) DirectLinkURL(id string) string { return c.BaseURL + "/d/" + id }

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: go run . <本地文件> [目标目录]")
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
