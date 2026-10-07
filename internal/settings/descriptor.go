package settings

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Key 是配置项标识。命名规则：`<分区>.<名称>`，分区与 Sections 的 ID 一致。
type Key string

// Kind 决定一个配置项如何被解析、校验与展示。
type Kind string

const (
	// KindString 是单行文本。
	KindString Kind = "string"
	// KindSecret 是敏感文本，落库前加密、展示时掩码。
	KindSecret Kind = "secret"
	// KindInt 是整数，可带上下限。
	KindInt Kind = "int"
	// KindBool 是布尔值。
	KindBool Kind = "bool"
	// KindDuration 是时长，如 "15m"、"24h"。
	KindDuration Kind = "duration"
	// KindSize 是字节数，如 "8M"、"100G"。
	KindSize Kind = "size"
	// KindEnum 是枚举，取值限定在 Enum 里。
	KindEnum Kind = "enum"
	// KindCSV 是逗号分隔列表（如可信代理网段）。
	KindCSV Kind = "csv"
)

// Scope 说明改动何时生效。
//
// 当前全部配置项都支持热生效（改完对新请求立即生效），保留类型是为管理端
// 展示与将来的扩展留出词汇。
type Scope string

// ScopeHot 表示改动立即对新请求生效。
const ScopeHot Scope = "hot"

// Section 是配置分组，仅用于管理端展示。
type Section struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Sections 按管理端展示顺序列出全部分组。
var Sections = []Section{
	{ID: "site", Title: "站点"},
	{ID: "share", Title: "分享与取件"},
	{ID: "auth", Title: "账号与注册"},
	{ID: "storage", Title: "存储"},
	{ID: "pan123", Title: "123 云盘"},
	{ID: "crypto", Title: "加密"},
	{ID: "delivery", Title: "下载与预览"},
	{ID: "upload", Title: "上传"},
	{ID: "archive", Title: "归档"},
	{ID: "mail", Title: "邮件"},
	{ID: "ops", Title: "运维"},
}

// PublicSections 返回包含可管理配置的分区。
//
// 内部项（主密钥等）不参与统计：只含内部项的分区（如加密）在管理界面
// 没有可编辑的内容，列出只会得到一个空页签。
func PublicSections() []Section {
	used := make(map[string]bool)
	for _, d := range registry {
		if d.Internal {
			continue
		}
		used[d.Section] = true
	}
	out := make([]Section, 0, len(Sections))
	for _, section := range Sections {
		if used[section.ID] {
			out = append(out, section)
		}
	}
	return out
}

// Descriptor 描述一个配置项。
//
// 用描述符表而不是一堆 GetString("xxx")：默认值、类型、校验、是否敏感、生效
// 范围集中在一处，管理端可以直接由它生成表单，不必在前端再抄一份配置元数据
// ——两份元数据必然漂移。
type Descriptor struct {
	Key     Key
	Section string
	Title   string
	Help    string
	Kind    Kind
	// Default 是内置默认值，永远不为空（即使语义是"关闭"也写 "false"）。
	Default string
	Scope   Scope
	// Enum 仅在 Kind == KindEnum 时有效。
	Enum []string
	// Min / Max 仅对 KindInt / KindSize / KindDuration（秒）生效；0 表示不限。
	Min int64
	Max int64
	// Placeholder 是管理端输入框的提示文本。
	Placeholder string
	// Warn 是管理端必须显示的风险提示（例如"关闭后既有文件仍可读"）。
	Warn string
	// ValidateFn 是类型之外的额外校验（例如密钥集合的格式）。
	// 有了它，特殊格式的校验不必散落在写入路径里形成第二套规则。
	ValidateFn func(string) error
	// Internal 为真的配置项不进管理界面，只能在初始化等受控路径写入；
	// 更新、重置与导入接口都会拒绝它。
	Internal bool
}

func (d Descriptor) Secret() bool { return d.Kind == KindSecret }

// Validate 校验一个取值是否可以写入。
//
// 校验放在写入侧而不是读取侧：写入只有管理员偶尔发生，读取每时每刻都在发生，
// 在读路径上做校验意味着一个坏值会让所有请求失败。
func (d Descriptor) Validate(raw string) error {
	if err := d.validateKind(raw); err != nil {
		return err
	}
	if d.ValidateFn != nil {
		return d.ValidateFn(strings.TrimSpace(raw))
	}
	return nil
}

