//go:build !webui

package webui

import (
	"embed"
	"io/fs"
)

// FS 返回空文件系统；普通 Go 构建通过 XPH_STATIC_DIR 提供前端。
func FS() fs.FS { return embed.FS{} }

// Available 报告当前构建未内嵌前端。
func Available() bool { return false }
