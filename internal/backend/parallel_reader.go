package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
)

const relayReadConcurrency = 4

// Windows are reserved in output order and retain their backend-wide slot until
// consumed. This bounds both requests and buffered ciphertext: four windows per
// reader (16 MiB), sixteen across one backend (64 MiB). Nonblocking speculative
// reservations avoid deadlocking demand reads behind out-of-order windows.
type parallelRangeReader struct {
	ctx                  context.Context
	client               *http.Client
	url                  string
	size, pos, end, next int64
	slots                chan struct{}
	cancel               context.CancelFunc
	readCtx              context.Context
	workers              sync.WaitGroup
	windows              []*readWindow
	sequential           *httpRangeReader
	sequentialRelease    func()
	closed               bool
}

type readWindow struct {
	offset  int64
	data    []byte
	err     error
	done    chan struct{}
	release func()
}

func newParallelRangeReader(ctx context.Context, client *http.Client, link string, size, offset, length int64, slots chan struct{}) (*parallelRangeReader, error) {
	if offset < 0 || offset > size {
		return nil, fmt.Errorf("backend: 读取位置超出对象范围")
	}
	end := size
	if length > 0 && length < size-offset {
		end = offset + length
	}
	return &parallelRangeReader{ctx: ctx, client: client, url: link, size: size, pos: offset, end: end, slots: slots}, nil
}

func (r *parallelRangeReader) reserve(wait bool) (func(), error) {
	if r.slots == nil {
		return func() {}, nil
	}
	if wait {
		select {
		case r.slots <- struct{}{}:
		case <-r.readCtx.Done():
			return nil, r.readCtx.Err()
		}
	} else {
		select {
		case r.slots <- struct{}{}:
		default:
			return nil, nil
		}
	}
	var once sync.Once
	return func() { once.Do(func() { <-r.slots }) }, nil
}

func (r *parallelRangeReader) fetch(offset, end int64, requirePartial bool) (*httpRangeReader, error) {
	base := &httpRangeReader{ctx: r.readCtx, client: r.client, url: r.url, size: r.size, pos: offset, readEnd: end, requirePartial: requirePartial}
	if err := base.fetchWindow(); err != nil {
		_ = base.Close()
		return nil, err
	}
	return base, nil
}

func (r *parallelRangeReader) windowEnd(offset int64) int64 {
	if r.end-offset < rangeReadChunk {
		return r.end
	}
	return offset + rangeReadChunk
}

func (r *parallelRangeReader) start() error {
	r.readCtx, r.cancel = context.WithCancel(r.ctx)
	release, err := r.reserve(true)
	if err != nil {
		r.stop()
		return err
	}
	// Probe Range support before spawning requests. A CDN returning 200 gets one
	// retained sequential response, never four full-file responses.
	base, err := r.fetch(r.pos, r.windowEnd(r.pos), false)
	if err != nil {
		release()
		r.stop()
		return err
	}
	if base.rangeIgnored {
		base.readEnd = r.end
		r.sequential = base
		r.sequentialRelease = release
		return nil
	}
	done := make(chan struct{})
	close(done)
	r.windows = []*readWindow{{offset: r.pos, data: base.buf, done: done, release: release}}
	r.next = r.pos + int64(len(base.buf))
	return r.fill(false)
}

func (r *parallelRangeReader) fill(wait bool) error {
	for len(r.windows) < relayReadConcurrency && r.next < r.end {
		release, err := r.reserve(wait && len(r.windows) == 0)
		if err != nil {
			return err
		}
		if release == nil {
			break
		}
		offset, end := r.next, r.windowEnd(r.next)
		w := &readWindow{offset: offset, done: make(chan struct{}), release: release}
		r.windows = append(r.windows, w)
		r.next = end
		r.workers.Add(1)
		go func() {
			defer r.workers.Done()
			defer close(w.done)
			base, err := r.fetch(offset, end, true)
			if err != nil {
				w.err = err
				return
			}
			w.data = base.buf
		}()
	}
	return nil
}

func (r *parallelRangeReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.pos >= r.end {
		return 0, io.EOF
	}
	if r.cancel == nil {
		if err := r.start(); err != nil {
			return 0, err
		}
	}
	if r.sequential != nil {
		n, err := r.sequential.Read(p)
		r.pos += int64(n)
		return n, err
	}
	if len(r.windows) == 0 {
		if err := r.fill(true); err != nil {
			return 0, err
		}
	}
	w := r.windows[0]
	select {
	case <-w.done:
	case <-r.readCtx.Done():
		err := r.readCtx.Err()
		r.stop()
		return 0, err
	}
	if w.err != nil {
		err := w.err
		r.stop()
		return 0, err
	}
	n := copy(p, w.data[r.pos-w.offset:])
	r.pos += int64(n)
	if r.pos == w.offset+int64(len(w.data)) {
		w.data = nil
		w.release()
		r.windows = r.windows[1:]
		_ = r.fill(false)
	}
	return n, nil
}

func (r *parallelRangeReader) stop() {
	if r.cancel != nil {
		r.cancel()
		r.workers.Wait()
		r.cancel = nil
	}
	if r.sequential != nil {
		_ = r.sequential.Close()
		r.sequential = nil
		r.sequentialRelease()
		r.sequentialRelease = nil
	}
	for _, w := range r.windows {
		w.release()
	}
	r.windows = nil
}

func (r *parallelRangeReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	probe := httpRangeReader{pos: r.pos, size: r.size}
	target, err := probe.Seek(offset, whence)
	if err != nil {
		return 0, err
	}
	if target != r.pos {
		r.stop()
		r.pos = target
	}
	return target, nil
}

func (r *parallelRangeReader) Close() error { r.closed = true; r.stop(); return nil }
