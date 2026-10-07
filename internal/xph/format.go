// Package xph 实现 XPH-Crypt v1：定长块 AES-256-GCM 分块加密。
//
// 文件布局：
//
//	Header(64 B, 明文) ‖ [CT₀‖Tag₁₆] [CT₁‖Tag₁₆] … [CTₙ₋₁‖Tag₁₆]
//
// 密文长度 = 64 + plainSize + 16 × N，N = ceil(plainSize / B)。
//
// 块大小固定使明文偏移与密文偏移可以直接换算，因此 Range、音视频 seek、断点续传
// 与预览在**密文对象**上全部可用，不需要下载整个文件。
//
// 关键约束：GCM 的 tag 覆盖整块，**块不可截断**。任何读取都必须先按块对齐算出
// 完整密文区间，读出整块解密后再裁剪两端。
//
// 文件头是自描述的并作为 AAD 参与每块认证：篡改块大小或明文长度会让解密直接
// 认证失败，而不是解析出错后产生难以归因的行为。头的内容与数据库记录交叉校验，
// 可捕获占位对象、串文件与截断。
package xph

import (
	"encoding/binary"
	"errors"
)

const (
	// Magic 是文件头魔数，末位内嵌格式版本。
	Magic      = "XPHCRPT1"
	MagicLen   = len(Magic)
	HeaderSize = 64
	// AADSize 是参与认证的头部前缀长度（magic..noncePrefix）。
	// 刻意排除 reserved 段：将来使用 reserved 不应破坏既有对象的解密。
	AADSize = 40
	// TagSize 是 AES-GCM 认证标签长度。
	TagSize = 16
	// KeySize 是 AES-256 密钥长度。
	KeySize = 32
	// NonceSize 是 GCM nonce 长度。
	NonceSize = 12
	// SaltSize 是每文件随机盐长度。盐不参与任何密钥派生（密钥只有数据库
	// 信封一个来源），仅作为 AEAD 关联数据把密文与文件头绑定。
	SaltSize = 16
	// EnvelopeSize 是 KEK 包裹 DEK 后的信封长度：nonce ‖ 密文 ‖ tag。
	EnvelopeSize = 12 + KeySize + TagSize

	// AlgoAESGCM 是唯一支持的算法标识。
	AlgoAESGCM byte = 1
	// DefaultBlockLog2 是默认块大小指数：1<<20 = 1 MiB。
	DefaultBlockLog2 byte = 20
	// MinBlockLog2 是块大小指数下限：1<<9 = 512 B。
	MinBlockLog2 byte = 9
	// MaxBlockLog2 是块大小指数上限：1<<26 = 64 MiB。
	MaxBlockLog2 byte = 26

	maxInt64 = int64(^uint64(0) >> 1)
)

var (
	ErrBadMagic      = errors.New("xph: 魔数不匹配")
	ErrShortHeader   = errors.New("xph: 文件头被截断")
	ErrBadAlgo       = errors.New("xph: 不支持的加密算法")
	ErrBadBlockLog2  = errors.New("xph: 非法的块大小")
	ErrBadPlainSize  = errors.New("xph: 非法的明文长度")
	ErrShortCipher   = errors.New("xph: 密文过短")
	ErrBadCipherSize = errors.New("xph: 密文长度与文件头不一致")
	ErrBadEnvelope   = errors.New("xph: 密钥信封损坏")
	ErrBadKeySize    = errors.New("xph: 密钥长度必须为 32 字节")
	ErrBadRange      = errors.New("xph: 非法的读取区间")
)

// Header 是 64 字节文件头的内存表示。
type Header struct {
	Algo        byte
	BlockLog2   byte
	Flags       uint16
	FileSalt    [SaltSize]byte
	PlainSize   int64
	NoncePrefix uint32
}

// NewHeader 构造一个头部骨架：算法、块大小与明文长度已填好，
// FileSalt 与 NoncePrefix 由调用方（或 NewHeaderRandom）填充。
func NewHeader(plainSize int64, blockLog2 byte) Header {
	return Header{
		Algo:      AlgoAESGCM,
		BlockLog2: blockLog2,
		PlainSize: plainSize,
	}
}

