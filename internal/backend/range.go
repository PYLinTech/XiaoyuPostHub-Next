package backend

import (
	"context"
	"io"
)

// RangeOpener bounds prefetch to the actual ciphertext interval required by the
// caller. length <= 0 means the remainder of the object.
type RangeOpener interface {
	OpenRange(ctx context.Context, ref string, offset, length int64) (io.ReadCloser, error)
}

// OpenRange uses bounded prefetch when supported and retains the Backend contract
// for externally injected backends.
func OpenRange(ctx context.Context, b Backend, ref string, offset, length int64) (io.ReadCloser, error) {
	if opener, ok := b.(RangeOpener); ok {
		return opener.OpenRange(ctx, ref, offset, length)
	}
	base, err := b.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	if _, err = base.Seek(offset, io.SeekStart); err != nil {
		_ = base.Close()
		return nil, err
	}
	if length <= 0 {
		return base, nil
	}
	return &limitedReadCloser{Reader: io.LimitReader(base, length), Closer: base}, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}
