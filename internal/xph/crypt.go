package xph

import (
	"bytes"
	"errors"
	"io"
)

// ErrTooMuchData 表示来源数据比文件头声明的明文长度更长。
var ErrTooMuchData = errors.New("xph: 输入数据超出声明的明文长度")

// Encryptor 是流式加密读取器：先输出 64 字节文件头，随后逐块输出「密文‖tag」。
//
// 明文总长必须事先已知（上传场景天然有 Content-Length），这是生成文件头与确定
// 最后一块长度的前提。输出严格等于 hdr.CipherSize() 字节：来源多出哪怕 1 字节
// 也会报错，避免"声明的长度"与"实际写入的长度"不一致地落库。
type Encryptor struct {
	src        io.Reader
	dek        []byte
	hdr        Header
	aad        []byte
	header     []byte
	headerSent bool
	blockIndex int64
	pending    []byte
	pendingAt  int
	srcBuf     []byte
	err        error
}

// NewEncryptor 构造加密读取器。hdr 必须已填好 Algo / BlockLog2 / PlainSize，
// 并建议用 NewHeaderRandom 生成随机的 FileSalt 与 NoncePrefix。
func NewEncryptor(src io.Reader, dek []byte, hdr Header) (*Encryptor, error) {
	if len(dek) != KeySize {
		return nil, ErrBadKeySize
	}
	if hdr.BlockLog2 < MinBlockLog2 || hdr.BlockLog2 > MaxBlockLog2 {
		return nil, ErrBadBlockLog2
	}
	hdr.Algo = AlgoAESGCM
	if hdr.PlainSize < 0 || hdr.CipherSize() < 0 {
		return nil, ErrBadPlainSize
	}
	return &Encryptor{
		src:    src,
		dek:    dek,
		hdr:    hdr,
		aad:    hdr.AAD(),
		header: hdr.Marshal(),
		srcBuf: make([]byte, hdr.BlockSize()),
	}, nil
}

func (e *Encryptor) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if !e.headerSent {
		if len(p) == 0 {
			return 0, nil
		}
		n := copy(p, e.header)
		e.header = e.header[n:]
		if len(e.header) == 0 {
			e.headerSent = true
		}
		return n, nil
	}
	if e.pendingAt < len(e.pending) {
		n := copy(p, e.pending[e.pendingAt:])
		e.pendingAt += n
		return n, nil
	}
	if err := e.encryptNextBlock(); err != nil {
		// 末块输出完毕后的那一次调用才会返回 io.EOF，收尾校验必须放在这里，
		// 直接返回 EOF 会让它永远跑不到。
		if err == io.EOF {
			if derr := e.ensureSourceDrained(); derr != nil {
				err = derr
			}
		}
		e.err = err
		return 0, err
	}
	n := copy(p, e.pending[e.pendingAt:])
	e.pendingAt += n
	return n, nil
}

func (e *Encryptor) encryptNextBlock() error {
	start := e.blockIndex * e.hdr.BlockSize()
	if start >= e.hdr.PlainSize {
		return io.EOF
	}
	want := e.hdr.PayloadLen(e.blockIndex)
	n, err := io.ReadFull(e.src, e.srcBuf[:want])
	if int64(n) != want {
		if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	nonce := BlockNonce(e.hdr.NoncePrefix, e.blockIndex)
	out, encErr := ChunkEncrypt(e.dek, e.aad, nonce, e.srcBuf[:want])
	if encErr != nil {
		return encErr
	}
	e.pending = out
	e.pendingAt = 0
	e.blockIndex++
	return nil
}

// ensureSourceDrained 确认来源在声明长度之后没有多余数据。
func (e *Encryptor) ensureSourceDrained() error {
	var probe [1]byte
	n, err := e.src.Read(probe[:])
	if n > 0 {
		return ErrTooMuchData
	}
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// Decryptor 是按块顺序解密的读取器，用于把连续密文流还原为明文流
// （服务端兜底交付、测试与一致性校验）。底层流应已跳过 64 字节文件头。
//
// 内存占用与单块大小相当，与文件体积无关。
type Decryptor struct {
	src        io.Reader
	dek        []byte
	hdr        Header
	aad        []byte
	blockIndex int64
	pending    []byte
	pendingAt  int
	produced   int64
	buf        []byte
	err        error
}

// NewDecryptor 构造顺序解密读取器。
func NewDecryptor(body io.Reader, dek []byte, hdr Header) (*Decryptor, error) {
	if len(dek) != KeySize {
		return nil, ErrBadKeySize
	}
	return &Decryptor{
		src: body,
		dek: dek,
		hdr: hdr,
		aad: hdr.AAD(),
	}, nil
}

func (d *Decryptor) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	for {
		if d.pendingAt < len(d.pending) {
			n := copy(p, d.pending[d.pendingAt:])
			d.pendingAt += n
			return n, nil
		}
		if d.produced >= d.hdr.PlainSize {
			d.err = io.EOF
			return 0, io.EOF
		}
		if err := d.decryptNextBlock(); err != nil {
			d.err = err
			return 0, err
		}
	}
}

func (d *Decryptor) decryptNextBlock() error {
	cipherLen := d.hdr.BlockCipherLen(d.blockIndex)
	if int64(cap(d.buf)) < cipherLen {
		d.buf = make([]byte, cipherLen)
	}
	buf := d.buf[:cipherLen]
	if _, err := io.ReadFull(d.src, buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return ErrShortCipher
		}
		return err
	}
	nonce := BlockNonce(d.hdr.NoncePrefix, d.blockIndex)
	plain, err := ChunkDecrypt(d.dek, d.aad, nonce, buf)
	if err != nil {
		return err
	}
	d.pending = plain
	d.pendingAt = 0
	d.produced += int64(len(plain))
	d.blockIndex++
	return nil
}

// EncryptAll 一次性加密整个明文，供测试与一致性校验使用。
func EncryptAll(plain []byte, dek []byte, hdr Header) ([]byte, error) {
	enc, err := NewEncryptor(bytes.NewReader(plain), dek, hdr)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(enc)
}

// DecryptAll 一次性解密整个密文，供测试与一致性校验使用。
func DecryptAll(cipher []byte, dek []byte) ([]byte, error) {
	hdr, err := ParseHeader(cipher)
	if err != nil {
		return nil, err
	}
	if err := hdr.VerifyCipherSize(int64(len(cipher))); err != nil {
		return nil, err
	}
	dec, err := NewDecryptor(bytes.NewReader(cipher[HeaderSize:]), dek, hdr)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(dec)
}