func (h Header) BlockSize() int64 {
	if h.BlockLog2 < MinBlockLog2 || h.BlockLog2 > MaxBlockLog2 {
		return 0
	}
	return int64(1) << h.BlockLog2
}

// BlockCount 返回块数；空文件为 0。
func (h Header) BlockCount() int64 {
	if h.PlainSize <= 0 {
		return 0
	}
	bs := h.BlockSize()
	if bs == 0 || h.PlainSize > maxInt64-(bs-1) {
		return 0
	}
	return (h.PlainSize + bs - 1) / bs
}

// PayloadLen 返回第 i 块承载的明文长度。
func (h Header) PayloadLen(i int64) int64 {
	n := h.BlockCount()
	if i < 0 || i >= n {
		return 0
	}
	bs := h.BlockSize()
	if bs == 0 {
		return 0
	}
	if i < n-1 {
		return bs
	}
	return h.PlainSize - (n-1)*bs
}

// CipherSize 返回整个密文对象的长度。
func (h Header) CipherSize() int64 {
	_, _, size, ok := h.geometry()
	if !ok {
		return -1
	}
	return size
}

// BlockCipherOffset 返回第 i 块密文（含 tag）的起始偏移。
func (h Header) BlockCipherOffset(i int64) int64 {
	bs := h.BlockSize()
	if i < 0 || bs == 0 || i > (maxInt64-int64(HeaderSize))/(bs+TagSize) {
		return -1
	}
	return int64(HeaderSize) + i*(bs+TagSize)
}

// BlockCipherLen 返回第 i 块密文（含 tag）的长度。
func (h Header) BlockCipherLen(i int64) int64 { return h.PayloadLen(i) + TagSize }

// geometry returns a checked block layout. The database is trusted only as far
// as its schema types; a hand-edited or corrupted row must not make arithmetic
// wrap around into a smaller remote range.
func (h Header) geometry() (blockSize, blocks, cipherSize int64, ok bool) {
	blockSize = h.BlockSize()
	if h.PlainSize < 0 || blockSize == 0 {
		return 0, 0, 0, false
	}
	if h.PlainSize > maxInt64-int64(HeaderSize) {
		return 0, 0, 0, false
	}
	if h.PlainSize > 0 {
		if h.PlainSize > maxInt64-(blockSize-1) {
			return 0, 0, 0, false
		}
		blocks = (h.PlainSize + blockSize - 1) / blockSize
	}
	remaining := maxInt64 - int64(HeaderSize) - h.PlainSize
	if blocks > remaining/TagSize {
		return 0, 0, 0, false
	}
	cipherSize = int64(HeaderSize) + h.PlainSize + blocks*TagSize
	return blockSize, blocks, cipherSize, true
}

// ChunkRange 返回覆盖明文区间 [offset, offset+limit) 所需的完整密文区间。
//
// 返回的 [start, end) 一定按块对齐取满；limit <= 0 表示读到明文末尾。
// skip 是首块内需要丢弃的字节数（明文偏移在块内的位置）。
func (h Header) ChunkRange(offset, limit int64) (first, last, start, end, skip int64, err error) {
	bs, blocks, _, ok := h.geometry()
	if !ok {
		return 0, 0, 0, 0, 0, ErrBadRange
	}
	if offset < 0 {
		return 0, 0, 0, 0, 0, ErrBadRange
	}
	if offset > h.PlainSize {
		return 0, 0, 0, 0, 0, ErrBadRange
	}
	remaining := h.PlainSize - offset
	if limit <= 0 || limit > remaining {
		limit = h.PlainSize - offset
	}
	if limit == 0 {
		return 0, 0, 0, 0, 0, nil
	}
	first = offset / bs
	// offset 与 limit 已经分别验证过，且 limit <= PlainSize-offset，
	// 因此这里不会因恶意超大 Range 溢出回绕。
	last = (offset + limit - 1) / bs
	if first < 0 || first >= blocks || last < first || last >= blocks {
		return 0, 0, 0, 0, 0, ErrBadRange
	}
	skip = offset - first*bs
	start, ok = checkedBlockCipherOffset(first, bs)
	if !ok {
		return 0, 0, 0, 0, 0, ErrBadRange
	}
	lastStart, ok := checkedBlockCipherOffset(last, bs)
	if !ok || h.BlockCipherLen(last) < 0 || lastStart > maxInt64-h.BlockCipherLen(last) {
		return 0, 0, 0, 0, 0, ErrBadRange
	}
	end = lastStart + h.BlockCipherLen(last)
	return first, last, start, end, skip, nil
}

