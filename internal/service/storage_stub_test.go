package service

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/backend"
)

// 存储后端桩。仅测试用。
//
// 存在的意义是让"存储已就绪"成为测试可以显式打开的条件：真实实现需要 123 的
// 凭据，而大量业务行为（会话数上限、配额预扣、秒传）并不需要真的连上游，
// 只需要一个"就绪"的状态。反过来，未安装桩的测试装置就是"存储未就绪"，
// 这正好覆盖了 fail-fast 那条路径。
type storageStub struct {
	mu      sync.Mutex
	objects map[string][]byte
	next    int
	// deleted 记录每一次 Delete 调用的定位符，供断言"旧对象确实被删过"。
	deleted []string
	// presignReady 控制直链能力：它决定交付走直链还是本机中转。
	presignReady bool
	// presignErr 非 nil 时 Presign 直接失败，用于编排层"直链签发失败 →
	// 服务端中转"降级测试。
	presignErr error
	// deleteErr 非 nil 时 Delete 返回该错误，用于连通性探针的清理失败路径。
	deleteErr error
	// openErr 非 nil 时 Open 返回该错误，用于模拟上游瞬时打开失败
	// （播放 seek 分片遇到网关错误等）。
	openErr error
	// directLinks 按调用顺序记录每次直链空间开关的目标状态。
	directLinks []bool
	// toggleErr 非 nil 时 SetDirectLink 返回该错误。
	toggleErr error
}

func newStorageStub() *storageStub {
	return &storageStub{objects: map[string][]byte{}, presignReady: true}
}

func (s *storageStub) Kind() string { return "stub" }

func (s *storageStub) Put(_ context.Context, req backend.PutRequest) (backend.PutResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ref := "stub-" + strconv.Itoa(s.next)
	// Source 是 io.ReaderAt：按声明长度读出整份密文。
	body := make([]byte, req.SizeWire)
	if req.SizeWire > 0 {
		if _, err := req.Source.ReadAt(body, 0); err != nil && !errors.Is(err, io.EOF) {
			return backend.PutResult{}, err
		}
	}
	s.objects[ref] = body
	return backend.PutResult{ObjectRef: ref}, nil
}

func (s *storageStub) Open(_ context.Context, ref string) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openErr != nil {
		return nil, s.openErr
	}
	body, ok := s.objects[ref]
	if !ok {
		return nil, errors.New("stub: 对象不存在")
	}
	return readSeekCloser{reader: newByteReader(body)}, nil
}

func (s *storageStub) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.objects, ref)
	s.deleted = append(s.deleted, ref)
	return nil
}

// deletedRefs 返回 Delete 的调用记录副本。
func (s *storageStub) deletedRefs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

func (s *storageStub) Stat(_ context.Context, ref string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[ref]
	if !ok {
		return 0, errors.New("stub: 对象不存在")
	}
	return int64(len(body)), nil
}

func (s *storageStub) Presign(_ context.Context, ref string, _ backend.PresignOptions) (string, error) {
	if s.presignErr != nil {
		return "", s.presignErr
	}
	return "https://stub.invalid/" + ref, nil
}

func (s *storageStub) PresignReady() bool { return s.presignReady }

func (s *storageStub) SliceMD5Enabled() bool { return false }

// SetDirectLink 实现 backend.DirectLinkSwitcher。
func (s *storageStub) SetDirectLink(_ context.Context, enabled bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toggleErr != nil {
		return "", s.toggleErr
	}
	s.directLinks = append(s.directLinks, enabled)
	return "stub-直链目录", nil
}

// directLinkToggles 返回直链开关调用记录副本。
func (s *storageStub) directLinkToggles() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.directLinks...)
}

type readSeekCloser struct {
	reader *byteReader
}

func (r readSeekCloser) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r readSeekCloser) Seek(off int64, whence int) (int64, error) {
	return r.reader.Seek(off, whence)
}
func (r readSeekCloser) Close() error { return nil }

type byteReader struct {
	data []byte
	pos  int64
}

func newByteReader(data []byte) *byteReader { return &byteReader{data: data} }

func (b *byteReader) Read(p []byte) (int, error) {
	if b.pos >= int64(len(b.data)) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += int64(n)
	return n, nil
}

func (b *byteReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		b.pos = off
	case io.SeekCurrent:
		b.pos += off
	case io.SeekEnd:
		b.pos = int64(len(b.data)) + off
	}
	if b.pos < 0 {
		return 0, errors.New("stub: 偏移越界")
	}
	return b.pos, nil
}

// enableStorage 让测试装置具备一个"已就绪"的存储后端。
func (f *shareFixture) enableStorage() {
	f.t.Helper()
	f.svc.Backend = newStorageStub()
}
