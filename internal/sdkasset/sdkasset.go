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

// Zip 返回完整 SDK 压缩包的字节。
func Zip() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := fs.WalkDir(files, "files", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, "files/")
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
