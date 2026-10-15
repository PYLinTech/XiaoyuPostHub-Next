package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func rangeResponse(start, end, size int64, body io.ReadCloser) *http.Response {
	h := make(http.Header)
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	return &http.Response{StatusCode: 206, Header: h, Body: body}
}

func parseTestRange(req *http.Request) (int64, int64) {
	var start, end int64
	_, _ = fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end)
	return start, end
}

func TestParallelReaderFetchesAheadInOrderAndBoundsRange(t *testing.T) {
	const start = int64(123)
	const length = int64(rangeReadChunk*5 + 17)
	const size = start + length + rangeReadChunk
	ready := make(chan struct{}, relayReadConcurrency)
	release := make(chan struct{})
	var peak, active atomic.Int64
	var mu sync.Mutex
	var ranges [][2]int64
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		off, end := parseTestRange(req)
		mu.Lock()
		ranges = append(ranges, [2]int64{off, end})
		mu.Unlock()
		if off > start {
			n := active.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			defer active.Add(-1)
			ready <- struct{}{}
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		data := bytes.Repeat([]byte{byte((off - start) / rangeReadChunk)}, int(end-off+1))
		return rangeResponse(off, end, size, io.NopCloser(bytes.NewReader(data))), nil
	})}
	r, err := newParallelRangeReader(context.Background(), client, "https://cdn/object", size, start, length, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first := make([]byte, 1)
	if _, err = r.Read(first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < relayReadConcurrency-1; i++ {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatal("prefetch did not run concurrently")
		}
	}
	close(release)
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	got := append(first, rest...)
	if int64(len(got)) != length {
		t.Fatalf("length=%d", len(got))
	}
	for i, v := range got {
		if v != byte(i/rangeReadChunk) {
			t.Fatalf("out of order at %d: %d", i, v)
		}
	}
	if peak.Load() < 3 || peak.Load() > relayReadConcurrency {
		t.Fatalf("peak=%d", peak.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, v := range ranges {
		if v[0] < start || v[1] >= start+length {
			t.Fatalf("over-read requested interval: %v", v)
		}
	}
}

func TestParallelReaderSmallRangeDoesNotPrefetch(t *testing.T) {
	var calls int
	const size = int64(100 * rangeReadChunk)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		start, end := parseTestRange(req)
		if start != 999 || end != 1005 {
			t.Errorf("range=%s", req.Header.Get("Range"))
		}
		return rangeResponse(start, end, size, io.NopCloser(strings.NewReader("1234567"))), nil
	})}
	r, _ := newParallelRangeReader(context.Background(), client, "https://cdn/object", size, 999, 7, nil)
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "1234567" || calls != 1 {
		t.Fatalf("got=%q calls=%d err=%v", got, calls, err)
	}
}

func TestParallelReaderCancelsPrefetchOnCloseAndSeek(t *testing.T) {
	for _, seek := range []bool{false, true} {
		t.Run(fmt.Sprint(seek), func(t *testing.T) {
			entered := make(chan struct{}, 4)
			var active atomic.Int64
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				start, end := parseTestRange(req)
				if start > 0 {
					active.Add(1)
					defer active.Add(-1)
					entered <- struct{}{}
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return rangeResponse(start, end, 10*rangeReadChunk, io.NopCloser(bytes.NewReader(make([]byte, end-start+1)))), nil
			})}
			r, _ := newParallelRangeReader(context.Background(), client, "https://cdn/object", 10*rangeReadChunk, 0, 0, nil)
			defer r.Close()
			if _, err := r.Read(make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("prefetch not started")
				}
			}
			if seek {
				if _, err := r.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = r.Close()
			}
			if active.Load() != 0 {
				t.Fatalf("requests still running=%d", active.Load())
			}
			if seek {
				if _, err := r.Read(make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestParallelReaderIgnoresRangeWithOnlyOneResponse(t *testing.T) {
	data := bytes.Repeat([]byte("abc"), 3*rangeReadChunk)
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	r, _ := newParallelRangeReader(context.Background(), client, "https://cdn/object", int64(len(data)), 10, rangeReadChunk+5, nil)
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data[10:10+rangeReadChunk+5]) || calls != 1 {
		t.Fatalf("len=%d calls=%d err=%v", len(got), calls, err)
	}
}

func TestParallelReaderPropagatesPrefetchFailure(t *testing.T) {
	boom := errors.New("upstream broken")
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		start, end := parseTestRange(req)
		if start >= rangeReadChunk {
			return nil, boom
		}
		return rangeResponse(start, end, 3*rangeReadChunk, io.NopCloser(bytes.NewReader(make([]byte, end-start+1)))), nil
	})}
	r, _ := newParallelRangeReader(context.Background(), client, "https://cdn/object?xph=secret", 3*rangeReadChunk, 0, 0, nil)
	defer r.Close()
	n, err := io.Copy(io.Discard, r)
	if n != rangeReadChunk || !errors.Is(err, boom) {
		t.Fatalf("written=%d err=%v", n, err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("credentials leaked in error")
	}
}

func TestParallelReadersShareUpstreamGate(t *testing.T) {
	var active, peak atomic.Int64
	gate := make(chan struct{}, 2)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		start, end := parseTestRange(req)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-time.After(time.Millisecond):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return rangeResponse(start, end, 5*rangeReadChunk, io.NopCloser(bytes.NewReader(make([]byte, end-start+1)))), nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := newParallelRangeReader(context.Background(), client, "https://cdn/object", 5*rangeReadChunk, 0, 0, gate)
			defer r.Close()
			if _, err := io.Copy(io.Discard, r); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 2 || len(gate) != 0 {
		t.Fatalf("peak=%d slots=%d", peak.Load(), len(gate))
	}
}

func BenchmarkRelayWindowRead(b *testing.B) {
	const size = int64(rangeReadChunk * 16)
	data := make([]byte, rangeReadChunk)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		start, end := parseTestRange(req)
		// Fixed upstream round-trip cost; measures pipelining, not actual CDN speed.
		time.Sleep(5 * time.Millisecond)
		return rangeResponse(start, end, size, io.NopCloser(bytes.NewReader(data[:end-start+1]))), nil
	})}
	for _, parallel := range []bool{false, true} {
		b.Run(fmt.Sprintf("parallel=%t", parallel), func(b *testing.B) {
			b.SetBytes(size)
			for i := 0; i < b.N; i++ {
				var r io.ReadCloser
				if parallel {
					r, _ = newParallelRangeReader(context.Background(), client, "https://cdn/object", size, 0, 0, nil)
				} else {
					r = &httpRangeReader{ctx: context.Background(), client: client, url: "https://cdn/object", size: size}
				}
				if _, err := io.Copy(io.Discard, r); err != nil {
					b.Fatal(err)
				}
				_ = r.Close()
			}
		})
	}
}
