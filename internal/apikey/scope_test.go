package apikey

import "testing"

// TestScopeWireValues 锁定能力位的线上值：前端 types.ts 按同一组数字解码，
// 位序漂移会让界面勾选与实际能力错位。
func TestScopeWireValues(t *testing.T) {
	for bit, want := range map[Scope]int64{
		Upload: 1, Download: 2, Preview: 4, Manage: 8, Share: 16, DirectLink: 32,
	} {
		if int64(bit) != want {
			t.Fatalf("能力位 %d 应为 %d", bit, want)
		}
	}
	if All != 63 {
		t.Fatalf("All = %d，应为 63", All)
	}
	if Default != 59 {
		t.Fatalf("Default = %d，应为 59", Default)
	}
}

func TestValidRejectsEmptyAndUnknownBits(t *testing.T) {
	for _, mask := range []int64{0, -1, int64(All) | 1<<10} {
		if Valid(mask) {
			t.Fatalf("非法掩码 %d 不应通过校验", mask)
		}
	}
	if !Valid(int64(All)) || !Valid(int64(Upload)) {
		t.Fatal("已知能力组合应通过校验")
	}
}
