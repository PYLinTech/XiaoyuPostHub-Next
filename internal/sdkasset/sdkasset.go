// Package sdkasset 打包随站点分发的示例 SDK。
//
// 资产以源码形式内嵌，下载时在内存里现场压缩：仓库不存放预构建产物，
// 修改示例后重启即生效，也不会出现压缩包与源码不一致。
package sdkasset

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed files
var files embed.FS

// Version 是示例 SDK 的版本号。v2 起每枚 API Key 绑定一个文件夹，SDK 内所有
// 路径都相对于该文件夹；v4 起示例拆成「后端直连，由后端做中转」与「后端请求得到
// 地址后，前端直接去拿」两类：前者由后端取回并在密文计划上本地解密，后者只在后端
// 申请交付计划、由前端直接向 123 取密文并本地解密；两类都会核对长度与 SHA-256 后才
// 替换目标文件，接口参数与工作区语义沿用 v2。
const Version = "v4"

// isJunkAsset 排除编辑器与解释器留下的文件，避免它们随嵌入资源进入压缩包。
func isJunkAsset(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db":
		return true
	}
	return strings.HasSuffix(name, ".pyc") || strings.HasSuffix(name, ".pyo")
}

// ZipFileName 返回下载时的压缩包文件名。
func ZipFileName() string {
	return "XiaoyuPostHub-Next_SDK_" + Version + ".zip"
}

// Zip 返回完整 SDK 压缩包的字节。
func Zip() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := fs.WalkDir(files, "files", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "__pycache__" {
				return fs.SkipDir
			}
			return nil
		}
		if isJunkAsset(d.Name()) {
			return nil
		}
		rel := strings.TrimPrefix(path, "files/")
		// files/go 里若直接放 go.mod 会变成嵌套模块，//go:embed 会整段跳过，
		// 下载包里就没有 Go 示例；源文件用 .tmpl 保存，打包时再还原文件名。
		if rel == "go/go.mod.tmpl" {
			rel = "go/go.mod"
		}
		w, err := zw.Create(rel)
		if err != nil {
			return err
		}
		data, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sdkasset: 打包失败: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("sdkasset: 关闭压缩包失败: %w", err)
	}
	return buf.Bytes(), nil
}
