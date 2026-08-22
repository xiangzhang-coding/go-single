// Package metrics 提供指标注册器（client_golang）与 HTTP 中间件指标。
//
// /metrics 端点自动含 Go runtime（go_*）与进程（process_*）指标；
// 各业务模块（秒杀/订单/MQ/优惠券等）可经 Register 扩展自定义指标点。
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// metricRoute 中间件不计自身抓取流量，避免轮询打点污染 QPS。
	metricRoute = "/metrics"
	// websocketRoute 由 WS 连接状态单独观测，不计入普通 HTTP 请求生命周期。
	websocketRoute = "/ws"
)

// Registry 指标注册器：独立 registry（不污染全局默认注册器），内置 Go/进程采集器。
type Registry struct {
	reg *prometheus.Registry

	requestsTotal  *prometheus.CounterVec // HTTP 请求总数（QPS 指标）
	requestSeconds *prometheus.HistogramVec
	errorsTotal    *prometheus.CounterVec // 4xx/5xx 错误计数
	requestsActive prometheus.Gauge
}

// New 构建指标注册器并注册 HTTP 三件套 + 活跃请求 + Go runtime / 进程采集器。
func New() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	r := &Registry{
		reg: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP 请求总数（QPS 指标），按 method/route/status 分桶",
		}, []string{"method", "route", "status"}),
		requestSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP 请求延迟直方图（50/90/99 分位经 histogram_quantile 计算）",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"method", "route"}),
		errorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_errors_total",
			Help: "HTTP 4xx/5xx 错误计数，按 class（4xx/5xx）分桶",
		}, []string{"method", "route", "class"}),
		requestsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_active",
			Help: "当前处理中的 HTTP 请求数（活跃请求）",
		}),
	}
	reg.MustRegister(r.requestsTotal, r.requestSeconds, r.errorsTotal, r.requestsActive)
	return r
}

// Handler 返回 /metrics 抓取端点（自动含 go_*/process_* 指标）。
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// Register 供业务模块注册自定义指标（秒杀/订单/MQ/优惠券等指标点）。
func (r *Registry) Register(c prometheus.Collector) error {
	return r.reg.Register(c)
}

// GinMiddleware 记录每请求指标。应注册在 Recovery 之前（最外层），
// 使 panic 被恢复为 500 后仍能完成计数与活跃请求递减。
func (r *Registry) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if route := c.FullPath(); route == metricRoute || route == websocketRoute {
			c.Next()
			return
		}

		start := time.Now()
		r.requestsActive.Inc()
		defer r.requestsActive.Dec()

		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		method := c.Request.Method
		status := c.Writer.Status()

		r.requestsTotal.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
		r.requestSeconds.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		if class := statusClass(status); class != "" {
			r.errorsTotal.WithLabelValues(method, route, class).Inc()
		}
	}
}

// statusClass 返回 4xx/5xx 分类，非错误返回空串。
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	default:
		return ""
	}
}
