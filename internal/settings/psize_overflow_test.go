package settings

import "testing"

// TestParseSizeRejectsOverflow 回归：ParseSize 过去只约束乘数本身，
// n*unit 可以静默回绕成负数。upload.max_file_size 没有配置上下界兜底，
// 负数会被原样存下并当合法上限用，导致之后每一次上传都以
// "单文件上限配置无效"失败，而配置界面显示保存成功。
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
