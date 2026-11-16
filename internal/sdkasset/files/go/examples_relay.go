package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type relayDeliveryRequest struct {
	Path string `json:"path"`
}

// runRelayDeliveryServer 演示「后端中转交付」：
// 后端用 SDK 下载文件，密文计划在本地解密，再把明文字节转发给浏览器；API Key 不出后端。
func runRelayDeliveryServer(addr string) {
	client, err := NewClient("", "")
	if err != nil {
		log.Fatal(err)
	}
	page := exampleWebFile("relay-delivery.html")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, page)
	})
	mux.HandleFunc("POST /api/delivery/relay", func(w http.ResponseWriter, r *http.Request) {
		var req relayDeliveryRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			exampleError(w, http.StatusBadRequest, "请求体不是有效 JSON")
			return
		}
		req.Path = strings.TrimSpace(req.Path)
		if req.Path == "" {
			exampleError(w, http.StatusBadRequest, "path 不能为空")
			return
		}

		temp, err := os.CreateTemp("", "xph-relay-*")
		if err != nil {
			exampleError(w, http.StatusInternalServerError, "无法创建中转临时文件")
			return
		}
		tempPath := temp.Name()
		_ = temp.Close()
		defer os.Remove(tempPath)

		plan, err := client.Download(r.Context(), req.Path, tempPath, false)
		if err != nil {
			exampleUpstreamError(w, err)
			return
		}
		file, err := os.Open(tempPath)
		if err != nil {
			exampleError(w, http.StatusInternalServerError, "无法读取中转临时文件")
			return
		}
		defer file.Close()

		name := plan.FileName
		if name == "" {
			name = "download.bin"
		}
		mimeType := plan.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Content-Disposition", exampleContentDisposition(name))
		w.Header().Set("Cache-Control", "private, no-store")
		http.ServeContent(w, r, name, time.Now(), file)
	})
	log.Printf("中转交付示例已启动：http://%s（文件字节经本服务转发）", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
