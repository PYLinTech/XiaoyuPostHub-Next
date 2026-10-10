package settings

import "testing"

// TestParseSizeRejectsOverflow 回归：ParseSize 过去只约束乘数本身，
// n*unit 可以静默回绕成负数，造成配置界面保存成功但运行时限制失真。
func TestParseSizeRejectsOverflow(t *testing.T) {
	for _, s := range []string{"8388608T", "9007199254740992K", "8796093022208G"} {
		if n, err := ParseSize(s); err == nil {
			t.Errorf("ParseSize(%q) = %d，应报溢出错误", s, n)
		}
	}
	// 范围内的正常值不受影响。
	for _, s := range []string{"100G", "25M", "8M", "1G", "0B", "512K"} {
		if _, err := ParseSize(s); err != nil {
			t.Errorf("ParseSize(%q) 意外失败: %v", s, err)
		}
	}
}
