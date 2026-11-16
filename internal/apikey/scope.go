// Package apikey 定义 API Key 自身的能力范围。
//
// 这些位不是用户组权限位：组权限回答账号能做什么，Key 能力回答某一枚凭据
// 最多能调用哪一类用户侧接口。两者取交集才是 Key 的实际能力，因此 Key 的
// 能力范围只写在本表中，不参与用户组配置。
package apikey

// Scope 是一枚 API Key 的能力位掩码。
type Scope int64

const (
	// Upload 允许上传文件。
	Upload Scope = 1 << iota
	// Download 允许下载文件。
	Download
	// Preview 允许在线预览文件。
	Preview
	// Manage 允许新建、删除、重命名、移动文件与目录，以及管理归档。
	Manage
	// Share 允许创建和管理分享与取件码。
	Share
	// DirectLink 允许创建和管理直链。
	DirectLink
)

const (
	// All 是全部可授予的 Key 能力。
	All Scope = Upload | Download | Preview | Manage | Share | DirectLink
	// Default 是新建 Key 未显式指定能力时的默认范围。
	Default Scope = Upload | Download | Manage | Share | DirectLink
)

// Has 判断能力掩码中是否包含指定能力。
func Has(mask int64, bit Scope) bool {
	return mask&int64(bit) != 0
}

// HasAny 判断能力掩码中是否包含任一所列能力。
func HasAny(mask int64, bits Scope) bool {
	return mask&int64(bits) != 0
}

// Valid 判断掩码是否只包含已知能力且至少有一项。
func Valid(mask int64) bool {
	return mask != 0 && mask&^int64(All) == 0
}
