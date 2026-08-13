---
sidebar_position: 12
---

# 11 可观测性

## Q1. Prometheus 指标类型：Counter / Gauge / Histogram

**答案要点**

- **Counter**：只增不减（请求数、错误数），查询用 `rate()` 看速率。
- **Gauge**：可增可减（当前库存余量、活跃连接数），看瞬时值。
- **Histogram**：分桶累计（延迟分布），用 `histogram_quantile` 求分位值。
- 命名规范：`<domain>_<name>_<unit>_<type>`（如 `http_requests_total`、`seckill_prededuct_total`）。

**可运行代码**

```go title="interview/ch11_observability/q01_metric_types/main.go"
package main

import (
	"fmt"
	"sort"
)

// 三种类型的内存实现（语义与 prometheus 客户端一致）。
type counter struct{ v float64 } // 只增不减：请求总数、错误总数
func (c *counter) inc()          { c.v++ }

type gauge struct{ v float64 } // 可增可减：当前库存余量、活跃连接
func (g *gauge) set(v float64) { g.v = v }

type histogram struct { // 分桶累计：延迟分布
	buckets []float64
	counts  []int64
}

func (h *histogram) observe(v float64) {
	h.counts[sort.SearchFloat64s(h.buckets, v)]++
}

func (h *histogram) p95() float64 {
	total := int64(0)
	for _, c := range h.counts {
		total += c
	}
	need := int64(float64(total) * 0.95)
	acc := int64(0)
	for i, c := range h.counts {
		acc += c
		if acc >= need {
			return h.buckets[i]
		}
	}
	return h.buckets[len(h.buckets)-1]
}

func main() {
	var total counter
	total.inc()
	fmt.Println("Counter 请求总数:", total.v)

	var stock gauge
	stock.set(49) // 预扣后刷新
	fmt.Println("Gauge 活动库存余量:", stock.v)

	h := &histogram{buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1}}
	for _, v := range []float64{0.002, 0.002, 0.003, 0.008, 0.02} {
		h.observe(v)
	}
	fmt.Println("Histogram P95 延迟:", h.p95())
}

```

**项目位置**：`internal/platform/metrics/metrics.go`（http 四件套）+ `business.go`（业务指标）；Grafana 面板 `deploy/monitoring/grafana/dashboards`。

## Q2. HTTP 四大件指标：中间件自动打点

**答案要点**

- 四件套：`http_requests_total{method,route,status}`、`http_request_duration_seconds`、`http_errors_total`、`http_requests_active`。
- 打点放**中间件**：业务零侵入，全路由覆盖。
- 标签（method/route/status）是查询维度；route 用路由模板而非真实路径（防基数爆炸）。
- 自采 `/metrics` 要排除，避免递归打点。

**可运行代码**

```go title="interview/ch11_observability/q02_http_four/main.go"
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

```

**项目位置**：`internal/platform/metrics/metrics.go` 的 `GinMiddleware`（75-101，排除 `/metrics`）；Grafana `http.json` 面板消费四类指标。

## Q3. 业务指标：带标签的计数器与 Gauge 刷新时机

**答案要点**

- 业务指标回答"业务健康吗"：预扣成功率、库存余量、订单数、MQ 消费失败率。
- 标签给维度：`{result=success|fail}`、`{order_type}`、`{reason=permanent|transient}`。
- **Gauge 刷新时机**要选对：预扣/回补/页面浏览/上下架后刷新，保证近似实时。
- 计数位置要紧贴业务事件（预扣成功即 +1），避免遗漏分支。

**可运行代码**

```go title="interview/ch11_observability/q03_business_metrics/main.go"
package main

import "fmt"

// 业务指标设计要点：标签区分维度（result=success|fail），计数打点位置要紧贴业务事件。
type businessMetrics struct {
	preDeduct map[string]int64 // seckill_prededuct_total{result}
	stock     map[int64]int64  // seckill_stock_remaining{activity_id} gauge
}

func (b *businessMetrics) incPreDeduct(result string) {
	b.preDeduct[result]++
}

func (b *businessMetrics) setStock(activityID, left int64) {
	b.stock[activityID] = left
}

func main() {
	m := &businessMetrics{preDeduct: map[string]int64{}, stock: map[int64]int64{}}
	for _, r := range []string{"success", "success", "sold_out", "success"} {
		m.incPreDeduct(r)
	}
	m.setStock(1001, 46) // 预扣/回补/页面浏览/上下架后刷新
	fmt.Println("seckill_prededuct_total:", m.preDeduct)
	fmt.Println("seckill_stock_remaining:", m.stock)

	fmt.Println("其他业务点：orders_created_total{order_type}、orders_payment_total{result}、")
	fmt.Println("mq_consume_failed_total{reason=permanent|transient}、coupon_issued_total")
}

```

