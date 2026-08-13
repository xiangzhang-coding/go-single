// Q2 HTTP 四大件指标：中间件自动打点。
// 运行：go run ./interview/ch11_observability/q02_http_four
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"
)

type metric struct {
	label string
	v     float64
}

var registry = map[string]*metric{}

func count(key string) {
	if registry[key] == nil {
		registry[key] = &metric{label: key}
	}
	registry[key].v++
}

// 对应 metricRegistry.GinMiddleware：method + route + status 为标签。
func observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_ = start
		rec := &recorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		route := r.URL.Path
		count(fmt.Sprintf("http_requests_total{method=%s,route=%s,status=%d}", r.Method, route, rec.status))
		count(fmt.Sprintf("http_request_duration_seconds{method=%s,route=%s}", r.Method, route))
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

func main() {
	h := observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/orders" {
			w.WriteHeader(202)
		} else {
			w.WriteHeader(404)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/orders", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/orders", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))

	for _, m := range registry {
		fmt.Printf("%-70s → %v\n", m.label, m.v)
	}
}

// 项目位置：internal/platform/metrics/metrics.go 的 GinMiddleware（75-101，
// 排除 /metrics 自采）；Grafana http.json 面板直接消费这四类指标（QPS/延迟/错误/活跃）。
