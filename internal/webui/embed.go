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
// dist/ 目录里始终保留一个 .gitkeep：go:embed 要求被内嵌的目录在编译时存在，
// 而"目录不存在"是编译期错误——那意味着没跑过 build.sh 的人连 go build 都
// 执行不了。真正的产物不进版本库（见 .gitignore）。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS 返回内嵌的前端产物，根目录就是 dist/ 的内容。
//
// 始终返回可用的 fs.FS（哪怕里面只有 .gitkeep）：调用方要靠 Available 判断
// 有没有真正的页面，而不是靠这里的返回值是否为 nil。
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
// 判据是 index.html 在不在，而不是"dist 里有没有文件"：占位用的 .gitkeep
// 也会被 all: 内嵌进来，只看"非空"会把占位状态误判成已构建。
func Available() bool {
	f, err := FS().Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