**项目位置**：`internal/platform/metrics/business.go`（44-94）；打点调用点：flashsale `preDeductSuccess/Failed`、order `OrderCreated/OrderStatusChanged`、payment `PaymentResult`、coupon `CouponIssued/Redeemed`。

## Q4. 结构化日志 + 日志聚合（Loki）

**答案要点**

- 一条日志 = 结构化 JSON 行（zap）：时间、级别、消息、业务字段。
- 采集链：应用输出（stdout/文件）→ promtail（容器 + 文件作业）→ Loki → Grafana。
- Loki 以标签索引 + 内容全文检索（`{job="go-single"} |~ "关键词"`），比 ES 轻量。
- 日志与指标分工：日志看单笔细节，指标看整体分布。

**可运行代码**

```go title="interview/ch11_observability/q04_log_aggregation/main.go"
package main

import (
	"encoding/json"
	"fmt"
)

type logLine struct {
	Ts         string `json:"ts"`
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	ActivityID int64  `json:"activity_id,omitempty"`
	OrderNo    string `json:"order_no,omitempty"`
	Elapsed    string `json:"elapsed"`
}

func main() {
	// 一条 zap JSON 日志（logger.go 输出格式，ISO8601 ts）。
	line := logLine{Ts: "2026-08-13T21:30:00.123+08:00", Level: "info",
		Msg: "秒杀预扣成功", ActivityID: 1001, OrderNo: "O20260813001", Elapsed: "1.2ms"}
	b, _ := json.Marshal(line)
	fmt.Println("日志行:", string(b))

	fmt.Println()
	fmt.Println("采集链：zap 输出 stdout / 镜像 log.file → promtail（docker 容器 + 文件作业）")
	fmt.Println("        → Loki（labels: job=go-single）→ Grafana 日志面板按 label + 关键词检索")
	fmt.Println("检索示例：{job=\"go-single\"} |~ \"秒杀预扣成功\"")
}

```

**项目位置**：`internal/platform/logger/logger.go`；`deploy/monitoring/promtail/config.yml`、`loki/config.yml`、`grafana/dashboards/logs.json`。

## Q5. PromQL 概念：rate 与 histogram_quantile 的数学

**答案要点**

- `rate(counter[1m])`：窗口内增量 ÷ 秒数 = 每秒增速（Counter 重启归零用差值免疫）。
- `histogram_quantile(0.95, bucket)`：从分桶累计分布线性插值求分位值。
- 平均值会掩盖长尾：P95/P99 才是用户体验。
- 面板公式示例：QPS = `rate(http_requests_total[5m])`；P95 = `histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))`。

**可运行代码**

```go title="interview/ch11_observability/q05_promql_math/main.go"
package main

import (
	"fmt"
	"sort"
)

// rate()：Counter 在时间窗口内的每秒增速（防重启归零，用差值/时长）。
func rate(deltaCounter, windowSeconds float64) float64 {
	return deltaCounter / windowSeconds
}

// histogram_quantile：从分桶累计分布近似分位值（线性插值）。
func quantile(need, total float64, buckets []float64, counts []int64) float64 {
	acc := int64(0)
	for i := range buckets {
		acc += counts[i]
		if float64(acc) >= total*need {
			prev := 0.0
			if i > 0 {
				prev = buckets[i-1]
			}
			fraction := (float64(counts[i]) - (float64(acc) - total*need)) / float64(counts[i])
			return prev + (buckets[i]-prev)*fraction
		}
	}
	return buckets[len(buckets)-1]
}

func main() {
	fmt.Printf("秒杀预扣 QPS ≈ rate(seckill_prededuct_total[1m]) = %.1f\n", rate(3000, 60))
	buckets := []float64{0.001, 0.005, 0.01, 0.05, 0.1}
	counts := []int64{50, 400, 300, 200, 50}
	total := int64(0)
	for _, c := range counts {
		total += c
	}
	fmt.Printf("延迟 P95 ≈ histogram_quantile(0.95, http_request_duration_seconds_bucket) = %v\n",
		quantile(0.95, float64(total), buckets, counts))
	_ = sort.SearchFloat64s
}

```

