package backend

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
)

type multipartReader struct {
	backend Backend
	ctx     context.Context
	parts   []ObjectPart
	size    int64
	pos     int64
	index   int
	current io.ReadSeekCloser
}

func totalPartSize(parts []ObjectPart) int64 {
	if len(parts) == 0 {
		return 0
	}
	last := parts[len(parts)-1]
	return last.Offset + last.Size
}

func (r *multipartReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	var total int
	for len(p) > 0 && r.pos < r.size {
		if r.current == nil {
			if r.index >= len(r.parts) {
				return total, io.ErrUnexpectedEOF
			}
			part := r.parts[r.index]
			reader, err := r.backend.Open(r.ctx, part.Ref)
			if err != nil {
				return total, err
			}
			r.current = reader
			if _, err := reader.Seek(r.pos-part.Offset, io.SeekStart); err != nil {
				_ = reader.Close()
				r.current = nil
				return total, err
			}
		}
		part := r.parts[r.index]
		left := part.Offset + part.Size - r.pos
		want := len(p)
		if int64(want) > left {
			want = int(left)
		}
		n, err := r.current.Read(p[:want])
		total += n
		p = p[n:]
		r.pos += int64(n)
		if err != nil && err != io.EOF {
			return total, err
		}
		if r.pos == part.Offset+part.Size || err == io.EOF {
			if err == io.EOF && r.pos != part.Offset+part.Size {
				return total, io.ErrUnexpectedEOF
			}
			_ = r.current.Close()
			r.current = nil
			r.index++
		}
		if n == 0 && err == nil {
			return total, io.ErrNoProgress
		}
	}
	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}

func (r *multipartReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		if offset > 0 && r.pos > math.MaxInt64-offset || offset < 0 && r.pos < math.MinInt64-offset {
			return r.pos, fmt.Errorf("backend: 逻辑对象定位超出范围")
		}
		next = r.pos + offset
	case io.SeekEnd:
		if offset > 0 && r.size > math.MaxInt64-offset || offset < 0 && r.size < math.MinInt64-offset {
			return r.pos, fmt.Errorf("backend: 逻辑对象定位超出范围")
		}
		next = r.size + offset
	default:
		return r.pos, fmt.Errorf("backend: 无效的逻辑对象定位方式")
	}
	if next < 0 {
		return r.pos, fmt.Errorf("backend: 逻辑对象定位不能小于零")
	}
	if r.current != nil {
		_ = r.current.Close()
		r.current = nil
	}
	r.pos = next
	r.index = sort.Search(len(r.parts), func(i int) bool {
		return r.parts[i].Offset+r.parts[i].Size > next
	})
	return next, nil
}

func (r *multipartReader) Close() error {
	if r.current == nil {
		return nil
	}
	err := r.current.Close()
	r.current = nil
	return err
}
