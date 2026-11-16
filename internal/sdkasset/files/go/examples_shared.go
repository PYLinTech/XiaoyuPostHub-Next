package main

import (
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
)

func printUsage() {
	fmt.Println("用法: go run . <本地文件> [目标目录]")
	fmt.Println("      go run . --version  (公开版本查询，无需 API Key)")
	fmt.Println("      go run . --serve-relay [监听地址]   (方式 1：后端直连，由后端做中转)")
	fmt.Println("      go run . --serve-direct [监听地址]  (方式 2：后端请求得到地址后，前端直接去拿)")
}

func exampleAddr(args []string, fallback string) string {
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		return strings.TrimSpace(args[0])
	}
	return fallback
}

func exampleJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func exampleError(w http.ResponseWriter, status int, message string) {
	exampleJSON(w, status, map[string]string{"error": message})
}

func exampleUpstreamError(w http.ResponseWriter, err error) {
	log.Printf("示例后端请求站点失败: %v", err)
	exampleError(w, http.StatusBadGateway, err.Error())
}

func exampleContentDisposition(name string) string {
	value := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if value == "" {
		return "attachment"
	}
	return value
}

// exampleWebFile 返回压缩包根目录 web/ 下的演示页面路径。
func exampleWebFile(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "web", name)
}
