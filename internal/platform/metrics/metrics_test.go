// 指标单元测试：黑盒验证 HTTP 三件套（QPS/延迟/错误）+ 活跃请求 + Go runtime，
// 直接抓取 /metrics 端点解析断言（不依赖 MySQL/Redis）。
package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
)

// findSample 在抓取结果中按 指标名 + 标签子集 定位样本值。
// 直方图返回 SampleCount（观测次数），counter/gauge 返回当前值。
func findSample(t *testing.T, families map[string]*dto.MetricFamily, name string, wantLabels map[string]string) (float64, bool) {
	t.Helper()
	fam, ok := families[name]
	if !ok {
		return 0, false
	}
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		matched := true
		for k, v := range wantLabels {
			if labels[k] != v {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		switch {
		case m.GetCounter() != nil:
			return m.GetCounter().GetValue(), true
		case m.GetGauge() != nil:
			return m.GetGauge().GetValue(), true
		case m.GetHistogram() != nil:
			return float64(m.GetHistogram().GetSampleCount()), true
		default:
			return 0, true
		}
	}
	return 0, false
}

func scrape(t *testing.T, h http.Handler) map[string]*dto.MetricFamily {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "go_goroutines", "应包含 Go runtime 指标")

	var parser = expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(rec.Body)
	require.NoError(t, err)
	return families
}

func doRequest(t *testing.T, h http.Handler, method, target string) int {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestGinMiddlewareRecordsHTTPMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := metrics.New()

	r := gin.New()
	r.Use(reg.GinMiddleware(), gin.Recovery())
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/bad", func(c *gin.Context) { c.Status(http.StatusBadRequest) })
	r.GET("/panic", func(c *gin.Context) { panic("boom") })

	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ok"))
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ok"))
	require.Equal(t, http.StatusBadRequest, doRequest(t, r, http.MethodGet, "/bad"))
	require.Equal(t, http.StatusInternalServerError, doRequest(t, r, http.MethodGet, "/panic"))
	require.Equal(t, http.StatusNotFound, doRequest(t, r, http.MethodGet, "/no/such/route"))

	families := scrape(t, reg.Handler())

	// QPS（counter）：2 次 200 + 1 次 400 + 1 次 500（panic 被 Recovery 恢复）+ 1 次 404。
	v, ok := findSample(t, families, "http_requests_total", map[string]string{"method": "GET", "route": "/ok", "status": "200"})
	require.True(t, ok, "http_requests_total{/ok,200} 缺失")
	require.Equal(t, float64(2), v)

	// 4xx/5xx 错误计数。
	v, ok = findSample(t, families, "http_errors_total", map[string]string{"route": "/bad", "class": "4xx"})
	require.True(t, ok, "http_errors_total{/bad,4xx} 缺失")
	require.Equal(t, float64(1), v)
	v, ok = findSample(t, families, "http_errors_total", map[string]string{"route": "/panic", "class": "5xx"})
	require.True(t, ok, "http_errors_total{/panic,5xx} 缺失（Recovery 应恢复为 500）")
	require.Equal(t, float64(1), v)

	// 未匹配路由归入 unmatched，避免路径参数打爆基数。
	v, ok = findSample(t, families, "http_requests_total", map[string]string{"route": "unmatched", "status": "404"})
	require.True(t, ok, "404 应计入 unmatched 路由")
	require.Equal(t, float64(1), v)

	// 延迟直方图：/ok 被观测 2 次。
	v, ok = findSample(t, families, "http_request_duration_seconds", map[string]string{"method": "GET", "route": "/ok"})
	require.True(t, ok, "直方图 {/ok} 缺失")
	require.Equal(t, float64(2), v)

	// 活跃请求：全部完成后归零（panic 场景也应递减）。
	v, ok = findSample(t, families, "http_requests_active", nil)
	require.True(t, ok, "http_requests_active 缺失")
	require.Equal(t, float64(0), v)
}

func TestGinMiddlewareSkipsMetricsEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := metrics.New()

	r := gin.New()
	r.Use(reg.GinMiddleware())
	r.GET("/metrics", gin.WrapH(reg.Handler()))
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })

	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/metrics"))
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/metrics"))
	require.Equal(t, http.StatusOK, doRequest(t, r, http.MethodGet, "/ok"))

	families := scrape(t, reg.Handler())
	_, ok := findSample(t, families, "http_requests_total", map[string]string{"route": "/metrics"})
	require.False(t, ok, "抓取流量不应计入指标")
	_, ok = findSample(t, families, "http_requests_total", map[string]string{"route": "/ok"})
	require.True(t, ok, "业务请求仍应计入指标")
}

func TestRegistryRejectsDuplicateCollector(t *testing.T) {
	reg := metrics.New()
	dup := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "与内置指标同名同标签，注册应冲突",
	}, []string{"method", "route", "status"})
	require.Error(t, reg.Register(dup))
}

func TestRegistryHandlerServesRuntimeAndProcessMetrics(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	metrics.New().Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")

	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "go_goroutines")
	require.Contains(t, string(body), "go_memstats_alloc_bytes")
	require.Contains(t, string(body), "process_cpu_seconds_total")
}
