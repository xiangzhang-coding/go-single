// Q5 PromQL 概念：rate 与 histogram_quantile 背后的数学。
// 运行：go run ./interview/ch11_observability/q05_promql_math
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

// 项目位置：Grafana 面板查询用 rate(http_requests_total[5m]) 与
// histogram_quantile(0.95, ...)（deploy/monitoring/grafana/dashboards/http.json）；
// 桶定义在 internal/platform/metrics/metrics.go（.001~5 十档）。
