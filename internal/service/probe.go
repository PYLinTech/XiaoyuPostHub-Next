package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/perm"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/settings"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// 探针票据只服务于一次自检，有效期与次数都刻意收窄：它不承载真实流量，
// 只需要覆盖前端「start → 下载验证 → finish」的几秒窗口。
const (
	probeTicketTTL     = 2 * time.Minute
	probeTicketMaxUses = 64
)

// probeObject 是探针数据面的产物：一个按真实 XPH-Crypt 格式加密的小对象。
type probeObject struct {
	fileID   string // 存储后端定位符
	fileName string
	checksum string // 密文 MD5，同时作为内容池主键
	plain    []byte
	dek      []byte
	hdr      xph.Header
	cipher   []byte
}

// activeProbe 记录一次管理端自检在数据库侧留下的痕迹，供 finish 清理。
type activeProbe struct {
	checksum string
	ticketID string
}

// 探针明文摘要用 SHA-256 而不是 MD5：前端没有也不应为自检引入 MD5 原语，
// 而下载链路的内容校验本就统一用 SHA-256。（密文内容池主键仍用 MD5，
// 那是上游 Put 接口 CipherMD5 的既有口径，与前端无关。）

// plainSHA256 返回明文的十六进制 SHA-256 摘要。
func plainSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// activeProbes 在内存中保存进行中的自检：fileID → 数据库痕迹。
// 进程重启后映射丢失，过期票据会被维护任务清理；内容池残留行需要重新自检
// 后由 finish 按新映射清理（与远端对象残留同属崩溃后的人工处置范畴）。
var activeProbes sync.Map

// SetupProbeResult 是初始化阶段服务端自检的分阶段结果，字段与前端完成页对齐。
type SetupProbeResult struct {
	FileName string `json:"fileName"`
	FileID   string `json:"fileId,omitempty"`
	// Bytes 是探针文件的明文字节数（固定 1）。
	Bytes int `json:"bytes"`
	// Uploaded / Downloaded / Verified / Deleted 分别对应上传成功、
	// 下载打开成功、解密内容逐字节一致、测试后删除成功四个阶段。
	Uploaded   bool `json:"uploaded"`
	Downloaded bool `json:"downloaded"`
	Verified   bool `json:"verified"`
	Deleted    bool `json:"deleted"`
}

// StorageProbeSession 是管理端自检 start 接口返回给前端的全部材料。
//
// 自检由前端主导：后端只负责上传探针文件、按当前交付配置准备好通道；
// 前端自己向直链 URL 或中转 URL 发起请求、解密并比对摘要，从而真实
// 判定「当前下载通道」到底是哪一种。
type StorageProbeSession struct {
	FileID   string `json:"fileId"`
	FileName string `json:"fileName"`
	// Mode 是按当前配置解析出的交付模式，与真实下载 PrepareDelivery
	// 的决策完全一致：direct / proxy / proxy_decrypt。
	Mode string `json:"mode"`
	// SizePlain / SizeWire 是明文与密文长度。
	SizePlain int64 `json:"sizePlain"`
	SizeWire  int64 `json:"sizeWire"`
	// 以下为 XPH 交叉校验/解密材料，proxy_decrypt 模式不下发（前端拿明文）。
	// 文件盐不在这里重复下发：它包含在 XPH 文件头（也是每块 GCM 的 AAD）中，
	// 前端取回对象时自然得到。
	BlockLog2   int    `json:"blockLog2,omitempty"`
	NoncePrefix uint32 `json:"noncePrefix,omitempty"`
	// Key 是 base64 编码的内容密钥。
	Key string `json:"key,omitempty"`
	// PlainSHA256 是明文摘要：直链/中转加密下用于解密后比对，中转解密下
	// 直接比对响应体。
	PlainSHA256 string `json:"plainSha256"`
	// DirectURL 是 123 直链地址（Mode=direct 时非空）。
	DirectURL string `json:"directUrl,omitempty"`
	// StreamURL 是本机中转地址（Mode=proxy/proxy_decrypt 时非空）。
	StreamURL string `json:"streamUrl,omitempty"`
}

// StorageProbeVerifyResult 是前端完成验证后的回报，仅用于审计。
type StorageProbeVerifyResult struct {
	Mode      string `json:"mode"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail,omitempty"`
	BytesGot  int    `json:"bytesGot"`
	LatencyMs int64  `json:"latencyMs"`
}