**项目位置**：Grafana 面板查询（`deploy/monitoring/grafana/dashboards/http.json`）；桶定义 `internal/platform/metrics/metrics.go`（.001~5 十档）。

## Q6. 健康检查端点：聚合状态

**答案要点**

- `/healthz` 返回 `{"status": "ok|unavailable", "checks": {依赖: up|down}}`。
- 依赖探测并发 + 超时；聚合：全 up 才 200，否则 503。
- 探活只验证"能干活"，不做业务操作（不加鉴权、无副作用）。
- 同源复用：compose healthcheck、负载均衡摘流量、告警都吃这个端点。

**可运行代码**

```go title="interview/ch11_observability/q06_healthz/main.go"
package main

import (
	"context"
	"fmt"
	"time"
)

// 探活函数族：每个依赖带超时，聚合出整体健康。
func probeMySQL(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return nil
	}
}

func main() {
	// 整体健康 = 全依赖健康；任一超时 → 503 + checks 明细（healthHandler 输出）。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	healthy := true
	for name, probe := range map[string]func(context.Context) error{
		"mysql": probeMySQL, "redis": probeMySQL, "mq": probeMySQL,
	} {
		if err := probe(ctx); err != nil {
			fmt.Printf("checks.%s = down（%v）\n", name, err)
			healthy = false
		} else {
			fmt.Printf("checks.%s = up\n", name)
		}
	}
	fmt.Println("整体 status:", map[bool]string{true: "ok(200)", false: "unavailable(503)"}[healthy])
}

```

**项目位置**：`internal/platform/health/health.go`（并发探测 + buffered channel + 2s 超时）、`cmd/server/main.go` 的 GET `/healthz`（395-404）。

## Q7. 装饰器模式打点：WrapMQ

**答案要点**

- 打点不侵入业务：装饰器包裹基础设施客户端，进出各记一笔。
- 标签含队列与结果：`mq_published_total{queue,result}`、`mq_consume_failed_total{queue,reason}`。
- 装饰器可叠加：`raw → metrics.WrapMQ → mq.WrapCircuitBreaker`，职责分离。
- 打点失败不能影响主流程（指标库内部容错）。

**可运行代码**

```go title="interview/ch11_observability/q07_decorator_metrics/main.go"
package main

import (
	"fmt"
)

type mqClient interface {
	Publish(queue string, body []byte) error
	Consume(queue string) error
}

// 真实实现。
type rabbitMQ struct{}

func (rabbitMQ) Publish(queue string, body []byte) error {
	return fmt.Errorf("simulated publish to %s", queue)
}

func (rabbitMQ) Consume(queue string) error { return nil }

// 装饰器：不改原实现，外层加指标打点。
type metricsMQ struct {
	inner mqClient
	count map[string]int64
}

func (m *metricsMQ) Publish(queue string, body []byte) error {
	err := m.inner.Publish(queue, body)
	key := fmt.Sprintf("mq_published_total{queue=%s,result=%s}", queue, resultOf(err))
	m.count[key]++
	return err
}

func (m *metricsMQ) Consume(queue string) error {
	return m.inner.Consume(queue)
}

func resultOf(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func main() {
	// 装饰器栈（main.go 221-230）：raw → metrics.WrapMQ → mq.WrapCircuitBreaker。
	m := &metricsMQ{inner: rabbitMQ{}, count: map[string]int64{}}
	_ = m.Publish("flashsale.order.create", []byte(`{"order_no":"O1"}`))
	for k, v := range m.count {
		fmt.Printf("%-65s → %d\n", k, v)
	}
	fmt.Println("打点对业务代码零侵入：业务不感知指标层存在")
}

```

**项目位置**：`internal/platform/metrics/business.go` 的 `WrapMQ` 装饰器（177-207）；装配套接在 `cmd/server/main.go`（221-230）。
