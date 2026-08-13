// Q1 HTTP 中间件链：洋葱模型。
// 运行：go run ./interview/ch03_network/q01_middleware
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

// 中间件：包一层 http.Handler，处理前后各挂一段逻辑。
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("→ 请求日志:", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
		fmt.Println("← 响应已写出")
	})
}

func metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("→ 指标计数 +1")
		next.ServeHTTP(w, r)
	})
}

func main() {
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	// 链式装配：外部先执行，内层后执行（与 Gin 的 Use 顺序一致）。
	h = logging(metrics(h))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/products", nil))
	fmt.Println("状态码:", rec.Code)
}

// 项目位置：cmd/server/main.go 装配链——metricRegistry.GinMiddleware() → gin.Recovery()
// → requestLogger → platformcors.Middleware；/api 组再挂 requestTimeout；
// 鉴权 auth.Middleware / auth.RequireAdmin() 按路由挂载。