// buildProbeObject 生成并上传一个 XPH 加密探针对象。
//
// 探针对象用独立随机 DEK 加密，不落库信封：内容与文件名都随机，绕开上游
// 秒传去重，确保真正走过上传分片链路。
func (s *Service) buildProbeObject(ctx context.Context) (*probeObject, error) {
	var payload [1]byte
	if _, err := rand.Read(payload[:]); err != nil {
		return nil, fmt.Errorf("生成探针内容失败: %w", err)
	}
	dek, err := xph.NewDEK()
	if err != nil {
		return nil, fmt.Errorf("生成探针内容密钥失败: %w", err)
	}
	hdr, err := xph.NewHeaderRandom(int64(len(payload)), xph.DefaultBlockLog2)
	if err != nil {
		return nil, fmt.Errorf("生成探针文件头失败: %w", err)
	}
	cipher, err := xph.EncryptAll(payload[:], dek, hdr)
	if err != nil {
		return nil, fmt.Errorf("加密探针内容失败: %w", err)
	}
	sum := md5.Sum(cipher)
	suffix := make([]byte, 2)
	if _, err := rand.Read(suffix); err != nil {
		return nil, fmt.Errorf("生成探针文件名失败: %w", err)
	}
	obj := &probeObject{
		fileName: fmt.Sprintf("xph-probe-%d-%s.xph", time.Now().UnixNano(), hex.EncodeToString(suffix)),
		plain:    payload[:],
		dek:      dek,
		hdr:      hdr,
		cipher:   cipher,
		checksum: hex.EncodeToString(sum[:]),
	}

	put, err := s.Backend.Put(ctx, backend.PutRequest{
		LogicalName: obj.fileName,
		// 直接上传到配置的根目录：文件名带纳秒时间戳与随机后缀，用完即删。
		ParentDir: "",
		SizePlain: int64(len(payload)),
		SizeWire:  int64(len(cipher)),
		CipherMD5: obj.checksum,
		Source:    bytes.NewReader(cipher),
		// 与真实对象相同的分块粒度（默认 1 MiB）。
		BlockSize:   hdr.BlockSize(),
		MaxPartSize: s.Settings.Runtime(ctx).Upload.MaxVolumeBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("上传失败: %w", err)
	}
	obj.fileID = put.ObjectRef
	return obj, nil
}

// verifyProbeDownload 从存储后端读回探针对象，解密并逐字节比对。
func (s *Service) verifyProbeDownload(ctx context.Context, obj *probeObject) error {
	reader, err := s.Backend.Open(ctx, obj.fileID)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return fmt.Errorf("读取失败: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭失败: %w", closeErr)
	}
	plain, err := xph.DecryptAll(got, obj.dek)
	if err != nil {
		return fmt.Errorf("解密失败: %w", err)
	}
	if !bytes.Equal(plain, obj.plain) {
		return errors.New("内容校验失败：读回内容与上传不一致")
	}
	return nil
}

// deleteProbeObject 删除探针对象；失败时错误携带 fileID 便于人工清理。
func (s *Service) deleteProbeObject(ctx context.Context, obj *probeObject) error {
	if err := s.Backend.Delete(ctx, obj.fileID); err != nil {
		return fmt.Errorf("清理失败（fileID=%s）: %w", obj.fileID, err)
	}
	return nil
}

// runSetupProbe 是初始化提交时的服务端自检：上传 → 读回解密校验 → 删除。
//
// 初始化场景没有登录态，也不验证交付通道（直链/中转由管理端自检在前端
// 主导完成）；这里只体检存储数据面。清理无条件执行，删除失败整体判失败。
func (s *Service) runSetupProbe(ctx context.Context) (SetupProbeResult, error) {
	result := SetupProbeResult{Bytes: 1}
	obj, err := s.buildProbeObject(ctx)
	if err != nil {
		return result, err
	}
	result.FileName = obj.fileName
	result.FileID = obj.fileID
	result.Uploaded = true

	verifyErr := s.verifyProbeDownload(ctx, obj)
	if verifyErr == nil {
		result.Downloaded = true
		result.Verified = true
	}

	delErr := s.deleteProbeObject(ctx, obj)
	if delErr != nil {
		result.Deleted = false
		return result, errors.Join(verifyErr, delErr)
	}
	result.Deleted = true
	return result, verifyErr
}

