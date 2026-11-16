package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

type directDeliveryRequest struct {
	Path      string `json:"path"`
	ExpiresAt int64  `json:"expiresAt"`
	MaxHits   int    `json:"maxHits"`
}

// runDirectDeliveryServer 演示「后端接入 + 前端直连交付」：
// 后端只用自己的 API Key 创建直链，文件字节由浏览器直接向站点请求。
func runDirectDeliveryServer(addr string) {
	client, err := NewClient("", "")
	if err != nil {
		log.Fatal(err)
	}
	page := exampleWebFile("direct-delivery.html")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, page)
	})
	mux.HandleFunc("POST /api/delivery/direct", func(w http.ResponseWriter, r *http.Request) {
		var req directDeliveryRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			exampleError(w, http.StatusBadRequest, "请求体不是有效 JSON")
			return
		}
		req.Path = strings.TrimSpace(req.Path)
		if req.Path == "" {
			exampleError(w, http.StatusBadRequest, "path 不能为空")
			return
		}
		expiresAt := req.ExpiresAt
		if expiresAt == 0 {
			expiresAt = time.Now().Add(time.Hour).Unix()
		}
		link, err := client.CreateDirectLink(r.Context(), req.Path, expiresAt, req.MaxHits)
		if err != nil {
			exampleUpstreamError(w, err)
			return
		}
		exampleJSON(w, http.StatusOK, map[string]any{
			"url":       client.DirectLinkURL(link.ID),
			"fileName":  link.SourceName,
			"size":      link.SizePlain,
			"expiresAt": link.ExpiresAt,
		})
	})
	log.Printf("直连交付示例已启动：http://%s（浏览器会直接向站点取文件）", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
