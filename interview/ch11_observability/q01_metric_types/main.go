// Q1 Prometheus 指标类型：Counter / Gauge / Histogram。
// 运行：go run ./interview/ch11_observability/q01_metric_types
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

// 项目位置：internal/platform/metrics/metrics.go（http 四件套：requests_total /
// duration_seconds / errors_total / requests_active）与 business.go（seckill_prededuct_total
// Counter、seckill_stock_remaining Gauge）；PromQL 面板见 deploy/monitoring/grafana/dashboards。
