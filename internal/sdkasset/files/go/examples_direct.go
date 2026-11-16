package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
)

type browserPlanRequest struct {
	Path            string `json:"path"`
	ClientPublicKey string `json:"clientPublicKey"`
}

type browserSettleRequest struct {
	TicketID  string `json:"ticketId"`
	Completed bool   `json:"completed"`
}

// runDirectDeliveryServer 演示「后端请求得到地址后，前端直接去拿」：
// 后端只申请交付计划，浏览器拿计划里的 123 地址取密文并在本地解密；
// API Key 不出后端，站点与后端都不经过文件字节。
func runDirectDeliveryServer(addr string) {
	client, err := NewClient("", "")
	if err != nil {
		log.Fatal(err)
	}
	page := exampleWebFile("direct-delivery.html")
	module := exampleWebFile("browser-delivery.mjs")
	moduleBytes, err := os.ReadFile(module)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, page)
	})
	mux.HandleFunc("GET /browser-delivery.mjs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(moduleBytes)
	})
	mux.HandleFunc("POST /api/delivery/plan", func(w http.ResponseWriter, r *http.Request) {
		var req browserPlanRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			exampleError(w, http.StatusBadRequest, "请求体不是有效 JSON")
			return
		}
		req.Path = strings.TrimSpace(req.Path)
		req.ClientPublicKey = strings.TrimSpace(req.ClientPublicKey)
		if req.Path == "" {
			exampleError(w, http.StatusBadRequest, "path 不能为空")
			return
		}
		if req.ClientPublicKey == "" {
			exampleError(w, http.StatusBadRequest, "clientPublicKey 不能为空")
			return
		}
		plan, err := client.PrepareDownload(r.Context(), req.Path, req.ClientPublicKey, false)
		if err != nil {
			exampleUpstreamError(w, err)
			return
		}
		if plan.Mode != "direct" || plan.ContentForm != "ciphertext" {
			// 密文票据由客户端回执；proxy_decrypt 由服务端在开流时自行结算，回执会被拒绝。
			if plan.ContentForm == "ciphertext" {
				_ = client.Settle(r.Context(), plan.TicketID, false)
			}
			exampleError(w, http.StatusConflict,
				"本次交付被降级为站点中转，浏览器无法直接向 123 取密文；请确认站点开启「优先直链交付」且 123 直链空间可用")
			return
		}
		exampleJSON(w, http.StatusOK, plan)
	})
	mux.HandleFunc("POST /api/delivery/settle", func(w http.ResponseWriter, r *http.Request) {
		var req browserSettleRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			exampleError(w, http.StatusBadRequest, "请求体不是有效 JSON")
			return
		}
		if strings.TrimSpace(req.TicketID) == "" {
			exampleError(w, http.StatusBadRequest, "ticketId 不能为空")
			return
		}
		if err := client.Settle(r.Context(), req.TicketID, req.Completed); err != nil {
			exampleUpstreamError(w, err)
			return
		}
		exampleJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	log.Printf("方式 2 示例（后端请求得到地址后，前端直接去拿）已启动：http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
