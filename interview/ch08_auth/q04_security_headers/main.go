// Q4 安全响应头：防 XSS/点击劫持/嗅探。
// 运行：go run ./interview/ch08_auth/q04_security_headers
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff") // 禁止 MIME 嗅探
		w.Header().Set("X-Frame-Options", "DENY")           // 防点击劫持（frame 内嵌）
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func main() {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	for k, v := range rec.Header() {
		fmt.Printf("%-28s: %s\n", k, v[0])
	}
	fmt.Println("CSP 注意：内联脚本/外域资源需要放行（项目为 minio 图片与 /ws 加了例外）")
}

// 项目位置：deploy/nginx/nginx.conf——安全头配置在 server 与 /assets 两个块
//（add_header 不继承，需重复声明）；CSP 放行 self + minio + ws；T26 修订补充。
