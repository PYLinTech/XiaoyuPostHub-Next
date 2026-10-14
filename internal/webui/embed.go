//go:build webui

// 内嵌的前端构建产物。
//
// 产物目录 dist/ 由 ../../build.sh 从 frontend/dist 同步进来，再由 go:embed
// 打进二进制。发布时只需要拷一个文件，不必再记住"前端 dist 也要跟着发"。
//
// 为什么用 embed 而不是运行时读目录：
//
//  1. 少一个会丢的部件。运行时读目录时，"二进制部署上去了、dist 忘了拷"是
//     一类很常见、而且只在打开页面时才暴露的部署事故。
//  2. 产物随版本走。磁盘上的 dist 可能与二进制不是同一版构建的，两者混用会
//     得到"前端请求了旧接口"的诡异现象。
//
// 为什么仍然保留 XPH_STATIC_DIR 这个运行时开关：embed 进来的是构建那一刻的
// 产物，运维临时换一套前端、或用未内嵌的二进制跑 go run 联调时，需要能不重新
// 编译就换掉。优先级是 环境变量指定的目录 > 内嵌产物。
//
// 仅正式构建启用 webui 标签；普通 Go 构建不依赖前端产物。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS 返回内嵌的前端产物，根目录就是 dist/ 的内容。
//
// 调用方通过 Available 判断是否有可用页面。
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// Sub 只在路径不存在时出错，而 dist 由编译期 embed 保证存在。
		// 真到了这里说明内嵌声明被改坏了，直接让 panic 暴露在启动阶段，
		// 好过返回一个空 FS 让页面在运行时莫名其妙 404。
		panic("webui: 内嵌目录 dist 不可用: " + err.Error())
	}
	return sub
}

// Available 报告内嵌产物里是否真的有页面。
//
// 以入口文件 index.html 是否存在为准。
func Available() bool {
	f, err := FS().Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
