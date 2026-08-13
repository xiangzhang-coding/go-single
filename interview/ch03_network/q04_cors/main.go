// Q4 CORS 预检与白名单。
// 运行：go run ./interview/ch03_network/q04_cors
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

// 白名单校验 + 预检应答（对应 internal/platform/cors 的简化版）。
func corsMiddleware(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r) // 非跨源直接放行
			return
		}
		if !allowed[origin] {
			w.WriteHeader(http.StatusForbidden) // 非白名单：预检 403
			fmt.Fprintln(w, "origin not allowed")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		if r.Method == http.MethodOptions { // 预检：只回响应头
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent) // 204
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	allowed := map[string]bool{"http://localhost:5173": true}
	h := corsMiddleware(allowed, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	preflight := httptest.NewRequest(http.MethodOptions, "/api/orders", nil)
	preflight.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, preflight)
	fmt.Printf("白名单预检 → %d, Allow-Origin=%s\n", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))

	evil := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	evil.Header.Set("Origin", "http://evil.example")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, evil)
	fmt.Printf("非白名单跨源 → %d\n", rec2.Code)
}

// 项目位置：internal/platform/cors/cors.go（白名单 + 预检 403/204 + max-age 86400），
// 白名单来自配置 server.cors.allow_origins；nginx 层另有安全头（deploy/nginx/nginx.conf）。
