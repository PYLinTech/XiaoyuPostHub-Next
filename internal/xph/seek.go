package xph

import (
	"bytes"
	"context"
	"io"
)

// OpenRange 按 (offset, length) 打开底层密文流；length <= 0 表示读到末尾。
type OpenRange func(ctx context.Context, offset, length int64) (io.ReadCloser, error)

// DecryptSeek 返回一个从明文 offset 起读 limit 字节的解密流（limit <= 0 表示读到末尾）。
//
// 只打开一次底层密文流：先按块对齐算出所需密文区间，再顺序读出每块、解密、
// 丢弃块首偏移与尾部越界部分。内存占用与单块大小相当，与文件体积无关。
//
// 这是服务端兜底交付（客户端不具备前端解密能力时）与 WebDAV/S3 等消费入口
// 的取数原语。
func DecryptSeek(ctx context.Context, open OpenRange, hdr Header, dek []byte, offset, limit int64) (io.ReadCloser, error) {
	if len(dek) != KeySize {
		return nil, ErrBadKeySize
	}
	if offset < 0 || offset > hdr.PlainSize {
		return nil, ErrBadRange
	}
	remaining := hdr.PlainSize - offset
	if limit <= 0 || limit > remaining {
		limit = hdr.PlainSize - offset
	}
	if limit <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	first, last, start, end, skip, err := hdr.ChunkRange(offset, limit)
	if err != nil {
		return nil, err
	}

	// 派生上下文要在打开底层对象之前创建，并传给 open：调用方关闭返回的
	// reader 时，远端 Range 请求也必须随之取消。若这里仍把原始 ctx 传下去，
	// Pipe 虽然关闭了，HTTP 客户端可能还会把连接挂到自身超时。
	readCtx, cancel := context.WithCancel(ctx)
	rc, err := open(readCtx, start, end-start)
	if err != nil {
		cancel()
		return nil, err
	}

	// readCtx 在调用方 Close 或上游 ctx 取消时被取消，用于尽快中断阻塞中的网络读取。
	pr, pw := io.Pipe()
	aad := hdr.AAD()

	go func() {
		defer cancel()
		defer rc.Close()

		var writeErr error
		defer func() { _ = pw.CloseWithError(writeErr) }()

		remaining := limit
		skipNow := skip
		for index := first; index <= last; index++ {
			if err := readCtx.Err(); err != nil {
				writeErr = err
				return
			}
			cipherLen := hdr.BlockCipherLen(index)
			buf := make([]byte, cipherLen)
			if _, err := io.ReadFull(rc, buf); err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					err = ErrShortCipher
				}
				writeErr = err
				return
			}
			nonce := BlockNonce(hdr.NoncePrefix, index)
			plain, err := ChunkDecrypt(dek, aad, nonce, buf)
			if err != nil {
				writeErr = err
				return
			}
			from := skipNow
			if from > int64(len(plain)) {
				from = int64(len(plain))
			}
			chunk := plain[from:]
			if int64(len(chunk)) > remaining {
				chunk = chunk[:remaining]
			}
			if len(chunk) > 0 {
				if _, err := pw.Write(chunk); err != nil {
					writeErr = err
					return
				}
				remaining -= int64(len(chunk))
				if remaining <= 0 {
					return
				}
			}
			skipNow = 0
		}
	}()

	// ctx 被取消时立即关断管道，让仍在网络读取中的 goroutine 尽快退出，
	// 而不是把连接一直挂到超时。
	go func() {
		<-readCtx.Done()
		_ = pw.CloseWithError(readCtx.Err())
	}()

	return &seekReader{Reader: pr, cancel: cancel}, nil
}

// seekReader 在 Close 时同步取消派生 ctx，避免底层连接长时间挂着。
type seekReader struct {
	io.Reader
	cancel context.CancelFunc
	closed bool
}

func (r *seekReader) Close() error {
	if !r.closed {
		r.closed = true
		r.cancel()
	}
	return nil
}
