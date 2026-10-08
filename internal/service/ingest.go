package service

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/xph"
)

// MailBody 是邮件正文的加密容器载荷。
//
// HTML 与纯文本打成一个 JSON 对象后作为**1 个**内容池对象加密存储：
// 邮件部件数固定为"1 正文 + N 附件"，借壳交付时前端只取一次正文，
// 多 part/alternative 版本选择在客户端完成，服务端不解释渲染策略。
// 两种版本都可能为空（极端邮件只有附件），但容器本身始终存在。
type MailBody struct {
	HTML string `json:"html"`
	Text string `json:"text"`
}

// EncodeMailBody 把正文编码为入库的容器字节（UTF-8 JSON）。
func EncodeMailBody(b MailBody) ([]byte, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("编码邮件正文容器失败: %w", err)
	}
	return raw, nil
}

// DecodeMailBody 解析正文容器字节。
func DecodeMailBody(raw []byte) (MailBody, error) {
	var b MailBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return MailBody{}, fmt.Errorf("解析邮件正文容器失败: %w", err)
	}
	return b, nil
}

// IngestPlaintext 读入任意明文流，完成"哈希 → XPH 加密 → 写存储后端 →
// 登记 files 行（normal, ref_count=0）"的完整入库，返回内容池记录。
//
// 这是邮件入站 / 草稿与文件上传共用的内容入库原语，但刻意**不**做三件事：
//   - 不建用户可见节点（邮件走 mail_parts 引用）；
//   - 不调 AddFileRef（引用生命周期由调用方的落信事务管理）；
//   - 不碰配额（邮件配额按收件人人头在调用方预扣/结算）。
//
// 同 checksum 已有可用对象时直接复用，不重复上传后端（群发与重复投递只存一份）。
// 明文先落临时盘再加密：XPH 头需要明文长度，且后端 Put 需要可寻址的密文文件
// 来计算密文 MD5；这与上传分片收尾的约束相同。
func (s *Service) IngestPlaintext(ctx context.Context, r io.Reader, createdBy int64) (store.File, error) {
	if r == nil {
		return store.File{}, fmt.Errorf("%w: 缺少明文数据", ErrBadRequest)
	}
	if !s.EncryptionReady(ctx) {
		return store.File{}, fmt.Errorf("%w: 服务端未配置加密密钥", ErrUnavailable)
	}
	if !s.StorageReady() {
		return store.File{}, fmt.Errorf("%w: 存储后端未就绪", ErrStorageNotReady)
	}

	plainPath, plainSize, checksum, err := s.spoolPlaintext(ctx, r)
	if err != nil {
		return store.File{}, err
	}
	defer os.Remove(plainPath)

	// 去重在加密前判定：命中可用对象时连加密与上传都省掉。
	if existing, err := store.GetDedupCandidate(ctx, s.DB.R(), checksum, plainSize); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.File{}, err
	}
	// 可用记录未命中时，同 checksum 可能是一封旧草稿/旧附件释放引用后进入
	// 待回收状态的对象：物理对象还在，直接复活复用（重复正文/群发附件是
	// 高频路径，重走加密上传既慢又会撞主键）。被封禁内容不得复活。
	if existing, err := store.GetFile(ctx, s.DB.R(), checksum); err == nil {
		switch existing.Status {
		case store.FileArchive:
			if revived, err := store.ReviveArchivedFile(ctx, s.DB.W(), checksum); err == nil {
				return revived, nil
			} else if !errors.Is(err, store.ErrNoRowsAffected) {
				return store.File{}, err
			}
			// 并发下状态已被别的请求改变：落入正常入库路径，Insert 冲突
			// 分支会再次收敛。
		case store.FileDisabled:
			return store.File{}, fmt.Errorf("%w: 该内容已被管理员封禁", ErrForbidden)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.File{}, err
	}

	rec, err := s.newContentRecord(ctx, checksum, plainSize, createdBy)
	if err != nil {
		return store.File{}, err
	}
	// 邮件入库没有"上传中"阶段：加密与登记之间不存在用户驱动的会话，
	// 直接以可用状态登记。
	rec.Status = store.FileNormal

	cipherPath, cipherMD5, err := s.encryptPlainFile(ctx, plainPath, rec)
	if err != nil {
		return store.File{}, err
	}
	defer os.Remove(cipherPath)
	cipherFile, err := os.Open(cipherPath)
	if err != nil {
		return store.File{}, fmt.Errorf("%w: 打开密文临时文件失败", ErrUnavailable)
	}
	defer cipherFile.Close()

	put, err := s.putCipherObject(ctx, rec, cipherFile, cipherMD5)
	if err != nil {
		return store.File{}, err
	}
	rec.PanFileID = put.ObjectRef
	rec.PanObjectName = put.objectPath
	rec.PanSizeWire = put.sizeWire

	// 并发入库同一内容时另一路可能已经登记：复用既有行，删掉自己刚写的
	// 远端副本，保证物理对象不产生两份计费。
	if err := store.InsertFileNormal(ctx, s.DB.W(), rec); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return store.File{}, err
		}
		existing, getErr := store.GetFile(ctx, s.DB.R(), checksum)
		if getErr != nil {
			return store.File{}, err
		}
		if existing.Status == store.FileArchive {
			if revived, revErr := store.ReviveArchivedFile(ctx, s.DB.W(), checksum); revErr == nil {
				existing = revived
			} else {
				return store.File{}, err
			}
		} else if existing.Status != store.FileNormal {
			return store.File{}, err
		}
		if delErr := s.Backend.Delete(ctx, put.ObjectRef); delErr != nil {
			// 删不掉的副本只能等它成为孤儿后由维护侧发现；不阻断主流程。
			_ = delErr
		}
		return existing, nil
	}
	return store.GetFile(ctx, s.DB.R(), rec.Checksum)
}