func (d Descriptor) validateKind(raw string) error {
	value := strings.TrimSpace(raw)
	switch d.Kind {
	case KindString:
		if value == "" && d.Default != "" {
			return fmt.Errorf("不得为空")
		}
		return nil
	case KindSecret:
		// 允许写入空串表示"清除该密钥"。
		return nil
	case KindInt:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("必须是整数")
		}
		return d.checkRange(n)
	case KindBool:
		if _, err := parseBool(value); err != nil {
			return fmt.Errorf("必须是 true 或 false")
		}
		return nil
	case KindDuration:
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("必须是时长，如 15m、24h")
		}
		return d.checkRange(int64(duration.Seconds()))
	case KindSize:
		n, err := ParseSize(value)
		if err != nil {
			return err
		}
		if d.Min > 0 || d.Max > 0 {
			return d.checkRange(n)
		}
		return nil
	case KindEnum:
		for _, allowed := range d.Enum {
			if value == allowed {
				return nil
			}
		}
		return fmt.Errorf("取值必须是 %s 之一", strings.Join(d.Enum, " / "))
	case KindCSV:
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item != "" && strings.ContainsAny(item, " \t\r\n") {
				return fmt.Errorf("条目 %q 含空白字符", item)
			}
		}
		return nil
	}
	return fmt.Errorf("未知的配置类型 %s", d.Kind)
}

func (d Descriptor) checkRange(n int64) error {
	if d.Min != 0 && n < d.Min {
		return fmt.Errorf("不得小于 %d", d.Min)
	}
	if d.Max != 0 && n > d.Max {
		return fmt.Errorf("不得大于 %d", d.Max)
	}
	return nil
}

// Normalize 把取值规范到标准写法（布尔统一小写、数值去掉前导空白等）。
func (d Descriptor) Normalize(raw string) string {
	value := strings.TrimSpace(raw)
	switch d.Kind {
	case KindBool:
		if b, err := parseBool(value); err == nil {
			return strconv.FormatBool(b)
		}
	case KindInt:
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
	case KindSize:
		if n, err := ParseSize(value); err == nil {
			return strconv.FormatInt(n, 10)
		}
	case KindCSV:
		parts := make([]string, 0, 4)
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item != "" {
				parts = append(parts, item)
			}
		}
		return strings.Join(parts, ",")
	}
	return value
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("无法识别为布尔值")
}

// ParseSize 解析带后缀的字节数。K/M/G/T 按 1024 进制（内存与容量的惯用写法），
// 裸数字按字节；不接受十进制后缀（KB/MB），混用两种进制是容量估算出错的常见来源。
func ParseSize(raw string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return 0, fmt.Errorf("不得为空")
	}
	// 必须先剥掉尾部的 B 再判数量级：顺序反了的话 "8MB" 永远匹配不上 "M"，
	// 而调用方（含从旧环境变量迁移过来的值）恰恰常写成 "8MB"。
	s = strings.TrimSuffix(s, "B")
	unit := int64(1)
	switch {
	case strings.HasSuffix(s, "T"):
		unit, s = 1<<40, s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		unit, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		unit, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "K"):
		unit, s = 1<<10, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析为大小，如 8M、1G")
	}
	if n < 0 {
		return 0, fmt.Errorf("不得为负")
	}
	// 乘法必须查溢出。ParseInt 只约束了乘数本身，n*unit 仍可能回绕成负数
	// （8388608T = 2^23 * 2^40 = 2^63）。而 upload.max_file_size 没有配置
	// 上下界兜底，负数会被原样存下并被当成合法上限使用，最终让每一次上传
	// 都以"单文件上限配置无效"失败——配置界面却显示保存成功。
	if n > math.MaxInt64/unit {
		return 0, fmt.Errorf("数值过大，超出可表示范围")
	}
	return n * unit, nil
}

// FormatSize 把字节数格式化为便于阅读的形式。
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	value := float64(n)
	for _, suffix := range []string{"K", "M", "G", "T", "P"} {
		value /= unit
		if value < unit {
			return strconv.FormatFloat(value, 'f', 1, 64) + suffix
		}
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + "E"
}