// AdminStorageProbeStart 上传探针对象并按当前交付配置准备好通道。仅管理员可用。
//
// 探针文件会临时注册进内容池并签发正式票据：这样直链 Presign 能带票据参数，
// CDN 回源鉴权开启时 123 的问询也能被真实应答——自检走的就是真实交付链路，
// 而不是另开一条旁路。探针不预扣流量配额（ReservedBytes=0）。
func (s *Service) AdminStorageProbeStart(ctx context.Context, p auth.Principal) (*StorageProbeSession, error) {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return nil, err
	}
	if !s.StorageReady() {
		return nil, fmt.Errorf("%w: 存储后端尚未配置凭据", ErrUnavailable)
	}

	obj, err := s.buildProbeObject(ctx)
	if err != nil {
		return nil, err
	}

	// 交付模式只取决于运行期配置与后端能力，在上传后即可确定。
	// proxy_decrypt 必须把探针 DEK 用主密钥包裹后落库，否则中转流打开时
	// UnwrapDEK 会拿到空信封直接失败——自检在中转解密通道下必然报错。
	rt := s.Settings.Runtime(ctx)
	mode := store.TicketProxy
	if rt.Delivery.Direct && s.Backend.PresignReady() {
		mode = store.TicketDirect
	}
	if mode == store.TicketProxy && rt.Delivery.ProxyDecrypt {
		mode = store.TicketProxyDecrypt
	}
	// 只要主密钥可用就把探针 DEK 包裹落库：直链 Presign 失败后可能回落为
	// proxy_decrypt，届时信封必须已经就位。主密钥不可用且当前又确实要求
	// 中转解密时，直接失败而不是下发一张注定打不开的票据；直链/中转加密
	// 不受影响（信封列留空 BLOB，DEK 仅经响应体交给前端）。
	dekEnvelope := []byte{}
	if kek, kerr := s.KEKFor(ctx, settings.PrimaryKeyID); kerr == nil {
		if env, werr := xph.WrapDEK(kek, obj.dek); werr == nil {
			dekEnvelope = env
		}
	}
	if mode == store.TicketProxyDecrypt && len(dekEnvelope) != xph.EnvelopeSize {
		_ = s.deleteProbeObject(ctx, obj)
		return nil, fmt.Errorf("%w: 主密钥不可用，中转解密自检无法进行", ErrUnavailable)
	}

	// 注册临时内容池记录（上传中 → 正常）。checksum 随机碰撞概率为零，
	// 若真撞上已有记录，放弃本次探针而不是复用他人对象。
	now := s.Now()
	meta := store.File{
		Checksum:       obj.checksum,
		SizePlain:      int64(len(obj.plain)),
		PanObjectName:  obj.fileName,
		EncAlgo:        "AES-256-GCM",
		EncChunkLog2:   int(obj.hdr.BlockLog2),
		EncNoncePrefix: int64(obj.hdr.NoncePrefix),
		EncSalt:        obj.hdr.FileSalt[:],
		DEKEnvelope:    dekEnvelope,
		KEKKeyID:       settings.PrimaryKeyID,
		CreatedBy:      p.UserID(),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	created, err := store.InsertFilePlaceholder(ctx, s.DB.W(), meta)
	if err != nil {
		_ = s.deleteProbeObject(ctx, obj)
		return nil, err
	}
	if !created {
		_ = s.deleteProbeObject(ctx, obj)
		return nil, fmt.Errorf("%w: 探针校验码冲突，请重试", ErrConflict)
	}
	if err := store.FinalizeFile(ctx, s.DB.W(), obj.checksum, obj.fileID, obj.fileName, int64(len(obj.cipher))); err != nil {
		_ = store.ForceDeleteFileRow(ctx, s.DB.W(), obj.checksum)
		_ = s.deleteProbeObject(ctx, obj)
		return nil, err
	}

	ticketID, err := store.NewID("tk")
	if err != nil {
		_ = s.cleanupProbeArtifacts(ctx, obj.checksum, obj.fileID, "")
		return nil, err
	}
	ticket := store.Ticket{
		ID:           ticketID,
		FileChecksum: obj.checksum,
		ActorType:    p.Actor,
		UserID:       p.UserID(),
		ClientIP:     p.ClientIP.String(),
		IPPrefix:     p.IPPrefix,
		GroupName:    p.GroupName(),
		Purpose:      store.PurposeDownload,
		DeliveryMode: mode,
		// 探针不占流量配额；0 字节票据在过期清理时也不会触发退款。
		ReservedBytes: 0,
		MaxUses:       probeTicketMaxUses,
		ExpiresAt:     time.Now().UTC().Add(probeTicketTTL),
		CreatedAt:     s.Now(),
	}
	if err := store.CreateTicket(ctx, s.DB.W(), ticket); err != nil {
		_ = s.cleanupProbeArtifacts(ctx, obj.checksum, obj.fileID, "")
		return nil, err
	}

	session := &StorageProbeSession{
		FileID:      obj.fileID,
		FileName:    obj.fileName,
		SizePlain:   int64(len(obj.plain)),
		SizeWire:    int64(len(obj.cipher)),
		PlainSHA256: plainSHA256(obj.plain),
	}

	if mode == store.TicketDirect {
		link, presignErr := s.Backend.Presign(ctx, obj.fileID, backend.PresignOptions{
			TTL:      probeTicketTTL,
			TicketID: ticketID,
		})
		switch {
		case presignErr == nil:
			// 直链签发成功：前端直接向 123 链接发起请求。
			session.Mode = string(store.TicketDirect)
			session.DirectURL = link
		default:
			// 直链签发失败（直链空间未开、流量用尽、上游不可达）时回落中转，
			// 与真实下载的降级路径一致，票据模式同步翻转。
			mode = store.TicketProxy
			if rt.Delivery.ProxyDecrypt {
				mode = store.TicketProxyDecrypt
			}
			if err := store.SetTicketDeliveryMode(ctx, s.DB.W(), ticketID, mode); err != nil {
				_ = s.cleanupProbeArtifacts(ctx, obj.checksum, obj.fileID, ticketID)
				return nil, err
			}
		}
	}
	if mode != store.TicketDirect {
		session.Mode = string(mode)
		session.StreamURL = fmt.Sprintf("/api/fs/stream?ticket=%s", ticketID)
	}

	// 直链加密与中转加密都由前端本地解密，下发密钥与文件头材料；
	// 中转解密由服务器完成，只交明文，不交付任何密钥材料。
	if mode != store.TicketProxyDecrypt {
		session.BlockLog2 = int(obj.hdr.BlockLog2)
		session.NoncePrefix = obj.hdr.NoncePrefix
		session.Key = base64.StdEncoding.EncodeToString(obj.dek)
	}

	activeProbes.Store(obj.fileID, activeProbe{checksum: obj.checksum, ticketID: ticketID})
	s.audit(ctx, p, "storage.probe.start", obj.fileID,
		fmt.Sprintf("探针文件 %s 已上传，交付模式=%s", obj.fileName, session.Mode))
	return session, nil
}