// spoolPlaintext 把明文流落临时盘，同时计算长度与 SHA-256（十六进制）。
func (s *Service) spoolPlaintext(ctx context.Context, r io.Reader) (path string, size int64, checksum string, err error) {
	tmpDir, err := s.tempSubdir("ingest")
	if err != nil {
		return "", 0, "", err
	}
	f, err := os.CreateTemp(tmpDir, "plain-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("%w: 创建明文临时文件失败", ErrUnavailable)
	}
	path = f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), r)
	if err != nil {
		return "", 0, "", fmt.Errorf("%w: 接收明文失败: %v", ErrUnavailable, err)
	}
	if err := f.Sync(); err != nil {
		return "", 0, "", fmt.Errorf("%w: 明文落盘失败", ErrUnavailable)
	}
	ok = true
	return path, n, hex.EncodeToString(hash.Sum(nil)), nil
}

// encryptPlainFile 按内容池记录里冻结的加密参数，把明文文件加密为临时密文，
// 返回密文路径与密文 MD5。与上传 buildCiphertext 的加密段等价，但输入是
// 完整明文文件而非分片序列。
func (s *Service) encryptPlainFile(ctx context.Context, plainPath string, rec store.File) (string, string, error) {
	kek, err := s.KEKFor(ctx, rec.KEKKeyID)
	if err != nil {
		return "", "", err
	}
	dek, err := xph.UnwrapDEK(kek, rec.DEKEnvelope)
	if err != nil {
		return "", "", fmt.Errorf("%w: 解开内容密钥失败", ErrUnavailable)
	}
	hdr := xph.Header{
		Algo:        xph.AlgoAESGCM,
		BlockLog2:   byte(rec.EncChunkLog2),
		PlainSize:   rec.SizePlain,
		NoncePrefix: uint32(rec.EncNoncePrefix),
	}
	copy(hdr.FileSalt[:], rec.EncSalt)

	in, err := os.Open(plainPath)
	if err != nil {
		return "", "", fmt.Errorf("%w: 打开明文临时文件失败", ErrUnavailable)
	}
	defer in.Close()

	tmpDir, err := s.tempSubdir("ingest")
	if err != nil {
		return "", "", err
	}
	out, err := os.CreateTemp(tmpDir, "cipher-*.xph")
	if err != nil {
		return "", "", fmt.Errorf("%w: 创建密文临时文件失败", ErrUnavailable)
	}
	cipherPath := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			os.Remove(cipherPath)
		}
	}()

	cipherHash := md5.New()
	enc, err := xph.NewEncryptor(in, dek, hdr)
	if err != nil {
		return "", "", err
	}
	written, err := io.Copy(io.MultiWriter(out, cipherHash), enc)
	if err != nil {
		return "", "", fmt.Errorf("%w: 加密失败: %v", ErrUnavailable, err)
	}
	if want := hdr.CipherSize(); written != want {
		return "", "", fmt.Errorf("%w: 密文长度 %d，期望 %d", ErrBadRequest, written, want)
	}
	if err := out.Sync(); err != nil {
		return "", "", fmt.Errorf("%w: 密文落盘失败", ErrUnavailable)
	}
	ok = true
	return cipherPath, hex.EncodeToString(cipherHash.Sum(nil)), nil
}

// cipherPutResult 是一次后端写入的结果及其库内定位信息。
type cipherPutResult struct {
	// ObjectRef 是后端侧对象定位符（123 fileID）。
	ObjectRef string
	// objectPath 是逻辑对象名（YYYY/MM/DD/...），即 files.pan_object_name。
	objectPath string
	sizeWire   int64
}

// putCipherObject 把已就绪的密文文件写入存储后端。邮件入库与上传收尾共用，
// 保证对象命名、双口径长度与 MD5 语义只有一处实现。
//
// 调用方负责传入已经 Stat 可用的 *os.File；函数内部不再 Seek。
func (s *Service) putCipherObject(ctx context.Context, rec store.File, cipher *os.File, cipherMD5 string) (cipherPutResult, error) {
	panDir, panName, err := s.ObjectName(ctx, rec.Checksum)
	if err != nil {
		return cipherPutResult{}, err
	}
	info, err := cipher.Stat()
	if err != nil {
		return cipherPutResult{}, fmt.Errorf("%w: 读取密文长度失败", ErrUnavailable)
	}
	put, err := s.Backend.Put(ctx, backend.PutRequest{
		LogicalName: panName,
		ParentDir:   panDir,
		SizePlain:   rec.SizePlain,
		SizeWire:    info.Size(),
		CipherMD5:   cipherMD5,
		Source:      cipher,
		BlockSize:   s.BlockSize(ctx),
		MaxPartSize: s.Settings.Runtime(ctx).Upload.MaxVolumeBytes,
	})
	if err != nil {
		return cipherPutResult{}, fmt.Errorf("%w: 写入存储后端失败: %w", ErrUnavailable, err)
	}
	return cipherPutResult{
		ObjectRef:  put.ObjectRef,
		objectPath: panDir + "/" + panName,
		sizeWire:   info.Size(),
	}, nil
}
