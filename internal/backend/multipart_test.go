package backend

import (
	"bytes"
	"context"
	"io"
	"math"
	"strconv"
	"testing"
)

type multipartTestBackend struct{ objects map[string][]byte }

func (b *multipartTestBackend) Kind() string { return "test" }
func (b *multipartTestBackend) Put(context.Context, PutRequest) (PutResult, error) {
	return PutResult{}, ErrNotSupported
}
func (b *multipartTestBackend) Open(_ context.Context, ref string) (io.ReadSeekCloser, error) {
	data, ok := b.objects[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return &testReadSeekCloser{Reader: bytes.NewReader(data)}, nil
}
func (b *multipartTestBackend) Delete(context.Context, string) error { return nil }
func (b *multipartTestBackend) Stat(_ context.Context, ref string) (int64, error) {
	return int64(len(b.objects[ref])), nil
}
func (b *multipartTestBackend) Presign(context.Context, string, PresignOptions) (string, error) {
	return "", ErrNotSupported
}
func (b *multipartTestBackend) PresignReady() bool    { return false }
func (b *multipartTestBackend) SliceMD5Enabled() bool { return false }

type testReadSeekCloser struct{ *bytes.Reader }

func (testReadSeekCloser) Close() error { return nil }

func TestMultipartReaderReadsAndSeeksAcrossParts(t *testing.T) {
	backend := &multipartTestBackend{objects: map[string][]byte{"p1": []byte("hello "), "p2": []byte("world")}}
	reader := &multipartReader{
		backend: backend,
		ctx:     context.Background(),
		parts: []ObjectPart{
			{Ref: "p1", Offset: 0, Size: 6},
			{Ref: "p2", Offset: 6, Size: 5},
		},
		size: 11,
	}
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("跨卷读取结果错误：%q，err=%v", got, err)
	}
	if _, err := reader.Seek(-5, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(reader)
	if err != nil || string(got) != "world" {
		t.Fatalf("跨卷定位读取结果错误：%q，err=%v", got, err)
	}
	if _, err := reader.Seek(math.MaxInt64, io.SeekCurrent); err == nil {
		t.Fatal("溢出定位必须被拒绝")
	}
}

func TestComposeAndSplitObjectRef(t *testing.T) {
	ref, err := ComposeObjectRef([]ObjectPart{
		{Ref: "101", Offset: 0, Size: 12},
		{Ref: "102", Offset: 12, Size: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	parts, err := SplitObjectRef(ref, 19)
	if err != nil || len(parts) != 2 || parts[1].Ref != "102" || parts[1].Offset != 12 {
		t.Fatalf("复合定位符解析错误：%+v，err=%v", parts, err)
	}
	if _, err := SplitObjectRef(ref, 18); err == nil {
		t.Fatal("逻辑长度不一致时必须拒绝")
	}
	plainRef, err := ComposeObjectRef([]ObjectPart{{Ref: "103", Offset: 0, Size: 4}})
	if err != nil || plainRef != "103" {
		t.Fatalf("单卷定位符应保持旧格式：%q，err=%v", plainRef, err)
	}
}

func TestCipherPartRangesRespectVolumeLimit(t *testing.T) {
	const plainSize, blockSize, maxPartSize = int64(300), int64(100), int64(216)
	wireSize := int64(64) + plainSize + 3*16
	ranges, err := cipherPartRanges(plainSize, wireSize, blockSize, maxPartSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 3 {
		t.Fatalf("应有三卷，得到 %d", len(ranges))
	}
	var plain, wire int64
	for i, part := range ranges {
		if part.offset != wire || part.plainSize <= 0 || part.wireSize > maxPartSize {
			t.Fatalf("第 %s 卷边界无效：%+v", strconv.Itoa(i+1), part)
		}
		plain += part.plainSize
		wire += part.wireSize
	}
	if plain != plainSize || wire != wireSize {
		t.Fatalf("分卷覆盖不完整：plain=%d wire=%d", plain, wire)
	}
}