func checkedBlockCipherOffset(index, blockSize int64) (int64, bool) {
	if index < 0 || blockSize <= 0 || blockSize > maxInt64-TagSize {
		return 0, false
	}
	unit := blockSize + TagSize
	if index > (maxInt64-int64(HeaderSize))/unit {
		return 0, false
	}
	return int64(HeaderSize) + index*unit, true
}

// Marshal 序列化为 64 字节。
func (h Header) Marshal() []byte {
	buf := make([]byte, HeaderSize)
	copy(buf, Magic)
	buf[MagicLen] = h.Algo
	buf[MagicLen+1] = h.BlockLog2
	binary.LittleEndian.PutUint16(buf[10:12], h.Flags)
	copy(buf[12:28], h.FileSalt[:])
	binary.LittleEndian.PutUint64(buf[28:36], uint64(h.PlainSize))
	binary.LittleEndian.PutUint32(buf[36:40], h.NoncePrefix)
	return buf
}

// AAD 返回参与每块认证的关联数据。
func (h Header) AAD() []byte {
	return h.Marshal()[:AADSize]
}

// ParseHeader 从至少 64 字节的缓冲区恢复文件头。
func ParseHeader(buf []byte) (Header, error) {
	var h Header
	if len(buf) < HeaderSize {
		return h, ErrShortHeader
	}
	if string(buf[:MagicLen]) != Magic {
		return h, ErrBadMagic
	}
	h.Algo = buf[MagicLen]
	if h.Algo != AlgoAESGCM {
		return h, ErrBadAlgo
	}
	h.BlockLog2 = buf[MagicLen+1]
	if h.BlockLog2 < MinBlockLog2 || h.BlockLog2 > MaxBlockLog2 {
		return h, ErrBadBlockLog2
	}
	h.Flags = binary.LittleEndian.Uint16(buf[10:12])
	copy(h.FileSalt[:], buf[12:28])
	raw := binary.LittleEndian.Uint64(buf[28:36])
	if raw > 1<<62 {
		return h, ErrBadPlainSize
	}
	h.PlainSize = int64(raw)
	h.NoncePrefix = binary.LittleEndian.Uint32(buf[36:40])
	return h, nil
}

// CipherSizeOf 由明文长度与块大小指数推算密文总长度。
func CipherSizeOf(plainSize int64, blockLog2 byte) int64 {
	return NewHeader(plainSize, blockLog2).CipherSize()
}

// BlockNonce 生成第 index 块的 12 字节 nonce：4 字节随机前缀 + 8 字节大端块号。
//
// 因为每文件的 DEK 是随机的，(DEK, nonce) 天然全局不重复，前缀只是纵深防御。
func BlockNonce(prefix uint32, index int64) []byte {
	nonce := make([]byte, NonceSize)
	binary.BigEndian.PutUint32(nonce[0:4], prefix)
	binary.BigEndian.PutUint64(nonce[4:12], uint64(index))
	return nonce
}

// VerifyCipherSize 校验密文长度与头部声明是否自洽。
func (h Header) VerifyCipherSize(actual int64) error {
	if actual < 0 || h.CipherSize() < 0 || h.CipherSize() != actual {
		return ErrBadCipherSize
	}
	return nil
}
