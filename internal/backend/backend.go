// Package backend 是物理存储后端的抽象。
//
// 本系统把每个文件存为一个完整对象（在 123 上是单文件，其分片由对方内部处理）。
//
// 对象名与内容密钥无关：命名由校验码经 HMAC 派生，重命名/移动不影响解密，
// 也无法从对象名反推明文内容。
package backend

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotSupported = errors.New("backend: 后端不支持该操作")
	ErrNotFound     = errors.New("backend: 对象不存在")
	// ErrSliceMisaligned：分片边界与加密块边界错位后密文流无法跨片连续解密，
	// Range 与预览都会读到垃圾，因此宁可上传失败也不能写进去。
	ErrSliceMisaligned    = errors.New("backend: 存储分片粒度与加密块大小不对齐")
	ErrPresignUnavailable = errors.New("backend: 直链不可用")
)

// PutRequest 描述一次对象写入。
type PutRequest struct {
	// LogicalName 是后端侧的对象名（在 123 上是上传文件名）。
	LogicalName string
	// ParentDir 是对象应放置的逻辑目录，例如 "2026/09/21"。空表示根目录。
	ParentDir string
	// SizePlain 是明文长度；SizeWire 是密文长度。
	SizePlain int64
	SizeWire  int64
	// CipherMD5 是密文的 MD5（小写十六进制）。
	//
	// 123 侧存的是密文，传明文哈希会让 (etag, size) 不自洽而匹配不上，
	// 代价是需要先把密文落到临时文件。
	CipherMD5 string
	// Source 必须支持随机读：分片上传会并发读取不同区间。
	Source io.ReaderAt
	// BlockSize 是加密块大小，用于校验分片对齐。
	BlockSize int64
	// OnProgress 上报已上传的密文字节数。
	OnProgress func(uploaded int64)
}

// PutResult 是一次写入的结果。
type PutResult struct {
	// ObjectRef 是后端真实定位符（123 上是数字 fileID 的字符串形式）。
	ObjectRef string
}

// HealthChecker 由支持连通性自检的后端实现。
//
// 单独抽一个可选接口而不是塞进 Backend：自检是运维动作，绝大多数调用路径
// 都不需要它，放主接口里会强迫每个实现（含测试用的桩）都写一份。
type HealthChecker interface {
	// Health 执行一次轻量调用，验证凭据与网络可达性。
	Health(ctx context.Context) (map[string]any, error)
}

// DirectLinkSwitcher 由支持对存放目录启用或关闭直链空间的后端实现。
//
// 与 HealthChecker 同理：这是一次性运维动作，不进入 Backend 主接口。
// 勾选/取消勾选配置项即一次显式开关指令，只在配置写入路径调用，
// 启动时不做任何自动对账。
type DirectLinkSwitcher interface {
	// SetDirectLink 对当前配置的存放根目录启用（enabled 为真）或关闭直链空间，
	// 返回目录名。操作幂等，重复执行无害。
	SetDirectLink(ctx context.Context, enabled bool) (string, error)
}

// PresignOptions 描述一次直链签发。
type PresignOptions struct {
	// TTL 是直链本身的有效期。
	TTL time.Duration
	// TicketID 会作为查询参数附在直链上。
	//
	// 数据面（跨域 CDN）与登录态不同源，跨域自定义头会触发预检并被拒，
	// 因此票据只能经 URL 传递；独立参数比塞进 auth_key 更依赖上游回传原值。
	TicketID string
}

// Backend 是物理存储后端。
type Backend interface {
	// Kind 返回后端类型标识。
	Kind() string
	// Put 写入一个对象。ref 逻辑名由调用方给出，返回真实定位符。
	Put(ctx context.Context, req PutRequest) (PutResult, error)
	// Open 打开对象用于顺序或随机读取（服务端解密通道使用）。
	Open(ctx context.Context, ref string) (io.ReadSeekCloser, error)
	// Delete 删除对象。对象不存在不应视为错误（幂等）。
	Delete(ctx context.Context, ref string) error
	// Stat 返回对象在存储侧的字节数。
	Stat(ctx context.Context, ref string) (int64, error)
	// Presign 返回对象的第三方直链地址，opt.TicketID 会作为票据标识附在 URL 上。
	Presign(ctx context.Context, ref string, opt PresignOptions) (string, error)
	// PresignReady 表示当前配置下是否具备提供直链的能力。
	PresignReady() bool
	// SliceMD5Enabled 表示后端在 create 时要求提供密文哈希（决定上传链路
	// 是否必须先把密文落到临时文件）。
	SliceMD5Enabled() bool
}