// AdminStorageProbeFinish 清理探针的远端对象、内容池临时行与票据。仅管理员可用。
func (s *Service) AdminStorageProbeFinish(ctx context.Context, p auth.Principal, fileID string, result StorageProbeVerifyResult) error {
	if err := auth.RequirePermission(p, perm.AdminStorage); err != nil {
		return err
	}
	trace, ok := activeProbes.LoadAndDelete(fileID)
	if !ok {
		// 映射丢失（进程重启）时仍尽力删除远端对象；数据库侧残留交给
		// 过期票据维护与人工处置。
		if delErr := s.Backend.Delete(ctx, fileID); delErr != nil {
			return fmt.Errorf("清理失败（fileID=%s）: %w", fileID, delErr)
		}
		return fmt.Errorf("%w: 探针会话不存在或已过期，远端对象已清理", ErrNotFound)
	}
	ap := trace.(activeProbe)
	// 一次性物理清理票据、远端对象与内容池行（顺序见 cleanupProbeArtifacts
	// 的外键说明）。探针票据 reserved=0，无需结算或退款。
	if err := s.cleanupProbeArtifacts(ctx, ap.checksum, fileID, ap.ticketID); err != nil {
		s.audit(ctx, p, "storage.probe.finish", fileID, fmt.Sprintf("清理失败: %v", err))
		return err
	}
	s.audit(ctx, p, "storage.probe.finish", fileID,
		fmt.Sprintf("探针验证完成：模式=%s 成功=%v 字节=%d 耗时=%dms 详情=%s",
			result.Mode, result.OK, result.BytesGot, result.LatencyMs, result.Detail))
	return nil
}

// cleanupProbeArtifacts 删除探针的全部痕迹：票据、远端对象、内容池临时行。
//
// 顺序不能换：tickets.file_checksum 对 files 有外键约束，必须先物理删除
// 票据行再删内容池行，否则 FOREIGN KEY 失败，留下删不掉的库行与远端孤儿。
// ticketID 为空表示票据尚未创建（start 早期失败路径）。
func (s *Service) cleanupProbeArtifacts(ctx context.Context, checksum, fileID, ticketID string) error {
	var joined error
	if ticketID != "" {
		if err := store.DeleteTicket(ctx, s.DB.W(), ticketID); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	if err := s.Backend.Delete(ctx, fileID); err != nil {
		joined = errors.Join(joined, fmt.Errorf("删除存储对象失败: %w", err))
	}
	if err := store.ForceDeleteFileRow(ctx, s.DB.W(), checksum); err != nil {
		joined = errors.Join(joined, fmt.Errorf("删除内容池记录失败: %w", err))
	}
	return joined
}
