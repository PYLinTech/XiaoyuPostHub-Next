package service

import "testing"

func TestParseByteRange(t *testing.T) {
	const size = int64(100)

	tests := []struct {
		name    string
		header  string
		offset  int64
		limit   int64
		ranged  bool
		wantErr bool
	}{
		{name: "无 Range 头", header: "", offset: 0, limit: 0, ranged: false},
		{name: "闭区间", header: "bytes=10-19", offset: 10, limit: 10, ranged: true},
		{name: "开区间读到末尾", header: "bytes=90-", offset: 90, limit: 0, ranged: true},
		{name: "终点超出尾部截断", header: "bytes=90-500", offset: 90, limit: 10, ranged: true},
		{name: "后缀式取尾部", header: "bytes=-30", offset: 70, limit: 30, ranged: true},
		{name: "后缀式超过总长度", header: "bytes=-1000", offset: 0, limit: 100, ranged: true},
		{name: "后缀式正好全量", header: "bytes=-100", offset: 0, limit: 100, ranged: true},
		{name: "起点等于总长度越界", header: "bytes=100-", wantErr: true},
		{name: "起点超出总长度", header: "bytes=101-", wantErr: true},
		{name: "终点小于起点", header: "bytes=50-40", wantErr: true},
		{name: "负起点", header: "bytes=-1-5", wantErr: true},
		{name: "只有破折号", header: "bytes=-", wantErr: true},
		{name: "多区间不支持", header: "bytes=0-10,20-30", wantErr: true},
		{name: "错误单位", header: "items=0-10", wantErr: true},
		{name: "后缀零不合法", header: "bytes=-0", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			offset, limit, ranged, err := parseByteRange(tc.header, size)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望错误，实际 offset=%d limit=%d", offset, limit)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if offset != tc.offset || limit != tc.limit || ranged != tc.ranged {
				t.Fatalf("parseByteRange(%q) = (%d,%d,%v)，期望 (%d,%d,%v)",
					tc.header, offset, limit, ranged, tc.offset, tc.limit, tc.ranged)
			}
		})
	}
}
