// Q2 反向代理与静态托管：Nginx 的 /api 反代语义。
// 运行：go run ./interview/ch10_deploy/q02_reverse_proxy
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
)

func main() {
	// 后端（Go 服务）。
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend got: %s", r.URL.Path)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxy := httputil.NewSingleHostReverseProxy(target) // 标准库反向代理

	// 前端入口：静态文件（SPA）与 /api 分流（对应 nginx try_files + location /api）。
	proxyReq := httptest.NewRequest("GET", "/api/products", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, proxyReq)
	fmt.Printf("请求 /api/products → 转发后端 → %q\n", rec.Body.String())

	fmt.Println("nginx 侧：location /api { proxy_pass http://host.docker.internal:8080; }")
	fmt.Println("         location / { try_files $uri /index.html; }（SPA 回退）")
	fmt.Println("         location /ws { proxy_http_version 1.1; Upgrade 头; }")
}

// 项目位置：deploy/nginx/nginx.conf——静态托管 web/faire/dist + /api 反代 +
// /ws upgrade 代理（read_timeout 90s）+ try_files SPA 回退；compose nginx 挂载
// ../web/faire/dist，见 deploy/docker-compose.yml。
